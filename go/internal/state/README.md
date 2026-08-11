# `internal/state` — live per-SV state and the integrity *compute*

**Headline:** the part that's really ours. Every raw frame lands here, gets decoded by the `gnss`
library, folded into a per-satellite×signal record, propagated to now, and differenced against
the previous ephemeris to produce orbit-disco and time-disco. This is the stage that turns a
stream of bits into an opinion about whether a satellite is behaving.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `state.go` | The core: `Store`, `Key`, the sharded map, `Apply`'s per-constellation dispatch, the eight `applyXxx` folders, `computeDisco`/`computeGloDisco`, `Propagate`, `Expire`. |
| `feed.go` | The `Feed*` read models — the JSON shapes `serve` publishes. |
| `capability.go` | The empirical capability fingerprint and `CapabilityDiff`. |
| `rf.go` | PNT-defense Tier-0: RF telemetry → per-station jamming and spoof-gate metrics. |
| `iono.go` | The measured dual-frequency ionosphere per receiver. |
| `snapshot.go` | The compact `/debug/state` diagnostic view. |
| `*_test.go` | 27 test files — per-constellation folding, disco computation, health, expiry, capability, RF, and the regression guards. |
| `README.md` | This file. |

Imports `github.com/ptudor/gnss` (all of it), `internal/ingest` (for `RawFrame`), and
`internal/metrics`.

---

## Summary

### The data model

```go
type Key struct {
    G   gnss.GNSSID
    Sv  int
    Sig int
}
func (k Key) Name() string  // "G05@0", "J03@0", "E14@3"
```

**One entry per satellite × signal**, not per satellite. That's deliberate and load-bearing:
Galileo's E1-B I/NAV and E5a F/NAV carry the same ephemeris on different signals, and keeping
them as separate entries (`E14@0` and `E14@3`) is what makes the cross-signal integrity
comparison possible at all. Same for GPS L1 C/A vs L2C CNAV, and BeiDou B1I vs B2a.

### Concurrency

The store is **sharded, and each shard is mutex-guarded.** Ingest goroutines fold frames
concurrently while the serve side reads race-free. No unlocked shared map anywhere — that's the
C1–C11 discipline, and `make race` is the enforcement.

Shard count comes from `[state].shards`.

### The public surface

```go
func New(n int) *Store
func (s *Store) Apply(f *ingest.RawFrame)          // fold one frame
func (s *Store) Propagate(now time.Time)           // re-propagate every current ephemeris
func (s *Store) Expire(now, ttl)                   // drop SVs unseen for ttl
func (s *Store) ExpireStations(now)                // drop stale sbas/rf/almanac entries

func (s *Store) FeedSVs(now) map[string]FeedSV     // the svs feed
func (s *Store) FeedGlobal(now) GlobalFeed
func (s *Store) FeedAlmanac(now) map[string]AlmanacEntry
func (s *Store) FeedSBAS(now) map[string]SBASEntry
func (s *Store) FeedStationRF(now) map[string]StationRF
func (s *Store) FeedStationCapabilities(now) map[string][]StationCapability
func (s *Store) FeedCapabilityReports(now) map[string]StationCapReport
func (s *Store) Snapshot(now) Snapshot             // /debug/state
func (s *Store) SBASDetect(now) map[string]SBASEntry
func (s *Store) LiveReceivers(now) int
func (s *Store) StationLastSeen(now) map[string]int
func (s *Store) SetDeclaredCapabilities(decl map[string][]CapSignal)
func CapabilityDiff(observed, declared) (unexpected, missing []CapSignal)
func SetLeapSeconds(n int)
```

**Compute is here; detection is next door.** This package produces metrics. `internal/detect`
turns them into debounced, typed events. Keeping the split clean is what lets `detect` be a pure
function of a snapshot.

---

## Details

### `Apply` — the dispatch

One frame in, one fold. The dispatch routes on `(gnssId, sigId)`:

| Constellation / signal | Folder | Notes |
|---|---|---|
| GPS or QZSS, L1 C/A | `applyGPSLNAV` | QZSS reuses the GPS decoder verbatim. |
| GPS or QZSS, L2C/L5 | `applyGPSCNAV` | Keyed to its own sigId entry. |
| Galileo sigId 0/1 (E1-B/C) | `applyGalileoINAV` | The `@0` entry. |
| Galileo sigId 3/4 (E5a-I/Q) | `applyGalileoFNAV` | A separate `E##@3` entry. |
| BeiDou B1I | `applyBeiDouD1` | |
| BeiDou B2a | `applyBeiDouBCNAV2` | The `C##@8` entry. |
| GLONASS | `applyGLONASS` | Strings, plus almanac pairs. |
| SBAS | `applySBAS` | Message-type tracking and MT 0. |
| NavIC | — | Counted under `navic_deferred` and dropped. |

Two guards run before any decode:

- **`gnssId` range**  — an id outside 0..7-minus-IMES is rejected and counted as
  `DecodeErrorsTotal{gnssid="out_of_range", kind="gnssid_range"}`. The raw byte is deliberately
  **not** printed, since an attacker-supplied id would otherwise mint an unbounded Prometheus
  label set.
- **`svId` range** — the SFRBX header's SV id is unauthenticated by the message's own CRC or
  parity, so per-constellation envelopes from vendored primary texts bound it. The **SBAS case is
  load-bearing**: `DecodeSBASL1` validates only the message's own CRC, so without this an
  out-of-range PRN would mint a bogus GEO entry. Rejects are counted under the `svid_range` label
  so a misbehaving receiver is visible rather than silently filtered.

### The integrity compute — `computeDisco`

This is the heart of it. When a new ephemeris arrives for an SV that already had one, the two are
compared **at a common instant**:

- **orbit-disco (metres)** — the distance between the position the *old* ephemeris predicts and
  the position the *new* one predicts.
- **time-disco (nanoseconds)** — the same difference for the clock correction.

Two design choices matter:

**Evaluate at the midpoint.** Each side extrapolates only half the update interval, which keeps
both within their fit window instead of asking the outgoing set to predict far past its validity.

**Guard on freshness and prior state.** The outgoing set must actually be older than the
incoming one, and the SV must have had a prior ephemeris (`haveEph`). That second guard is why a
restart doesn't produce a wave of fake discos: after a restart there is no previous set, so the
first ephemeris establishes a baseline and the second one produces the first legitimate disco
.

`computeGloDisco` does the same thing for GLONASS, on the RK4 path with day-relative timing.

**Partial-state safety** : the disco computation is written so a panic or error partway
through cannot leave the entry holding a half-updated ephemeris that would produce a garbage
disco on the *next* frame.

### Health — harder than it looks

Health is not one bit, and this package refuses to pretend it is.

- **`HaveHealth`**  — health is meaningful only when true. "Healthy" and "not yet decoded"
  are different states and the feed distinguishes them.
- **GPS/QZSS CNAV** carries a 3-bit L1/L2/L5 field (MT10 bits 52–54). `cnavCarrierHealth` maps it
  to the entry's own carrier — and **deliberately drops the L1 bit**, because this
  entry is the L2C/L5 sigId and the `@0` entry carries its own independently decoded LNAV health.
  Folding CNAV's L1 opinion into the L1 entry would destroy exactly the LNAV-vs-CNAV cross-signal
  comparison the integrity design wants.
- **Galileo** keeps E1-B health and E5b health as separate fields (bit 69 vs bit 67) — see
  `gnss/frame`'s regression fix.
- **GLONASS** needs Bn's MSB, ℓn, and the almanac's Cn considered jointly per ICD Table 5.1, with
  Cn's **inverted** polarity (1 = operable) normalized on the way into the feed.
- **SBAS** health is not keyed on the last message: a GEO interleaves MT 0 with its normal stream
  (the DO-229 "MT0/2" pattern that EGNOS-SDD-OS §4.1 warns about), so naive last-message keying
  flapped OK ↔ do-not-use.

### The capability fingerprint

Every observer is a real receiver with real silicon limits — a ZED-F9T-00B is L1+L2, a -10B is
L1+L5, and they report the *same* MON-VER string. So a node's **demonstrated** signal set is what
the integrity layer needs, and this package learns it empirically: every successfully decoded nav
frame is evidence that station tracks that `(gnssId, sigId)`.

Crucially, **capability is recorded only after a structural decode succeeds**  — never off
a frame that merely arrived with a tag. Otherwise a mis-tagged or crafted frame could arm the
capability detectors with a signal the station cannot produce.

`CapabilityDiff` then compares observed against the declared *tudorgps* fingerprint from config:

- **unexpected** = observed but not declared — a signal the silicon shouldn't be able to produce.
- **missing** = declared but never observed — a signal the node should produce and hasn't.

Both are nil when nothing is declared, because there's nothing to compare against. Output is
deterministically ordered by `(gnss, sig)`.

### PNT-defense Tier-0 (`rf.go`)

Turns the RF telemetry the fleet's receivers already produce — MON-RF/MON-HW AGC and jamming
indicators, NAV-SAT C/N₀ and elevation — into per-station detection metrics:

- **jamming**: departures from a *learned per-station baseline*, not a fixed threshold. Every site
  has its own noise floor.
- **spoofing**: the C/N₀-vs-elevation gate. Real satellites show a characteristic C/N₀ rise with
  elevation; a ground transmitter doesn't. `Cn0Stats` carries the per-constellation fit.

Thresholding is **not** done here — `internal/detect` classifies, and the design rule is that no
single gate fires a spoofing alarm (a ≥2-gate fusion is required, and a lone metric departure is
reported as a degradation, not an attack).

### Measured ionosphere (`iono.go`)

Where a receiver reports dual-frequency observables, the geometry-free combination from
`gnss/iono` gives the *actual* slant delay on that line of sight, per receiver — the
`FeedPerRecv.IonoDelayM` field, with `IonoCal` flagging that the receiver DCB has not been
separated. Comparing measured against broadcast-model delay is a spoofing tell.

### Feed models vs `/debug/state`

`feed.go` builds the rich read models that `serve` publishes. `snapshot.go` is a deliberately
smaller diagnostic view for `/debug/state`, and it stays a subset on purpose.

`snapshot.go` also records the design fact worth repeating : **there is no cross-restart
state persistence anywhere in the daemon.** Every restart rebuilds from live ingest — positions
absent until each SV re-broadcasts a full set, discos absent until a second post-restart
ephemeris. That's consistent with the "re-decode from raw frames" design: `nav_frames` is the
durable record and offline replay is the recovery path.

`Reset()` is the privacy-withdrawal counterpart: it drops a complete audience materialization
and advances its generation. Whole-view reset is intentional because merged ephemeris,
confidence, RF, capability, and almanac state cannot subtract one withdrawn source exactly.
Feed rendering retries if the generation changes mid-build, preventing a mixed pre/post-policy
body.

### Optional fields are pointers

Throughout the feed models, an unknown or not-yet-computable value is a `nil` pointer and is
**absent from the JSON** rather than a zero or a sentinel (`docs/OUTPUT.md §1.1`). "Absent =
unknown" is the contract, and it's why a withdrawn broadcast GGTO clears the field instead of
leaving a stale offset.

### `SetLeapSeconds`

`[state].leap_seconds` overrides the compiled-in ΔtLS (GPS−UTC) used for every wall-clock →
GNSS-time-of-week conversion. The design mandate is "transcribe, don't invent": BeiDou
B-CNAV2 UTC parameters are decoded and cross-checked against this value (surfacing as
`leap_mismatch`), but the daemon deliberately does **not** derive a fleet-wide consensus or
auto-replace the process-wide value. Set it explicitly after a leap event — otherwise every
conversion shifts by 1 s, which is about 3.9 km of satellite motion.

### Expiry, and keeping it in sync with detect

`Expire` drops SVs unseen for `[state].sv_ttl`; `ExpireStations` drops stale SBAS, RF, and
GLONASS-almanac entries.

**regression fix is a standing coupling to respect:** the expiry window must stay comfortably longer
than the detector's silence thresholds. If state expires an SV before `detect` can observe it
going silent, the silence event never fires — a constellation would simply vanish from the
dashboard while reporting "live." Today's margin is roughly 10×, and both sides carry the
cross-reference comment.

---

## Tests

The largest test surface in the daemon, roughly grouped:

- **Per-constellation folding** — one file per signal family, exercising the fold, IOD handling,
  health, and the assembler pairing rules from the collector's side.
- **Disco computation** — midpoint evaluation, the freshness and prior-state guards, GLONASS's
  day-relative variant, and the no-fake-disco-after-restart property.
- **Health semantics** — `HaveHealth`, the CNAV L1-bit drop, GLONASS's joint Bn/ℓn/Cn rule, the
  SBAS MT0-interleave regression.
- **Capability** — decode-then-record ordering, and `CapabilityDiff` in both directions.
- **RF and iono** — baseline learning, the C/N₀-vs-elevation fit, geometry-free measurement.
- **Lifecycle** — expiry windows, station expiry, and the regression fix threshold coupling.

```sh
go test ./internal/state/
go test -race ./internal/state/     # the C1–C11 discipline
```

---

## See also

- `../../../gnss/README.md` — the math library this package drives.
- `../detect/README.md` — what consumes these metrics.
- `../serve/README.md` — what publishes these feed models.
- `../../../docs/INTEGRITY.md` — the integrity design; `../../../docs/DEFENSE-PNT.md` — Tier-0.
- `../../../docs/OUTPUT.md §1` — the served field contract.
