-- navlistener persistence schema (docs/OUTPUT.md §4).
--
-- nav_frames is the forensic system of record: the raw broadcast nav frame bytes
-- side by side with a decoded JSONB projection, organized on the near-monotonic
-- INGEST clock (radio/GNSS arrives late and out of order, so event time is a
-- separate indexed column). Raw + decoded means a decoder fix can re-derive every
-- historical ephemeris from `raw` without re-collecting.
--
-- (gnss_events and gnss_snapshots from §4 are created by the integrity/serve passes.)

CREATE TABLE IF NOT EXISTS nav_frames (
    ts          TIMESTAMPTZ NOT NULL,   -- ingest time (hypertable dimension)
    received_at TIMESTAMPTZ NOT NULL,   -- receiver/host reception time (indexed)
    source_id   TEXT        NOT NULL,   -- ingest source / observer id
    gnssid      SMALLINT    NOT NULL,   -- gnssId 0..7 (docs/CONSTELLATIONS.md §0)
    svid        SMALLINT    NOT NULL,
    sigid       SMALLINT    NOT NULL,
    msg_type    SMALLINT    NOT NULL,   -- GNF1 nav message type (docs/CONSTELLATIONS.md §6)
    raw         BYTEA       NOT NULL,   -- the broadcast nav frame, untouched (re-decodable)
    decoded     JSONB,                  -- normalized projection (ephemeris/almanac params), nullable
    decoder_ver TEXT
);

SELECT create_hypertable('nav_frames', 'ts',
    chunk_time_interval => INTERVAL '1 hour', if_not_exists => TRUE);

-- Query paths: per-SV history, and the recent-by-reception forensic scan.
CREATE INDEX IF NOT EXISTS idx_nav_frames_sv   ON nav_frames (gnssid, svid, ts DESC);
CREATE INDEX IF NOT EXISTS idx_nav_frames_recv ON nav_frames (received_at DESC);

-- Columnar compression: segment by constellation, order by SV then time so the
-- repetitive nav bitstream compresses hard. The compress/retention POLICIES are
-- applied (config-tunable) by the store at startup.
ALTER TABLE nav_frames SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'gnssid',
    timescaledb.compress_orderby   = 'svid, ts DESC'
);

-- gnss_events: confirmed integrity transitions (docs/OUTPUT.md §4). navlistener
-- writes; a serve-side process (this daemon's SSE broker, or intsat) consumes.
-- The AFTER INSERT trigger fires pg_notify('gnss_event', …) so external LISTENers
-- see events without polling. This schema is a superset of intsat's 001_init.sql,
-- so its read path keeps working while it is still the serve front.
CREATE TABLE IF NOT EXISTS gnss_events (
    id         BIGSERIAL,
    time       TIMESTAMPTZ NOT NULL,
    sv         TEXT        NOT NULL,
    event_type TEXT        NOT NULL,
    old_value  TEXT,
    new_value  TEXT,
    severity   SMALLINT    NOT NULL DEFAULT 0,
    message    TEXT,
    raw        JSONB
);
SELECT create_hypertable('gnss_events', 'time', if_not_exists => TRUE);
CREATE INDEX IF NOT EXISTS idx_gnss_events_sv_time       ON gnss_events (sv, time DESC);
CREATE INDEX IF NOT EXISTS idx_gnss_events_type_time     ON gnss_events (event_type, time DESC);
CREATE INDEX IF NOT EXISTS idx_gnss_events_severity_time ON gnss_events (severity, time DESC);

CREATE OR REPLACE FUNCTION notify_gnss_event() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('gnss_event', json_build_object(
        'id', NEW.id, 'sv', NEW.sv, 'type', NEW.event_type,
        'severity', NEW.severity, 'message', NEW.message)::text);
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS gnss_event_notify ON gnss_events;
CREATE TRIGGER gnss_event_notify AFTER INSERT ON gnss_events
    FOR EACH ROW EXECUTE FUNCTION notify_gnss_event();

-- gnss_snapshots: periodic feed dumps for replay/backfill (docs/OUTPUT.md §4).
CREATE TABLE IF NOT EXISTS gnss_snapshots (
    time     TIMESTAMPTZ NOT NULL,
    endpoint TEXT        NOT NULL,  -- 'svs' | 'global' | 'observers' | 'almanac' | 'sbas'
    data     JSONB       NOT NULL
);
SELECT create_hypertable('gnss_snapshots', 'time', if_not_exists => TRUE);
CREATE INDEX IF NOT EXISTS idx_gnss_snapshots_endpoint ON gnss_snapshots (endpoint, time DESC);
ALTER TABLE gnss_snapshots SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'endpoint',
    timescaledb.compress_orderby   = 'time DESC'
);
