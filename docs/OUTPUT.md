# navlistener — Output Contract

**Status: design (2026-07-07).** This document specifies **every byte `navlistener` emits**.
It is the contract the existing Integrity-Constellation consumers already speak, so a
developer implementing the serve path can produce output that **intsat** and **mapintsat**
accept *unmodified*. `navlistener` supplies navigation data to these applications. Authorship is **ICD-first** — decoders
and orbit math are written from the public ICDs with **no galmon code copied** — while galmon
is permitted as a **differential-test oracle** for cross-checking numeric output (the
byte-compat validation in §6 is exactly that use). See `docs/DESIGN.md` for the pipeline,
`docs/MATH.md` for how the emitted numbers are computed, `docs/CONSTELLATIONS.md` for the
signal/SV coverage behind the feeds, and `docs/INTEGRITY.md` for the event-detection logic.

> **Prime directive of this file:** the Phase-1 galmon-compatible feeds must be
> *byte-shape-identical* to what `galmon.eu`'s `navparse` emits today (hyphenated keys,
> `name@sigid` map keys, ECEF-meters in `svs`, ECEF-**kilometres** in `almanac`, polymorphic
> `healthissue`). Any deviation breaks a shipped consumer. Additional QZSS and
> NavIC fields form a **documented superset**, never by changing an existing field's shape.

---

## 0. Why two tiers, and who consumes which

`navlistener` serves two output tiers from the same in-RAM state, under `intsat.space` so
existing client URLs keep resolving:

| Tier | Path root | Shape | Primary consumer |
|---|---|---|---|
| **(a) galmon-compatible** | `/api/*.json` | galmon `navparse` JSON, hyphenated keys | intsat's `gnss-history-collect` (parses these); mapintsat `.NET`/Swift `FeedClient` raw `svs.json` |
| **(b) schema-1.1 normalized** | `/gnss/api/v1.1/*` | envelope `{"schema":"1.1",…}`, underscored keys, numeric codes | intsat Vue frontend (`useHealth.js`), mapintsat `AlmanacEntry` (`/gnss/api/v1.1/almanac`), new native clients |

Consumer-specific facts pinned from the current code:

- **mapintsat** (`dotnet/IntegrityMap.Core/Services/FeedClient.cs`, Swift mirror) fetches raw
  `https://intsat.space/api/svs.json` **and** `https://intsat.space/gnss/api/v1.1/almanac`.
  It decodes `svs` with `FlexibleIntConverter`/`FlexibleBoolConverter` — tolerant of absence
  and of `healthissue` arriving as bool *or* int. `AlmanacEntry.EcefMeters` multiplies the
  almanac ECEF by **1000** (confirming almanac is km), and derives GLONASS positions there.
- **intsat** `internal/fetch/parse.go` parses all five galmon feeds; `parseHealthIssueLevel()`
  handles the polymorphic `healthissue`. `internal/serve/v11.go` re-emits the normalized
  tier. Our Phase-1 job is to *be* the thing `internal/fetch` points at.

**Tier (a) is the drop-in.** Tier (b) is native and preferred by the browser, and is where our
QZSS/NavIC superset and coded fields live for clients that want to localize themselves.

---

## 1. galmon-compatible feed set (Tier a — the drop-in)

Five endpoints, all `Content-Type: application/json`, all keys **hyphenated**, refreshed from
RAM on each request (see §6 cadence). Field provenance (which raw nav frame / which MATH.md
routine fills each) is called out inline.

### 1.1 `GET /api/svs.json`

A JSON **object keyed by SV signal id** in `name@sigid` form — e.g. `"G05@0"`, `"E14@1"`,
`"C06@0"`, `"J03@0"` (QZSS, ours), `"I04@0"` (NavIC, ours). One entry per **satellite×signal**.

Per-SV fields (hyphenated; pointers/absent-tolerant — a consumer treats any missing field as
unknown):

| Field | Type | Meaning / source |
|---|---|---|
| `fullName` | string | e.g. `"GPS-5"`, `"Galileo-14 (E1B)"` — human label |
| `name` | string | short SV name `"G05"` (letter+PRN; galmon numbering, §4) |
| `gnssid` | int | **galmon-emit** gnssid (GPS 0, Galileo 2, BeiDou 3, GLONASS 6; **ours:** QZSS 5, NavIC 7) |
| `svid` | int | PRN within constellation |
| `sigid` | int | signal id (0 = primary; galmon-emit numbering, §4.2) |
| `health` | string | free-text broadcast health, e.g. `"OK"`, `"NOT OK: 3"`, `"DON'T USE"`, Galileo composite `"OK/OK/val/val"` (MATH.md §health) |
| `healthissue` | **bool OR int** | **polymorphic** — legacy shape is bool; severity shape is int `0`/`1`/`2`. We emit the **int** form (0 none / 1 warn / 2 error); consumers accept both. Never emit as string. |
| `eph-age-m` | float | ephemeris age in minutes = `ephAge(tow,t0e)/60` (MATH.md §age) |
| `sisa` | string | accuracy label `"200 cm"`, or sentinel `"NO SISA AVAILABLE"`/`"NONE"` (MATH.md §URA/SISA) |
| `sisa-m` | float | numeric accuracy in metres; `0`/absent when sentinel |
| `iod` | int | issue-of-data (IODE/IODnav/AODE per constellation) |
| `orbit-disco` | float | latest ephemeris-changeover position discontinuity, metres (INTEGRITY.md §orbit-disco); `-1` sentinel when untrusted (eph age > 4 h, first-store, NaN) |
| `orbit-disco-age` | float | seconds since that discontinuity |
| `time-disco` | float | latest clock discontinuity at changeover, **nanoseconds** (INTEGRITY.md §time-disco) |
| `osnma` | bool | Galileo OSNMA authentication seen active (false for non-Galileo) |
| `alma-dist` | float | metres between broadcast-ephemeris position and this SV's almanac/TLE position (cross-check) |
| `last-seen-s` | int | seconds since any receiver last reported this SV |
| `x`,`y`,`z` | float | **ECEF metres**, broadcast-ephemeris solution at last TOW (MATH.md §Kepler / §GLONASS). **GLONASS omits these** (see note) |
| `tow` | int | time-of-week of the solution |
| `wn` | int | week number |
| `best-tle` | string | best-matching TLE line-set text (from `tlecatch` cross-check), or `""` |
| `best-tle-dist` | float | metres to that TLE |
| `delta-utc` | string | galmon-formatted UTC offset, e.g. `"-3.8 -0.8/d"` (value and drift/day) |
| `delta-gps` | string | galmon-formatted inter-system offset to GPS |
| `a0g`,`a1g` | float | GNSS-to-GPS time-offset polynomial terms |
| `af0`,`af1`,`af2` | float | SV clock correction terms (raw broadcast, MATH.md §clock) |
| `aodc`,`aode` | int | BeiDou age-of-data clock/ephemeris (BeiDou only) |
| `t0g`,`t0t` | int | reference times for the offset terms |
| `wn0g`,`wn0t` | int | reference weeks for the offset terms |
| `perrecv` | object | **nested map keyed by observer id** (below) |

`perrecv[<observerId>]` — one entry per receiver currently hearing this SV:

| Field | Type | Meaning |
|---|---|---|
| `azi` | float | azimuth deg (MATH.md §look-angles) |
| `elev` | float | elevation deg |
| `db` | int | C/N0 dB-Hz |
| `qi` | int | receiver signal quality indicator |
| `prres` | float | pseudorange residual (m) reported by the receiver PVT |
| `used` | bool | SV used in the receiver's nav solution |
| `last-seen-s` | int | seconds since this receiver last reported the SV |
| `delta_hz` | float | observed Doppler − ephemeris-predicted Doppler (Hz) (INTEGRITY.md §delta-hz) |
| `delta_hz_corr` | float | `delta_hz` after receiver clock-drift correction |

> **GLONASS `x/y/z` are omitted in `svs.json`** under the legacy API contract. GLONASS positions are delivered via `almanac.json` (§1.4). Consumers
> already special-case this (`AlmanacEntry` fills GLONASS from the almanac feed).

**Representative `svs.json` fragment:**

```json
{
  "G05@0": {
    "fullName": "GPS-5", "name": "G05", "gnssid": 0, "svid": 5, "sigid": 0,
    "health": "OK", "healthissue": 0,
    "eph-age-m": 12.4, "sisa": "240 cm", "sisa-m": 2.4, "iod": 61,
    "orbit-disco": 0.42, "orbit-disco-age": 733.0, "time-disco": 0.9,
    "osnma": false, "alma-dist": 118.3, "last-seen-s": 2,
    "x": -15637892.3, "y": 20984773.1, "z": 6512240.7, "tow": 453612, "wn": 2371,
    "best-tle": "1 25933U 99055A ...", "best-tle-dist": 940.2,
    "delta-utc": "-3.8 -0.8/d", "af0": 0.000123, "af1": 1.1e-11, "af2": 0.0,
    "perrecv": {
      "0x00a1c3": {"azi": 143.2, "db": 47, "elev": 61.0, "qi": 7, "prres": 0.4,
                   "used": true, "last-seen-s": 2, "delta_hz": -1.2, "delta_hz_corr": 0.3}
    }
  },
  "J03@0": { "fullName": "QZSS-3", "name": "J03", "gnssid": 5, "svid": 3, "...": "..." }
}
```

### 1.2 `GET /api/global.json`

A **flat object** of system-wide counters and time offsets:

| Field | Type | Meaning |
|---|---|---|
| `last-seen` | int | epoch seconds of most recent observation across the fleet |
| `gps-svs`, `gps-sigs` | int | live GPS satellites / satellite-signals |
| `gps-utc-offset-ns` | float | broadcast GPS→UTC offset (ns) |
| `galileo-svs`, `galileo-sigs` | int | live Galileo counts |
| `gst-utc-offset-ns` | float | Galileo GST→UTC offset (ns) |
| `gst-gps-offset-ns` | float | GGTO, Galileo→GPS (ns) |
| `beidou-svs`, `beidou-sigs`, `beidou-utc-offset-ns` | int/float | BeiDou counts + BDT→UTC |
| `glonass-svs`, `glonass-sigs` | int | GLONASS counts |
| `leap-seconds` | int | current broadcast leap seconds |
| `total-live-receivers` | int | observers reporting within the online window |
| `total-live-signals` | int | distinct SV-signals live |
| `total-live-svs` | int | distinct SVs live |
| `qzss-svs`, `qzss-sigs`, `navic-svs`, `navic-sigs` | int | **ours** — superset counters |

### 1.3 `GET /api/observers.json`

A JSON **array** (not an object) of station records:

| Field | Type | Meaning |
|---|---|---|
| `id` | int/string | observer/source id (the GNF1 `sourceID`, rendered as galmon does) |
| `owner`, `remark` | string | operator + free note (`ObserverDetails`) |
| `latitude`, `longitude` | float | station position (deg) |
| `h` | float | ellipsoidal height (m) |
| `hwversion`, `swversion`, `githash` | string | receiver HW/FW + feeder build id |
| `vendor`, `mods`, `serialno` | string | receiver vendor / module string / serial |
| `last-seen` | int | epoch seconds |
| `uptime` | int | seconds |
| `clockdriftns` | float | receiver clock drift (ns) |
| `acc` | float | reported position accuracy (m) |
| `svs` | object | map keyed `E04@1` → per-SV reception (below) |

`svs[<name@sigid>]`: `{fullName, name, gnss (int), sv (svid), sigid, azi, elev, db, qi,
prres, used, last-seen, age-s}`. Note several numeric fields historically arrive as floats;
emit them as JSON numbers (intsat's `rawObserver` coerces float64→int).

### 1.4 `GET /api/almanac.json`

A JSON **object keyed by SV name** (`"C01"`, `"R07"`, `"J02"`, `"I03"`). **This feed supplies
positions `svs.json` omits — above all GLONASS.**

> ⚠️ **UNIT WARNING:** almanac ECEF is in **KILOMETRES**, whereas `svs.json` ECEF is in
> **METRES**. This is part of the shipped legacy consumer contract (`AlmanacEntry.EcefMeters`
> multiplies by 1000). Do not "fix" it. Emit `eph-ecefX/Y/Z` in km.

| Field | Type | Meaning |
|---|---|---|
| `name` | string | SV name |
| `gnssid` | int | galmon-emit gnssid (+ ours 5/7) |
| `observed` | bool | true if we currently hear it (vs. almanac-only) |
| `eph-ecefX`,`eph-ecefY`,`eph-ecefZ` | float | **ECEF kilometres** (almanac-propagated, MATH.md §almanac) |
| `eph-latitude`,`eph-longitude` | float | sub-satellite lat/lon (deg) — fallback when ECEF absent |
| `inclination` | float | orbital inclination (rad or deg per galmon convention — match galmon: radians) |
| `t0e` | int | almanac reference time |
| `t` | int | evaluation time |
| `lambdana`, `tlambdana` | float | **GLONASS-only** — longitude of ascending node & its time (the F0 fix path; MATH.md §GLONASS-almanac) |
| `eph-source` | string | `""` = computed by us from broadcast almanac; `"tle-sgp4"` = filled from CelesTrak TLE when no broadcast almanac (MATH.md §TLE-fill) |

### 1.5 `GET /api/sbas.json`

A JSON **object keyed by SBAS PRN** (`"120"`, `"131"`, …):

| Field | Type | Meaning |
|---|---|---|
| `health` | string | SBAS health text |
| `last-seen` | int | epoch seconds |
| `last-seen-s` | int | seconds since last seen |
| `last-type-0` | int | epoch of last MT0 (test/do-not-use) |
| `last-type-0-s` | int | seconds since last MT0 |
| `perrecv` | object | map keyed by observer id → `{last-seen, last-seen-s}` |

SBAS coverage (WAAS/EGNOS/**MSAS** (Japan)/**GAGAN** (India)/SDCM) is enumerated in
`docs/CONSTELLATIONS.md §SBAS`.

---

## 2. schema-1.1 normalized API (Tier b — native)

Same data, **numeric-coded and underscored**, wrapped in a version envelope so clients localize
themselves instead of parsing English. Endpoints:

`/gnss/api/v1.1/svs`, `/global`, `/observers`, `/almanac`, `/sbas`.

Envelope:

```json
{ "schema": "1.1", "svs": { "E14@1": { ... } } }
```

The coded substitutions (everything else is the §1 field, **underscored**):

| galmon (Tier a) | schema-1.1 (Tier b) | Type | Notes |
|---|---|---|---|
| `health` (string) | `health_code` | int enum | **0** unknown · **1** OK · **2** not-ok · **3** dont-use. **Frozen contract** — mirror of frontend `useHealth.js CODE_BY_NUM`; never reorder or renumber. |
| `healthissue` (bool/int) | `health_issue_level` | int | 0/1/2 severity |
| `health` trailing `": N"` | `health_subcode` | int | broadcast health bits (0 when none) |
| `sisa` (string) | `sisa_valid` | bool | false for `NONE`/`NO SISA AVAILABLE`/empty |
| `sisa-m` | `sisa_m` | float | numeric accuracy (m) |
| `eph-age-m` | `eph_age_m` | float | |
| `orbit-disco` | `orbit_disco` | float | |
| `time-disco` | `time_disco` | float | |
| `last-seen-s` | `last_seen_s` | int | |
| `best-tle` | `best_tle` | string | |
| `delta-utc`/`delta-gps` | `delta_utc`/`delta_gps` | string | kept for parity |

Tier b is a **full superset**: it also carries `best_tle`, `best_tle_dist`, `a0g`,`a1g`,
`af0`,`af1`,`af2`, `aodc`,`aode`, `t0g`,`t0t`,`wn0g`,`wn0t`, and the `perrecv` map. QZSS/NavIC
SVs appear here first-class (that's the point of the coded feed — the browser renders them
without English-parsing hacks).

**Representative v1.1 `svs` entry:**

```json
{
  "schema": "1.1",
  "svs": {
    "E14@1": {
      "full_name": "Galileo-14 (E1B)", "name": "E14", "gnssid": 2, "svid": 14, "sigid": 1,
      "health_code": 1, "health_issue_level": 0, "health_subcode": 0,
      "sisa_valid": true, "sisa_m": 3.12,
      "eph_age_m": 8.7, "orbit_disco": 0.0, "time_disco": 0.0,
      "osnma": true, "last_seen_s": 1,
      "x": 12345678.9, "y": -8765432.1, "z": 23456789.0, "tow": 453612, "wn": 1298,
      "af0": 1.2e-4, "af1": 4.5e-12, "af2": 0.0,
      "perrecv": { "0x00a1c3": { "azi": 210.4, "elev": 33.1, "db": 44, "qi": 6,
                                 "prres": 0.7, "used": true, "last_seen_s": 1,
                                 "delta_hz": 0.4, "delta_hz_corr": 0.1 } }
    }
  }
}
```

Additional native JSON (envelope `{"ok":true,"time":"…","data":{…}}`, non-versioned):
`/gnss/api/{sky, sv/{sv}, constellations, events, events/summary, stations, station/{id},
geolocate}` — carried forward from intsat's serve API so its clients keep working when
`navlistener` absorbs the serve role in Phase 2.

---

## 3. SSE + event API

Live state-change events (the integrity namesake). SSE stream at **`GET /gnss/events`**; the
same events are queryable at `/gnss/api/events` and summarized at `/gnss/api/events/summary`.

Event object:

| Field | Type | Meaning |
|---|---|---|
| `id` | int64 | monotonic event id (BIGSERIAL in Phase 2) |
| `time` | RFC3339 | confirmation time (post-debounce) |
| `sv` | string | `name@sigid` |
| `type` | string | event type (below) |
| `old_value`,`new_value` | string | pre/post state |
| `severity` | int | **0** info · **1** warn · **2** crit |
| `message` | string | English fallback headline |
| `params` | object | interpolation values for client-side i18n (sv, constellation, magnitudes) |
| `raw` | object | the raw discriminators (JSONB) |

Event types (the galmonmon set + our supersets):

`health_change`, `eph_aged`, `orbit_disco`, `clock_jump`, `sisa_change`, `observation_lost`,
`osnma_change`, `sbas_health`, **`qzss_health`**, **`navic_health`** (ours — first-class Japan/
India monitoring).

Detection thresholds, debounce, and severity escalation are defined once in
`docs/INTEGRITY.md §thresholds` — **do not duplicate the numbers here**; this section is the
wire shape only.

---

## 4. Persistence contract (Phase 2)

Phase 1 emits feeds only; intsat's `gnss-history-collect` still owns the DB. Phase 2 absorbs
collect, and `navlistener` writes TimescaleDB directly and fires `NOTIFY` itself. The schema is
a **superset** of intsat's `migrations/001_init.sql` so its serve path keeps reading unchanged.

```sql
-- Events (intsat-compatible; navlistener writes, serve LISTENs)
CREATE TABLE gnss_events (
    id         BIGSERIAL,
    time       TIMESTAMPTZ NOT NULL,
    sv         TEXT        NOT NULL,
    event_type TEXT        NOT NULL,
    old_value  TEXT, new_value TEXT,
    severity   SMALLINT    NOT NULL DEFAULT 0,
    message    TEXT,
    raw        JSONB
);
SELECT create_hypertable('gnss_events','time', if_not_exists => TRUE);
CREATE INDEX ON gnss_events (sv, time DESC);
CREATE INDEX ON gnss_events (event_type, time DESC);
CREATE INDEX ON gnss_events (severity, time DESC);
-- AFTER INSERT trigger -> pg_notify('gnss_event', json{id,sv,type,severity,message})
CREATE TRIGGER gnss_events_notify AFTER INSERT ON gnss_events
    FOR EACH ROW EXECUTE FUNCTION notify_gnss_event();

-- Endpoint snapshots (raw feed dumps for replay/backfill)
CREATE TABLE gnss_snapshots (
    time     TIMESTAMPTZ NOT NULL,
    endpoint TEXT        NOT NULL,   -- 'svs' | 'global' | 'observers' | 'almanac' | 'sbas'
    data     JSONB       NOT NULL
);
SELECT create_hypertable('gnss_snapshots','time', if_not_exists => TRUE);
-- compress after 7 days, drop after 90 days (add_compression_policy / add_retention_policy)
```

Plus the **raw-nav-frame hypertable** `navlistener` adds — the forensic system of record,
following the `radiolistener` `observations` discipline (raw bytes **+** decoded projection,
organized on the near-monotonic **ingest** clock, event time indexed; see `docs/DESIGN.md`):

```sql
CREATE TABLE nav_frames (
    ts          TIMESTAMPTZ NOT NULL,   -- ingest time (hypertable dimension)
    received_at TIMESTAMPTZ NOT NULL,   -- receiver reception time (indexed)
    source_id   TEXT        NOT NULL,   -- GNF1 sourceID (observer)
    gnssid      SMALLINT    NOT NULL,   -- internal u-blox gnssId (0..7)
    svid        SMALLINT    NOT NULL,
    sigid       SMALLINT    NOT NULL,
    msg_type    SMALLINT    NOT NULL,   -- GNF1 nav message type (CONSTELLATIONS.md §registry)
    raw         BYTEA       NOT NULL,   -- the broadcast nav frame, untouched (re-decodable)
    decoded     JSONB,                  -- normalized projection (ephemeris/almanac params)
    decoder_ver TEXT
);
SELECT create_hypertable('nav_frames','ts', chunk_time_interval => INTERVAL '1 hour');
-- compress_segmentby = 'gnssid', compress_orderby = 'svid, ts DESC'; short raw retention,
-- long-term lives in per-SV continuous aggregates (ephemeris history).
```

Raw-plus-decoded means a decoder bug fix lets us **re-derive every historical ephemeris** from
`raw` without re-collecting — the same re-decodability guarantee `radiolistener` keeps.

---

## 5. Serving topology

Read and write paths are **physically separate** (the `radiolistener` rule):

- **Ingest (write):** the GNF1 push endpoint (authenticated feeders → sharded state), a
  different listener/authz/DB pool. See `docs/DESIGN.md §ingest`.
- **Serve (read):** live Tier-a/Tier-b feeds from **RAM**; history + SSE from the DB via
  `LISTEN gnss_event`. Binds **loopback**; a reverse proxy terminates TLS and fronts it as
  `intsat.space` so every shipped client URL keeps resolving. All third-party keys (CelesTrak
  TLE fetch, etc.) stay server-side.

Poll/emit cadence (satellites move slowly; over-polling wastes cache):

| Feed | Cadence |
|---|---|
| `svs.json` / v1.1 `svs` | 30 s (matches intsat's satellite layer) |
| `global.json` | 30 s |
| `observers.json` | 30 s |
| `almanac.json` | 60–120 s |
| `sbas.json` | 30 s |
| `/gnss/events` SSE | push-on-change (server-driven) |

An Apache `mod_cache` layer in front (as intsat runs today) is compatible and encouraged.

---

## 6. Migration / cutover

Staged integration of the navlistener feeds with intsat:

1. **Stand up `navlistener` on `collector-host`** serving Tier-a feeds on loopback, fronted at a staging
   host (e.g. `gnss.staging.intsat.net`). At least one real receiver (the `.91` u-blox, plus an
   F9T/Septentrio) feeding it via `navfeeder`.
2. **Byte-compat validation.** For a receiver that also feeds `galmon.eu`, fetch both
   `svs.json` at the same instant and structurally diff: same key set (`name@sigid`), same
   `gnssid`/`sigid` numbering, ECEF within numerical tolerance (metres), `healthissue` int-form
   accepted, `perrecv` shape identical. Confirm `almanac.json` ECEF is in **km** and GLONASS
   entries carry `lambdana`/`tlambdana`. Automate as a golden-file test using intsat's own
   `testdata/{svs,almanac,observers,global,sbas}.json` fixtures as the shape oracle.
3. **Point intsat's collector at staging.** Change `gnss-history-collect`'s upstream base URL
   (config only) from `galmon.eu` to `navlistener`. Run both collectors in parallel writing to
   *separate* DBs; diff the `gnss_events` streams for a day. Check matching signals for agreement and validate additional
   QZSS/NavIC events against their signal-specific fixtures.
4. **Promote.** Repoint production `gnss-history-collect` at `navlistener`; keep `galmon.eu` as
   a warm fallback URL for one release. `mapintsat` needs **no change** — it already hits
   `intsat.space/api/svs.json`, which now originates from us.
5. **Phase 2.** Absorb collect: `navlistener` writes `gnss_events`/`gnss_snapshots` directly
   (§4) and fires `NOTIFY`; retire `gnss-history-collect`.

**Rollback** at every step is a one-line URL revert, because Tier-a is byte-compatible: nothing
downstream knows the origin changed.
