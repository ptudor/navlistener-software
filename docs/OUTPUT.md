# navlistener — Output Contract

**Status: design (2026-07-07).** This document specifies **every byte `navlistener` emits**.
It is the output standard, designed here. The existing clients (intsat, mapintsat) are
**consumers, not designers** — they align to this contract on our schedule (§6.1). See
`docs/DESIGN.md` for the pipeline, `docs/MATH.md` for how the emitted numbers are computed,
`docs/CONSTELLATIONS.md` for signal/SV coverage, and `docs/INTEGRITY.md` for event detection.

> **Native API.** `navlistener` serves one versioned API. Consumers use the
> contract below. A differential-test harness may translate independent feed
> formats, including galmon's, to compare numerical outputs over identical
> captured frames (`docs/MATH.md §12`).

---

## 0. The contract at a glance

- **One API**, versioned: `/gnss/api/v2/{svs, global, observers, almanac, sbas}` plus the
  operational endpoints (§2), the event API and SSE stream (§3).
- **Envelope** on every JSON response:

  ```json
  { "ok": true, "time": "2026-07-07T12:00:00Z",
    "data": { "schema": "2.0", "svs": { "E14@0": { "...": "..." } } } }
  ```

  Errors: `{ "ok": false, "error": "…", "code": … }`. `time` is RFC3339 UTC.
- **Keys** are `snake_case`. **Units are SI and explicit**: metres, seconds, nanoseconds —
  the same unit for the same quantity in every feed (the almanac is metres, like everything
  else). **Every field has exactly one JSON type** — no bool-or-int fields, no
  English-to-parse. Enums are numeric codes defined in §2.2.
- **SV keys** are `name@sigid` (e.g. `G05@0`, `E14@0`, `J03@0`, `I04@0`); SV names are the
  RINEX letter + zero-padded PRN. gnssid numbering is the single internal convention
  (u-blox order, `docs/CONSTELLATIONS.md §0`): GPS 0, SBAS 1, Galileo 2, BeiDou 3, QZSS 5,
  GLONASS 6, NavIC 7. **sigid 0 is the constellation's primary civil signal** (§1.1), so the
  key normalizes to `@0` even where the u-blox NAV-SAT/SFRBX sigId differs — e.g. Galileo
  E1-B (u-blox sigId 1) is keyed `E14@0`, not `E14@1`. Consumers key on the numeric
  gnssid + this normalized sigid, never the receiver's raw sigId.
- **GLONASS is a first-class constellation**: its `x_m/y_m/z_m` appear in `svs` like every
  other SV (from the PZ-90 RK4 propagator, MATH.md §3). No feed omits a constellation's
  position as a special case.

Current consumer behaviors, pinned 2026-07-07 (migration facts, not design constraints):

- **mapintsat** (`dotnet/IntegrityMap/Services/FeedClient.cs`; converters/models in
  `IntegrityMap.Core`; Swift mirror `swift/IntegrityMap/Services/FeedClient.swift`) fetches
  `https://intsat.space/api/svs.json` and `https://intsat.space/gnss/api/v1.1/almanac`,
  multiplies almanac ECEF by 1000 (`AlmanacEntry.EcefMeters`), tolerates `healthissue` as
  bool-or-int (`FlexibleBoolConverter`), and resolves GLONASS positions from the almanac.
  Its gnssid table (`Gnss.cs`/`GNSS.swift`) is **wrong at 4 and 7** (`4=NavIC`, `7=KASS`).
- **intsat**: `gnss-history-collect` fetches the five galmon feeds via `internal/fetch`
  (upstream `[upstream] base_url`, **currently `http://localhost/api`** — an Apache
  `mod_cache` proxy fronting galmon.eu); its own detector is a galmonmon port; `serve/v11.go`
  re-emits a coded "schema 1.1" tier whose almanac passes galmon's hyphenated km fields
  through untouched.

All of that is what §6.1 migrates. None of it constrains this contract.

---

## 1. The feed set (`/gnss/api/v2/`)

### 1.1 `svs` — one entry per satellite×signal

Object keyed `name@sigid`. Fields (absent = unknown; a present field always has the type
below):

| Field | Type | Meaning / source |
|---|---|---|
| `full_name` | string | human label, e.g. `"GPS-5"`, `"Galileo-14 (E1B)"` |
| `name` | string | SV name `"G05"` (RINEX letter + PRN) |
| `gnssid` | int | constellation id (`docs/CONSTELLATIONS.md §0`) |
| `svid` | int | PRN within constellation — except **QZSS**, which serves the u-blox svId 1–10 (`J03` = svid 3 = PRN 195; PRN = svid + 192). Intentional: see `docs/CONSTELLATIONS.md §3.1` ("svId, not PRN") and the `"J03@0"` example below  |
| `sigid` | int | signal id (0 = the constellation's primary civil signal) |
| `health_code` | int | §2.2 enum: 0 unknown · 1 OK · 2 not-ok · 3 do-not-use |
| `health_issue_level` | int | 0 none · 1 warning · 2 error |
| `health_subcode` | int | raw broadcast health bits (0 when none). **GLONASS**: a packed pair — low 3 bits = the raw Bn word (only its MSB, value 4, is the malfunction flag), bit 3 (value 8) = the GLONASS-M ℓn fast malfunction flag (regression fix; GLO-ICD-5.1 §4.4, Table 5.1) — so a Bn-vs-ℓn disagreement window is visible |
| `eph_age_m` | float | ephemeris age, minutes = `ephAge(tow,t0e)/60` (MATH.md §1.1). **Sign convention :** legitimately **negative** while now precedes the reference epoch — for GLONASS that is the steady state for roughly the first half of every tb interval, because the immediate data are referred to the *middle* of the interval (GLO-ICD-5.1 §4.4); Kepler-family ages can likewise be briefly negative before toe. Consumers must not assert `eph_age_m ≥ 0`. Past the constellation's serving cap the value switches to the monotone wall-clock age since apply  |
| `sisa_valid` | bool | false when the broadcast accuracy is a "none/no accuracy" sentinel |
| `sisa_m` | float | URA/SISA in metres (MATH.md §6); meaningful only when `sisa_valid` |
| `acc_index` | int | raw broadcast accuracy index (URA / URA_ED / SISA per constellation, MATH.md §6); present whenever an accuracy field has been decoded — including the "no accuracy prediction, use at own risk" sentinels (GPS/QZSS URA 15, IS-GPS-200N §20.3.3.3.1.3; CNAV URA_ED 15/−16; Galileo SISA 255) that `sisa_valid=false` alone can't distinguish from "not yet decoded"  |
| `alert` | bool | GPS/QZSS broadcast URA-alert flag (regression fix/regression fix; IS-GPS-200N §20.3.3.2 LNAV HOW bit 18, CNAV header bit 38): true = the SV declares its URA may be worse than broadcast — use at own risk; absent until decoded |
| `wn_mismatch` | bool | broadcast week number disagrees with the collector wall-clock week after rollover disambiguation; absent until a broadcast WN has been decoded |
| `iod` | int | issue-of-data (IODE/IODnav/AODE per constellation) |
| `orbit_disco_m` | float | position discontinuity at last ephemeris changeover, metres (INTEGRITY.md §3); **absent** when not yet computable (first ephemeris, stale, failed guard) — never a sentinel number |
| `orbit_disco_age_s` | float | seconds since that changeover |
| `time_disco_ns` | float | clock discontinuity at changeover, nanoseconds; absent like `orbit_disco_m` |
| `osnma` | bool | Galileo OSNMA authentication seen active (absent for non-Galileo) |
| `alma_dist_m` | float | broadcast-ephemeris vs almanac/TLE position distance (cross-check) |
| `last_seen_s` | int | seconds since any receiver last reported this SV |
| `freq_ch` | int | GLONASS-only : the FDMA frequency channel k ∈ [−7,+6] the tracked signal was received on (receiver `freqId − 7`, boundary-validated). Cross-checkable against the almanac entry's `freq_ch` (HnA-derived) for the same slot — a mismatch means mis-identification or spoofing. Absent for other constellations |
| `x_m`,`y_m`,`z_m` | float | ECEF metres at `tow` — **all constellations, GLONASS included** |
| `tow` | int | time-of-week (s) of the solution; `wn` | int | week number (full, disambiguated) — **GPS-continuous** (GPS week number, no 1024-week rollover) for GPS, Galileo, QZSS, and NavIC; **BeiDou is the exception**, reported as its own native BDT week (GPS week − 1356, BDT epoch 2006-01-01) — regression fix. A consumer diffing `wn` against the broadcast WN sees a 1024-week offset for Galileo/NavIC but not for BeiDou; this is intentional, not a bug, and is not expected to change without a version bump. |
| `best_tle` | string | name of best-matching CelesTrak object (MATH.md §11), absent if none |
| `best_tle_dist_m` | float | metres to the SGP4 position of that object |
| `klob_alpha`, `klob_beta` | float[4] | raw broadcast ionosphere coefficient sets  — today BeiDou B1I D1 subframe-1 α/β (BDS-SIS-ICD-B1I §5.2.4.7, a materially different model from GPS's Klobuchar — do not feed to a GPS evaluator); served for query/replay and evaluator; absent until decoded |
| `bdgim` | float[9] | BeiDou B-CNAV2 MT30's BDGIM α1..α9 (B2a Table 7-10, TECu), raw — evaluation is follow-up |
| `dif`, `sif`, `aif` | bool | BeiDou B-CNAV2 entries : the B2a signal's broadcast real-time integrity flags (BDS-SIS-ICD-B2a Table 7-23 — data/signal/accuracy integrity), refreshed ~every 3 s; absent until the flag block decodes |
| `sismai` | int | the raw 4-bit signal-in-space monitoring accuracy index accompanying them (§7.17 semantics deferred by the ICD — raw only) |
| `utc_offset_ns` | float | broadcast system→UTC offset (ns), from the UTC parameters (MATH.md §8), evaluated at the feed instant. Populated today for BeiDou B-CNAV2 entries from MT34's BDT-UTC set (regression fix, Eq. 7-25 with the post-event ΔtLSF arm); sign: t_system − t_UTC (positive = the system's scale is ahead of UTC). Absent until decoded |
| `dt_ls`, `dt_lsf`, `wn_lsf`, `dn` | int | raw broadcast leap schedule accompanying `utc_offset_ns` (current/post-event leap counts, event week, event day) so consumers can handle a pending leap themselves; absent with it |
| `leap_mismatch` | bool | the broadcast current leap count disagrees with this collector's configured GPS−UTC count (regression fix — the regression fix cross-check); absent until a UTC set has decoded |
| `utc_drift_ns_day` | float | its drift term, ns/day |
| `gps_offset_ns` | float | broadcast system→GPS offset (ns; GGTO for Galileo, τ_GPS for GLONASS…), evaluated at the feed instant. Sign: t_system − t_GPS (positive = the system's time scale is ahead of GPS — Galileo Eq. 24 Δt_systems, GAL-OS-SIS-ICD-2.2 §5.1.8). Absent until decoded, and absent again on Galileo's all-ones broadcast withdrawal (§5.1.8) — never a stale offset  |
| `a0g`,`a1g`,`t0g`,`wn0g` | float/int | raw inter-system offset polynomial terms (Galileo: a0g s, a1g s/s, t0g s, wn0g raw 6-bit truncated week — consumers re-evaluating at their own epoch disambiguate wn0g mod-64, exact under §5.1.8's ±31-week bound) |
| `af0`,`af1`,`af2` | float | raw SV clock polynomial (MATH.md §4) |
| `aodc`,`aode` | int | BeiDou age-of-data (BeiDou only) |
| `conf` | int | corroboration count (INTEGRITY.md §6): distinct sources with a structurally-decoded nav frame for this satellite×signal within the 60 s fresh-receiver window. Always present — 0 = no current nav corroboration (e.g. an observation-only entry), 1 = a single receiver's testimony, ≥ 2 = independently corroborated. Also stamped into every SV event's `params`. The §6 broadcast-agreement *divergence* detector (same SV/IOD, different bits → hard alarm) is tracked P7 work — conf counts presence, it does not yet compare element sets |
| `perrecv` | object | per-observer reception, keyed by observer id (below) |

`perrecv[<observer_id>]`:

| Field | Type | Meaning |
|---|---|---|
| `azi_deg`, `elev_deg` | float | look angles from that observer (MATH.md §5.2) |
| `cn0_db_hz` | int | carrier-to-noise density |
| `qi` | int | receiver quality indicator (0–7, u-blox scale) |
| `prres_m` | float | pseudorange residual reported by the receiver PVT |
| `used` | bool | SV used in the receiver's own nav solution |
| `last_seen_s` | int | seconds since this receiver last reported the SV |
| `delta_hz` | float | observed − predicted Doppler (INTEGRITY.md §3) |
| `delta_hz_corr` | float | `delta_hz` after receiver clock-drift correction |
| `iono_delay_m` | float | **measured** slant ionospheric delay, carrier-leveled geometry-free dual-frequency (MATH.md §7.4); absent when the receiver tracks only one frequency of this SV |
| `iono_model_m` | float | broadcast-model slant delay for the same epoch/geometry (Klobuchar/NeQuick-G/BDGIM) |
| `iono_resid_m` | float | `iono_delay_m − iono_model_m` — the model-vs-reality integrity signal |
| `iono_pair_sigid` | int | sigid of the second signal in the measuring pair |
| `iono_cal` | int | receiver-DCB calibration method: 0 uncalibrated · 1 daily model fit · 2 external DCB product |

Representative entry:

```json
{ "ok": true, "time": "2026-07-07T12:00:00Z", "data": { "schema": "2.0", "svs": {
  "G05@0": {
    "full_name": "GPS-5", "name": "G05", "gnssid": 0, "svid": 5, "sigid": 0,
    "health_code": 1, "health_issue_level": 0, "health_subcode": 0,
    "eph_age_m": 12.4, "sisa_valid": true, "sisa_m": 2.4, "iod": 61,
    "orbit_disco_m": 0.42, "orbit_disco_age_s": 733.0, "time_disco_ns": 0.9,
    "alma_dist_m": 118.3, "last_seen_s": 2,
    "x_m": -15637892.3, "y_m": 20984773.1, "z_m": 6512240.7, "tow": 453612, "wn": 2427,
    "best_tle": "GPS BIIR-2 (PRN 05)", "best_tle_dist_m": 940.2,
    "utc_offset_ns": -3.8, "utc_drift_ns_day": -0.8,
    "af0": 0.000123, "af1": 1.1e-11, "af2": 0.0,
    "perrecv": {
      "0x00a1c3": { "azi_deg": 143.2, "elev_deg": 61.0, "cn0_db_hz": 47, "qi": 7,
                    "prres_m": 0.4, "used": true, "last_seen_s": 2,
                    "delta_hz": -1.2, "delta_hz_corr": 0.3 } }
  },
  "J03@0": { "full_name": "QZSS-3", "name": "J03", "gnssid": 5, "svid": 3, "...": "..." }
} } }
```

### 1.2 `global` — system-wide counters and time offsets

Flat object inside the envelope: `last_seen` (epoch s), per-constellation `<c>_svs` /
`<c>_sigs` live counts for `gps, galileo, beidou, glonass, qzss, navic, sbas`,
`gps_utc_offset_ns`, `gst_utc_offset_ns`, `gst_gps_offset_ns`, `bdt_utc_offset_ns`,
`glonass_utc_su_offset_ns`, `leap_seconds`, `total_live_receivers`, `total_live_signals`,
`total_live_svs`. Offsets are transcribed broadcast values (MATH.md §8), not computed by us;
receiver disagreement on them is an integrity signal, not an averaging problem.

### 1.3 `observers` — the station list

Array of station records: `id`, `owner`, `remark`, `latitude_deg`, `longitude_deg`,
`height_m` (ellipsoidal), `hw_version`, `sw_version`, `git_hash`, `vendor`, `mods`,
`serial_no`, `last_seen` (epoch s), `uptime_s`, `clock_drift_ns`, `accuracy_m`, and `svs` —
a map keyed `name@sigid` of per-SV reception mirroring the `perrecv` shape (§1.1) plus
`age_s`. String fields that originate on the receiver (`owner`, `remark`, `vendor`, …) are
sanitized before serialization (INTEGRITY.md §9).

### 1.4 `almanac` — coarse orbits for every known SV

Object keyed by SV name (`"C01"`, `"R07"`, `"J02"`, `"I03"`). This is the long-life,
all-SV view (acquisition-grade); `svs` remains the precision view. Fields:

| Field | Type | Meaning |
|---|---|---|
| `name` | string | SV name |
| `gnssid` | int | constellation id |
| `observed` | bool | currently heard by ≥1 receiver (vs. almanac-only) |
| `ecef_x_m`,`ecef_y_m`,`ecef_z_m` | float | almanac-propagated ECEF, **metres** (MATH.md §10) |
| `lat_deg`,`lon_deg` | float | sub-satellite point (fallback / display) |
| `inclination_rad` | float | orbital inclination, radians |
| `t0e` | int | constellation-native almanac reference: GPS-family `Toe` in seconds-of-week; GLONASS `t_lambda` in seconds-of-day |
| `t` | int | evaluation time as Unix UTC seconds |
| `lambda_na`,`t_lambda_na` | float | GLONASS-only: ascending-node longitude and its epoch (MATH.md §3.1) |
| `operable` | bool | GLONASS-only : the almanac CnA ground-segment health flag, `true` = operable. **Polarity note:** the broadcast word is inverted vs Bn/ℓn — Cn = 0 means malfunction (GLO-ICD-5.1 §5.3); this field re-normalizes it so `true` is always healthy. For an out-of-view slot this is the SV's only broadcast health surface (the ground path reaches every SV's almanac within ~16 h, §5.3); an inoperable slot's entry is still served — it's the flag that matters, not suppression. Absent for other constellations and until the slot's almanac decodes. |
| `freq_ch` | int | GLONASS-only : the slot's FDMA channel k from the broadcast almanac word HnA (GLO-ICD-5.1 Table 4.10) — the almanac side of the eph-vs-almanac channel cross-check (see `svs.freq_ch`) |
| `eph_source` | int | §2.2 enum: 0 broadcast-almanac · 1 tle-sgp4 fill |

`t0e` and `t` deliberately use different time bases; `t - t0e` is **not** an almanac age.

### 1.5 `sbas` — augmentation-system health

Object keyed by SBAS PRN (`"131"`, `"136"`, …): `provider` (string, e.g. `"WAAS"`,
`"EGNOS"`, `"MSAS"`, `"GAGAN"` — the §CONSTELLATIONS provider table), `health_code`,
`last_seen`, `last_seen_s`, `last_type` (the raw message type of the most recent decoded
message, 0–63 — regression fix), `last_type_0`, `last_type_0_s` (when the last MT0 was seen;
absent until one has been).

`health_code` is 1 (OK) or 3 (do-not-use) and **latches on MT0 recency** : it
reads 3 while the last MT0 ("do not use for safety applications") is younger than the
DO-229-family 60 s exclusion (`sbasType0Hold`, QZSS-L1S §4.1.2.3), because a system
under test interleaves MT0 with its normal stream (the MT0/2 pattern, EGNOS-SDD-OS
§4.1) and a DO-229 receiver excludes the GEO on any MT0 sighting — the last *message*
being MT≠0 does not mean the GEO is usable. Consumers wanting the raw recency read
`last_type_0_s`.

`perrecv` (observer id → `{last_seen, last_seen_s}`) is **contract-reserved, not yet
served** : the SBAS state is currently store-global per PRN, with no
per-observer reception map; the field will appear when per-station SBAS attribution
lands.

---

## 2. Operational endpoints, enums

### 2.1 Operational API

Same envelope, non-versioned paths carried forward from intsat's serve role (verified
against its `server.go` route table): `/gnss/api/{sky, sky/history, sv/{sv},
constellations, events, events/summary, stations, station/{id}, availability, geolocate}`
and `/gnss/health`. When `navlistener` absorbs the serve role these move here unchanged in
shape; they are read-side conveniences over the same state and DB.

New enrichment endpoints (ours — the Japan/India product surface, `docs/CONSTELLATIONS.md
§3.2/§4.1`): `/gnss/api/qzss-dcr` (planned; unavailable until the QZSS L1S decoder lands) and
`/gnss/api/navic-text` (planned; unavailable until the NavIC decoder lands). Same envelope; message streams,
not positioning inputs.

### 2.2 Enums (frozen)

| Enum | Values |
|---|---|
| `health_code` | **0** unknown · **1** OK · **2** not-ok · **3** do-not-use. Matches the deployed `useHealth.js CODE_BY_NUM` / intsat `model.HealthCode` — retained because the numbering is already right; never reorder. |
| `health_issue_level` | 0 none · 1 warning · 2 error |
| `severity` (events) | 0 info · 1 warning · 2 critical |
| `eph_source` | 0 computed from broadcast almanac · 1 filled from CelesTrak TLE via SGP4 |

---

## 3. Events: SSE + query API

Live integrity events (the namesake). SSE stream at **`GET /gnss/events`**; queryable at
`/gnss/api/events`, summarized at `/gnss/api/events/summary`.

Event object (SSE `data:` payload and API rows; the JSON key is `type` — the DB column is
`event_type`):

| Field | Type | Meaning |
|---|---|---|
| `id` | int64 | monotonic event id (BIGSERIAL) |
| `time` | RFC3339 | confirmation time (post-debounce) |
| `sv` | string | `name@sigid` |
| `type` | string | event type (INTEGRITY.md §5 — the authoritative vocabulary) |
| `old_value`,`new_value` | string | pre/post state |
| `severity` | int | §2.2 |
| `message` | string | English fallback headline |
| `params` | object | interpolation values for client-side i18n |
| `raw` | object | raw discriminators (JSONB) |

SSE contract (defined here; intsat's shipped broker already matches it, verified 2026-07-07):
named events `event: gnss` (an integrity event, `id:` set), `event: status` (heartbeat,
default every 60 s), `event: resolved`; reconnect via `Last-Event-ID` replays from that id
(bounded), else the most recent N (default 20). `Content-Type: text/event-stream`,
`X-Accel-Buffering: no`.

Event types and their thresholds/severities are defined once, in `docs/INTEGRITY.md §2/§5` —
this section is the wire shape only.

---

## 4. Persistence contract

`navlistener` writes TimescaleDB directly and fires `NOTIFY`. The schema is a **superset of
intsat's `migrations/001_init.sql`** (verified 2026-07-07) so intsat's read path keeps
working while it is still the serve front; the additions are ours.

```sql
-- Events (navlistener writes; serve-side LISTENs)
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
CREATE INDEX idx_gnss_events_sv_time       ON gnss_events (sv, time DESC);
CREATE INDEX idx_gnss_events_type_time     ON gnss_events (event_type, time DESC);
CREATE INDEX idx_gnss_events_severity_time ON gnss_events (severity, time DESC);
-- notify_gnss_event(): pg_notify('gnss_event', json{id,sv,type,severity})
-- The payload identifies the row; LISTENers fetch the full row (including message) by id.
CREATE TRIGGER gnss_event_notify AFTER INSERT ON gnss_events
    FOR EACH ROW EXECUTE FUNCTION notify_gnss_event();

-- Endpoint snapshots (feed dumps for replay/backfill)
CREATE TABLE gnss_snapshots (
    time     TIMESTAMPTZ NOT NULL,
    endpoint TEXT        NOT NULL,   -- 'svs' | 'global' | 'observers' | 'almanac' | 'sbas'
    data     JSONB       NOT NULL
);
SELECT create_hypertable('gnss_snapshots','time', if_not_exists => TRUE);
-- compress after 7 days, drop after 90 days (add_compression_policy / add_retention_policy)
```

(intsat's migration also carries a small `daemon_state` key/value table; it stays intsat's.)

Plus the **raw-nav-frame hypertable** — the forensic system of record, following the
`radiolistener` `observations` discipline (raw bytes **+** decoded projection, organized on
the near-monotonic **ingest** clock, event time indexed; see `docs/DESIGN.md`):

```sql
CREATE TABLE nav_frames (
    ts          TIMESTAMPTZ NOT NULL,   -- ingest time (hypertable dimension)
    received_at TIMESTAMPTZ NOT NULL,   -- receiver reception time (indexed)
    source_id   TEXT        NOT NULL,   -- GNF1 sourceID (observer)
    gnssid      SMALLINT    NOT NULL,   -- gnssId (0..7, CONSTELLATIONS.md §0)
    svid        SMALLINT    NOT NULL,
    sigid       SMALLINT    NOT NULL,
    msg_type    SMALLINT    NOT NULL,   -- GNF1 nav message type (CONSTELLATIONS.md §6)
    raw         BYTEA       NOT NULL,   -- the broadcast nav frame, untouched (re-decodable)
    decoded     JSONB,                  -- normalized projection (ephemeris/almanac params)
    decoder_ver TEXT
);
SELECT create_hypertable('nav_frames','ts', chunk_time_interval => INTERVAL '1 hour');
-- compress_segmentby = 'gnssid', compress_orderby = 'svid, ts DESC'; short raw retention,
-- long-term lives in per-SV continuous aggregates (ephemeris history).
```

Raw-plus-decoded means a decoder bug fix lets us **re-derive every historical ephemeris**
from `raw` without re-collecting — the same re-decodability guarantee `radiolistener` keeps.

---

## 5. Serving topology

Read and write paths are **physically separate** (the `radiolistener` rule):

- **Ingest (write):** the GNF1 push endpoint (authenticated feeders → sharded state), a
  different listener/authz/DB pool. See `docs/DESIGN.md §1`.
- **Serve (read):** live feeds from **RAM**; history + SSE from the DB via
  `LISTEN gnss_event`. Binds **loopback**; a reverse proxy terminates TLS and fronts it as
  `intsat.space` (host consolidation — same box, same front). All third-party keys
  (CelesTrak TLE fetch, etc.) stay server-side.

Refresh cadence (satellites move slowly; over-polling wastes cache):

| Feed | Cadence |
|---|---|
| `svs`, `global`, `observers`, `sbas` | 30 s |
| `almanac` | 60–120 s |
| `/gnss/events` SSE | push-on-change (server-driven) |

An Apache `mod_cache` layer in front is compatible and encouraged.

---

## 6. Migration

`navlistener` stands up serving **only this contract**. The consumers move to it; nothing
in the product bends toward what they parse today.

1. **Stand up `navlistener` on `collector-host`** serving `/gnss/api/v2/*` on loopback, fronted at a
   staging host. At least one real receiver feeding it via `navfeeder` (the `.91` u-blox,
   plus an F9T/Septentrio).
2. **Differential validation (CI, not production):** run galmon (`third_party/galmon`,
   `integrity` branch) and `navlistener` over the same captured raw frames; the harness maps
   galmon's `svs.json` fields onto §1.1 names and diffs the numbers (ECEF, clock, disco).
   Check the constants (MATH.md §0) and coordinate frames (MATH.md §3.1)
   used by each implementation when interpreting differences.
3. **Migrate intsat:** configure `gnss-history-collect` to use a v2 client
   (one envelope with typed fields),
   and retire its internal detector in favor of consuming our `gnss_events` (INTEGRITY.md).
   Run old and new side by side against separate DBs for a day; our stream must cover every
   event the old path caught, plus QZSS/NavIC events it never could.
4. **Migrate mapintsat:** point `FeedClient` (C# + Swift) at `/gnss/api/v2/{svs,almanac}`,
   apply the §6.1 fixes, delete the ×1000 and the flexible converters (typed fields make
   them dead code).
5. **Absorb serve:** `navlistener` (or its serve process) takes over the `/gnss/api/*`
   routes on `intsat.space`; `gnss-history-collect` retires; intsat's dependency on
   external feed providers ends for these consumers.

Rollback at each step: the previous upstream stays configured until the step after it
succeeds; reverting is a config change.

### 6.1 Consumer conformance (they align to this standard)

| Consumer | Required change |
|---|---|
| **mapintsat** `Gnss.cs` / `GNSS.swift` | Fix the gnssid table: **7 = NavIC, 5 = QZSS, 4 = IMES (never emitted)**. Today it labels `4=NavIC`, `7=KASS` — KASS is an SBAS *provider* (PRN 134 under gnssid 1), not a constellation. Must land before NavIC SVs appear in any feed. |
| **mapintsat** `FeedClient`/models | Fetch v2; almanac is metres (drop `EcefMeters`'s ×1000); fields are single-typed (drop `FlexibleBoolConverter`/`FlexibleIntConverter` usage for this feed). |
| **intsat** collect | Use the v2 feed client; upstream stays one `base_url` config key. |
| **intsat** detect | Retire the galmonmon-port detector; consume `gnss_events` produced by us. Until then, its thresholds were verified equal to INTEGRITY.md §2 (2026-07-07). |
| **intsat** model/constants | Add QZSS (5) and NavIC (7) constellation constants and monitored signals (5,0), (7,0); adopt the two new event types (`qzss_health`, `navic_health`). |
| **intsat** glonass TLE shim | Retire `internal/glonass` (CelesTrak SGP4 synthesis) — v2 carries real GLONASS positions from broadcast ephemeris/almanac. |
| **intsat** frontend / v1.1 | The v1.1 tier is intsat's to sunset; `useHealth.js` numbering already matches §2.2 (no change). |

No client parses SV-name letters anywhere (verified — they key on numeric `gnssid` and treat
names as opaque strings), so the `J`/`I` names flow through with zero client work.
