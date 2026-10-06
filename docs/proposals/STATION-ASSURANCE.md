# Proposal: station integrity assurance

Status: **accepted; implementation in progress**. Section 7 tracks each work item.

Prepared: 2026-10-05. Implementation baseline examined: `f90ee1a`.

This proposal extends the station-scoped PNT defense in
[DEFENSE-PNT.md](../DEFENSE-PNT.md) from receiver RF telemetry to the receiver's
own navigation solution, and makes every station judgment explainable,
versioned and replayable. It keeps the established rules: the collector decides,
no single check alleges spoofing, and a receiver's own spoofing or jamming flag
is an input rather than a verdict.

## 1. Baseline at `f90ee1a`

What already exists and is reused:

- A debounced per-(subject, metric) detector in `go/internal/detect`, with one
  symmetric 60 s dwell. A machine's first observation seeds its state silently.
- Four station RF events from MON-RF/MON-HW and NAV-SAT: `jamming_detected`,
  `station_rf_degraded`, `antenna_fault`, and `spoofing_suspected`. The last needs
  two independent gates and only the C/N₀-vs-elevation gate is wired, so it
  cannot fire.
- The private `rf_samples` hypertable stores NAV-SAT and MON-RF bodies with their
  receipt provenance, sharing the replay-ledger transaction.
- The stationary GPS received-power history: a sidereal C/N₀ reference on the
  observer and in the collector, checkpointed in `reception_power_models` and
  keyed by a site identity that includes `power_model_epoch`
  ([RECEPTION.md](../RECEPTION.md#received-power-histories)).
- Session-level station identity and hardware trust stamped on every stored row,
  with authority-scoped trust anchors ([COMMISSIONING.md](../COMMISSIONING.md)).
- The ObserverDetails timing tag: 1 Hz GNSS PPS and RTC pulse capture, including
  the RTC-minus-GNSS phase ([OBSERVER-TELEMETRY.md](../OBSERVER-TELEMETRY.md)).

What is missing:

- NAV-PVT is enabled on the ESP32 boards, but the firmware parses only the time,
  fix flags and latitude/longitude, and none of it leaves the observer. NAV-CLOCK
  is not enabled. The collector never sees position, velocity, accuracy, or
  receiver clock state, so it cannot test any of them.
- NAV-STATUS `spoofDetState` and the PPS/RTC phase reach the collector but no
  detector reads them.
- A station that starts inside a degraded RF environment never raises an event,
  because the first observation seeds silently.
- The AGC baseline is seeded by its first sample and spans about one minute. A
  station that starts jammed keeps a low baseline, and a slowly rising jammer
  moves the baseline with it.
- The four station RF events report a clear at their raise severity, while the
  comparable integrity alarms (`wn_mismatch`, `ura_alert`, `xsig_divergence`)
  clear at info.
- Events carry a few aggregate numbers. They name no algorithm version,
  threshold set or contributing check, link to no stored input, and outlive the
  seven-day raw retention of the samples that caused them.

## 2. Receiver-solution telemetry (GNF1 type `0x03`)

The reserved `0x03 ObserverPosition` telemetry type becomes a per-epoch receiver
solution record. It is receiver-neutral: a u-blox connector fills it from
NAV-PVT, NAV-CLOCK and NAV-STATUS, and another receiver family can fill the same
fields. One record is sent per navigation epoch. The sender assembles the blocks
that share an epoch time and flushes the record when the epoch time changes or on
NAV-EOE, drops a repeated block of an epoch it already sent (a polled message), and
stamps the record with the arrival time of the epoch's first block. The shared C
implementation is `common/receiver_solution.h`; `testdata/receiver_solution_v1.txt`
is the golden case the Go codec, the ESP32 parser and the C feeder must reproduce.

Body version 1, big-endian like the other telemetry bodies:

| Offset | Bytes | Field |
|---:|---:|---|
| 0 | 1 | Body version = 1 |
| 1 | 1 | Present blocks: solution = 1, clock = 2, status = 4; other bits zero |

Then, in this order, each present block.

Solution block, 64 bytes (from NAV-PVT):

| Offset | Type | Field |
|---:|---|---|
| 0 | U32 | GPS time of week of the epoch, ms |
| 4 | U16 | UTC year |
| 6 | U8 ×5 | UTC month, day, hour, minute, second |
| 11 | U8 | UTC validity: date valid = 1, time valid = 2, fully resolved = 4 |
| 12 | U32 | Time accuracy estimate, ns |
| 16 | I32 | UTC fraction of second, ns (−1e9..1e9) |
| 20 | U8 | Fix type: none = 0, dead reckoning = 1, 2D = 2, 3D = 3, GNSS + dead reckoning = 4, time only = 5 |
| 21 | U8 | Fix flags: fix OK = 1, differential = 2, carrier float = 0x40, carrier fixed = 0x80 |
| 22 | U8 | Satellites used in the solution |
| 23 | U8 | Reserved zero |
| 24 | I32 ×3 | Longitude and latitude in 1e-7 degrees, height above ellipsoid in mm |
| 36 | U32 ×2 | Horizontal and vertical accuracy estimates, mm |
| 44 | I32 ×3 | Velocity north, east, down, mm/s |
| 56 | U32 | Speed accuracy estimate, mm/s |
| 60 | U16 | Position DOP × 100 |
| 62 | U16 | Position flags: coordinates invalid = 1; other bits zero |

Clock block, 20 bytes (from NAV-CLOCK): GPS time of week (U32 ms), clock bias
(I32 ns), clock drift (I32 ns/s), time accuracy (U32 ns), frequency accuracy
(U32 ps/s).

Status block, 16 bytes (from NAV-STATUS): GPS time of week (U32 ms), fix type
(U8), status flags (U8), fix status (U8), receiver spoofing state (U8: unknown
or deactivated = 0, no spoofing indicated = 1, spoofing indicated = 2, multiple
indications = 3), time to first fix (U32 ms), milliseconds since receiver start
(U32).

The collector rejects unknown versions and presence bits, a wrong length,
out-of-range coordinates or UTC fields, and blocks whose time of week exceeds a
week. Stored bodies go to `rf_samples` with kind `solution`. Coordinates are
private: no feed serves them, and evidence bundles that contain them are served
only to private audiences. The station assessment is served to private audiences
only, like board telemetry, since it is evaluated from these records and the
board's pulse timing.

## 3. Checks

Checks live in a new pure package, `go/internal/integrity`. Each check has a
name, a version, the inputs it needs, and an evaluation that returns a state, the
measured values, the thresholds it applied, and reason codes. The algorithms
follow the CISA Epsilon and PNT Integrity designs (section 6), reimplemented in Go
against our record types. Their thresholds are starting points, to be calibrated
from stored station data the same way the GLONASS discontinuity bands were.

| Check | Inputs | Test | Domain |
|---|---|---|---|
| `static_position` | solution, surveyed position | horizontal and vertical error from the surveyed position, against bands scaled by the reported accuracy; also a ten-minute mean offset for slow drift, inconsistent beyond 8 m and unassured beyond 25 m horizontally | position |
| `stationary_velocity` | solution, fixed station | reported speed against bands scaled by the speed accuracy | position |
| `motion_bound` | solution, mobile station | distance from the last assured position must fit within elapsed time × maximum speed plus both accuracy estimates | position |
| `position_velocity` | solution | velocity implied by position differences over a five-second window against the mean reported velocity | position |
| `clock_bias_drift` | clock | change in clock bias against the integrated clock drift over 30–40 s; whole-millisecond receiver clock adjustments are removed and recorded | receiver clock |
| `clock_drift_rate` | clock | rate of change of clock drift over 60–120 s | receiver clock |
| `utc_offset` | solution UTC; the observer's NTP stamp or the collector's clock | receiver UTC against an independent wall-clock stamp of the same record | time reference |
| `pps_rtc_phase` | timing tag; solution fix state | a step in the RTC-minus-GNSS phase against its recent linear trend | time reference |
| `cn0_uniformity` | NAV-SAT | the existing C/N₀-vs-elevation gate, unchanged | signal power |
| `agc` | MON-RF | the existing AGC departure, CW and receiver jam-state classification | RF environment |
| `receiver_spoofing` | status block, or the ObserverDetails receiver context | the receiver's own spoofing state | receiver verdict |

Each raw test passes an M-of-N filter (three of the last four evaluations by
default) before it changes a check's state, so one noisy epoch cannot move it.
A check with too little input reports `unavailable`, never `assured`.

The `pps_rtc_phase` test sees only steps under half a second, because the phase
wraps at ±0.5 s; `utc_offset` covers whole seconds. Neither detects a
slow drag smaller than the RTC model's uncertainty. Whole-millisecond clock
adjustments that the clock check removes are still visible to the phase test,
since the PPS follows GNSS time rather than the receiver's local clock.

## 4. Assurance states and hysteresis

Each check and each station has one of four states:

| State | Meaning |
|---|---|
| `unavailable` | Too little input, or the check has not run |
| `unassured` | The input is likely untrustworthy |
| `inconsistent` | Trust cannot be established reliably |
| `assured` | The input is likely trustworthy |

Degradation takes effect at once. A recovery to a better state must hold for the
check's recovery period (five minutes by default) before it is served. The
station state fuses the held check states with PNT Integrity's mapping
(`unassured` = −1, `inconsistent` = 0, `assured` = +1, weighted mean thresholded at
±0.5). `cn0_uniformity`, `agc` and `receiver_spoofing` may only lower a station's
state: they take part only when they are not assured. A station is `unassured`
only when a physics domain is unassured and a second domain agrees (another
physics domain, the receiver's verdict, or the RF environment); otherwise any
unassured check holds it at `inconsistent`. An assured mean never hides an
unassured check. A check that was merely unavailable recovers at once; one that
was degraded holds the worst candidate seen during its recovery period.

Station RF event machines get separate onset and clear dwell times. Onset keeps
the standard 60 s, and a clear needs five minutes; the integrity events clear after
the standard 60 s, because their checks already hold every recovery. A station's first observation no
longer seeds silently when it is degraded; it goes through the onset dwell, so a
station that starts jammed or spoofed raises its event. The SV event families keep
their symmetric dwell.

## 5. Spoofing fusion by evidence domain

`spoofing_suspected` counts independent evidence domains, not checks. Checks in
one domain share their underlying measurements and count once, as the two C/N₀
fits already do. The physics domains are signal power, position, receiver
clock and time reference. A domain counts when any of its checks is `unassured`.
The event is raised when at least two physics domains agree, or when one does and
the receiver's own spoofing state is 2 or 3. The quorum of two is unchanged; the
receiver flag alone never raises the event.

The event's parameters list the agreeing domains, each check's state, measured
values, thresholds and version, the station profile, and the configuration hash.
A new `station_assurance` event reports confirmed changes of the fused station
state with the same parameters.

## 6. Upstream algorithm provenance

Both upstream projects are reference designs; no code is copied.

| Project | License | Commit examined | Used for |
|---|---|---|---|
| [cisagov/Epsilon](https://github.com/cisagov/Epsilon) | CC0-1.0 | `ecb18488f2adfb7ca38c06ac2863594a907fc8c6` (2021-02-24) | the clock bias/drift divergence and drift-rate monitors, the stationary position and velocity monitors and their M-of-N filters, the simultaneous C/N₀ drop monitor |
| [cisagov/PNT-Integrity](https://github.com/cisagov/PNT-Integrity) | BSD-3-Clause | `74d5770002e061c85fb88d9f0c14c82797f1714c` (release 3.2.1 lineage, 2021-10-01) | the four assurance levels, weighted fusion, positive-weighting restrictions, one-sided recovery hysteresis, and the position-jump, position-velocity and range-position check designs |

Epsilon's stationary position monitor compares against a running average of
accepted positions. A slow drag moves that average with it, so the
`static_position` check instead compares against the configured surveyed position.

## 7. Work items

Status values: planned, in progress, done (with commit), deferred (with reason).

### Phase 0 — groundwork

| # | Item | Status |
|---|---|---|
| 0.1 | Persist NAV-SAT and MON-RF evidence in `rf_samples` | done (`ba25c3f`) |
| 0.2 | Report station RF event clears at info severity | done (`df7c834`) |
| 0.3 | Separate onset and clear dwell for station machines; degraded first observations go through onset | done (`63e04bd`) |
| 0.4 | `go/internal/integrity`: states, check contract, profile with versioned defaults, configuration hash, M-of-N filter, held recovery, fusion | done (`b98465f`) |

### Phase 1 — receiver-solution checks

| # | Item | Status |
|---|---|---|
| 1.1 | Go codec for telemetry `0x03`, push decoding, `rf_samples` kind `solution` | done (`d5857fd`) |
| 1.2 | Collector dial-mode UBX parsing of NAV-PVT, NAV-CLOCK and NAV-STATUS, with epoch assembly | done (`d5857fd`) |
| 1.3 | ESP32: parse NAV-PVT fully, enable NAV-CLOCK and NAV-EOE, encode and send `0x03` | done (`683937e`); awaiting bench acknowledgement of the new keys |
| 1.4 | C feeder: parse and send `0x03`; `--configure-ubx` enables the messages | done (`8f537d9`) |
| 1.5 | Station integrity profile: fixed or mobile, maximum speed, surveyed position shared with `[[reception.station]]` | done (`d5857fd`) |
| 1.6 | Position checks: `static_position`, `stationary_velocity`, `motion_bound`, `position_velocity` | done (`88fceb6`) |
| 1.7 | Clock checks: `clock_bias_drift`, `clock_drift_rate` | done (`88fceb6`) |
| 1.8 | Time-reference checks `utc_offset` and `pps_rtc_phase` | done (`88fceb6`) |
| 1.9 | `receiver_spoofing` input, and the existing C/N₀ and AGC logic as checks | done (`88fceb6`) |
| 1.10 | Per-station assessment in live state, served as `integrity` in the private observers feed | done (`d5857fd`) |
| 1.11 | Domain-based spoofing fusion and the `station_assurance` event, with check parameters, versions and configuration hash | done (`10b5cf8`) |
| 1.12 | Integrity Station app: show the assessment and handle `station_assurance` | planned |

### Phase 2 — evidence, replay, durable baselines

| # | Item | Status |
|---|---|---|
| 2.1 | Evidence bundle written when a station event is confirmed: a bounded window of stored inputs, check results, baselines, versions and configuration, exempt from raw retention | done (`ca1b111`, receipt clocks `8416a4a`); the check results, versions and configuration hash travel in the event's parameters |
| 2.2 | Private single-event API returning the event with its evidence | done (`ca1b111`, `/gnss/api/v2/event-evidence`) |
| 2.3 | Replay command: run stored station inputs or a bundle through the checks with a pinned or current profile, and compare with stored events | done (`62c5ff3`, `go/cmd/stationreplay`, current profile; the timeline names its configuration hash) |
| 2.4 | Durable AGC baseline: longer decimated window, warm-up reported as unavailable, bounded drift rate, checkpoint and restore like the power model | done (`88f9b8a`) |
| 2.5 | Fixtures for normal sky, receiver and clock resets, slow drift, position and time steps, uniform C/N₀, AGC compression, loss and reacquisition | done (synthetic, `go/cmd/stationreplay/scenarios_test.go`); recorded normal-sky fixtures await bench captures |

### Phase 3 — richer observables where the hardware has them

| # | Item | Status |
|---|---|---|
| 3.1 | NAV-SAT azimuth and signal identity in `0x01` body version 2 | planned |
| 3.2 | Simultaneous C/N₀ drop as jamming corroboration | planned |
| 3.3 | MAX board: IMU motion state against GNSS velocity | planned |
| 3.4 | ZED-X20P: SEC-SIG in `0x05`, after bench confirmation of support | planned |
| 3.5 | ZED-X20P: RXM-RAWX as `0x02`, and a Doppler-vs-ephemeris check using the receiver clock drift | planned |
| 3.6 | Receiver capability table from bench verification | planned |

### Phase 4 — network checks

| # | Item | Status |
|---|---|---|
| 4.1 | Regional correlation of simultaneous station RF events; neighbour corroboration as a fusion input | planned |
| 4.2 | Range check between co-located stations with a known baseline | planned |
| 4.3 | `rf_trust` weighting of the served confidence count | planned |

## 8. Not in scope

- Acquisition, correlator and raw IF checks. The u-blox modules do not expose
  these measurements; they remain the research tier in DEFENSE-PNT §7.
- Multi-antenna angle-of-arrival. It needs two synchronized receivers reporting
  carrier phase.
- Station checks on the observer. The observer forwards measurements; versioned,
  replayable judgment stays in the collector. The existing edge reception alarms
  are unchanged.
- A weighted aggregate as the only output. Per-check states and their evidence are
  always served beside the fused state.

## 9. Verification limits

Software tests, synthetic fixtures and replay establish that the checks compute
what this document specifies. They do not establish detection performance against
real jamming or spoofing, which needs recorded or controlled RF test data. Firmware
changes are host-tested; a receiver message is considered supported on a board only
after that board acknowledges its configuration on the bench. At `f90ee1a`, only
the NEO-M9N had done so.
