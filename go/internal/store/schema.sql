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
