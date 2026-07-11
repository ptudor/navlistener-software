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
	"strings"
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

// pruneEvery is how often nav_frames_seq_seen (the regression fix replay-dedup ledger) is
// swept of entries older than the raw-retention window. Coarse cadence: the table
// is small (one row per historically-seen (source, feeder_seq) pair) and the
// window is days-scale, so hourly is more than enough to keep it bounded.
const pruneEvery = 1 * time.Hour

const defaultSeqSeenRetention = 7 * 24 * time.Hour // mirrors the "7 days" RawRetention default

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

	// SourceSeq is the feeder's GNF1 global sequence (push-path only, HasSourceSeq
	// true). It is the dedup key for reconnect replay : a feeder replays
	// every DATA frame past its last ack, and decode/live-state tolerate the
	// duplicate, but the historian must not persist it twice. Dial-mode frames
	// carry no such sequence and always persist (HasSourceSeq false).
	SourceSeq    uint64
	HasSourceSeq bool
}

// Store owns the connection pool and the batched writer.
type Store struct {
	pool             *pgxpool.Pool
	in               chan *NavFrame
	batchSize        int
	batchEvery       time.Duration
	retry            flushRetry
	log              *slog.Logger
	seqSeenRetention time.Duration // prune window for nav_frames_seq_seen 
	copy             copyRowsFunc  // defaults to s.copyRows (pool-backed); tests substitute a fake
	atomicPersist    bool          // production: claim replay keys and copy rows in one transaction
	shutdownBudget   time.Duration // defaults to shutdownFlushBudget; tests shrink it to run fast
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
	if err := verifyRequiredColumns(cctx, pool); err != nil {
		pool.Close()
		return nil, err
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
	seqSeenRetention := defaultSeqSeenRetention
	if d, err := parseSimpleInterval(cfg.RawRetention); err == nil {
		seqSeenRetention = d
	}
	s := &Store{
		pool:             pool,
		in:               make(chan *NavFrame, queueDepth),
		batchSize:        bs,
		batchEvery:       be,
		retry:            defaultFlushRetry,
		log:              log,
		seqSeenRetention: seqSeenRetention,
		atomicPersist:    true,
		shutdownBudget:   shutdownFlushBudget,
	}
	s.copy = s.copyRows
	return s, nil
}

// parseSimpleInterval parses the same "N minute(s)/hour(s)/day(s)/week(s)" shape
// applyPolicies validates (intervalRe) into a time.Duration.
func parseSimpleInterval(s string) (time.Duration, error) {
	var n int
	var unit string
	if _, err := fmt.Sscanf(s, "%d %s", &n, &unit); err != nil {
		return 0, err
	}
	unit = strings.TrimSuffix(unit, "s")
	var per time.Duration
	switch unit {
	case "minute":
		per = time.Minute
	case "hour":
		per = time.Hour
	case "day":
		per = 24 * time.Hour
	case "week":
		per = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("unknown interval unit %q", unit)
	}
	return time.Duration(n) * per, nil
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

// requiredColumns is the set of columns this build's WriteEvent/QueryEvents and
// snapshot writer actually read or write, per table. gnss_events/gnss_snapshots
// are `CREATE TABLE IF NOT EXISTS` : the DSN can point at a database where
// intsat already created these tables (docs §"superset of intsat's 001_init.sql"),
// in which case applySchema's CREATE TABLE is a silent no-op — if intsat's shape
// ever drifts from ours, code assuming the superset would fail at runtime on the
// first INSERT/SELECT touching a missing column, not at startup. This check turns
// that into a clear, fail-fast error instead.
var requiredColumns = map[string][]string{
	"gnss_events":    {"id", "time", "sv", "event_type", "old_value", "new_value", "severity", "message", "raw"},
	"gnss_snapshots": {"time", "endpoint", "data"},
}

// verifyRequiredColumns fails fast with an actionable message if a required table
// is missing a column this build reads or writes  — most likely because the
// DSN points at a database where gnss_events/gnss_snapshots pre-date this schema
// (e.g. an older intsat deployment) and CREATE TABLE IF NOT EXISTS left them as-is.
func verifyRequiredColumns(ctx context.Context, pool *pgxpool.Pool) error {
	for table, want := range requiredColumns {
		rows, err := pool.Query(ctx,
			`SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1`,
			table)
		if err != nil {
			return fmt.Errorf("verify schema: query columns of %s: %w", table, err)
		}
		have := make(map[string]bool)
		for rows.Next() {
			var col string
			if err := rows.Scan(&col); err != nil {
				rows.Close()
				return fmt.Errorf("verify schema: scan columns of %s: %w", table, err)
			}
			have[col] = true
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("verify schema: %s: %w", table, err)
		}
		var missing []string
		for _, col := range want {
			if !have[col] {
				missing = append(missing, col)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf(
				"table %q is missing column(s) %v that this build requires — "+
					"it likely pre-exists from an older/incompatible schema (e.g. intsat's 001_init.sql) "+
					"and CREATE TABLE IF NOT EXISTS left it unchanged; reconcile the table manually before starting navlistener",
				table, missing)
		}
	}
	return nil
}

// applyPolicies installs the columnar-compression and raw-retention policies at the
// configured intervals (idempotent; reset so the interval can change). The
// intervalRe check here is defense-in-depth : config.finalize applies the
// same config.IntervalRe so `-check-config` catches a malformed interval up front,
// but this guard against the later SQL-DDL interpolation stays regardless of
// caller.
func applyPolicies(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, cfg config.Store) error {
	compAfter, rawRet := cfg.CompressAfter, cfg.RawRetention
	if compAfter == "" {
		compAfter = "1 day"
	}
	if rawRet == "" {
		rawRet = "7 days"
	}
	if !config.IntervalRe.MatchString(compAfter) || !config.IntervalRe.MatchString(rawRet) {
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
	// raw BYTEA NOT NULL : Raw == nil maps to SQL NULL and would poison the
	// whole batch (bisected, quarantined, dropped with only a generic log). A
	// non-nil empty []byte{} stores fine as an empty bytea and is not guarded here.
	if f.Raw == nil {
		metrics.StoreEmptyRawTotal.Inc()
		s.log.Warn("dropping nav frame with nil Raw", "source", f.SourceID, "gnssid", f.GnssID, "svid", f.SvID)
		return
	}
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
	pruneTicker := time.NewTicker(pruneEvery)
	defer pruneTicker.Stop()

	flush := func() {
		if len(batch) == 0 {
			return
		}
		s.flush(batch, time.Now().Add(normalFlushBudget))
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
		case <-pruneTicker.C:
			s.pruneSeqSeen(ctx)
		case <-ctx.Done():
			// One shared deadline for the entire shutdown drain, not a fresh budget
			// per chunk flush — a full queue at batchSize chunks would otherwise take
			// up to (queueDepth/batchSize)*shutdownFlushBudget, far past
			// ShutdownTimeout, and main's os.Exit would kill the store mid-flush.
			deadline := time.Now().Add(s.shutdownBudget)
			s.drain(&batch, deadline)
			s.flush(batch, deadline)
			if s.pool != nil { // nil only in tests that construct a Store without New()
				s.pool.Close()
			}
			s.log.Info("store drained and closed")
			return
		}
	}
}

// pruneSeqSeen deletes replay-dedup ledger entries older than the raw-retention
// window : once nav_frames itself has retired a chunk that old, there's
// nothing left for a stale entry to deduplicate against.
func (s *Store) pruneSeqSeen(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cutoff := time.Now().Add(-s.seqSeenRetention)
	tag, err := s.pool.Exec(cctx, `DELETE FROM nav_frames_seq_seen WHERE seen_at < $1`, cutoff)
	if err != nil {
		s.log.Warn("nav_frames_seq_seen prune failed", "error", err)
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		s.log.Info("nav_frames_seq_seen pruned", "rows", n, "cutoff", cutoff)
	}
}

// drain empties the in-flight queue, flushing full-size chunks against the shared
// shutdown deadline (not a fresh budget per chunk).
func (s *Store) drain(batch *[]*NavFrame, deadline time.Time) {
	for {
		select {
		case f := <-s.in:
			*batch = append(*batch, f)
			if len(*batch) >= s.batchSize {
				s.flush(*batch, deadline)
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

// seqKey identifies one feeder-assigned sequence for the regression fix replay-dedup ledger.
type seqKey struct {
	source string
	seq    uint64
}

// persistAtomicOnce claims replay keys and inserts their corresponding raw rows in
// one transaction. A failed CopyFrom or commit rolls the claims back, so reconnect
// replay can retry them. Existing claims are durable proof that the row committed
// in an earlier transaction and are therefore omitted. Duplicate keys inside one
// batch are also emitted only once.
func (s *Store) persistAtomicOnce(ctx context.Context, batch []*NavFrame) (written int64, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	unique := make([]seqKey, 0, len(batch))
	seen := make(map[seqKey]bool, len(batch))
	for _, f := range batch {
		if !f.HasSourceSeq {
			continue
		}
		k := seqKey{f.SourceID, f.SourceSeq}
		if !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}

	fresh := make(map[seqKey]bool, len(unique))
	if len(unique) > 0 {
		sources := make([]string, len(unique))
		seqs := make([]int64, len(unique))
		for i, k := range unique {
			sources[i], seqs[i] = k.source, int64(k.seq)
		}
		rows, qerr := tx.Query(ctx,
			`INSERT INTO nav_frames_seq_seen (source_id, feeder_seq)
			 SELECT * FROM unnest($1::text[], $2::bigint[])
			 ON CONFLICT (source_id, feeder_seq) DO NOTHING
			 RETURNING source_id, feeder_seq`, sources, seqs)
		if qerr != nil {
			return 0, qerr
		}
		for rows.Next() {
			var src string
			var seq int64
			if qerr = rows.Scan(&src, &seq); qerr != nil {
				rows.Close()
				return 0, qerr
			}
			fresh[seqKey{src, uint64(seq)}] = true
		}
		qerr = rows.Err()
		rows.Close()
		if qerr != nil {
			return 0, qerr
		}
	}

	copyRows := make([][]any, 0, len(batch))
	emitted := make(map[seqKey]bool, len(fresh))
	for _, f := range batch {
		if f.HasSourceSeq {
			k := seqKey{f.SourceID, f.SourceSeq}
			if !fresh[k] || emitted[k] {
				continue
			}
			emitted[k] = true
		}
		copyRows = append(copyRows, navFrameToRow(f))
	}
	if len(copyRows) > 0 {
		written, err = tx.CopyFrom(ctx, pgx.Identifier{"nav_frames"}, copyColumns, pgx.CopyFromRows(copyRows))
		if err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return written, nil
}

func (s *Store) persistAtomicRetry(ctx context.Context, batch []*NavFrame) (written int64, dropped int) {
	if len(batch) == 0 {
		return 0, 0
	}
	backoff := s.retry.backoff
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, s.retry.attemptTO)
		n, err := s.persistAtomicOnce(cctx, batch)
		cancel()
		if err == nil {
			return n, 0
		}
		metrics.StoreErrorsTotal.Inc()
		if isPoison(err) {
			if len(batch) == 1 {
				s.log.Error("store quarantined a poison row", "error", err)
				return 0, 1
			}
			mid := len(batch) / 2
			w1, d1 := s.persistAtomicRetry(ctx, batch[:mid])
			w2, d2 := s.persistAtomicRetry(ctx, batch[mid:])
			return w1 + w2, d1 + d2
		}
		s.log.Warn("store flush failed; will retry", "error", err, "rows", len(batch), "attempt", attempt)
		if attempt >= s.retry.attempts || ctx.Err() != nil {
			s.log.Error("store flush giving up; leaving batch replayable", "rows", len(batch), "attempts", attempt)
			return 0, len(batch)
		}
		if !sleepCtx(ctx, backoff) {
			return 0, len(batch)
		}
		backoff *= 2
	}
}

// checkSeqSeen upserts keys into nav_frames_seq_seen in one round trip and returns
// the subset that were newly inserted (i.e. not a replay of an already-persisted
// sequence). ON CONFLICT DO NOTHING + RETURNING means a key already present in the
// ledger is silently absent from the result — exactly the "already stored, drop
// this replay" signal dedupBatch needs.
func (s *Store) checkSeqSeen(ctx context.Context, keys []seqKey) (map[seqKey]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	sources := make([]string, len(keys))
	seqs := make([]int64, len(keys))
	for i, k := range keys {
		sources[i] = k.source
		seqs[i] = int64(k.seq)
	}
	rows, err := s.pool.Query(ctx,
		`INSERT INTO nav_frames_seq_seen (source_id, feeder_seq)
		 SELECT * FROM unnest($1::text[], $2::bigint[])
		 ON CONFLICT (source_id, feeder_seq) DO NOTHING
		 RETURNING source_id, feeder_seq`,
		sources, seqs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fresh := make(map[seqKey]bool, len(keys))
	for rows.Next() {
		var src string
		var seq int64
		if err := rows.Scan(&src, &seq); err != nil {
			return nil, err
		}
		fresh[seqKey{src, uint64(seq)}] = true
	}
	return fresh, rows.Err()
}

// dedupBatch drops replayed push-path frames: an entry with a feeder sequence not
// present in fresh was already persisted on a prior flush. Frames without
// a sequence (dial-mode ingest) always pass through — replay dedup only applies to
// the push path's ack/retransmit contract; duplicates across different receivers
// remain intentional. Returns a new slice; does not alias batch's backing array.
func dedupBatch(batch []*NavFrame, fresh map[seqKey]bool) []*NavFrame {
	out := make([]*NavFrame, 0, len(batch))
	for _, f := range batch {
		if !f.HasSourceSeq || fresh[seqKey{f.SourceID, f.SourceSeq}] {
			out = append(out, f)
		}
	}
	return out
}

// flush bulk-loads a batch with bounded retry + poison-row quarantine on a detached
// context (so the final shutdown flush still runs) capped by deadline. Callers in a
// shutdown drain pass the *same* deadline to every flush call  so the total
// shutdown flush time is bounded by one budget, not a fresh one per chunk. Push-path
// frames are first deduplicated against nav_frames_seq_seen; a dedup-ledger
// failure fails open (persists the batch un-deduped) — losing the forensic record
// is worse than an occasional duplicate row.
func (s *Store) flush(batch []*NavFrame, deadline time.Time) {
	if len(batch) == 0 {
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if s.atomicPersist {
		written, dropped := s.persistAtomicRetry(ctx, batch)
		if written > 0 {
			metrics.StoreRowsTotal.Add(float64(written))
		}
		if dropped > 0 {
			metrics.StoreQuarantinedTotal.Add(float64(dropped))
		}
		return
	}

	var keys []seqKey
	for _, f := range batch {
		if f.HasSourceSeq {
			keys = append(keys, seqKey{f.SourceID, f.SourceSeq})
		}
	}
	if len(keys) > 0 {
		fresh, err := s.checkSeqSeen(ctx, keys)
		if err != nil {
			s.log.Warn("nav_frames_seq_seen check failed; persisting batch un-deduped", "error", err)
		} else {
			batch = dedupBatch(batch, fresh)
			if len(batch) == 0 {
				return
			}
		}
	}

	rows := make([][]any, 0, len(batch))
	for _, f := range batch {
		rows = append(rows, navFrameToRow(f))
	}
	written, dropped := persistRetry(ctx, s.retry, s.copy, rows, s.log)
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
