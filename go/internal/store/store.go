// Package store is the PERSIST stage: a TimescaleDB navigation/board historian fed by
// a single batched writer goroutine using pgx CopyFrom (bulk load — never row-by-row
// INSERT). It is off the live hot path: frames are handed over via a bounded queue
// with an explicit drop-on-overflow policy, so a slow database degrades the
// historian, never live decoding. Integrity events and periodic feed snapshots
// are lower-rate direct writes through the same store; navigation and board
// samples use the bounded batch queue. Same discipline as the radiolistener sibling.
package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
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
// first error. Budgets are wall-clock caps.
//
// the two budgets serve different masters. normalFlushBudget bounds a
// steady-state flush's retries, but a normal flush ALSO derives its context from
// storeCtx, so shutdown interrupts it immediately instead of waiting out up to
// ~30 s of retries the ≤15 s ShutdownTimeout can never cover — the interrupted
// batch is retained (not dropped) and re-flushed by the shutdown drain, which
// runs detached from storeCtx under the single shared shutdownFlushBudget
// (one deadline for the whole drain, never a fresh budget per chunk).
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

// Startup budgets. These were one shared 10 s context covering
// ping, extension check, schema migration, column verification AND policy
// replacement, so a slow earlier step could leave the policy work with almost no
// budget. Schema migration is the variable-cost step (an existing deployment may
// add columns or indexes); policy replacement is a short, fixed amount of work
// but is the one step whose interruption matters, so it gets its own reservation
// that earlier steps cannot spend.
const (
	schemaSetupBudget = 30 * time.Second
	policySetupBudget = 15 * time.Second
	// Bounded retry for a policy transaction that deadlocked against the
	// extension's own background job scheduler. Both fit inside policySetupBudget.
	policyRetryAttempts = 3
	policyRetryBackoff  = 250 * time.Millisecond
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

// provenanceColumns is how many leading copyColumns are the immutable receipt
// provenance that observer_samples shares with nav_frames (through
// policy_revision). Board rows reuse exactly that prefix.
const provenanceColumns = 23

var copyColumns = []string{
	"ts", "received_at", "source_id", "organization_id", "enrollment_id",
	"collector_instance_id", "collection_ids", "feed_grants", "declared_capabilities",
	"provenance", "credential_tier",
	"credential_fingerprint", "attestation_tier", "hardware_trust", "manufacturer_authority_id", "commissioning_fingerprint",
	"aggregate_use", "station_metadata", "event_visibility",
	"raw_export", "federation_peers", "publish_signals", "policy_revision",
	"gnssid", "svid", "sigid", "freqid", "msg_type",
	"raw", "decoded", "decoder_ver", "sbf_header", "source_session", "source_seq",
}

// NavFrame is one raw broadcast nav frame to persist, with its optional decoded
// projection. Raw is the untouched frame bytes (re-decodable); Decoded is a JSONB
// projection or nil.
type NavFrame struct {
	Board *BoardSample // non-nil routes to private observer_samples, never nav_frames

	Ts         time.Time
	ReceivedAt time.Time
	SourceID   string
	// The following fields are the immutable server-resolved ownership,
	// enrollment, and receipt-time publication decision. They are deliberately
	// denormalized so historical evidence never changes meaning after a transfer
	// or policy edit (docs/GROUPS-AND-FEDERATION.md §5.2/§5.4).
	OrganizationID        string
	EnrollmentID          string
	CollectorInstanceID   string
	CollectionIDs         []string
	FeedGrants            []string
	DeclaredCapabilities  []string
	Provenance            string
	CredentialTier        string
	CredentialFingerprint string
	AttestationTier       string
	// HardwareTrust and CommissioningFingerprint are what the collector verified
	// from the session's hardware evidence; "none" and empty for every source
	// that presented none.
	HardwareTrust            string
	ManufacturerAuthorityID  string
	CommissioningFingerprint string
	AggregateUse             string
	StationMetadata          string
	EventVisibility          string
	RawExport                string
	FederationPeers          []string
	PublishSignals           []string
	PolicyRevision           string
	GnssID                   int
	SvID                     int
	SigID                    int
	// FreqID is the GLONASS FDMA channel carrier as the receiver reported it
	// (k = FreqID - 7). Always written: it is receiver metadata that never appears
	// in Raw (RawBytes serialises only the nav words), so a GLONASS frame cannot be
	// replayed faithfully without it. Meaningless for other constellations, where
	// the ingest layer leaves it 0.
	FreqID     int
	MsgType    int
	SBFHeader  []byte // original eight-byte SBF header; nil for legacy/other inputs
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

	// Session is the feeder's GNF1 boot/session identity, the
	// third component of the dedup key (SourceID, Session, SourceSeq): a feeder
	// whose sequence space restarted presents a fresh session, so its new
	// seq 0.. can never be classified as a replay of the old ledger rows.
	// Empty only for dial-mode frames (which carry no sequence either).
	Session string
}

// Store owns the connection pool and the batched writer.
type Store struct {
	pool             *pgxpool.Pool
	closeOnce        sync.Once // guards Close() against Run's own pool shutdown
	in               chan *NavFrame
	batchSize        int
	batchEvery       time.Duration
	retry            flushRetry
	log              *slog.Logger
	seqSeenRetention time.Duration // prune window for nav_frames_seq_seen
	copy             copyRowsFunc  // defaults to s.copyRows (pool-backed); tests substitute a fake
	atomicPersist    bool          // production: claim replay keys and copy rows in one transaction
	shutdownBudget   time.Duration // defaults to shutdownFlushBudget; tests shrink it to run fast
	// persistOnce is the one-transaction claim+copy (s.persistAtomicOnce in
	// production); a seam so the retry/abort/retention policy around it
	// is unit-testable without a database, mirroring the copy seam above.
	persistOnce func(ctx context.Context, batch []*NavFrame) (int64, error)

	// flushFailStreak counts CONSECUTIVE flush cycles that exhausted their
	// bounded retries or wall budget; any successful persist resets it,
	// as does the regression fix idle-recovery probe below. It feeds Degraded() so
	// /healthz can report a historian that has been dropping the forensic record
	// for minutes, instead of staying green. Incremented at most ONCE per
	// top-level flush — a poison bisection can produce several
	// give-ups within one cycle, and counting each would reach Degraded()'s ≥2
	// threshold after one cycle instead of the intended two consecutive ones.
	flushFailStreak atomic.Int64

	// ping probes pool liveness for the regression fix idle-recovery check
	// (s.pingPool in production). It is a seam because tests construct a Store
	// with a nil pool; a nil ping simply disables the probe.
	ping func(ctx context.Context) error

	// lastIdleProbe is when the idle-recovery probe last ran. Touched ONLY by
	// the Run goroutine (from the flush closure), so it needs no
	// synchronisation — unlike flushFailStreak, which Degraded() reads from the
	// /healthz handler.
	lastIdleProbe time.Time

	// lastWriteAttempt is when a non-empty flush last ran (success or failure),
	// stamped at the top of flush(). The regression fix quiet gate: the idle probe may
	// clear a latched streak only when no write has been ATTEMPTED for a full
	// idleQuietWindow, because recent write attempts carry their own verdict.
	// Run-goroutine-only, like lastIdleProbe.
	lastWriteAttempt time.Time

	// onDurable, when set (SetDurableNotify), is called once per sequenced
	// frame the store has DURABLY RESOLVED — the regression fix ACK boundary:
	// the batch's transaction committed (including frames omitted as replays
	// of an already-committed claim), or a poison row was quarantined
	// (deterministic row-content failure a retransmit cannot fix), or a
	// nil-Raw frame was rejected at Enqueue (same unfixable class). It is
	// deliberately NOT called for retry-exhaustion/budget drops: those frames
	// were never committed, their ledger claims rolled back, and the feeder's
	// spool still holds them — withholding the ack is exactly what lets
	// reconnect replay redeliver them after the outage.
	onDurable func(source, session string, seq uint64)
}

// SetDurableNotify installs the regression fix durable-resolution callback. Must be
// called before Run (the writer goroutine reads the field without a lock).
func (s *Store) SetDurableNotify(fn func(source, session string, seq uint64)) { s.onDurable = fn }

// notifyDurable reports every sequenced frame in batch as durably resolved.
func (s *Store) notifyDurable(batch []*NavFrame) {
	if s.onDurable == nil {
		return
	}
	for _, f := range batch {
		if f.HasSourceSeq {
			s.onDurable(f.SourceID, f.Session, f.SourceSeq)
		}
	}
}

// New connects, applies the schema idempotently, installs the compression/retention
// policies, and returns a ready store. The caller runs Run in a goroutine and feeds
// it with Enqueue.
func New(ctx context.Context, cfg config.Store, log *slog.Logger) (*Store, error) {
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	// connect/schema work and policy work get separate budgets.
	// They used to share one 10 s context, so a slow ping, extension check, or
	// schema migration ate into the window the policy replacement needed — and a
	// policy replacement interrupted midway is the one step here that can leave
	// the database in a worse state than it started in.
	cctx, cancel := context.WithTimeout(ctx, schemaSetupBudget)
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
		// A pre-existing intsat-shaped gnss_events can make schema.sql's OWN
		// DDL fail first (gnss_events_public selects `raw`, which has no
		// ADD COLUMN IF NOT EXISTS migration) with a 42703 that names the
		// column but not the table. Run the column check so the operator
		// diagnostic names the table and the missing columns.
		if verr := verifyRequiredColumns(cctx, pool); verr != nil {
			pool.Close()
			return nil, fmt.Errorf("%w (schema application also failed: %v)", verr, err)
		}
		pool.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := verifyRequiredColumns(cctx, pool); err != nil {
		pool.Close()
		return nil, err
	}
	pctx, pcancel := context.WithTimeout(ctx, policySetupBudget)
	defer pcancel()
	if err := applyPolicies(pctx, pool, log, cfg); err != nil {
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
	// derive the replay-ledger horizon from the same parser that
	// validated the policy interval, and fail rather than silently falling back to
	// the default — a silent fallback is exactly how the database's retention and
	// the in-memory dedupe retention came to disagree. An empty value is the
	// documented "use the default" case (it matches applyPolicies' own "7 days").
	seqSeenRetention := defaultSeqSeenRetention
	if cfg.RawRetention != "" {
		d, err := parseSimpleInterval(cfg.RawRetention)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("raw_retention: %w", err)
		}
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
	s.persistOnce = s.persistAtomicOnce
	s.ping = s.pingPool
	return s, nil
}

// pingPool is the production regression fix health probe: a pool acquire + round trip,
// the cheapest honest "is the database reachable right now" question available.
func (s *Store) pingPool(ctx context.Context) error { return s.pool.Ping(ctx) }

// parseSimpleInterval is config.ParseInterval. this used to be a
// second, independent implementation of the same grammar with its own unchecked
// int conversion, so the replay-ledger horizon and `-check-config` could disagree
// about what a configured interval means. There is now exactly one parser, and
// this alias exists only so the store's call sites and tests read locally.
func parseSimpleInterval(s string) (time.Duration, error) { return config.ParseInterval(s) }

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
	"gnss_events":                 {"id", "time", "audience", "audience_seq", "redaction_class", "sv", "event_type", "old_value", "new_value", "severity", "message", "raw", "dedupe_key"},
	"gnss_event_audience_cursors": {"audience", "last_seq"},
	"gnss_snapshots":              {"time", "audience", "endpoint", "data"},
	// navlistener's own raw-frame tables get the same fail-fast drift check. A future
	// build that adds a copyColumns column against an existing deployment's older nav_frames
	// starts cleanly (CREATE TABLE IF NOT EXISTS is a no-op), then every CopyFrom fails 42703
	// undefined_column — class 42 is not poison, so each batch is retried and dropped forever
	// (silent forensic-record loss while /healthz stays OK). nav_frames_seq_seen is the
	// push-path dedup ledger; a drift there fails the atomic claim tx and drops the batch.
	"nav_frames":          copyColumns,
	"observer_samples":    boardColumns,
	"nav_frames_seq_seen": {"source_id", "session_id", "feeder_seq", "seen_at"},
}

// verifyRequiredColumns fails fast with an actionable message if a required table
// is missing a column this build reads or writes — most likely because the
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
	// regression fix follow-through: replacing all six policies in ONE transaction
	// holds locks on the extension's job catalog for the whole sequence, which can
	// deadlock against the background scheduler if it is executing one of those
	// very policies at startup. A deadlock is transient by definition — PostgreSQL
	// resolves it by aborting one side — and retrying is only safe *because* the
	// work is transactional: the aborted attempt changed nothing, so the retry
	// starts from the same state rather than from a half-replaced set.
	var err error
	for attempt := range policyRetryAttempts {
		err = applyPoliciesWithHook(ctx, pool, log, cfg, nil)
		if err == nil || !isTransientConflict(err) || ctx.Err() != nil {
			return err
		}
		log.Warn("historian policy replacement conflicted with a concurrent job; retrying",
			"attempt", attempt+1, "of", policyRetryAttempts, "error", err)
		if !sleepCtx(ctx, policyRetryBackoff) {
			return err
		}
	}
	return err
}

// isTransientConflict reports whether err is a deadlock or serialization failure
// (class 40), the two outcomes a retry of an all-or-nothing transaction fixes.
func isTransientConflict(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && len(pg.Code) >= 2 && pg.Code[:2] == "40"
}

// applyPoliciesWithHook is applyPolicies with a per-statement observation seam.
// The regression fix atomicity test uses it to interrupt the sequence at each
// remove/add boundary in turn; production always passes nil.
func applyPoliciesWithHook(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, cfg config.Store,
	afterExec func(sql string)) error {
	compAfter, rawRet := cfg.CompressAfter, cfg.RawRetention
	if compAfter == "" {
		compAfter = "1 day"
	}
	if rawRet == "" {
		rawRet = "7 days"
	}
	// config.ParseInterval re-applies config.IntervalRe, so this keeps its
	// defense-in-depth role as the injection guard for the DDL interpolation
	// below while additionally rejecting a syntactically valid but
	// unrepresentable magnitude before it reaches the database —
	// the case that let the policy and the Go-side horizon diverge.
	if _, err := config.ParseInterval(compAfter); err != nil {
		return fmt.Errorf("compress_after: %w", err)
	}
	if _, err := config.ParseInterval(rawRet); err != nil {
		return fmt.Errorf("raw_retention: %w", err)
	}

	// every replacement runs in ONE transaction. Each policy is
	// changed by remove-then-add, so as twelve autocommit statements any error,
	// timeout, or interruption between a remove and its add left that hypertable
	// with NO compression or NO retention. Startup returned an error, but the
	// database was already mutated — and if the previous binary kept serving, the
	// deploy rolled back, or the restart was delayed, raw frames grew unbounded or
	// stayed uncompressed with nothing reporting it.
	//
	// TimescaleDB's add_*/remove_*_policy are ordinary transactional functions
	// (verified against 2.25 on the deploy target: a ROLLBACK restores the exact
	// previous policy), so commit-or-nothing is achievable directly rather than by
	// compensating restoration.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once committed
	exec := func(sql string) error {
		_, err := tx.Exec(ctx, sql)
		if afterExec != nil {
			afterExec(sql)
		}
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
	// drop after 90 days (docs/OUTPUT.md §4). remove-then-add (the
	// nav_frames pattern above) so a future change to these interval constants
	// actually applies on an existing deployment — add_* with if_not_exists is a
	// silent no-op when a policy already exists.
	if err := exec(`SELECT remove_compression_policy('gnss_snapshots', if_exists => true)`); err != nil {
		return err
	}
	if err := exec(`SELECT add_compression_policy('gnss_snapshots', INTERVAL '7 days', if_not_exists => true)`); err != nil {
		return fmt.Errorf("snapshot compression policy: %w", err)
	}
	if err := exec(`SELECT remove_retention_policy('gnss_snapshots', if_exists => true)`); err != nil {
		return err
	}
	if err := exec(`SELECT add_retention_policy('gnss_snapshots', INTERVAL '90 days', if_not_exists => true)`); err != nil {
		return fmt.Errorf("snapshot retention policy: %w", err)
	}
	// events are retention-less BY DESIGN (durability of the confirmed
	// integrity record) — compression only, never a retention policy here.
	if err := exec(`SELECT remove_compression_policy('gnss_events', if_exists => true)`); err != nil {
		return err
	}
	if err := exec(`SELECT add_compression_policy('gnss_events', INTERVAL '30 days', if_not_exists => true)`); err != nil {
		return fmt.Errorf("events compression policy: %w", err)
	}
	// Board samples share the raw evidence retention/dedup horizon. Their own
	// table and source/kind compression keep timing separate from navigation.
	for _, sql := range []string{
		`SELECT remove_compression_policy('observer_samples', if_exists => true)`,
		fmt.Sprintf(`SELECT add_compression_policy('observer_samples', INTERVAL '%s', if_not_exists => true)`, compAfter),
		`SELECT remove_retention_policy('observer_samples', if_exists => true)`,
		fmt.Sprintf(`SELECT add_retention_policy('observer_samples', INTERVAL '%s', if_not_exists => true)`, rawRet),
	} {
		if err := exec(sql); err != nil {
			return fmt.Errorf("board sample policy: %w", err)
		}
	}

	// Nothing above is visible to any other session until this commits, so the
	// installed set is either entirely the old one or entirely the new one.
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit policies: %w", err)
	}
	log.Info("historian policies applied", "compress_after", compAfter, "raw_retention", rawRet)
	return nil
}

// Idle-recovery probe budgets. idleHealthPingTO bounds one probe (the
// Run goroutine is not reading its queue while it blocks, so it must be short);
// idleHealthEvery bounds how often a probe runs, because the batch timer can tick
// every second and a probe against a still-down DB costs a connection acquire and
// a round trip each time. idleQuietWindow is the regression fix gate: the probe clears a
// latched streak only after a full minute with NO write attempts, so it can never
// override the verdict of flushes that are actively failing (see
// clearStreakIfIdleHealthy). A minute comfortably exceeds any flush cadence
// (normalFlushBudget is ~35 s) while keeping the genuine ingest-silent recovery
// bounded at ~idleQuietWindow + idleHealthEvery.
const (
	idleHealthPingTO = 2 * time.Second
	idleHealthEvery  = 10 * time.Second
	idleQuietWindow  = time.Minute
)

// clearStreakIfIdleHealthy is the regression fix idle-recovery probe. flushFailStreak is
// otherwise cleared ONLY by a successful non-empty persist, so a database that
// fails for ≥2 cycles and then recovers during an ingest-silent window leaves
// Degraded() latched: /healthz keeps reporting "frames are being dropped" against
// a healthy DB until some frame happens to arrive. On an empty-batch cycle we can
// ask the pool directly instead — a passing Ping is proof the historian is not
// currently failing, so the stale streak is cleared.
//
// Deliberately narrow: it runs only while the streak is non-zero (a healthy idle
// daemon never touches the DB on this path) and only on an empty batch (a
// non-empty cycle carries its own verdict, which must not be overridden by a
// Ping that succeeds while CopyFrom fails — e.g. a disk-full or schema fault).
//
// the empty-batch condition alone is not enough. Under a write-side-only
// fault (failover to a read-only standby, disk full, revoked INSERT) Ping keeps
// passing while every flush fails, and at any frame rate low enough for the batch
// to empty between failing cycles the probe would zero the streak between them —
// Degraded() would never reach 2 and /healthz would report healthy through an
// outage that is dropping every frame. The probe therefore also requires a full
// idleQuietWindow with no write ATTEMPT at all: recent attempts, failed or not,
// own the verdict; the probe speaks only for genuinely ingest-silent stretches.
func (s *Store) clearStreakIfIdleHealthy(ctx context.Context, now time.Time) {
	if s.ping == nil || s.flushFailStreak.Load() == 0 {
		return
	}
	if !s.lastWriteAttempt.IsZero() && now.Sub(s.lastWriteAttempt) < idleQuietWindow {
		return // writes were attempted recently: their verdict stands
	}
	if !s.lastIdleProbe.IsZero() && now.Sub(s.lastIdleProbe) < idleHealthEvery {
		return
	}
	s.lastIdleProbe = now
	cctx, cancel := context.WithTimeout(ctx, idleHealthPingTO)
	defer cancel()
	if err := s.ping(cctx); err != nil {
		return // still down: the latched streak is telling the truth
	}
	s.flushFailStreak.Store(0)
	s.log.Info("historian reachable again during an ingest-idle window; clearing degraded status")
}

// Degraded reports a non-empty reason while the batched writer is persistently
// failing : two or more CONSECUTIVE flush cycles exhausted their bounded
// retries or wall budget — i.e. the historian has been unable to persist for over
// a minute (each cycle is ~35 s of retries) and is dropping the forensic record.
// A single give-up (a transient blip the next cycle absorbs) does not degrade, so
// /healthz cannot flap on one bad flush. Registered as a health probe in main.
func (s *Store) Degraded() string {
	if n := s.flushFailStreak.Load(); n >= 2 {
		return fmt.Sprintf("historian: %d consecutive flush cycles failed; frames are being dropped", n)
	}
	return ""
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
		// unfixable by retransmit (the frame's content is the problem);
		// resolve it so it can be acked instead of wedging the watermark.
		s.notifyDurable([]*NavFrame{f})
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
			// no work to do, but a latched failure streak may be stale —
			// probe the pool so a DB that recovered while ingest was silent does
			// not keep /healthz reporting the historian degraded.
			s.clearStreakIfIdleHealthy(ctx, time.Now())
			return
		}
		// once shutdown has begun, never START a new normal-budget flush —
		// fall through to the ctx.Done() branch, whose drain flushes everything
		// under the single bounded shutdownBudget instead.
		if ctx.Err() != nil {
			return
		}
		// The normal flush runs under ctx (so storeCancel interrupts it mid-retry
		// instead of it blocking shutdown for up to ~30 s of DB retries) capped by
		// its own wall budget. false = interrupted with nothing resolved: keep the
		// batch for the shutdown drain rather than discarding it.
		if s.flush(ctx, batch, time.Now().Add(normalFlushBudget)) {
			batch = batch[:0]
		}
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
			// The drain's flushes run on a DETACHED context (Background) bounded only
			// by the deadline: ctx is already cancelled here by definition, and this
			// is the one bounded chance the queued forensic record gets.
			deadline := time.Now().Add(s.shutdownBudget)
			s.drain(&batch, deadline)
			s.flush(context.Background(), batch, deadline)
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
// shutdown deadline (not a fresh budget per chunk). Shutdown-only: its
// flushes run detached (Background) because the store's own context is already
// cancelled when drain is reached.
func (s *Store) drain(batch *[]*NavFrame, deadline time.Time) {
	for {
		select {
		case f := <-s.in:
			*batch = append(*batch, f)
			if len(*batch) >= s.batchSize {
				s.flush(context.Background(), *batch, deadline)
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

// seqKey identifies one feeder-assigned sequence for the regression fix replay-dedup
// ledger. session partitions one observer's sequence spaces across feeder
// boots — two frames with equal (source, seq) but different
// sessions are DIFFERENT frames, never replays of each other.
type seqKey struct {
	source  string
	session string
	seq     uint64
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
		k := seqKey{f.SourceID, f.Session, f.SourceSeq}
		if !seen[k] {
			seen[k] = true
			unique = append(unique, k)
		}
	}

	fresh := make(map[seqKey]bool, len(unique))
	if len(unique) > 0 {
		sources := make([]string, len(unique))
		sessions := make([]string, len(unique))
		seqs := make([]int64, len(unique))
		for i, k := range unique {
			sources[i], sessions[i], seqs[i] = k.source, k.session, int64(k.seq)
		}
		rows, qerr := tx.Query(ctx,
			`INSERT INTO nav_frames_seq_seen (source_id, session_id, feeder_seq)
			 SELECT * FROM unnest($1::text[], $2::text[], $3::bigint[])
			 ON CONFLICT (source_id, session_id, feeder_seq) DO NOTHING
			 RETURNING source_id, session_id, feeder_seq`, sources, sessions, seqs)
		if qerr != nil {
			return 0, qerr
		}
		for rows.Next() {
			var src, sess string
			var seq int64
			if qerr = rows.Scan(&src, &sess, &seq); qerr != nil {
				rows.Close()
				return 0, qerr
			}
			fresh[seqKey{src, sess, uint64(seq)}] = true
		}
		qerr = rows.Err()
		rows.Close()
		if qerr != nil {
			return 0, qerr
		}
	}

	copyRows := make([][]any, 0, len(batch))
	boardRows := make([][]any, 0, len(batch))
	boardCounts := map[string]int{}
	emitted := make(map[seqKey]bool, len(fresh))
	for _, f := range batch {
		if f.HasSourceSeq {
			k := seqKey{f.SourceID, f.Session, f.SourceSeq}
			if !fresh[k] || emitted[k] {
				continue
			}
			emitted[k] = true
		}
		if f.Board != nil {
			boardRows = append(boardRows, boardFrameToRow(f))
			boardCounts[f.Board.Kind]++
		} else {
			copyRows = append(copyRows, navFrameToRow(f))
		}
	}
	if len(copyRows) > 0 {
		written, err = tx.CopyFrom(ctx, pgx.Identifier{"nav_frames"}, copyColumns, pgx.CopyFromRows(copyRows))
		if err != nil {
			return 0, err
		}
	}
	if len(boardRows) > 0 {
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"observer_samples"}, boardColumns, pgx.CopyFromRows(boardRows))
		if err != nil {
			return 0, err
		}
		written += n
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	for kind, n := range boardCounts {
		metrics.StoreBoardRowsTotal.WithLabelValues(kind).Add(float64(n))
	}

	return written, nil
}

// persistAtomicRetry drives persistOnce with bounded retry and poison-row
// bisection. aborted=true means ctx was cut (parent cancellation or
// deadline expiry) before ANY row of this call's batch was resolved — the
// invariant "aborted ⇒ written==0 && dropped==0" lets flush() safely retain the
// whole batch for a bounded re-flush (the rolled-back transaction released every
// replay-key claim, so a re-flush cannot duplicate rows). Once anything has been
// resolved (a committed sub-batch, a quarantined poison row), retention is no
// longer safe — re-flushing would duplicate the committed dial-mode rows — so an
// interruption after partial work counts the remainder dropped instead.
//
// gaveUp reports that at least one (sub-)batch exhausted its retry
// budget on a transient error. It is REPORTED, not counted, here: poison-row
// bisection recurses, and incrementing flushFailStreak inside each recursive
// give-up let one top-level flush bump the streak twice (two transient-failing
// halves), reaching Degraded()'s ≥2 threshold after effectively one cycle rather
// than the intended two consecutive ones. flush() folds it into a single Add.
// Note aborted ⇒ !gaveUp by construction: aborted means nothing in this subtree
// was resolved, and a give-up resolves rows (as dropped).
func (s *Store) persistAtomicRetry(ctx context.Context, batch []*NavFrame) (written int64, dropped int, aborted, gaveUp bool) {
	if len(batch) == 0 {
		return 0, 0, false, false
	}
	backoff := s.retry.backoff
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, s.retry.attemptTO)
		n, err := s.persistOnce(cctx, batch)
		cancel()
		if err == nil {
			s.flushFailStreak.Store(0) // any successful persist ends a failure streak
			// the transaction committed — every sequenced frame in the
			// batch is durably resolved (persisted, or omitted because its
			// ledger claim proves an earlier commit) and may now be acked.
			s.notifyDurable(batch)
			return n, 0, false, false
		}
		metrics.StoreErrorsTotal.Inc()
		if isPoison(err) {
			if len(batch) == 1 {
				s.log.Error("store quarantined a poison row", "error", err)
				// quarantine is this frame's durable disposition — the
				// failure is deterministic row content, so replaying it forever
				// against the same error would only wedge the feeder's spool.
				s.notifyDurable(batch)
				return 0, 1, false, false
			}
			mid := len(batch) / 2
			w1, d1, a1, g1 := s.persistAtomicRetry(ctx, batch[:mid])
			if a1 {
				// Nothing resolved in this call yet: propagate the abort so the
				// top-level flush can retain the whole batch.
				return 0, 0, true, false
			}
			w2, d2, a2, g2 := s.persistAtomicRetry(ctx, batch[mid:])
			if a2 {
				// The first half already resolved rows, so retention is off the
				// table — count the interrupted remainder dropped (the pre-regression fix
				// accounting for a cut-short bisection).
				return w1, d1 + len(batch[mid:]), false, g1
			}
			// OR the halves' give-ups into ONE report for the caller.
			return w1 + w2, d1 + d2, false, g1 || g2
		}
		if ctx.Err() != nil {
			// Interrupted mid-retry with nothing resolved: signal abort; the caller
			// (flush) decides retain-for-drain vs. budget-exhausted drop.
			return 0, 0, true, false
		}
		s.log.Warn("store flush failed; will retry", "error", err, "rows", len(batch), "attempt", attempt)
		if attempt >= s.retry.attempts {
			// regression fix/report the give-up; flush() counts it exactly once
			// for the whole top-level cycle, however many sub-batches gave up.
			s.log.Error("store flush giving up; leaving batch replayable", "rows", len(batch), "attempts", attempt)
			return 0, len(batch), false, true
		}
		if !sleepCtx(ctx, backoff) {
			return 0, 0, true, false
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
	sessions := make([]string, len(keys))
	seqs := make([]int64, len(keys))
	for i, k := range keys {
		sources[i] = k.source
		sessions[i] = k.session
		seqs[i] = int64(k.seq)
	}
	rows, err := s.pool.Query(ctx,
		`INSERT INTO nav_frames_seq_seen (source_id, session_id, feeder_seq)
		 SELECT * FROM unnest($1::text[], $2::text[], $3::bigint[])
		 ON CONFLICT (source_id, session_id, feeder_seq) DO NOTHING
		 RETURNING source_id, session_id, feeder_seq`,
		sources, sessions, seqs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fresh := make(map[seqKey]bool, len(keys))
	for rows.Next() {
		var src, sess string
		var seq int64
		if err := rows.Scan(&src, &sess, &seq); err != nil {
			return nil, err
		}
		fresh[seqKey{src, sess, uint64(seq)}] = true
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
		if !f.HasSourceSeq || fresh[seqKey{f.SourceID, f.Session, f.SourceSeq}] {
			out = append(out, f)
		}
	}
	return out
}

// flush bulk-loads a batch with bounded retry + poison-row quarantine under a
// context derived from parent and capped by deadline. Normal-path callers pass
// storeCtx as parent so shutdown interrupts an in-flight flush;
// shutdown-drain callers pass context.Background() (their parent is already
// cancelled) with the *same* deadline on every call so the total drain
// time is bounded by one budget, not a fresh one per chunk. Push-path frames are
// first deduplicated against nav_frames_seq_seen; a dedup-ledger failure
// fails open (persists the batch un-deduped) — losing the forensic record is
// worse than an occasional duplicate row.
//
// Returns false ONLY when parent was cancelled before any row was resolved: the
// caller must then retain the batch for the shutdown drain to re-flush under the
// bounded shutdownBudget instead of clearing it (nothing was committed, and the
// atomic claim+copy transaction rolled its replay-key claims back, so the
// re-flush cannot duplicate). Every completed outcome — success, quarantine,
// retry exhaustion, wall-budget expiry with the parent still live — returns true
// with the pre-regression fix accounting.
func (s *Store) flush(parent context.Context, batch []*NavFrame, deadline time.Time) bool {
	if len(batch) == 0 {
		return true
	}
	s.lastWriteAttempt = time.Now() // recent attempts gate the idle probe
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	if s.atomicPersist {
		written, dropped, aborted, gaveUp := s.persistAtomicRetry(ctx, batch)
		if aborted {
			if parent.Err() != nil {
				s.log.Warn("store flush interrupted by shutdown; batch retained for the bounded drain", "rows", len(batch))
				return false
			}
			// The wall budget expired with the parent still live: the writer must
			// stay live and memory-bounded through a long DB outage, so the batch
			// is dropped (counted), exactly the pre-regression fix policy.
			gaveUp = true
			dropped = len(batch)
			s.log.Error("store flush budget exhausted; dropping batch", "rows", len(batch))
		}
		// regression fix/exactly one streak increment per top-level flush cycle,
		// no matter how many sub-batches a poison bisection produced.
		if gaveUp {
			s.flushFailStreak.Add(1)
		}
		if written > 0 {
			metrics.StoreRowsTotal.Add(float64(written))
		}
		if dropped > 0 {
			metrics.StoreQuarantinedTotal.Add(float64(dropped))
		}
		return true
	}

	// Legacy (non-atomic) path — reachable only in tests (New always enables
	// atomicPersist). It never retains on interruption: checkSeqSeen pre-claims
	// replay keys OUTSIDE the copy transaction, so a retained re-flush would
	// dedup-drop push frames that were never actually written. Cancellation here
	// keeps the pre-regression fix drop-counted accounting.
	var keys []seqKey
	for _, f := range batch {
		if f.HasSourceSeq {
			keys = append(keys, seqKey{f.SourceID, f.Session, f.SourceSeq})
		}
	}
	if len(keys) > 0 {
		fresh, err := s.checkSeqSeen(ctx, keys)
		if err != nil {
			s.log.Warn("nav_frames_seq_seen check failed; persisting batch un-deduped", "error", err)
		} else {
			batch = dedupBatch(batch, fresh)
			if len(batch) == 0 {
				return true
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
	return true
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
	var sourceSeq any
	if f.HasSourceSeq {
		sourceSeq = int64(f.SourceSeq)
	} // preserve all uint64 bits, as in the dedup ledger

	var decoded any
	if len(f.Decoded) > 0 {
		decoded = string(f.Decoded)
	}
	collections := f.CollectionIDs
	if collections == nil {
		collections = []string{}
	}
	feedGrants := f.FeedGrants
	if feedGrants == nil {
		feedGrants = []string{}
	}
	declaredCapabilities := f.DeclaredCapabilities
	if declaredCapabilities == nil {
		declaredCapabilities = []string{}
	}
	peers := f.FederationPeers
	if peers == nil {
		peers = []string{}
	}
	signals := f.PublishSignals
	if signals == nil {
		signals = []string{}
	}
	return []any{
		f.Ts, f.ReceivedAt, f.SourceID,
		valueOr(f.OrganizationID, "local-unassigned"), valueOr(f.EnrollmentID, "legacy-unassigned"),
		valueOr(f.CollectorInstanceID, "local"), collections, feedGrants, declaredCapabilities,
		valueOr(f.Provenance, "local"),
		valueOr(f.CredentialTier, "local_dial"), f.CredentialFingerprint, valueOr(f.AttestationTier, "none"),
		valueOr(f.HardwareTrust, "none"), f.ManufacturerAuthorityID, f.CommissioningFingerprint,
		valueOr(f.AggregateUse, "private"), valueOr(f.StationMetadata, "none"), valueOr(f.EventVisibility, "private"),
		valueOr(f.RawExport, "deny"), peers, signals,
		valueOr(f.PolicyRevision, "legacy-private-v1"),
		int16(f.GnssID), int16(f.SvID), int16(f.SigID), int16(f.FreqID), int16(f.MsgType),
		f.Raw, decoded, nilIfEmpty(f.DecoderVer), f.SBFHeader, nilIfEmpty(f.Session), sourceSeq,
	}
}

func valueOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
