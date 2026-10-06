# navlistener — PNT defense: jamming & spoofing detection

**Status: RF monitoring, jamming detection, stationary received-power history and
station integrity checks implemented.** MON-RF/MON-HW, reception telemetry, the
receiver's own solution (telemetry `0x03`) and the board's pulse timing feed the
station detectors. Four independent physics domains are wired against the quorum of
two, so `spoofing_suspected` can be confirmed (§3) at stations whose receivers report
more than one of them. Doppler, cross-constellation and measured-ionosphere gates,
SEC-SIG transport and the hardware tiers remain planned.

This document describes how `navlistener` uses receiver telemetry for RF monitoring
and the planned **jamming and spoofing defense layer** for the fleet. This is
the RF-front-end half of the integrity mission: `docs/INTEGRITY.md` monitors the *broadcast
navigation message* (orbit/clock discontinuities, health, cross-receiver broadcast agreement);
this document monitors the *signal environment* (interference, counterfeit signals) that
reaches the receiver before any nav frame is decoded. Read `docs/INTEGRITY.md` first — this doc
extends its detector, thresholds, and event contract; it does not replace them.

> **The design rule, restated for RF.** "Verify physics, not signatures." Jamming and spoofing
> defense is a *plausibility* discipline: does the RF environment agree with physics and with
> what other receivers report? A cryptographic signature (OSNMA, our ATECC feeder auth) proves
> a frame's *provenance*; it says nothing about whether the *signal that carried it* was honest.
> A receiver fed a spoofing transmitter emits perfectly-authentic, perfectly-wrong PVT. So the
> defense is layered: hardware auth proves who sent the telemetry, and these physics gates prove
> the environment was real. Both are required; neither substitutes for the other.

**Division of labour.** This document specifies *what interference/spoofing signals we ingest,
how the collector derives detection metrics from them, and how they join the integrity event
contract.* It does **not** redefine the broadcast-integrity metrics (orbit-disco, time-disco,
delta-Hz, OSNMA) — those are `docs/INTEGRITY.md`. The raw telemetry plumbing is
`docs/CONSTELLATIONS.md §6.2`; the emitted fields and events are `docs/OUTPUT.md`; the hardware
observer is `docs/DESIGN.md §3`.

**Edge and collector roles.** Feeders forward raw AGC/CW indicators, receiver flags
and observables for the fleet detectors. A stationary ESP observer additionally owns
the bounded availability and received-power alarms in
[RECEPTION.md](RECEPTION.md): it compares GPS C/N₀ with its persisted sidereal
history and the collector's delivered history. The collector recomputes the delivered
comparison. This local warning does not emit the collector's
`spoofing_suspected` event or reduce its independent-gate quorum. A receiver's own
jamming/spoofing flag remains one input among several; commercial detectors can be
blind when a receiver boots inside an already degraded environment (§5).

---

## 1. What the receivers already tell us (the ingest surface)

The table maps receiver telemetry to current and proposed detector inputs. Current
GNF1 telemetry carries the supported UBX records described in
`docs/CONSTELLATIONS.md §6.2`. SEC-SIG/SEC-SIGLOG transport and Septentrio central
decoding remain planned; listing a receiver message here does not imply a wired detector.

| Source message | Carries | GNF1 type | Threat signal it feeds |
|---|---|---|---|
| **UBX-MON-RF** (F9+) | per-RF-band AGC, noise level, CW-suppression %, jamming state, antenna status | `JammingStats` (0x05) | broadband + narrowband jamming (§2) |
| **UBX-MON-HW** (legacy) | AGC monitor, noise, `jammingState`, CW indicator | `JammingStats` (0x05) | jamming, older receivers |
| **UBX-SEC-SIG / SEC-SIGLOG** | per-signal spoofing-detection state, jamming-detection state (multi-tier) | `JammingStats` (0x05) | receiver's own spoofing verdict (an input, §3) |
| **UBX-NAV-SAT** | per-SV C/N₀, elevation, azimuth, pseudorange residual, quality indicator, health, used-in-solution | `ReceptionData` (0x01) | C/N₀-vs-elevation plausibility (§3), simultaneous C/N₀ drop (§2) |
| **UBX-RXM-RAWX** | pseudorange, carrier phase, Doppler, lock-time, C/N₀ | `RFData` (0x02) | Doppler plausibility, measured iono (§3, `docs/MATH.md §7.4`) |
| **UBX-NAV-PVT / NAV-CLOCK / NAV-STATUS** | position, velocity, UTC, clock bias/drift, fix and spoofing state | `ReceiverSolution` (0x03) | position, clock and time-reference integrity checks |

**Software task P-Jam-1 (done for MON-RF/MON-HW/NAV-SAT):** the `JammingStats` (0x05) record
carries the full MON-RF per-band block (per RF path: AGC, noiseLevel, cwSuppression/jamInd,
jammingState, antStatus) and `ReceptionData` (0x01) carries the NAV-SAT satellite list (body
version 2, `common/reception_data.h`, shared by the ESP32 and the feeder), both as
fixed-layout GNF1 telemetry records that ride the existing DATA stream (no protobuf on the wire
— `docs/DESIGN.md §2`; body layouts in `go/internal/ingest/telemetry.go`). The collector sees the raw receiver numbers rather than a pre-digested
flag, and the PNT-defense detector runs identically over dial- and push-sourced stations.
**Remaining:** fold the UBX-SEC-SIG / SEC-SIGLOG per-signal spoofing/jamming state into the
`JammingStats` body (raises the spoof-gate fusion count, §3) once that source is wired.

---

## 2. Jamming detection (the RF front-end)

GNSS signals arrive far below the thermal noise floor (~−130 to −160 dBm), so they are trivially
drowned out; the receiver's own front-end is the best jamming sensor we have. Two regimes:

- **Broadband (noise) jamming.** The AGC steps its gain *down* to keep the ADC from saturating
  when wideband energy floods the band. The detection metric is **AGC departure from the
  station's own clear-sky baseline**, not an absolute threshold — every antenna/cable/site has a
  different nominal AGC. The collector learns a per-station, per-band AGC baseline and flags a
  sustained downward departure, correlated with a fleet-wide C/N₀ drop at that station. The
  baseline is the median of per-minute AGC medians over six hours. A sample more than 400 counts
  from it is measured but not learned. No baseline is served until ten quiet minutes exist, so a
  station is never measured against its first sample. Once established it moves at most 150
  counts an hour, so a jammer ramping more slowly than the learn band still shows as a departure
  instead of dragging the baseline along. A band that stays more than 400 counts *above* its
  baseline (quieter) for fifteen minutes learned that baseline under interference, for example
  after starting jammed, and starts learning again.
- **Narrowband (CW) jamming.** A discrete high-amplitude tone (e.g. near 1575.42 MHz) drives the
  receiver's notch filters; the **CW-suppression indicator** spikes toward 100%. The metric is
  the CW-suppression level crossing a band, debounced.

**Derived metrics (per station × RF band), computed centrally:**

```
agc_departure   = baseline_agc − current_agc          // dB-equivalent counts below baseline
cw_suppression  = broadcast MON-RF cwSuppression       // 0–100 %
noise_floor_dB  = broadcast MON-RF noise level
jam_state       = worst-of(receiver jamInd, our agc/cw classification)
```

(These are internal derived-quantity names. The **served** feed keys are `cw_suppress` and
the raw-indicator `noise_level` — §6, regression fix; no dB unit is claimed on the wire.)

A jamming event is **corroborated**, not taken from one box: a real jammer near a station shows
as `agc_departure` + `cw_suppression` + a C/N₀ collapse across *all* SVs that station tracks,
simultaneously. A single metric moving alone is more likely a receiver/antenna fault than an
attack — surface it as a station-health signal, not a jamming alarm.

The simultaneous drop is the integrity `cn0_drop` check, after the CISA Epsilon C/N₀ drop
monitor: every signal the receiver used three to five seconds earlier and still tracks has
fallen by at least 1 dB, the median by at least 3 dB, across at least six signals. A drop
marks the jammer's onset; C/N₀ then stays low without dropping again. A drop served at any
point during a band's current AGC departure therefore keeps corroborating that departure
until it ends, and a drop that had passed before a departure began corroborates nothing. A
slow ramp below the per-window step does not show as a drop; the bounded AGC baseline covers
it.

A jammer also reaches every station near it. A station's departure is corroborated when
another station within 30 km, located by its surveyed position or a fix from the last ten
minutes, shows interference evidence of its own within the last two minutes: a departure, a
CW tone, its receiver's jam flag or a simultaneous drop. Only a station's own evidence counts,
never a neighbour's corroboration, so two stations cannot confirm each other in a loop, and a
neighbour alone says nothing about a quiet station. The jamming event names the corroborating
neighbours. Each audience correlates only the stations it sees.

**Use of jamming context:** the `svs` feed's `conf_weighted` counts each fresh decoded
source by its vote weight: its `rf_trust`, lowered to ½ while its station assessment
is inconsistent and to 0 while it is unassured or indicates spoofing, since a spoofed
receiver's decoded navigation data is the attacker's. `conf` still counts every fresh
source. The planned broadcast-agreement comparison will use the same weights.

---

## 3. Spoofing detection (physics gates over baseband + nav)

Spoofing broadcasts counterfeit PRN codes to capture the receiver's tracking loops.
The target detector combines independent physics checks with receiver evidence.
The gates below include planned inputs; the implementation note after the fusion
rule identifies what is wired today, consistent with `docs/INTEGRITY.md §8`.

- **C/N₀-vs-elevation inconsistency.** Genuine C/N₀ varies with elevation and atmosphere: low SVs
  are weaker, high SVs stronger, with SV-to-SV spread. A constellation where *every* SV reports an
  identical, unnaturally high, elevation-independent C/N₀ is the classic single-transmitter
  spoofer signature. Metric: the **variance of C/N₀ residual after removing the elevation trend**
  collapsing toward zero across a constellation. (Uses `ReceptionData`.)
- **Repeating-ground-track C/N₀ departure.** A fixed GPS installation compares each
  satellite at the same sidereal phase with robust local and server histories. Both
  references must flag the same entries before the edge alarm advances. This catches
  an abrupt distinctly different level even when the constellation still has normal
  satellite counts. Both histories use the same receiver stream, so this is part of
  the C/N₀ plausibility family rather than another independent fusion vote. The full
  model, persistence and limitations are in [RECEPTION.md](RECEPTION.md#received-power-histories).
- **Doppler-vs-ephemeris.** Real SVs move at orbital velocity; a static ground spoofer cannot
  reproduce every SV's true range-rate. This is exactly the **delta-Hz** signal already defined in
  `docs/MATH.md §2.2` / `docs/INTEGRITY.md §3`: observed Doppler minus ephemeris-predicted Doppler.
  A *coherent* delta-Hz bias across many SVs at one receiver — all pulled toward a common origin —
  is a wide-area spoofer or a receiver clock error; the sign pattern distinguishes them.
- **Position consistency (implemented).** A fixed antenna does not move and a mobile one moves no
  faster than its platform. The receiver's own solution (telemetry `0x03`) is checked against the
  surveyed antenna position with accuracy-scaled bands and a ten-minute mean offset, its reported
  speed against zero, a mobile station's displacement against its maximum speed, and the velocity
  implied by position differences against the reported velocity. A spoofer that drags the solution
  breaks one of these unless it reproduces the installation's physics.
- **Receiver clock transients (implemented).** A spoofer capturing the tracking loops forces a
  non-physical change in the receiver's clock solution. The clock bias is checked against the
  integrated clock drift over 30–40 s, with whole-millisecond receiver adjustments removed, and
  the drift's rate of change over 60–120 s against what an oscillator can do.
- **Time reference (implemented).** The receiver's UTC is compared with an independent wall-clock
  stamp of the same record (the observer's NTP clock, or the collector's on a dial connection),
  catching whole-second steps; on ESP32 observers the GNSS PPS is compared with the board's
  free-running RTC pulse, catching sub-second steps the RTC did not take. A slow drag below the
  RTC model's uncertainty is not detected.
- **Cross-constellation contradiction.** A spoofer that targets only GPS L1 leaves the Galileo /
  GLONASS / BeiDou solutions disagreeing with the GPS one. Metric: divergence of the broadcast
  inter-system time offsets and per-constellation PVT beyond their normal agreement
  (`docs/MATH.md §8` offsets; a receiver whose GPS-only solution contradicts its multi-GNSS
  solution is suspect).
- **Measured-vs-model ionosphere.** A counterfeit constellation rarely reproduces a physically
  consistent dual-frequency ionospheric delay. The measured slant iono (`docs/MATH.md §7.4`) that
  diverges from the broadcast model *incoherently with the real space-weather picture the rest of
  the fleet sees* is a spoofing tell — one more independent physics gate the network gets for free.
- **Receiver spoofing verdict (NAV-STATUS implemented; SEC-SIG planned).** The receiver's own
  spoofing flag is ingested and **weighted, not trusted**: it *raises the prior*, and when it agrees
  with one independent physics gate above the event is confirmed; alone it is recorded but not
  alarmed (§5 — a black-box flag can miss a cold-boot spoof and can false-positive on multipath).
  NAV-STATUS `spoofDetState` arrives at epoch rate in telemetry `0x03` and at low rate in board
  reports; per-signal SEC-SIG state remains planned.

**Fusion rule.** No single gate fires a spoofing alarm. Gates are grouped by **evidence domain** —
the measurements they share — and a domain counts once however many of its checks agree, so two
views of one measurement cannot make a quorum. A confirmed `spoofing_suspected` event requires
**≥2 independent physics domains** unassured at one station (signal power, position, receiver
clock, time reference), or one together with the receiver's own spoofing flag, held for the
debounce window. A *neighbouring* station seeing the same anomaly on the same SVs would be
stronger corroboration still, since a genuine broadcast is identical for every receiver in view;
that input is planned. This mirrors the broadcast-agreement logic in `docs/INTEGRITY.md §6`.
Interference evidence (AGC, CW) does not count toward the quorum: jamming is not spoofing.

> **As-built status.** Four physics domains are wired (`WiredSpoofGates = 4`): signal power
> (C/N₀ vs elevation), position, receiver clock and time reference, evaluated by the station
> integrity checks in `go/internal/integrity` ([proposal](proposals/STATION-ASSURANCE.md)).
> Which domains a station contributes depends on what its receiver reports: a station that sends
> only NAV-SAT and MON-RF has the signal-power domain alone and cannot reach the quorum. Each
> station's integrity assessment shows its available checks. Doppler-vs-ephemeris needs RAWX on
> the push path, cross-constellation contradiction needs the coherence detector, measured-iono
> needs the residual evaluator, and SEC-SIG ingest is not yet transported. The
> `navlistener_spoof_gates_wired` / `navlistener_spoof_gate_quorum` gauges publish the wiring.
> The repeating-ground-track comparison is implemented as an ESP reception warning and private
> collector check; it shares the signal-power evidence family and adds no domain.

---

## 4. The detector, thresholds, and events (extending INTEGRITY)

These metrics flow through the **same debounced detector state machine** as the broadcast-integrity
metrics (`docs/INTEGRITY.md §4`) — per (station, metric) provisional→confirmed with a debounce
window — and emit through the **same event contract** (`docs/OUTPUT.md §3`). New event types,
added to the vocabulary in `docs/INTEGRITY.md §5`:

| `event_type` | Fires when | Severity |
|---|---|---|
| `jamming_detected` | AGC departure corroborated by a CW tone, the receiver's jam flag, a simultaneous C/N₀ drop or a neighbouring station's interference, debounced; a near-total AGC collapse alone | 1 → 2 on full lock loss; 0 on clear |
| `spoofing_suspected` | ≥2 independent physics domains (§3), or one with the receiver's spoofing flag, unassured at a station, debounced | 2; 0 on clear |
| `station_assurance` | the station's fused integrity state changes among `assured`, `inconsistent` and `unassured` ([proposal](proposals/STATION-ASSURANCE.md) §4) | 2 unassured, 1 inconsistent, 0 assured |
| `station_rf_degraded` | a single RF metric departs baseline, or a simultaneous C/N₀ drop with a quiet front end (fault-or-early-warning, not an attack claim) | 1; 0 on clear |
| `antenna_fault` | MON-RF antenna status open/short, or C/N₀ collapse with no jamming signature | 1; 0 on clear |

**Current thresholds are implemented in `go/internal/integrity`** (`DefaultProfile`, with the
station RF classifier names in `go/internal/detect/thresholds.go`). The RF state learns a
quiet-time AGC baseline, and the detector classifies gross departures using conservative
operating points. These remain subject to calibration
with station data; changes must keep the implementation and `docs/INTEGRITY.md`
aligned. Severity encoding is the shared `0 info · 1 warning · 2 critical`.

Constellation/station scope: these are **station-scoped** events (the threat is at a receiver's
antenna), unlike the SV-scoped broadcast events. `jamming_detected`, `station_rf_degraded` and
`antenna_fault` carry the station id and the aggregate RF indicators. `spoofing_suspected` and
`station_assurance` carry the station's whole assessment: fused state and score, unassured
domains, every available check's state, measured values, thresholds, version and reasons, the
names of unavailable checks, the engine version and the configuration hash. Station RF machines
confirm a recovery only after five minutes; the integrity checks hold their own recoveries, so
their events clear after the ordinary debounce.

---

## 5. Why we recompute instead of trusting the receiver's flag

The receiver's built-in jamming/spoofing detector is valuable but has two structural blind spots we
must design around:

1. **Cold-boot blindness.** Commercial detectors are optimized to catch the *transition* into a
   degraded environment. A receiver powered on *inside* an already-spoofed environment has no
   genuine baseline to compare against and may report "clean." Our defense against this is
   **cross-receiver corroboration** and **fleet baselines**: a station that boots into a spoofed
   RF field disagrees with its neighbours and with its own historical quiet baseline, which we hold
   centrally and it cannot. The station detector itself does not seed a degraded first
   observation as the normal state; it raises the event from `unknown` after the onset window
   (`docs/INTEGRITY.md §4`).
2. **Black-box opacity.** We cannot audit the vendor's thresholds, and they change across firmware.
   By deriving our own metrics from the raw MON-RF/RAWX numbers we get an explainable, versioned,
   fleet-consistent detector — the same reason we decode raw nav frames centrally instead of
   trusting the receiver's computed PVT (`docs/DESIGN.md §0`).

The receiver flag is therefore **one weighted input** into the fusion rule (§3), never the verdict.

---

## 6. Persistence & output

- **RF telemetry is stored like any observation** (`docs/OUTPUT.md §4`): `JammingStats` and
  `ReceptionData` land in the private `rf_samples` hypertable with exact raw bodies,
  decoded projections, receipt scope, session and sequence. The write shares the
  navigation replay-ledger transaction, so a detector improvement can be replayed
  over history without acknowledging evidence that failed storage.
- **Station RF health surfaces in the `observers` feed** (`docs/OUTPUT.md §1.3`): per-station,
  per-band `agc_departure`, `cw_suppress`, `noise_level`, `jam_state`, and a `rf_trust`
  scalar (how much this station's votes are currently down-weighted); `noise_level` is the
  receiver's raw MON-RF indicator, not a dB measurement. Confirmed events go to the
  SSE stream and `gnss_events` like every other integrity event.
- **Station events keep their evidence.** When a station event confirms, the collector copies
  that station's stored inputs from ten minutes before to one minute after into retention-less
  evidence tables, so the inputs outlive raw retention like the event itself. Private audiences
  read them with the event (`docs/OUTPUT.md §3`).
- **Station integrity assessments surface in the private `observers` feed** as `integrity`
  (`docs/OUTPUT.md §1.3`), with the evidence behind each check. Receiver solutions are stored in
  `rf_samples` with kind `solution`. Public views carry neither, and public projections drop
  station RF and solution telemetry entirely.
- **Baselines are state:** with the historian enabled, the server's repeating-track
  C/N₀ model is versioned in `reception_power_models`, while the observer keeps its
  own CRC-protected model in local NVS. The AGC baselines are checkpointed in
  `agc_baselines` every five minutes and at orderly shutdown and restored at startup
  (when under a day old), so a restart does not repeat the warm-up; a restored band is
  installed when it first reports. Changing a station's `power_model_epoch` discards its
  stored AGC baseline too. Scoped audience views learn their own baselines. Threshold
  policy and the explicit hardware/site epoch remain configuration.

---

## 7. Hardware roadmap (deliberately staged; mostly non-goal for v1)

The original brief sketched a full bare-metal RF monitor. That is a **later hardware tier**, not
part of the software daemon's first job, and it is scoped here so the daemon's telemetry contract
is forward-compatible with it. In priority order:

- **Tier 0 — now, zero new hardware:** the F9P/F9T fleet *as RF sensors*. MON-RF + SEC-SIG +
  RXM-RAWX already give us AGC, CW, noise, spoofing flags, and multi-band raw observables. Every
  detector in §2–§3 runs on this today. **This is the entire v1 scope.**
- **Tier 1 — the ATECC hardware observer (`docs/DESIGN.md §3`):** the ESP32-S3 + ATECC608 + DS3231
  board already in the design forwards the same UBX telemetry with hardware-signed provenance and a
  disciplined local clock — which directly strengthens the §3 time-jump gate (a trusted local time
  reference makes a spoofed time-step obvious). No new RF silicon; the F9x remains the front-end.
- **Tier 2 — dedicated RF front-end (research, not committed):** a MAX2769C/NT1065-class front-end
  streaming raw 2-bit I/Q to an MCU/FPGA for FFT-based jamming detection and PRN cross-correlation
  spoofing detection, bypassing the commercial module entirely. **Supply-chain note:** the MAX2771
  is EOL/hard-to-source as of 2026; the NTLab NT1065 (4-channel, L1/L2/L3/L5) is the current
  favorite, and the pragmatic fallback is exactly Tier 0 — the F9x *is* a multi-band front-end via
  RXM-RAWX. We do not commit to bare-metal RF until a detector need is proven that Tier 0 telemetry
  cannot meet.
  - **Prior art to build on:** PocketSDR (open GNSS SDR, originally MAX2771-based), GNSS-SDR (the
    open C++ I/Q-processing framework), early-gen SwiftNav Piksi (GNSS front-end + Spartan-6 FPGA),
    and the NT1065-based Nut4NT.
  - **Architecture sketch (recorded so the brief isn't lost; detailed design lands in `firmware/`
    if/when Tier 2 is committed):** RF stage — SMA on a 50 Ω coplanar waveguide into the front-end
    IC; bias-tee (3.3 V through an RF choke onto the SMA center pin) powering the active antenna;
    a TCXO reference (~16.368 MHz, per the IC's PLL) — the same clock the §3 time gate leans on.
    Digital stage — ESP32-S3: SPI control plane to configure the front-end's PLLs/AGC; data plane
    via the I2S / LCD-I80 peripheral as a DMA slave, sweeping the 2-bit I/Q bits into a PSRAM ring
    buffer with zero CPU overhead; a FreeRTOS task runs esp-dsp FFTs (jamming: a discrete bin
    spike = CW, a broadband floor rise = noise jamming) and PRN cross-correlation (spoofing:
    multiple PRNs arriving at identical, implausible power). Native ESP-IDF, not Arduino — the
    DMA/timing budget requires it.
  - **Rejected path — RTL-SDR-class silicon on the embedded tier:** the RTL2832U has no public
    datasheet and only emits I/Q as a USB 2.0 bulk stream; forcing an MCU to ingest ~2 MSps over
    USB while running DSP is a non-starter. An RTL-SDR + host-PC rig remains fine as a *lab
    prototyping* harness for the Tier-2 DSP (FFT + PRN correlation against live air), but it is
    not a fleet tier.

The software contract (`JammingStats` carrying raw per-band numbers, station-scoped RF events,
replayable telemetry storage) is designed so Tiers 1–2 slot in as richer *sources* of the same
metrics, never a rewrite of the detector.

---

## 8. Non-goals

- **We do not navigate or hold a PVT solution.** These gates monitor the environment; they do not
  compute a protected position. That is the receiver's job and `tudorgps`'s lab.
- **We do not demodulate raw RF in v1.** Tier 0 starts at the receiver's telemetry, consistent with
  the whole system's "start at the demodulated frame" boundary (`docs/DESIGN.md §7`).
- **We do not ship the receiver's flag as our verdict.** A vendor jamming/spoofing bit is an input,
  never the emitted event (§5).
- **We do not gold-plate the hardware.** Tier 2 bare-metal is research until Tier 0 is proven
  insufficient for a real detection need.
