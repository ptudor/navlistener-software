-- navlistener persistence schema (docs/OUTPUT.md §4).
--
-- nav_frames is the forensic system of record: the raw broadcast nav frame bytes
-- side by side with a decoded JSONB projection, organized on the near-monotonic
-- INGEST clock (radio/GNSS arrives late and out of order, so event time is a
-- separate indexed column). Raw + decoded means a decoder fix can re-derive every
-- historical ephemeris from `raw` without re-collecting.
--
-- The same startup schema also creates the durable integrity-event and periodic
-- feed-snapshot tables below.

CREATE TABLE IF NOT EXISTS nav_frames (
    ts          TIMESTAMPTZ NOT NULL,   -- ingest time (hypertable dimension)
    received_at TIMESTAMPTZ NOT NULL,   -- receiver/host reception time (indexed)
    source_id   TEXT        NOT NULL,   -- ingest source / observer id
    -- Server-resolved receipt-time scope (GROUPS-AND-FEDERATION.md §5). These
    -- fields are immutable evidence: a later transfer/policy change must not
    -- rewrite what authority admitted or disclosed this observation.
    organization_id       TEXT   NOT NULL DEFAULT 'local-unassigned',
    enrollment_id         TEXT   NOT NULL DEFAULT 'legacy-unassigned',
    collector_instance_id TEXT   NOT NULL DEFAULT 'local',
    collection_ids        TEXT[] NOT NULL DEFAULT '{}',
	feed_grants           TEXT[] NOT NULL DEFAULT '{}',
	declared_capabilities TEXT[] NOT NULL DEFAULT '{}',
    provenance            TEXT   NOT NULL DEFAULT 'local',
    credential_tier       TEXT   NOT NULL DEFAULT 'local_dial',
    credential_fingerprint TEXT  NOT NULL DEFAULT '',
    attestation_tier      TEXT   NOT NULL DEFAULT 'none',
    aggregate_use         TEXT   NOT NULL DEFAULT 'private',
    station_metadata      TEXT   NOT NULL DEFAULT 'none',
    event_visibility      TEXT   NOT NULL DEFAULT 'private',
    raw_export            TEXT   NOT NULL DEFAULT 'deny',
    federation_peers      TEXT[] NOT NULL DEFAULT '{}',
    publish_signals       TEXT[] NOT NULL DEFAULT '{}',
    policy_revision       TEXT   NOT NULL DEFAULT 'legacy-private-v1',
    gnssid      SMALLINT    NOT NULL,   -- gnssId 0..7 (docs/CONSTELLATIONS.md §0)
    svid        SMALLINT    NOT NULL,
    sigid       SMALLINT    NOT NULL,
    -- GLONASS FDMA channel carrier as the receiver reported it (k = freqid - 7,
    -- docs/CONSTELLATIONS.md §5). Receiver metadata that does NOT survive in `raw`
    -- (RawBytes serialises only the nav words), so a GLONASS frame cannot be replayed
    -- faithfully without it. NOT NULL by design decision: the alternative was a nullable
    -- column plus a "was this recorded?" flag threaded through every layer, permanently,
    -- to serve rows that raw_retention deletes within a week anyway.
    freqid      SMALLINT    NOT NULL,
    msg_type    SMALLINT    NOT NULL,   -- GNF1 nav message type (docs/CONSTELLATIONS.md §6)
    raw         BYTEA       NOT NULL,   -- the broadcast nav frame, untouched (re-decodable)
    decoded     JSONB,                  -- normalized projection (ephemeris/almanac params), nullable
    decoder_ver TEXT
);

SELECT create_hypertable('nav_frames', 'ts',
    chunk_time_interval => INTERVAL '1 hour', if_not_exists => TRUE);

-- Additive migration for deployments created before audience/provenance
-- scoping. Every legacy row becomes explicitly private/unassigned; no absence
-- can be interpreted as public by a newer read/export path.
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS organization_id       TEXT   NOT NULL DEFAULT 'local-unassigned';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS enrollment_id         TEXT   NOT NULL DEFAULT 'legacy-unassigned';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS collector_instance_id TEXT   NOT NULL DEFAULT 'local';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS collection_ids        TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS feed_grants           TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS declared_capabilities TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS provenance            TEXT   NOT NULL DEFAULT 'local';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS credential_tier       TEXT   NOT NULL DEFAULT 'local_dial';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS credential_fingerprint TEXT  NOT NULL DEFAULT '';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS attestation_tier      TEXT   NOT NULL DEFAULT 'none';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS aggregate_use         TEXT   NOT NULL DEFAULT 'private';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS station_metadata      TEXT   NOT NULL DEFAULT 'none';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS event_visibility      TEXT   NOT NULL DEFAULT 'private';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS raw_export            TEXT   NOT NULL DEFAULT 'deny';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS federation_peers      TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS publish_signals       TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS policy_revision       TEXT   NOT NULL DEFAULT 'legacy-private-v1';

-- additive SBF framing metadata. NULL preserves the legacy unknown
-- revision; raw remains the unchanged body. New SBF captures retain all eight
-- header bytes (sync, CRC, revision/block ID and length), with no live decoding.
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS sbf_header BYTEA;

-- Receipt-order migration: existing rows deliberately retain NULL ordering/provenance. A
-- separate ALTER DEFAULT avoids backfilling invented arrival IDs (and works
-- with legacy compressed chunks). New rows get a stable first-storage admission
-- order, including inserts from older writers during a rolling upgrade.
CREATE SEQUENCE IF NOT EXISTS nav_frames_receipt_order_seq AS BIGINT NO CYCLE;
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS receipt_order BIGINT;
ALTER TABLE nav_frames ALTER COLUMN receipt_order SET DEFAULT nextval('nav_frames_receipt_order_seq');
-- Part of this additive migration (the order test strips the whole block to build
-- the preceding schema). Replay orders by receipt_order; without an index in the same
-- NULLS FIRST order the planner sorts the whole window externally (~0.6 kB of
-- temp file per row at the default 2.5 M-row limit). Verified to create over
-- already compressed chunks and to re-apply idempotently.
CREATE INDEX IF NOT EXISTS idx_nav_frames_receipt_order ON nav_frames (receipt_order NULLS FIRST);
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS source_session TEXT;
ALTER TABLE nav_frames ADD COLUMN IF NOT EXISTS source_seq BIGINT;

-- Query paths: per-SV history, and the recent-by-reception forensic scan.
CREATE INDEX IF NOT EXISTS idx_nav_frames_sv   ON nav_frames (gnssid, svid, ts DESC);
CREATE INDEX IF NOT EXISTS idx_nav_frames_recv ON nav_frames (received_at DESC);
CREATE INDEX IF NOT EXISTS idx_nav_frames_org_recv ON nav_frames (organization_id, received_at DESC);

-- Dedup contract : on feeder reconnect, navfeeder replays every DATA frame
-- past the last ack it received (docs/DESIGN.md §2). decode/live-state tolerate the
-- resulting duplicate ("replay is harmless" — push.go), but the raw historian must
-- not, or a routine reconnect (any collector restart, any regression fix-class hang) permanently
-- duplicates rows for one receiver, polluting the dedup-on-read "N receivers saw this
-- SV" integrity signal with single-receiver replay artifacts. nav_frames itself can't
-- carry a UNIQUE dedup constraint — Timescale requires a hypertable's
-- unique index to include its partition column, and `ts` legitimately differs between
-- a frame and its replay (ingest time, not broadcast time). So the dedup key lives in
-- a small side ledger with a real (non-partitioned) unique constraint: the writer
-- claims each push-path frame's (source_id, session_id, feeder_seq) here in the same
-- transaction that CopyFrom's the newly seen rows. A copy/commit failure therefore
-- rolls the claim back and leaves the edge replay retryable.
--
-- session_id  is the feeder's boot/session identity from the GNF1
-- HELLO: replay identity is (canonical observer, session, seq), NOT
-- (observer, seq). Without it, a feeder that restarted without a recoverable
-- spool (or any ESP32 reboot — RAM-only ring) reset its sequence to zero and
-- this ledger's old rows classified every fresh post-reboot frame as a replay
-- until the new counter passed the old high-water mark — silent loss of fresh
-- forensic raw frames after a routine reboot. A fresh session per sequence
-- space makes that collision structurally impossible; the feeder persists the
-- session in its disk-spool header so a spool-recovering restart continues
-- session and sequence together.
--
-- Dial-mode frames carry no feeder sequence and always pass through unfiltered —
-- duplicates across *different* receivers remain intentional and untouched by this
-- table. Pruned by the store on the same interval as raw_retention (store.go
-- prunePolicy) — entries older than that are moot, since nav_frames itself has
-- already retired them.
--
-- Migration (regression fix, documented no-compat): a pre-session two-column ledger's
-- rows can never match a session-carrying key (every post-upgrade key has a
-- non-empty session), so the old-shape table is pure dead weight — drop and
-- recreate. The ledger is rebuildable dedup state, not forensic record; the
-- worst case is one bounded window of duplicate raw rows, which dedup-on-read
-- tolerates by design.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables
               WHERE table_schema = current_schema() AND table_name = 'nav_frames_seq_seen')
       AND NOT EXISTS (SELECT 1 FROM information_schema.columns
               WHERE table_schema = current_schema() AND table_name = 'nav_frames_seq_seen'
                 AND column_name = 'session_id') THEN
        DROP TABLE nav_frames_seq_seen;
    END IF;
END $$;
CREATE TABLE IF NOT EXISTS nav_frames_seq_seen (
    source_id  TEXT        NOT NULL,
    session_id TEXT        NOT NULL,
    feeder_seq BIGINT      NOT NULL,
    seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (source_id, session_id, feeder_seq)
);
CREATE INDEX IF NOT EXISTS idx_nav_frames_seq_seen_prune ON nav_frames_seq_seen (seen_at);

-- Upgrade note — nav_frames.freqid (design decision, no back-compat). There is
-- deliberately NO `ALTER TABLE ... ADD COLUMN` here. The column is NOT NULL and old
-- rows cannot be back-filled (freqid never appears in `raw`), so the only honest
-- options were to invent a channel for historical GLONASS frames or to drop them.
-- Dropping wins on the merits: nav_frames is a short-window forensic record
-- (raw_retention defaults to 7 days), so any pre-migration row expires within a week
-- regardless, and the coming rollout is already a coordinated no-compat deploy.
--
-- This file will NOT destroy data at startup. On an existing deployment the regression fix
-- column check fails fast with an actionable message; the operator then drops the
-- table and restarts, and CREATE TABLE above rebuilds it:
--
--     DROP TABLE nav_frames CASCADE;
--
-- Fresh installs need no action.

-- Columnar compression: segment by constellation, order by SV then time so the
-- repetitive nav bitstream compresses hard. The compress/retention POLICIES are
-- applied (config-tunable) by the store at startup.
ALTER TABLE nav_frames SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'gnssid',
    timescaledb.compress_orderby   = 'svid, ts DESC'
);

-- gnss_events: confirmed integrity transitions (docs/OUTPUT.md §4). navlistener
-- writes; the native SSE broker publishes its selected audience directly. A
-- public-only v2 NOTIFY channel is provided for migrated external consumers;
-- the legacy unscoped channel is intentionally not used because its global ids
-- leak private event volume and can make an old consumer fetch the wrong row.
CREATE TABLE IF NOT EXISTS gnss_events (
    id         BIGSERIAL,
    time       TIMESTAMPTZ NOT NULL,
    audience   TEXT        NOT NULL DEFAULT 'legacy-operator',
    audience_seq BIGINT    NOT NULL,
    redaction_class TEXT   NOT NULL DEFAULT 'private',
    sv         TEXT        NOT NULL,
    event_type TEXT        NOT NULL,
    old_value  TEXT,
    new_value  TEXT,
    severity   SMALLINT    NOT NULL DEFAULT 0,
    message    TEXT,
    raw        JSONB,
    dedupe_key TEXT                 -- regression fix idempotency key (see below); NULL on legacy rows
);
SELECT create_hypertable('gnss_events', 'time', if_not_exists => TRUE);
-- WriteEvent's INSERT can commit while the client observes a
-- timeout/connection error; the bounded retry would then store, notify, and
-- serve the same confirmed transition twice (two rows, two durable SSE ids).
-- Every event therefore carries an internal dedupe key generated once per
-- confirmed transition, BEFORE the first write attempt (cmd prepareEvent), and
-- the retry becomes an idempotent upsert against this index. A hypertable's
-- unique index must include the partition column, so the key is (time,
-- dedupe_key) — safe because retries reuse the identical EventRow, time
-- included. The ALTER is the migration for deployments whose gnss_events
-- pre-dates this column (CREATE TABLE IF NOT EXISTS never alters); NULL keys
-- (legacy/intsat rows) never conflict under PostgreSQL's NULLS DISTINCT.
ALTER TABLE gnss_events ADD COLUMN IF NOT EXISTS dedupe_key TEXT;
ALTER TABLE gnss_events ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'legacy-operator';
ALTER TABLE gnss_events ADD COLUMN IF NOT EXISTS redaction_class TEXT NOT NULL DEFAULT 'private';
ALTER TABLE gnss_events ADD COLUMN IF NOT EXISTS audience_seq BIGINT;
-- Legacy rows are operator-only. Their old global id is safe as a one-time
-- sequence inside that non-public audience; new public/private streams allocate
-- independently below.
UPDATE gnss_events SET audience_seq = id WHERE audience_seq IS NULL;
ALTER TABLE gnss_events ALTER COLUMN audience_seq SET NOT NULL;

CREATE TABLE IF NOT EXISTS gnss_event_audience_cursors (
    audience TEXT PRIMARY KEY,
    last_seq BIGINT NOT NULL CHECK (last_seq > 0)
);
INSERT INTO gnss_event_audience_cursors (audience, last_seq)
SELECT audience, max(audience_seq) FROM gnss_events GROUP BY audience
ON CONFLICT (audience) DO UPDATE
SET last_seq = GREATEST(gnss_event_audience_cursors.last_seq, EXCLUDED.last_seq);

CREATE UNIQUE INDEX IF NOT EXISTS idx_gnss_events_dedupe ON gnss_events (time, dedupe_key);
CREATE INDEX IF NOT EXISTS idx_gnss_events_audience_seq  ON gnss_events (audience, audience_seq DESC);
CREATE INDEX IF NOT EXISTS idx_gnss_events_audience_time ON gnss_events (audience, time DESC);
CREATE INDEX IF NOT EXISTS idx_gnss_events_sv_time       ON gnss_events (sv, time DESC);
CREATE INDEX IF NOT EXISTS idx_gnss_events_type_time     ON gnss_events (event_type, time DESC);
CREATE INDEX IF NOT EXISTS idx_gnss_events_severity_time ON gnss_events (severity, time DESC);
-- the notify contract directs external LISTENers to fetch the full row by id
-- (the trigger below carries only id/sv/type/severity). A hypertable PK must include the
-- partition column (time), so id has no index by default; without this, every notify-driven
-- `SELECT ... WHERE id = $1` seq-scans all chunks of this retention-less table — a cost that
-- lands invisibly on the consumer since navlistener itself never queries by id.
CREATE INDEX IF NOT EXISTS idx_gnss_events_id            ON gnss_events (id);

-- compress-enable gnss_events like the other two hypertables. Events are
-- deliberately retention-less (confirmed integrity transitions are the durable
-- record — no retention policy, ever), which is exactly why old chunks must not
-- stay row-oriented forever: a flapping detector over a multi-month run
-- accumulates uncompressed chunks without bound, and enabling compression later
-- requires this ALTER first — a policy alone cannot do it. The compression
-- POLICY (30 days) is applied by the store at startup (applyPolicies), like
-- nav_frames'. Segment by audience + event_type so compressed scans cannot
-- cross an authorization boundary.
ALTER TABLE gnss_events SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'audience,event_type',
    timescaledb.compress_orderby   = 'time DESC'
);

-- pg_notify has a hard ~8000-byte payload limit; a long NEW.message would raise in
-- this trigger and fail the whole INSERT in the same transaction. The
-- payload carries only the fields needed to identify the public row. `id` is
-- the public audience-local cursor; row_id is internal lookup provenance. Only
-- public events notify, so an operator incident cannot perturb the channel.
CREATE OR REPLACE FUNCTION notify_gnss_event() RETURNS trigger AS $$
BEGIN
    IF NEW.audience = 'public' THEN
        PERFORM pg_notify('gnss_event_v2', json_build_object(
            'id', NEW.audience_seq, 'row_id', NEW.id, 'audience', NEW.audience,
            'sv', NEW.sv, 'type', NEW.event_type, 'severity', NEW.severity)::text);
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS gnss_event_notify ON gnss_events;
CREATE TRIGGER gnss_event_notify AFTER INSERT ON gnss_events
    FOR EACH ROW EXECUTE FUNCTION notify_gnss_event();

CREATE OR REPLACE VIEW gnss_events_public AS
SELECT audience_seq AS id, time, sv, event_type, old_value, new_value,
       severity, message, raw, redaction_class
  FROM gnss_events
 WHERE audience = 'public';

-- gnss_snapshots: periodic feed dumps for replay/backfill (docs/OUTPUT.md §4).
CREATE TABLE IF NOT EXISTS gnss_snapshots (
    time     TIMESTAMPTZ NOT NULL,
    audience TEXT        NOT NULL DEFAULT 'legacy-operator',
    endpoint TEXT        NOT NULL,  -- 'svs' | 'global' | 'observers' | 'almanac' | 'sbas'
    data     JSONB       NOT NULL
);
SELECT create_hypertable('gnss_snapshots', 'time', if_not_exists => TRUE);
ALTER TABLE gnss_snapshots ADD COLUMN IF NOT EXISTS audience TEXT NOT NULL DEFAULT 'legacy-operator';
CREATE INDEX IF NOT EXISTS idx_gnss_snapshots_audience_endpoint ON gnss_snapshots (audience, endpoint, time DESC);
ALTER TABLE gnss_snapshots SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'audience,endpoint',
    timescaledb.compress_orderby   = 'time DESC'
);
