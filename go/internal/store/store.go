// Package store is the PERSIST stage: a TimescaleDB raw-nav-frame historian fed by
// a single batched writer goroutine using pgx CopyFrom (bulk load — never row-by-row
// INSERT). It is off the live hot path: frames are handed over via a bounded queue
// with an explicit drop-on-overflow policy, so a slow database degrades the
// historian, never live decoding. Same discipline as the radiolistener sibling.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
)

//go:embed schema.sql
var schemaSQL string

// queueDepth bounds the in-flight frames awaiting a flush.
const queueDepth = 16384

// Flush retry/quarantine budgets: a failed CopyFrom is retried with bounded backoff
// (transient DB blips) rather than discarding a batch of the forensic record on the
// first error. Budgets are wall-clock caps so the final shutdown flush stays within
// ShutdownTimeout even when the DB is down.
const (
	normalFlushBudget   = 35 * time.Second
	shutdownFlushBudget = 5 * time.Second
)

var defaultFlushRetry = flushRetry{attempts: 3, backoff: 250 * time.Millisecond, attemptTO: 10 * time.Second}

type flushRetry struct {
	attempts  int
	backoff   time.Duration
	attemptTO time.Duration
}

// copyRowsFunc bulk-loads rows and returns the count written. The pool's CopyFrom
// satisfies it in production; tests substitute a fake so the retry/quarantine policy
// needs no database.
type copyRowsFunc func(ctx context.Context, rows [][]any) (int64, error)

var copyColumns = []string{
	"ts", "received_at", "source_id", "gnssid", "svid", "sigid", "msg_type",
	"raw", "decoded", "decoder_ver",
}

// NavFrame is one raw broadcast nav frame to persist, with its optional decoded
// projection. Raw is the untouched frame bytes (re-decodable); Decoded is a JSONB
// projection or nil.
type NavFrame struct {
	Ts         time.Time
	ReceivedAt time.Time
	SourceID   string
	GnssID     int
	SvID       int
	SigID      int
	MsgType    int
	Raw        []byte
	Decoded    []byte // JSON, or nil
	DecoderVer string
}

// Store owns the connection pool and the batched writer.
type Store struct {
	pool       *pgxpool.Pool
	in         chan *NavFrame
	batchSize  int
	batchEvery time.Duration
	retry      flushRetry
	log        *slog.Logger
}

// New connects, applies the schema idempotently, installs the compression/retention
// policies, and returns a ready store. The caller runs Run in a goroutine and feeds
// it with Enqueue.
func New(ctx context.Context, cfg config.Store, log *slog.Logger) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(cctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	if err := requireTimescaleDB(cctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	if err := applySchema(cctx, pool); err != nil {
		pool.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := applyPolicies(cctx, pool, log, cfg); err != nil {
		pool.Close()
		return nil, fmt.Errorf("policies: %w", err)
	}
	bs := cfg.BatchSize
	if bs < 1 {
		bs = 1000
	}
	be := cfg.BatchEvery
	if be <= 0 {
		be = time.Second
	}
	return &Store{
		pool:       pool,
		in:         make(chan *NavFrame, queueDepth),
		batchSize:  bs,
		batchEvery: be,
		retry:      defaultFlushRetry,
		log:        log,
	}, nil
}

// requireTimescaleDB fails fast with an actionable message when the extension is
// absent. The nav_frames historian is a hypertable with columnar compression and a
// retention policy — plain PostgreSQL cannot satisfy the schema.
func requireTimescaleDB(ctx context.Context, pool *pgxpool.Pool) error {
	var ver string
	err := pool.QueryRow(ctx,
		`SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'`).Scan(&ver)
	if errors.Is(err, pgx.ErrNoRows) {
		return errors.New("TimescaleDB extension not installed in this database — " +
			"navlistener requires it; a superuser must run: CREATE EXTENSION timescaledb")
	}
	if err != nil {
		return fmt.Errorf("check timescaledb: %w", err)
	}
	return nil
}

// applySchema runs the multi-statement schema via the simple protocol.
func applySchema(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	_, err = conn.Conn().PgConn().Exec(ctx, schemaSQL).ReadAll()
	return err
}

var intervalRe = regexp.MustCompile(`^[1-9][0-9]* (minute|hour|day|week)s?$`)

// applyPolicies installs the columnar-compression and raw-retention policies at the
// configured intervals (idempotent; reset so the interval can change).
func applyPolicies(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, cfg config.Store) error {
	compAfter, rawRet := cfg.CompressAfter, cfg.RawRetention
	if compAfter == "" {
		compAfter = "1 day"
	}
	if rawRet == "" {
		rawRet = "7 days"
	}
	if !intervalRe.MatchString(compAfter) || !intervalRe.MatchString(rawRet) {
		return fmt.Errorf("intervals must be simple like \"7 days\" (compress_after=%q raw_retention=%q)", compAfter, rawRet)
	}

	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	exec := func(sql string) error {
		_, err := conn.Conn().PgConn().Exec(ctx, sql).ReadAll()
		return err
	}

	if err := exec(`SELECT remove_compression_policy('nav_frames', if_exists => true)`); err != nil {
		return err
	}
	if err := exec(fmt.Sprintf(`SELECT add_compression_policy('nav_frames', INTERVAL '%s', if_not_exists => true)`, compAfter)); err != nil {
		return fmt.Errorf("compression policy: %w", err)
	}
	if err := exec(`SELECT remove_retention_policy('nav_frames', if_exists => true)`); err != nil {
		return err
	}
	if err := exec(fmt.Sprintf(`SELECT add_retention_policy('nav_frames', INTERVAL '%s', if_not_exists => true)`, rawRet)); err != nil {
		return fmt.Errorf("retention policy: %w", err)
	}

	// Feed snapshots are the light replay/backfill record: compress after 7 days,
	// drop after 90 days (docs/OUTPUT.md §4). Events (gnss_events) carry no retention
	// policy — confirmed integrity transitions are the durable record.
	if err := exec(`SELECT add_compression_policy('gnss_snapshots', INTERVAL '7 days', if_not_exists => true)`); err != nil {
		return fmt.Errorf("snapshot compression policy: %w", err)
	}
	if err := exec(`SELECT add_retention_policy('gnss_snapshots', INTERVAL '90 days', if_not_exists => true)`); err != nil {
		return fmt.Errorf("snapshot retention policy: %w", err)
	}
	log.Info("historian policies applied", "compress_after", compAfter, "raw_retention", rawRet)
	return nil
}

// Enqueue hands a frame to the writer without blocking the caller. On overflow (the
// DB can't keep up) it drops the newest and records it — the live path stays healthy.
func (s *Store) Enqueue(f *NavFrame) {
	select {
	case s.in <- f:
	default:
		metrics.StoreDroppedTotal.Inc()
	}
}

// Run is the batched writer loop. It flushes on a size or time threshold and, on
// shutdown, drains and flushes the remainder before closing the pool.
func (s *Store) Run(ctx context.Context) {
	batch := make([]*NavFrame, 0, s.batchSize)
	ticker := time.NewTicker(s.batchEvery)
	defer ticker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.flush(batch, normalFlushBudget)
		batch = batch[:0]
	}

	for {
		select {
		case f := <-s.in:
			batch = append(batch, f)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-ctx.Done():
			s.drain(&batch, shutdownFlushBudget)
			s.flush(batch, shutdownFlushBudget)
			s.pool.Close()
			s.log.Info("store drained and closed")
			return
		}
	}
}

func (s *Store) drain(batch *[]*NavFrame, budget time.Duration) {
	for {
		select {
		case f := <-s.in:
			*batch = append(*batch, f)
			if len(*batch) >= s.batchSize {
				s.flush(*batch, budget)
				*batch = (*batch)[:0]
			}
		default:
			return
		}
	}
}

func (s *Store) copyRows(ctx context.Context, rows [][]any) (int64, error) {
	return s.pool.CopyFrom(ctx, pgx.Identifier{"nav_frames"}, copyColumns, pgx.CopyFromRows(rows))
}

// flush bulk-loads a batch with bounded retry + poison-row quarantine on a detached
// context (so the final shutdown flush still runs) capped by budget.
func (s *Store) flush(batch []*NavFrame, budget time.Duration) {
	if len(batch) == 0 {
		return
	}
	rows := make([][]any, 0, len(batch))
	for _, f := range batch {
		rows = append(rows, navFrameToRow(f))
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	written, dropped := persistRetry(ctx, s.retry, s.copyRows, rows, s.log)
	if written > 0 {
		metrics.StoreRowsTotal.Add(float64(written))
	}
	if dropped > 0 {
		metrics.StoreQuarantinedTotal.Add(float64(dropped))
	}
}

// persistRetry bulk-loads rows, retrying retryable failures with bounded backoff and
// quarantining poison rows. Returns the rows written and permanently dropped.
func persistRetry(ctx context.Context, r flushRetry, copy copyRowsFunc, rows [][]any, log *slog.Logger) (written int64, dropped int) {
	if len(rows) == 0 {
		return 0, 0
	}
	backoff := r.backoff
	for attempt := 1; ; attempt++ {
		n, err := copyOnce(ctx, r.attemptTO, copy, rows)
		if err == nil {
			return n, 0
		}
		metrics.StoreErrorsTotal.Inc()
		if isPoison(err) {
			return quarantine(ctx, r, copy, rows, log, err)
		}
		log.Warn("store flush failed; will retry", "error", err, "rows", len(rows), "attempt", attempt)
		if attempt >= r.attempts || ctx.Err() != nil {
			log.Error("store flush giving up; dropping batch", "rows", len(rows), "attempts", attempt)
			return 0, len(rows)
		}
		if !sleepCtx(ctx, backoff) {
			log.Error("store flush budget exhausted; dropping batch", "rows", len(rows))
			return 0, len(rows)
		}
		backoff *= 2
	}
}

func copyOnce(ctx context.Context, to time.Duration, copy copyRowsFunc, rows [][]any) (int64, error) {
	cctx, cancel := context.WithTimeout(ctx, to)
	defer cancel()
	return copy(cctx, rows)
}

// quarantine isolates the poison row(s) in a deterministically-failing batch by
// bisection, so one bad row can't wedge the writer or take the batch down with it.
func quarantine(ctx context.Context, r flushRetry, copy copyRowsFunc, rows [][]any, log *slog.Logger, cause error) (written int64, dropped int) {
	if len(rows) == 1 {
		log.Error("store quarantined a poison row", "error", cause)
		return 0, 1
	}
	if ctx.Err() != nil {
		return 0, len(rows)
	}
	mid := len(rows) / 2
	w1, d1 := persistRetry(ctx, r, copy, rows[:mid], log)
	w2, d2 := persistRetry(ctx, r, copy, rows[mid:], log)
	return w1 + w2, d1 + d2
}

// isPoison reports whether err is a deterministic, row-content error (data exception
// or integrity-constraint violation) that a retry can't fix; other failures
// (network, timeout, resource) are retryable.
func isPoison(err error) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && len(pg.Code) >= 2 {
		switch pg.Code[:2] {
		case "22", "23":
			return true
		}
	}
	return false
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// navFrameToRow maps a NavFrame to a CopyFrom row in copyColumns order.
func navFrameToRow(f *NavFrame) []any {
	var decoded any
	if len(f.Decoded) > 0 {
		decoded = string(f.Decoded)
	}
	return []any{
		f.Ts, f.ReceivedAt, f.SourceID,
		int16(f.GnssID), int16(f.SvID), int16(f.SigID), int16(f.MsgType),
		f.Raw, decoded, nilIfEmpty(f.DecoderVer),
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
