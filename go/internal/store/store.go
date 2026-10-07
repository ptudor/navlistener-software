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

// queueDepth bounds the in-flight frames awaiting a flush, and maxQueueBytes
// bounds their payload: a frame count alone let one feeder's oversized records
// pin queueDepth × their size in memory, so the queue is bounded by both and
// Enqueue drops (and counts) on whichever is reached first.
const (
	queueDepth    = 16384
	maxQueueBytes = 64 << 20
	// frameQueueOverhead is the accounting weight of a NavFrame beyond its
	// payload slices: the struct and its provenance strings.
	frameQueueOverhead = 512
)

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
	shutdownFlushBudget = 5 * time.Second // for a Store whose budget was never set (tests)
)

// PoolCloseReserve is the slice of the daemon's store phase kept for
// pool.Close() after the drain gives up: Close waits for acquired connections
// to return, and that wait must not come out of time the process no longer has.
const PoolCloseReserve = 500 * time.Millisecond

// minDrainBudget keeps a degenerate phase from producing a drain that cannot
// flush a single chunk.
const minDrainBudget = 100 * time.Millisecond

// DrainBudget is the shutdown drain deadline the writer derives from the
// daemon's store phase: the phase less PoolCloseReserve, so drain plus close
// fit the phase the daemon actually waits for. The daemon's plan test pins
// DrainBudget(phase) + PoolCloseReserve <= phase.
func DrainBudget(phase time.Duration) time.Duration {
	return max(phase-PoolCloseReserve, minDrainBudget)
}

// SetShutdownBudget tells the writer how long the daemon's shutdown plan gives
// the whole store phase; the drain gives up DrainBudget(phase) after it starts
// and the remainder closes the pool. Call before Run. The single shared
// deadline across drain chunks is unchanged — only where it comes from.
func (s *Store) SetShutdownBudget(phase time.Duration) {
	s.shutdownBudget = DrainBudget(phase)
}

// pruneEvery is how often nav_frames_seq_seen (the replay-dedup ledger) is
// swept of entries older than the raw-retention window. The sweep runs on its
// own goroutine, never the writer's, and deletes in chunks of pruneChunkRows,
// each its own statement under pruneChunkTimeout, so progress is durable chunk
// by chunk: a backlog one sweep's pruneSweepBudget cannot finish is continued
// by the next instead of being rolled back whole. At ~26 sequenced frames/s per
// receiver the hourly backlog is ~94k rows per receiver, so a fleet's sweep is
// a few hundred chunks.
const (
	pruneEvery        = 1 * time.Hour
	pruneChunkRows    = 10000
	pruneChunkTimeout = 10 * time.Second
	pruneSweepBudget  = 20 * time.Minute
)

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
const provenanceColumns = 25

func nullableAuthority(value string) any {
	if value == "" {
		return nil
	}
	return value
}

var copyColumns = []string{
	"ts", "received_at", "source_id", "organization_id", "enrollment_id",
	"collector_instance_id", "collection_ids", "feed_grants", "declared_capabilities",
	"provenance", "credential_tier",
	"credential_fingerprint", "attestation_tier", "hardware_trust", "manufacturer_authority_id", "commissioning_fingerprint",
	"operational_authority_id", "authority_evidence",
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
	RF    *RFSample    // non-nil routes to private rf_samples, never nav_frames

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
	OperationalAuthorityID   string
	AuthorityEvidence        string // immutable JSON issuer/core/commissioning/registry SPKI snapshot
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

	// Ledger sweep: its cadence and per-sweep budget default to pruneEvery and
	// pruneSweepBudget (tests shrink them); pruning guards against overlapping
	// sweeps and pruneWG lets Run wait the sweep out before closing the pool.
	pruneEvery       time.Duration
	pruneSweepBudget time.Duration
	pruning          atomic.Bool
	pruneWG          sync.WaitGroup

	// sessionKeys caches nav_frames_sessions keys per (source, session) so each
	// feeder boot is resolved once per process; filled only after the
	// resolving transaction committed, evicted when the sweep retires a session.
	sessionMu   sync.Mutex
	sessionKeys map[sessionPair]int64

	// queueBytes is the accounting weight of every frame in `in`
	// (frameQueueBytes), bounded by queueBytesLimit (0 = maxQueueBytes).
	queueBytes      atomic.Int64
	queueBytesLimit int64
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

	// The other two ways the historian discards frames feed Degraded() through
	// the same two-consecutive-cycles rule. A flush that merely succeeds slowly
	// never fails, yet while the writer is blocked in it the queue overflows and
	// Enqueue drops every further frame; and a batch whose rows are mostly
	// poison is quarantined row by row. Both used to be counters only.
	//
	// cycleOverflow counts Enqueue's queue-full drops since the previous cycle
	// ended (atomic: Enqueue runs on the ingest goroutines); cycleQuarantined
	// counts the rows this cycle quarantined (Run-goroutine-only, like
	// lastIdleProbe). endCycle samples both and advances or resets the streaks.
	cycleOverflow    atomic.Int64
	cycleQuarantined int
	overflowStreak   atomic.Int64
	quarantineStreak atomic.Int64

	// writer is the connection the Run goroutine holds for its whole life so
	// the forensic writer never waits behind API reads for a pool slot — the
	// pool is shared with every historian query the serve front runs, and a
	// read flood that held every connection used to stall the writer through
	// its retry budget and drop the batch. Acquired at Run start (or on the
	// first flush if that fails), handed back after any persist error (a
	// context cut mid-statement closes the underlying connection, and the pool
	// discards a closed one on Release) and re-acquired by the next attempt,
	// and released before the pool is closed. writerMu is held by the Run
	// goroutine across every persist and by Close, which a read-only consumer
	// or a test may call without ever starting Run: pool.Close blocks until
	// every acquired connection is back, so whoever closes must release it.
	writerMu sync.Mutex
	writer   *pgxpool.Conn

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
	// The pool is sized by config (max_conns, the DSN's pool_max_conns, or the
	// documented default) and always holds room for the writer's dedicated
	// connection plus at least one reader.
	poolCfg, err := cfg.PoolConfig()
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
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
	if err := checkSchemaVersion(cctx, pool); err != nil {
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
	if err := recordSchemaVersion(cctx, pool); err != nil {
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
	// The replay-ledger horizon is the interval config.finalize parsed and
	// validated (RawRetentionDuration), so it cannot disagree with the policy
	// DDL built from the same string. A Store built without Load (tests) carries
	// only the string: derive the horizon from the same parser then, and fail
	// rather than silently falling back to the default — a silent fallback is
	// exactly how the database's retention and the in-memory dedupe retention
	// came to disagree. An empty value is the documented "use the default" case
	// (it matches applyPolicies' own fallback).
	seqSeenRetention := cfg.RawRetentionDuration
	if seqSeenRetention <= 0 {
		seqSeenRetention = defaultSeqSeenRetention
		if cfg.RawRetention != "" {
			d, err := parseSimpleInterval(cfg.RawRetention)
			if err != nil {
				pool.Close()
				return nil, fmt.Errorf("raw_retention: %w", err)
			}
			seqSeenRetention = d
		}
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
		pruneEvery:       pruneEvery,
		pruneSweepBudget: pruneSweepBudget,
		sessionKeys:      map[sessionPair]int64{},
		queueBytesLimit:  maxQueueBytes,
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

// schemaVersion is the schema generation this build writes (schema.sql,
// navlistener_schema). Bump it with any change an older writer would trip on
// every row — a NOT NULL column without a default, a tightened CHECK, a new
// constraint — and never for a purely additive, idempotent migration, which an
// older binary can run against unharmed. A build refuses to start against a
// database marked with a higher version than it knows.
//
// Version 2 re-keyed the replay ledger: nav_frames_seq_seen is keyed by a
// BIGINT session_key into nav_frames_sessions instead of two TEXT columns. A
// version-1 binary's migration block would drop the re-keyed table (it looks
// like the pre-session shape to it) and recreate the old one, so the marker
// keeps that binary from starting at all.
//
// Version 3 keyed the point-state tables (reception_power_models,
// agc_baselines) by (collector_instance_id, source_id). A version-2 binary's
// checkpoint upsert names ON CONFLICT (source_id), which has no matching
// constraint once re-keyed, so every one of its checkpoints would fail.
const schemaVersion = 3

// checkSchemaVersion refuses to run against a database whose schema a newer
// build has marked: the migrations here can only ever be behind such a schema,
// and verifyRequiredColumns catches the shapes it knows about, not a constraint
// a newer build added. A database without the marker (first start, or one last
// written by a build that predates it) is accepted as version 0.
func checkSchemaVersion(ctx context.Context, pool *pgxpool.Pool) error {
	var table *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('navlistener_schema')::text`).Scan(&table); err != nil {
		return fmt.Errorf("verify schema version: %w", err)
	}
	if table == nil {
		return nil
	}
	var stored int
	err := pool.QueryRow(ctx, `SELECT version FROM navlistener_schema`).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("verify schema version: %w", err)
	}
	if stored > schemaVersion {
		return fmt.Errorf("database schema is version %d but this build knows version %d — a newer navlistener has upgraded it; "+
			"run that build (or newer) here instead of this one", stored, schemaVersion)
	}
	return nil
}

// recordSchemaVersion marks the database with this build's schema generation
// once the schema has been applied and verified. The marker only moves
// forward, so an older build that passes checkSchemaVersion against a database
// at its own or a lower version never rewinds it.
func recordSchemaVersion(ctx context.Context, pool *pgxpool.Pool) error {
	_, err := pool.Exec(ctx, `INSERT INTO navlistener_schema (singleton, version, applied_at) VALUES (TRUE, $1, now())
		 ON CONFLICT (singleton) DO UPDATE SET version = EXCLUDED.version, applied_at = EXCLUDED.applied_at
		 WHERE navlistener_schema.version < EXCLUDED.version`, schemaVersion)
	if err != nil {
		return fmt.Errorf("record schema version: %w", err)
	}
	return nil
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
	"gnss_events":                 {"id", "time", "audience", "audience_seq", "redaction_class", "sv", "event_type", "old_value", "new_value", "severity", "message", "raw", "dedupe_key", "collector_instance_id"},
	"event_evidence":              evidenceColumns,
	"event_evidence_samples":      evidenceSampleColumns,
	"gnss_event_audience_cursors": {"audience", "last_seq"},
	"gnss_snapshots":              {"time", "audience", "endpoint", "data"},
	// navlistener's own raw-frame tables get the same fail-fast drift check. A future
	// build that adds a copyColumns column against an existing deployment's older nav_frames
	// starts cleanly (CREATE TABLE IF NOT EXISTS is a no-op), then every CopyFrom fails 42703
	// undefined_column — class 42 is not poison, so each batch is retried and dropped forever
	// (silent forensic-record loss while /healthz stays OK). nav_frames_seq_seen is the
	// push-path dedup ledger; a drift there fails the atomic claim tx and drops the batch.
	"nav_frames":             copyColumns,
	"observer_samples":       boardColumns,
	"rf_samples":             rfColumns,
	"reception_power_models": {"collector_instance_id", "source_id", "updated_at", "model_id", "data"},
	"agc_baselines":          {"collector_instance_id", "source_id", "updated_at", "epoch", "data"},
	"nav_frames_seq_seen":    {"session_key", "feeder_seq", "seen_at"},
	"nav_frames_sessions":    {"session_key", "source_id", "session_id", "first_seen"},
}

// verifyRequiredColumns fails fast with an actionable message if a required table
// is missing a column this build reads or writes — most likely because the
// DSN points at a database where gnss_events/gnss_snapshots pre-date this schema
// (e.g. an older intsat deployment) and CREATE TABLE IF NOT EXISTS left them as-is.
//
// It also fails fast on the opposite drift: a NOT NULL column without a default
// (and neither identity nor generated) that this build's writer does not
// supply. A newer schema, or an operator's ALTER, adds such a column while an
// older binary keeps writing; every row then trips 23502, which is a
// constraint failure of the whole batch and would otherwise surface only as
// the writer giving up cycle after cycle.
func verifyRequiredColumns(ctx context.Context, pool *pgxpool.Pool) error {
	for table, want := range requiredColumns {
		rows, err := pool.Query(ctx,
			`SELECT column_name,
			        is_nullable = 'NO' AND column_default IS NULL AND is_identity = 'NO' AND is_generated = 'NEVER'
			   FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = $1`,
			table)
		if err != nil {
			return fmt.Errorf("verify schema: query columns of %s: %w", table, err)
		}
		have := make(map[string]bool)
		var mustSupply []string
		for rows.Next() {
			var col string
			var required bool
			if err := rows.Scan(&col, &required); err != nil {
				rows.Close()
				return fmt.Errorf("verify schema: scan columns of %s: %w", table, err)
			}
			have[col] = true
			if required {
				mustSupply = append(mustSupply, col)
			}
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
		supplied := make(map[string]bool, len(want))
		for _, col := range want {
			supplied[col] = true
		}
		var unsupplied []string
		for _, col := range mustSupply {
			if !supplied[col] {
				unsupplied = append(unsupplied, col)
			}
		}
		if len(unsupplied) > 0 {
			return fmt.Errorf(
				"table %q has NOT NULL column(s) %v without a default that this build does not write — "+
					"the schema is newer than this binary or was altered; run the build that added them, "+
					"or give them a default, before starting navlistener",
				table, unsupplied)
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

// rollbackBudget bounds the detached rollback of an abandoned transaction.
const rollbackBudget = 2 * time.Second

// rollback abandons tx on a short context of its own, never the operation's. A
// deferred rollback runs after the last statement, when the caller's deadline
// may already have passed; pgx answers a rollback it cannot send by closing the
// connection, so rolling back on the expired context turned an ordinary timeout
// on an error path into a discarded pooled connection and a reconnect. A no-op
// once tx has committed.
func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackBudget)
	defer cancel()
	_ = tx.Rollback(ctx)
}

// applyPoliciesWithHook is applyPolicies with a per-statement observation seam.
// The regression fix atomicity test uses it to interrupt the sequence at each
// remove/add boundary in turn; production always passes nil.
func applyPoliciesWithHook(ctx context.Context, pool *pgxpool.Pool, log *slog.Logger, cfg config.Store,
	afterExec func(sql string)) error {
	// config.finalize never leaves these empty; the fallback serves a Store built
	// without Load (tests) and keeps the same defaults.
	compAfter, rawRet := cfg.CompressAfter, cfg.RawRetention
	if compAfter == "" {
		compAfter = config.DefaultCompressAfter
	}
	if rawRet == "" {
		rawRet = config.DefaultRawRetention
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
	defer rollback(tx) // no-op once committed
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
	// Event evidence is retention-less like the events it explains.
	if err := exec(`SELECT remove_compression_policy('event_evidence_samples', if_exists => true)`); err != nil {
		return err
	}
	if err := exec(`SELECT add_compression_policy('event_evidence_samples', INTERVAL '30 days', if_not_exists => true)`); err != nil {
		return fmt.Errorf("event evidence compression policy: %w", err)
	}
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
	// Receiver RF samples use the same evidence horizon while remaining in a
	// dedicated table whose segment key supports station/kind model replay.
	for _, sql := range []string{
		`SELECT remove_compression_policy('rf_samples', if_exists => true)`,
		fmt.Sprintf(`SELECT add_compression_policy('rf_samples', INTERVAL '%s', if_not_exists => true)`, compAfter),
		`SELECT remove_retention_policy('rf_samples', if_exists => true)`,
		fmt.Sprintf(`SELECT add_retention_policy('rf_samples', INTERVAL '%s', if_not_exists => true)`, rawRet),
	} {
		if err := exec(sql); err != nil {
			return fmt.Errorf("RF sample policy: %w", err)
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
	if !s.lastWriteAttempt.IsZero() && now.Sub(s.lastWriteAttempt) < idleQuietWindow {
		return // writes were attempted recently: their verdict stands
	}
	// The drop streaks describe what happened to the frames of recent cycles.
	// A full quiet window with no write attempt means no frames arrived, so
	// none can have overflowed the queue or been quarantined: both streaks are
	// stale by definition and are cleared without a probe. (A non-empty cycle
	// clears them itself when it drops nothing; only a cycle can advance them.)
	s.overflowStreak.Store(0)
	s.quarantineStreak.Store(0)
	if s.ping == nil || s.flushFailStreak.Load() == 0 {
		return
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

// Degraded thresholds. degradedCycles is the anti-flap rule every streak
// shares: a single bad cycle (a transient blip the next cycle absorbs) never
// degrades, two consecutive ones do. A cycle counts toward the quarantine streak
// when more than one row in quarantineDegradeDivisor of the rows it flushed was
// quarantined — a lone poison row in a healthy batch is exactly what bisection
// exists for, while a batch that is mostly poison is a systemic fault the
// operator must see. Any queue-overflow drop at all counts toward the overflow
// streak: each one is a frame of the forensic record that is gone.
const (
	degradedCycles           = 2
	quarantineDegradeDivisor = 10
)

// Degraded reports a non-empty reason while the historian is persistently
// discarding the forensic record, on any of its three drop paths, for two or
// more CONSECUTIVE flush cycles: flushes that exhausted their bounded retries or
// wall budget (the historian has been unable to persist for over a minute — each
// cycle is ~35 s of retries), queue overflows (a database that succeeds too
// slowly to keep up, so Enqueue drops frames while the writer is blocked), or
// quarantine floods (most of a batch's rows failing deterministically). A single
// bad cycle does not degrade, so /healthz cannot flap on one bad flush.
// Registered as a health probe in main; /healthz stays 200 with "degraded".
func (s *Store) Degraded() string {
	if n := s.flushFailStreak.Load(); n >= degradedCycles {
		return fmt.Sprintf("historian: %d consecutive flush cycles failed; frames are being dropped", n)
	}
	if n := s.overflowStreak.Load(); n >= degradedCycles {
		return fmt.Sprintf("historian: queue overflowed in %d consecutive flush cycles; frames are being dropped", n)
	}
	if n := s.quarantineStreak.Load(); n >= degradedCycles {
		return fmt.Sprintf("historian: more than a tenth of the rows were quarantined in %d consecutive flush cycles; frames are being discarded", n)
	}
	return ""
}

// endCycle closes one completed flush cycle's drop accounting: it samples the
// queue overflows Enqueue counted since the previous cycle ended and the rows
// this cycle quarantined, then advances or resets the streaks Degraded reads.
// attempted is the number of rows the cycle tried to persist. Called once per
// completed flush, so a cycle that is interrupted and retained for the
// shutdown drain carries its overflows into the drain's own cycle.
func (s *Store) endCycle(attempted int) {
	if overflow := s.cycleOverflow.Swap(0); overflow > 0 {
		s.overflowStreak.Add(1)
	} else {
		s.overflowStreak.Store(0)
	}
	quarantined := s.cycleQuarantined
	s.cycleQuarantined = 0
	if quarantined > 0 && quarantined*quarantineDegradeDivisor > attempted {
		s.quarantineStreak.Add(1)
	} else {
		s.quarantineStreak.Store(0)
	}
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
	// The byte budget is reserved before the send so the two bounds compose:
	// a frame is queued only when both its slot and its bytes fit.
	n := frameQueueBytes(f)
	if s.queueBytes.Add(n) > s.queueByteLimit() {
		s.queueBytes.Add(-n)
		s.dropOverflow(f, "queue_bytes")
		return
	}
	select {
	case s.in <- f:
	default:
		s.queueBytes.Add(-n)
		s.dropOverflow(f, "queue_depth")
	}
}

// dropOverflow records a frame the queue could not take: by count or by bytes,
// both the same backpressure condition for the streak accounting.
func (s *Store) dropOverflow(f *NavFrame, bound string) {
	metrics.StoreDroppedTotal.Inc()
	// One warning per overflow streak: the first drop after a clean cycle.
	// Every later drop of the streak is visible through the counter and,
	// from the second consecutive cycle, through Degraded(); logging each
	// would amplify exactly the load that caused the overflow. (If a cycle
	// ends between this sample and its streak update, one extra line can
	// slip through — harmless.)
	if s.cycleOverflow.Add(1) == 1 && s.overflowStreak.Load() == 0 {
		s.log.Warn("historian queue full; dropping frames until the writer catches up",
			"source", f.SourceID, "bound", bound, "queue_depth", cap(s.in), "queue_bytes", s.queueBytes.Load(), "queue_bytes_limit", s.queueByteLimit())
	}
}

// queueByteLimit is the configured byte bound, maxQueueBytes for a Store built
// without New (tests).
func (s *Store) queueByteLimit() int64 {
	if s.queueBytesLimit > 0 {
		return s.queueBytesLimit
	}
	return maxQueueBytes
}

// frameQueueBytes is f's accounting weight in the queue: every payload slice
// it retains plus a fixed allowance for the struct and its provenance strings.
func frameQueueBytes(f *NavFrame) int64 {
	n := frameQueueOverhead + len(f.Raw) + len(f.Decoded) + len(f.SBFHeader) + len(f.AuthorityEvidence)
	if f.Board != nil {
		n += len(f.Board.Data)
	}
	if f.RF != nil {
		n += len(f.RF.Data)
	}
	return int64(n)
}

// dequeued releases f's byte reservation once the writer has taken it off the
// queue; the batch it joins is bounded by batchSize, not by this budget.
func (s *Store) dequeued(f *NavFrame) { s.queueBytes.Add(-frameQueueBytes(f)) }

// Run is the batched writer loop. It flushes on a size or time threshold and, on
// shutdown, drains and flushes the remainder before closing the pool.
func (s *Store) Run(ctx context.Context) {
	batch := make([]*NavFrame, 0, s.batchSize)
	ticker := time.NewTicker(s.batchEvery)
	defer ticker.Stop()
	every := s.pruneEvery
	if every <= 0 {
		every = pruneEvery
	}
	pruneTicker := time.NewTicker(every)
	defer pruneTicker.Stop()

	if s.pool != nil { // nil only in tests that construct a Store without New()
		// Reserve the writer's connection before the first frame arrives, so a
		// read flood can never hold every pool slot ahead of it. Not fatal if
		// it fails: persistAtomicOnce acquires under its own attempt budget.
		actx, acancel := context.WithTimeout(ctx, s.retry.attemptTO)
		s.writerMu.Lock()
		if _, err := s.writerConn(actx); err != nil {
			s.log.Warn("historian writer connection not reserved at start; acquiring on the first flush instead", "error", err)
		}
		s.writerMu.Unlock()
		acancel()
	}

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
			s.dequeued(f)
			batch = append(batch, f)
			if len(batch) >= s.batchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		case <-pruneTicker.C:
			// On its own goroutine: a sweep at fleet scale runs for minutes,
			// and the queue must keep draining meanwhile.
			s.startPrune(ctx)
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
			// pool.Close blocks until every acquired connection is back, so
			// the writer's must go first, and a sweep still in flight (its
			// chunk statement is already cancelled with ctx) must have returned.
			s.dropWriter()
			s.pruneWG.Wait()
			if s.pool != nil { // nil only in tests that construct a Store without New()
				s.pool.Close()
			}
			s.log.Info("store drained and closed")
			return
		}
	}
}

// startPrune runs one ledger sweep on its own goroutine unless the previous
// sweep is still running (a backlog that outlasts pruneEvery is simply picked
// up by the next tick after it finishes).
func (s *Store) startPrune(ctx context.Context) {
	if !s.pruning.CompareAndSwap(false, true) {
		return
	}
	s.pruneWG.Add(1)
	go func() {
		defer s.pruneWG.Done()
		defer s.pruning.Store(false)
		s.pruneSeqSeen(ctx)
	}()
}

// pruneSeqSeen deletes replay-dedup ledger entries older than the raw-retention
// window: once nav_frames itself has retired a chunk that old, there's nothing
// left for a stale entry to deduplicate against. It deletes in chunks of
// pruneChunkRows, each its own statement under its own timeout, until nothing
// is left or the sweep budget expires, so every finished chunk stays deleted
// (one whole-table DELETE under one timeout rolled back entirely when it could
// not finish, and the backlog then only grew). Sessions with no entries left
// and older than the window are retired afterwards; a session that resumes in
// the instant between that check and its deletion keeps claiming under its
// cached key until the eviction below makes the next flush re-create it.
func (s *Store) pruneSeqSeen(ctx context.Context) {
	cutoff := time.Now().Add(-s.seqSeenRetention)
	budget := s.pruneSweepBudget
	if budget <= 0 {
		budget = pruneSweepBudget
	}
	deadline := time.Now().Add(budget)
	var rows int64
	chunks := 0
	exhausted := false
	for ctx.Err() == nil {
		if !time.Now().Before(deadline) {
			exhausted = true
			break
		}
		cctx, cancel := context.WithTimeout(ctx, pruneChunkTimeout)
		tag, err := s.pool.Exec(cctx,
			`DELETE FROM nav_frames_seq_seen
			  WHERE ctid = ANY(ARRAY(SELECT ctid FROM nav_frames_seq_seen WHERE seen_at < $1 LIMIT $2))`,
			cutoff, pruneChunkRows)
		cancel()
		if err != nil {
			s.log.Warn("nav_frames_seq_seen prune failed; finished chunks are kept", "error", err, "rows", rows, "chunks", chunks)
			return
		}
		n := tag.RowsAffected()
		rows += n
		if n == 0 {
			break
		}
		chunks++
	}
	if ctx.Err() != nil {
		return
	}
	cctx, cancel := context.WithTimeout(ctx, pruneChunkTimeout)
	defer cancel()
	retired, err := s.pool.Query(cctx,
		`DELETE FROM nav_frames_sessions s
		  WHERE s.first_seen < $1
		    AND NOT EXISTS (SELECT 1 FROM nav_frames_seq_seen l WHERE l.session_key = s.session_key)
		  RETURNING s.source_id, s.session_id`, cutoff)
	if err != nil {
		s.log.Warn("nav_frames_sessions prune failed", "error", err)
		return
	}
	var sessions []sessionPair
	for retired.Next() {
		var p sessionPair
		if err := retired.Scan(&p.source, &p.session); err != nil {
			retired.Close()
			s.log.Warn("nav_frames_sessions prune failed", "error", err)
			return
		}
		sessions = append(sessions, p)
	}
	retired.Close()
	if err := retired.Err(); err != nil {
		s.log.Warn("nav_frames_sessions prune failed", "error", err)
		return
	}
	if len(sessions) > 0 {
		s.sessionMu.Lock()
		for _, p := range sessions {
			delete(s.sessionKeys, p)
		}
		s.sessionMu.Unlock()
	}
	if rows > 0 || len(sessions) > 0 {
		s.log.Info("nav_frames_seq_seen pruned", "rows", rows, "chunks", chunks, "sessions", len(sessions), "cutoff", cutoff, "budget_exhausted", exhausted)
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
			s.dequeued(f)
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

// writerConn returns the writer's dedicated connection, acquiring one from the
// pool when none is held. ctx bounds the acquisition — the caller's attempt
// budget — so a pool with every slot taken by a slow read cannot stall the
// writer past the retry policy it already lives under. The caller holds writerMu.
func (s *Store) writerConn(ctx context.Context) (*pgxpool.Conn, error) {
	if s.writer == nil {
		conn, err := s.pool.Acquire(ctx)
		if err != nil {
			return nil, fmt.Errorf("acquire writer connection: %w", err)
		}
		s.writer = conn
	}
	return s.writer, nil
}

// releaseWriter hands the dedicated connection back to the pool, which discards
// it if the failed statement closed it and keeps it otherwise, so the next
// persist starts from a usable connection either way. The caller holds writerMu.
func (s *Store) releaseWriter() {
	if s.writer != nil {
		s.writer.Release()
		s.writer = nil
	}
}

// dropWriter releases the dedicated connection, if one is held, so the pool
// can close. Safe from any goroutine; it waits for an in-flight persist.
func (s *Store) dropWriter() {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	s.releaseWriter()
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

// sessionPair is the TEXT half of a replay key — one feeder boot of one
// observer — which nav_frames_sessions normalises to a BIGINT session_key so
// the ledger carries two integers per frame instead of two strings.
type sessionPair struct {
	source  string
	session string
}

// rowQuerier is the one method the ledger claim needs from a pool or a
// transaction.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// resolveSessionKeys returns the session_key of every pair, creating the
// sessions that do not exist yet through q — the claim transaction in
// production, so a rolled-back commit also rolls back any session it created.
// Pairs already cached cost nothing. The caller publishes newly resolved keys
// with rememberSessionKeys once their transaction has committed; until then a
// key that the rollback discards is never cached.
func (s *Store) resolveSessionKeys(ctx context.Context, q rowQuerier, pairs []sessionPair) (map[sessionPair]int64, error) {
	keys := make(map[sessionPair]int64, len(pairs))
	var missing []sessionPair
	s.sessionMu.Lock()
	for _, p := range pairs {
		if k, ok := s.sessionKeys[p]; ok {
			keys[p] = k
		} else {
			missing = append(missing, p)
		}
	}
	s.sessionMu.Unlock()
	if len(missing) == 0 {
		return keys, nil
	}
	sources := make([]string, len(missing))
	sessions := make([]string, len(missing))
	for i, p := range missing {
		sources[i], sessions[i] = p.source, p.session
	}
	// Sessions the INSERT creates come back through its RETURNING; sessions
	// that already existed come from the base table, which the statement's own
	// snapshot shows without the rows the CTE just inserted, so the union never
	// repeats a pair and one round trip resolves both kinds.
	const resolve = `WITH wanted(source_id, session_id) AS (SELECT * FROM unnest($1::text[], $2::text[])),
	      created AS (
	        INSERT INTO nav_frames_sessions (source_id, session_id)
	        SELECT source_id, session_id FROM wanted
	        ON CONFLICT (source_id, session_id) DO NOTHING
	        RETURNING session_key, source_id, session_id)
	 SELECT session_key, source_id, session_id FROM created
	 UNION ALL
	 SELECT s.session_key, s.source_id, s.session_id
	   FROM nav_frames_sessions s JOIN wanted w USING (source_id, session_id)`
	if err := scanSessionKeys(ctx, q, resolve, sources, sessions, keys); err != nil {
		return nil, err
	}
	// A pair neither created nor found can only mean another writer inserted
	// it between this statement's snapshot and its ON CONFLICT; the store has
	// one writer, but a plain lookup settles it either way.
	var unresolved []sessionPair
	for _, p := range missing {
		if _, ok := keys[p]; !ok {
			unresolved = append(unresolved, p)
		}
	}
	if len(unresolved) > 0 {
		sources, sessions = sources[:0], sessions[:0]
		for _, p := range unresolved {
			sources, sessions = append(sources, p.source), append(sessions, p.session)
		}
		const lookup = `SELECT s.session_key, s.source_id, s.session_id
		   FROM nav_frames_sessions s
		   JOIN unnest($1::text[], $2::text[]) AS w(source_id, session_id) USING (source_id, session_id)`
		if err := scanSessionKeys(ctx, q, lookup, sources, sessions, keys); err != nil {
			return nil, err
		}
		for _, p := range unresolved {
			if _, ok := keys[p]; !ok {
				return nil, fmt.Errorf("nav_frames_sessions: no session for %s/%s", p.source, p.session)
			}
		}
	}
	return keys, nil
}

func scanSessionKeys(ctx context.Context, q rowQuerier, sql string, sources, sessions []string, into map[sessionPair]int64) error {
	rows, err := q.Query(ctx, sql, sources, sessions)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key int64
		var p sessionPair
		if err := rows.Scan(&key, &p.source, &p.session); err != nil {
			return err
		}
		into[p] = key
	}
	return rows.Err()
}

// rememberSessionKeys publishes resolved keys to the cache once the
// transaction that may have created their sessions has committed.
func (s *Store) rememberSessionKeys(keys map[sessionPair]int64) {
	if len(keys) == 0 {
		return
	}
	s.sessionMu.Lock()
	if s.sessionKeys == nil {
		s.sessionKeys = map[sessionPair]int64{}
	}
	for p, k := range keys {
		s.sessionKeys[p] = k
	}
	s.sessionMu.Unlock()
}

// claimSeqKeys claims every key in unique in the ledger through q and returns
// the ones that were newly claimed (not a replay of an already-persisted
// sequence) together with the session keys it resolved, which the caller
// caches once q's transaction has committed. ON CONFLICT DO NOTHING plus
// RETURNING means a key already in the ledger is silently absent from the
// result — exactly the "already stored, drop this replay" signal.
func (s *Store) claimSeqKeys(ctx context.Context, q rowQuerier, unique []seqKey) (fresh map[seqKey]bool, resolved map[sessionPair]int64, err error) {
	fresh = make(map[seqKey]bool, len(unique))
	if len(unique) == 0 {
		return fresh, nil, nil
	}
	pairs := make([]sessionPair, 0, len(unique))
	seenPair := make(map[sessionPair]bool, len(unique))
	for _, k := range unique {
		p := sessionPair{k.source, k.session}
		if !seenPair[p] {
			seenPair[p] = true
			pairs = append(pairs, p)
		}
	}
	resolved, err = s.resolveSessionKeys(ctx, q, pairs)
	if err != nil {
		return nil, nil, err
	}
	keys := make([]int64, len(unique))
	seqs := make([]int64, len(unique))
	byKey := make(map[int64]sessionPair, len(resolved))
	for p, k := range resolved {
		byKey[k] = p
	}
	for i, k := range unique {
		keys[i], seqs[i] = resolved[sessionPair{k.source, k.session}], int64(k.seq)
	}
	rows, err := q.Query(ctx,
		`INSERT INTO nav_frames_seq_seen (session_key, feeder_seq)
		 SELECT * FROM unnest($1::bigint[], $2::bigint[])
		 ON CONFLICT (session_key, feeder_seq) DO NOTHING
		 RETURNING session_key, feeder_seq`, keys, seqs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, seq int64
		if err := rows.Scan(&key, &seq); err != nil {
			return nil, nil, err
		}
		p := byKey[key]
		fresh[seqKey{p.source, p.session, uint64(seq)}] = true
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return fresh, resolved, nil
}

// persistAtomicOnce claims replay keys and inserts their corresponding raw rows in
// one transaction. A failed CopyFrom or commit rolls the claims back, so reconnect
// replay can retry them. Existing claims are durable proof that the row committed
// in an earlier transaction and are therefore omitted. Duplicate keys inside one
// batch are also emitted only once.
func (s *Store) persistAtomicOnce(ctx context.Context, batch []*NavFrame) (written int64, err error) {
	s.writerMu.Lock()
	defer s.writerMu.Unlock()
	conn, err := s.writerConn(ctx)
	if err != nil {
		return 0, err
	}
	// Any failure hands the connection back (after the rollback below, which
	// is deferred later and therefore runs first) so the next attempt
	// re-acquires: a context cut mid-statement has closed this connection.
	defer func() {
		if err != nil {
			s.releaseWriter()
		}
	}()
	tx, err := conn.Begin(ctx)
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

	// The session rows a claim may create live in this transaction too: a
	// rolled-back copy rolls them back, and their keys reach the cache only
	// after the commit below.
	fresh, resolved, err := s.claimSeqKeys(ctx, tx, unique)
	if err != nil {
		return 0, err
	}

	copyRows := make([][]any, 0, len(batch))
	boardRows := make([][]any, 0, len(batch))
	rfRows := make([][]any, 0, len(batch))
	boardCounts := map[string]int{}
	rfCounts := map[string]int{}
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
		} else if f.RF != nil {
			rfRows = append(rfRows, rfFrameToRow(f))
			rfCounts[f.RF.Kind]++
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
	if len(rfRows) > 0 {
		n, err := tx.CopyFrom(ctx, pgx.Identifier{"rf_samples"}, rfColumns, pgx.CopyFromRows(rfRows))
		if err != nil {
			return 0, err
		}
		written += n
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	s.rememberSessionKeys(resolved)
	for kind, n := range boardCounts {
		metrics.StoreBoardRowsTotal.WithLabelValues(kind).Add(float64(n))
	}
	for kind, n := range rfCounts {
		metrics.StoreRFRowsTotal.WithLabelValues(kind).Add(float64(n))
	}

	return written, nil
}

// persistCycle carries what one top-level flush cycle has learned across its
// bisection: whether any sub-batch committed — the proof that a constraint
// violation on a single row is that row's own content rather than a constraint
// every row trips — and the single rows whose verdict waits on that proof.
type persistCycle struct {
	committed bool
	deferred  []deferredFrame
}

// deferredFrame is a single row that failed with a failPoisonIfIsolated code
// before any sibling of its cycle had committed.
type deferredFrame struct {
	frame *NavFrame
	err   error
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
// budget on a transient error, or hit a constraint every row trips. It is
// REPORTED, not counted, here: poison-row bisection recurses, and incrementing
// flushFailStreak inside each recursive give-up let one top-level flush bump
// the streak twice (two transient-failing halves), reaching Degraded()'s ≥2
// threshold after effectively one cycle rather than the intended two
// consecutive ones. flush() folds it into a single Add. Note aborted ⇒ !gaveUp
// by construction: aborted means nothing in this subtree was resolved, and a
// give-up resolves rows (as dropped).
//
// Which failures are poison is classifyPersistError's call. A unique or check
// violation on a single row is quarantined (and acked) only once a sibling row
// of the same cycle has committed under the same constraints; until then the
// row is deferred, and a cycle in which nothing committed treats every deferred
// row as systemic: unacked, replayable, counted as a give-up. Every class-23
// error used to be acked as poison, so a schema that rejected every row
// released the feeder's spool copy of the whole forensic record while /healthz
// stayed green.
func (s *Store) persistAtomicRetry(ctx context.Context, batch []*NavFrame) (written int64, dropped int, aborted, gaveUp bool) {
	c := &persistCycle{}
	written, dropped, aborted, gaveUp = s.persistSub(ctx, c, batch)
	if aborted || len(c.deferred) == 0 {
		return written, dropped, aborted, gaveUp
	}
	if c.committed {
		for _, d := range c.deferred {
			s.quarantineFrame(d.frame, d.err)
		}
		return written, dropped + len(c.deferred), false, gaveUp
	}
	s.logSystemic(c.deferred[0].err, len(c.deferred), "no row of the cycle committed")
	return written, dropped + len(c.deferred), false, true
}

// persistSub is persistAtomicRetry's recursive body for one (sub-)batch.
func (s *Store) persistSub(ctx context.Context, c *persistCycle, batch []*NavFrame) (written int64, dropped int, aborted, gaveUp bool) {
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
			c.committed = true
			// the transaction committed — every sequenced frame in the
			// batch is durably resolved (persisted, or omitted because its
			// ledger claim proves an earlier commit) and may now be acked.
			s.notifyDurable(batch)
			return n, 0, false, false
		}
		metrics.StoreErrorsTotal.Inc()
		switch class := classifyPersistError(err); class {
		case failPoison, failPoisonIfIsolated:
			if len(batch) == 1 {
				if class == failPoisonIfIsolated && !c.committed {
					// Nothing has committed yet, so this may be a constraint
					// every row trips; the cycle decides once it knows.
					c.deferred = append(c.deferred, deferredFrame{frame: batch[0], err: err})
					return 0, 0, false, false
				}
				s.quarantineFrame(batch[0], err)
				return 0, 1, false, false
			}
			mid := len(batch) / 2
			w1, d1, a1, g1 := s.persistSub(ctx, c, batch[:mid])
			if a1 {
				// Nothing resolved in this call yet: propagate the abort so the
				// top-level flush can retain the whole batch.
				return 0, 0, true, false
			}
			w2, d2, a2, g2 := s.persistSub(ctx, c, batch[mid:])
			if a2 {
				if w1 == 0 && d1 == 0 {
					// The first half resolved nothing either (at most deferred
					// rows, which are not resolved): the whole batch is still
					// retainable.
					return 0, 0, true, false
				}
				// The first half already resolved rows, so retention is off the
				// table — count the interrupted remainder dropped (the pre-regression fix
				// accounting for a cut-short bisection).
				return w1, d1 + len(batch[mid:]), false, g1
			}
			// OR the halves' give-ups into ONE report for the caller.
			return w1 + w2, d1 + d2, false, g1 || g2
		case failSystemic:
			// Deterministic and not about any one row: bisecting would only run
			// 2n-1 failing transactions, and acking would release the feeder's
			// only copy. Leave the batch replayable for a build or schema that
			// accepts it.
			s.logSystemic(err, len(batch), "constraint rejects every row")
			return 0, len(batch), false, true
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

// quarantineFrame records one frame's quarantine — its final disposition: the
// row's identity and the SQLSTATE in the log (the only trace an acked-and-
// discarded frame leaves for triage), the per-table counter, the cycle's
// quarantine accounting, and the durable notification.
func (s *Store) quarantineFrame(f *NavFrame, err error) {
	table := frameTable(f)
	s.log.Error("store quarantined a poison row", append(append(pgFields(err), "table", table), frameFields(f)...)...)
	metrics.StoreQuarantinedRowsTotal.WithLabelValues(table).Inc()
	s.cycleQuarantined++
	s.notifyDurable([]*NavFrame{f})
}

// logSystemic reports a cycle give-up on a constraint that rejects every row,
// with the SQLSTATE and whatever table, column and constraint the server named.
func (s *Store) logSystemic(err error, rows int, why string) {
	s.log.Error("store flush failed on a constraint every row trips; leaving batch replayable and unacked",
		append(pgFields(err), "rows", rows, "reason", why)...)
}

// pgFields is a persist error's server-side identity for the log: the error,
// its SQLSTATE, and whichever of table, column and constraint the server named.
func pgFields(err error) []any {
	fields := []any{"error", err}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return fields
	}
	fields = append(fields, "sqlstate", pg.Code)
	if pg.TableName != "" {
		fields = append(fields, "relation", pg.TableName)
	}
	if pg.ColumnName != "" {
		fields = append(fields, "column", pg.ColumnName)
	}
	if pg.ConstraintName != "" {
		fields = append(fields, "constraint", pg.ConstraintName)
	}
	return fields
}

// frameFields is a frame's identity for the log: where it came from and what
// it was, enough to find it again in the feeder's spool or the receiver's output.
func frameFields(f *NavFrame) []any {
	return []any{"source", f.SourceID, "session", f.Session, "seq", f.SourceSeq, "sequenced", f.HasSourceSeq,
		"kind", frameKind(f), "gnssid", f.GnssID, "svid", f.SvID, "msg_type", f.MsgType, "raw_len", len(f.Raw)}
}

// frameTable names the table a frame is written to.
func frameTable(f *NavFrame) string {
	switch {
	case f.Board != nil:
		return "observer_samples"
	case f.RF != nil:
		return "rf_samples"
	}
	return "nav_frames"
}

// frameKind is the sample kind of a board or RF frame, "nav" for a nav frame.
func frameKind(f *NavFrame) string {
	switch {
	case f.Board != nil:
		return f.Board.Kind
	case f.RF != nil:
		return f.RF.Kind
	}
	return "nav"
}

// checkSeqSeen claims keys in nav_frames_seq_seen outside any transaction
// (the legacy, non-atomic path) and returns the subset that were newly
// claimed — not a replay of an already-persisted sequence — which is the
// signal dedupBatch needs. Each statement autocommits, so the sessions it
// resolved are durable and cached at once.
func (s *Store) checkSeqSeen(ctx context.Context, keys []seqKey) (map[seqKey]bool, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	fresh, resolved, err := s.claimSeqKeys(ctx, s.pool, keys)
	if err != nil {
		return nil, err
	}
	s.rememberSessionKeys(resolved)
	return fresh, nil
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
		// Whatever this cycle dropped beyond its quarantined rows was left
		// unacked and replayable (retries, budget, systemic constraint).
		if replayable := dropped - s.cycleQuarantined; replayable > 0 {
			metrics.StoreRetryDroppedTotal.Add(float64(replayable))
		}
		s.endCycle(len(batch))
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
				s.endCycle(0)
				return true
			}
		}
	}

	rows := make([][]any, 0, len(batch))
	for _, f := range batch {
		rows = append(rows, navFrameToRow(f))
	}
	written, dropped := s.persistRetry(ctx, rows)
	if written > 0 {
		metrics.StoreRowsTotal.Add(float64(written))
	}
	if dropped > 0 {
		metrics.StoreQuarantinedTotal.Add(float64(dropped))
	}
	if replayable := dropped - s.cycleQuarantined; replayable > 0 {
		metrics.StoreRetryDroppedTotal.Add(float64(replayable))
	}
	s.endCycle(len(rows))
	return true
}

// persistRetry bulk-loads rows through the copy seam, retrying retryable
// failures with bounded backoff and quarantining poison rows. Returns the rows
// written and permanently dropped; quarantined rows also count toward the
// cycle's quarantine accounting like the atomic path's.
func (s *Store) persistRetry(ctx context.Context, rows [][]any) (written int64, dropped int) {
	if len(rows) == 0 {
		return 0, 0
	}
	backoff := s.retry.backoff
	for attempt := 1; ; attempt++ {
		n, err := copyOnce(ctx, s.retry.attemptTO, s.copy, rows)
		if err == nil {
			return n, 0
		}
		metrics.StoreErrorsTotal.Inc()
		switch classifyPersistError(err) {
		case failPoison, failPoisonIfIsolated:
			// This path never acks and has no cycle to defer into, so an
			// isolated-only code is bisected like poison.
			return s.quarantine(ctx, rows, err)
		case failSystemic:
			s.logSystemic(err, len(rows), "constraint rejects every row")
			return 0, len(rows)
		}
		s.log.Warn("store flush failed; will retry", "error", err, "rows", len(rows), "attempt", attempt)
		if attempt >= s.retry.attempts || ctx.Err() != nil {
			s.log.Error("store flush giving up; dropping batch", "rows", len(rows), "attempts", attempt)
			return 0, len(rows)
		}
		if !sleepCtx(ctx, backoff) {
			s.log.Error("store flush budget exhausted; dropping batch", "rows", len(rows))
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
func (s *Store) quarantine(ctx context.Context, rows [][]any, cause error) (written int64, dropped int) {
	if len(rows) == 1 {
		// Rows on this path are nav_frames CopyFrom rows in copyColumns order;
		// index 2 is source_id.
		s.log.Error("store quarantined a poison row", append(pgFields(cause), "table", "nav_frames", "source", rows[0][2])...)
		metrics.StoreQuarantinedRowsTotal.WithLabelValues("nav_frames").Inc()
		s.cycleQuarantined++
		return 0, 1
	}
	if ctx.Err() != nil {
		return 0, len(rows)
	}
	mid := len(rows) / 2
	w1, d1 := s.persistRetry(ctx, rows[:mid])
	w2, d2 := s.persistRetry(ctx, rows[mid:])
	return w1 + w2, d1 + d2
}

// failureClass is what a persist error says about the batch, which decides
// whether the rows are retried, bisected and quarantined, or left replayable.
type failureClass int

const (
	// failTransient: network, timeout, resource — retry with backoff, then
	// give up with the batch replayable.
	failTransient failureClass = iota
	// failPoison: SQLSTATE class 22 (data exception), deterministic content of
	// one row (a NUL in JSON, an out-of-range smallint). Bisect to isolate it;
	// quarantine is its durable disposition, so it is acked.
	failPoison
	// failPoisonIfIsolated: unique (23505) or check (23514) violation. Row
	// content when one row trips a constraint its siblings satisfy; systemic
	// when the constraint rejects every row (a CHECK tightened by a newer
	// schema). Bisect, and quarantine a single row only once a sibling of the
	// same cycle has committed.
	failPoisonIfIsolated
	// failSystemic: the rest of class 23 — above all not-null (23502) and
	// foreign-key (23503) violations, which a newer schema's column or an
	// operator's constraint inflicts on every row. No bisection, no ack: the
	// cycle gives up and the batch stays replayable.
	failSystemic
)

// classifyPersistError maps a persist error to its failureClass. Anything that
// is not a server error with a class 22 or 23 SQLSTATE is transient.
func classifyPersistError(err error) failureClass {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) || len(pg.Code) < 2 {
		return failTransient
	}
	switch pg.Code[:2] {
	case "22":
		return failPoison
	case "23":
		switch pg.Code {
		case "23505", "23514":
			return failPoisonIfIsolated
		}
		return failSystemic
	}
	return failTransient
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
		valueOr(f.HardwareTrust, "none"), nullableAuthority(f.ManufacturerAuthorityID), f.CommissioningFingerprint,
		f.OperationalAuthorityID, valueOr(f.AuthorityEvidence, "{}"),
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
