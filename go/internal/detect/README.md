# `internal/detect` — the integrity DETECT stage

**Headline:** a debounced state machine that turns the per-SV and per-station metrics computed in
`internal/state` into **confirmed, typed integrity events**. It is deliberately pure — `Tick`
takes a snapshot and returns events — so the daemon owns persistence, SSE broadcast, and ids, and
the detector can be tested by handing it a map.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `detect.go` | `Detector`, `Event`, and the per-SV / SBAS classifiers. |
| `thresholds.go` | Every operating point, collected in one place. |
| `rf.go` | The four station-scoped PNT-defense classifiers. |
| `capability.go` | The capability-plausibility classifiers. |
| `*_test.go` | Threshold behavior, debounce, flap suppression, and per-classifier transitions. |
| `README.md` | This file. |

Imports `internal/state` — and only in that direction. `state` never imports `detect`.

---

## Summary

```go
func New(debounce time.Duration) *Detector
func (d *Detector) Tick(now, svs map[string]state.FeedSV, sbas map[string]state.SBASEntry, liveReceivers int) []Event
func (d *Detector) TickStations(now, stations map[string]state.StationRF) []Event
func (d *Detector) TickStationLiveness(now, lastSeen map[string]int) []Event
func (d *Detector) TickCapabilities(now, reports map[string]state.StationCapReport) []Event
func (d *Detector) Reset()

type Event struct {
    Time     time.Time
    SV       string
    Type     string
    OldValue string
    NewValue string
    Severity int              // SevInfo | SevWarning | SevCritical
    Message  string
    Params   map[string]any
}
```

**The split with `state` is the design.** `state` computes numbers; `detect` decides when a number
has *changed enough, for long enough, to be worth telling someone about.* That means every
classifier here is a state machine over transitions, not a threshold test over an instant.

Every classifier shares one debounce discipline (`DebounceDuration`, 60 s by default), so a
capability event and an orbit-disco event carry the same confirmation semantics and the same
event contract (`docs/OUTPUT.md §3`).

---

## The event types

**Per-SV** (`Tick`): `orbit_disco`, `clock_jump`, `eph_aged`, `sisa_change`, `ura_alert`,
`wn_mismatch`, `leap_mismatch`, `bds_integrity_flag`, `osnma_change`, `observation_lost`,
`position_unknown`, `xsig_divergence`.

**SBAS** (`Tick`): `sbas_lost`, `sbas_health`.

**Station RF** (`TickStations`): the four station-scoped classifiers — `jamming_detected`,
`spoofing_suspected` (always `SevCritical`), `antenna_fault`, and `station_rf_degraded`. Station
liveness comes separately from `TickStationLiveness` → `station_offline`.

**Capability** (`TickCapabilities`): `capability_impossible`, `capability_signal_lost`.

---

## Details

### The thresholds, and the reasoning behind the awkward ones

All in `thresholds.go`, deliberately together so the whole detector is tuned in one place. The
values are the standard defined in `docs/INTEGRITY.md §2` (cross-checked against intsat's shipped
detector).

| Threshold | Value | Notes |
|---|---|---|
| `EphAgeThresholdGalileo` | 105 min | Galileo refreshes fastest, so it's stricter. |
| `EphAgeThresholdGPS` | 140 min | The default for every non-Galileo constellation; QZSS and NavIC inherit it. |
| `OrbitDiscoThreshold` / `Severe` | 1.45 m / 10 m | Warn band, then severe. |
| `TimeDiscoThreshold` / `Severe` | 2.5 ns / 10 ns | The number that makes `physconst`'s per-constellation μ matter. |
| `SISAAlertThreshold` / `SISAExitThreshold` | 3.0 m / 2.5 m | **Asymmetric on purpose** — see below. |
| `SilentThreshold` | 3600 s | An SV unseen this long is lost. |
| `ObserverOfflineThreshold` | 300 s | Shorter, because it's operator-actionable. |
| `SilenceMinReceivers` | 4 | A fleet-footprint floor — see below. |
| `FreshReceiverThreshold` | 60 s | How recently a receiver must have seen an SV for its vote to count. |
| `DebounceDuration` | 60 s | The shared confirmation window. |

**The asymmetric SISA band ** is the most instructive one. URA and SISA are *quantized* —
GPS URA steps 2.83 m and 4.0 m straddle 3.0 m — so a real, perfectly healthy SV dwells on both
sides of a plain threshold for minutes at a time. That's **longer than the debounce window**,
which means debouncing alone cannot suppress the repeated ok/degraded pairs. The exit band is the
actual flap filter for this metric. `SISAAlertThreshold` itself must not move.

**`SilenceMinReceivers = 4` ** is a floor below which the per-SV silence classifier
doesn't run at all, and the reasoning is worth reading in full because it's a genuine limitation
rather than a tuning choice. `observation_lost`'s premise is "the constellation is always in
view" — true of the constellation as seen by a *globally distributed fleet*, and emphatically not
true of any single station's sky. From one station, every MEO and IGSO satellite legitimately
sets below the horizon once per orbital pass; with `sv_ttl` (2 h) longer than `SilentThreshold`
(1 h), each pass produced a false lost/recovery warning pair — dozens per day of pure orbital
mechanics, which is exactly how you train an operator to ignore the events channel.

The real fix is per-station geometry (propagated elevation above a mask from at least one live
station), which is the observer-geometry pass. Until then, live-receiver count is the only
footprint signal available. **4 is a conservative *necessary-not-sufficient* proxy**: fewer than
4 stations cannot plausibly hold a MEO constellation in continuous view however they're placed,
while 4+ only *may* — they could all share one footprint. Operators with a concentrated 4+ fleet
should expect residual rise/set noise until the geometry gate lands.

Below the floor, constellation-outage coverage still exists, via the visibility-independent
detectors: `eph_aged`, `position_unknown`, and `capability_signal_lost` (a demonstrated signal
gone dark station-wide).

**`FreshReceiverThreshold` tunes nothing on its own.** `state.freshReceiverWindow` in
`../state/feed.go` is the constant that actually *computes* the served confirmation count; it's
duplicated there because `detect` depends on `state` and not the reverse. **Move both together**, or the
documented corroboration window stops describing the served one.

### The station RF classifiers

`detectStationRF` runs four station-scoped PNT-defense classifiers over one observer's RF read
model (`docs/DEFENSE-PNT.md §4`). The metrics — AGC departures from the learned baseline, the
C/N₀-vs-elevation residual — are computed in `internal/state`; this only classifies and
debounces.

**The design rule is explicit and non-negotiable: no single gate fires a spoofing alarm.** A ≥2-gate
fusion is required, and a lone metric departure is reported as a *degradation*, not an attack.
Thresholds are conservative and observational in v1.

That rule exists because the cost asymmetry is brutal. A missed jamming event is a gap in a log;
a false "you are being spoofed" is an operator scrambling a response for nothing, twice, before
they stop believing the system.

### Capability classifiers

`TickCapabilities` runs the capability-plausibility checks over the same debounced state machines
as everything else, so a capability event shares the confirmation discipline and the event
contract:

- **`capability_impossible`** — a station delivered a signal its declared silicon cannot produce.
- **`capability_signal_lost`** — a signal the station has *demonstrated* it can produce has gone
  dark.

The second one is more useful than it sounds: it's visibility-independent, so it keeps working
below `SilenceMinReceivers` where the per-SV silence classifier is suppressed.

### Purity, and why it matters

`Tick` takes a snapshot and returns events. It does not write, publish, log, or allocate ids.
That means:

- **Testing is trivial** — build a `map[string]state.FeedSV`, tick, assert on the returned slice.
- **The daemon controls ordering** — persist first (which assigns the monotonic id), then publish
  to SSE, with retry policy owned by `main`.
- **`Reset()` exists** for tests and for a clean restart of the state machines without rebuilding
  the detector.

---

## Tests

`detect_test.go`, `rf_test.go`, and `capability_test.go` cover, per classifier: the threshold
crossing in both directions, the debounce window (a transition that reverts inside the window
emits nothing), the flap suppression that debounce alone can't provide (the SISA band), the
`SilenceMinReceivers` floor, and the severity assignment across warn and severe bands.

```sh
go test ./internal/detect/
```

---

## See also

- `../state/README.md` — where every metric is computed.
- `../../cmd/navlistener/README.md` — the event pipeline that persists and publishes these.
- `../../../docs/INTEGRITY.md §1–§2` — the detector design and the threshold standard.
- `../../../docs/DEFENSE-PNT.md §4` — the RF classifiers and the fusion rule.
- `../../../docs/OUTPUT.md §3` — the event contract.
