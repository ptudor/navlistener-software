# navlistener — PNT defense: jamming & spoofing detection

**Status: design (2026-07-08).** How `navlistener` turns the RF-environment telemetry its
receivers already produce into a **jamming and spoofing defense layer** for the fleet. This is
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

**The edge does not decide.** As everywhere in this system, the feeder forwards *raw telemetry*
(AGC/CW indicators, spoofing flags, raw observables) and the collector does all the detection
centrally. A receiver's own jamming/spoofing flag is treated as **one input among several**, not
as ground truth — commercial detectors are a black box tuned to detect the *transition* into a
degraded environment and can be blind to a receiver cold-booted already inside one (§5). We
recompute what we can from physics and corroborate across receivers, exactly as we do for the
broadcast.

---

## 1. What the receivers already tell us (the ingest surface)

Every threat metric below is derived from telemetry the fleet's u-blox F9P/F9T (and Septentrio)
receivers already produce — no new hardware is required to start. These ride the existing GNF1
telemetry types (`docs/CONSTELLATIONS.md §6.2`); the feeder forwards them verbatim.

| Source message | Carries | GNF1 type | Threat signal it feeds |
|---|---|---|---|
| **UBX-MON-RF** (F9+) | per-RF-band AGC, noise level, CW-suppression %, jamming state, antenna status | `JammingStats` (0x05) | broadband + narrowband jamming (§2) |
| **UBX-MON-HW** (legacy) | AGC monitor, noise, `jammingState`, CW indicator | `JammingStats` (0x05) | jamming, older receivers |
| **UBX-SEC-SIG / SEC-SIGLOG** | per-signal spoofing-detection state, jamming-detection state (multi-tier) | `JammingStats` (0x05) | receiver's own spoofing verdict (an input, §3) |
| **UBX-NAV-SAT / NAV-SIG** | per-SV C/N₀, elevation, azimuth, used-in-solution | `ReceptionData` (0x01) | C/N₀-vs-elevation plausibility (§3) |
| **UBX-RXM-RAWX** | pseudorange, carrier phase, Doppler, lock-time, C/N₀ | `RFData` (0x02) | Doppler plausibility, measured iono (§3, `docs/MATH.md §7.4`) |
| **UBX-NAV-PVT / TIMEUTC** | fix time, clock offset/drift, fix validity flags | `ObserverPosition`/`ObserverDetails` (0x03/0x04) | time-jump plausibility (§3) |

`JammingStats` currently carries only a coarse subset. **Software task P-Jam-1:** extend the
`JammingStats` GNF1 record to carry the full MON-RF per-band block (AGC, noiseLevel,
cwSuppression, jamInd, antStatus per RF path) and the SEC-SIG per-signal state, so the collector
sees the raw numbers rather than a pre-digested flag. Keep it a fixed-layout record (no protobuf
on the wire — `docs/DESIGN.md §2`).

---

## 2. Jamming detection (the RF front-end)

GNSS signals arrive far below the thermal noise floor (~−130 to −160 dBm), so they are trivially
drowned out; the receiver's own front-end is the best jamming sensor we have. Two regimes:

- **Broadband (noise) jamming.** The AGC steps its gain *down* to keep the ADC from saturating
  when wideband energy floods the band. The detection metric is **AGC departure from the
  station's own clear-sky baseline**, not an absolute threshold — every antenna/cable/site has a
  different nominal AGC. The collector learns a per-station, per-band AGC baseline (robust median
  over a rolling quiet window) and flags a sustained downward departure, correlated with a
  fleet-wide C/N₀ drop at that station.
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

A jamming event is **corroborated**, not taken from one box: a real jammer near a station shows
as `agc_departure` + `cw_suppression` + a C/N₀ collapse across *all* SVs that station tracks,
simultaneously. A single metric moving alone is more likely a receiver/antenna fault than an
attack — surface it as a station-health signal, not a jamming alarm.

**The most important product use of jamming context** is not the alarm itself: it is
**down-weighting**. A station reporting active jamming has its votes in the broadcast-integrity
corroboration (`docs/INTEGRITY.md §6`) and the spoofing gates below reduced — its RF environment
is untrustworthy, so it should not veto or confirm on its own.

---

## 3. Spoofing detection (physics gates over baseband + nav)

Spoofing broadcasts counterfeit PRN codes to capture the receiver's tracking loops. We detect it
the way the design rule demands — physical and mathematical impossibilities — combining the
receiver's own SEC-SIG verdict with gates we compute independently. Most of these already exist
as plausibility gates in `docs/INTEGRITY.md §8`; this section is the RF-specific set and how they
fuse.

- **C/N₀-vs-elevation inconsistency.** Genuine C/N₀ varies with elevation and atmosphere: low SVs
  are weaker, high SVs stronger, with SV-to-SV spread. A constellation where *every* SV reports an
  identical, unnaturally high, elevation-independent C/N₀ is the classic single-transmitter
  spoofer signature. Metric: the **variance of C/N₀ residual after removing the elevation trend**
  collapsing toward zero across a constellation. (Uses `ReceptionData`.)
- **Doppler-vs-ephemeris.** Real SVs move at orbital velocity; a static ground spoofer cannot
  reproduce every SV's true range-rate. This is exactly the **delta-Hz** signal already defined in
  `docs/MATH.md §2.2` / `docs/INTEGRITY.md §3`: observed Doppler minus ephemeris-predicted Doppler.
  A *coherent* delta-Hz bias across many SVs at one receiver — all pulled toward a common origin —
  is a wide-area spoofer or a receiver clock error; the sign pattern distinguishes them.
- **Time / phase transients.** A spoofer capturing the tracking loops forces a non-physical jump
  in the receiver's clock offset or a carrier-phase step (the receiver's EKF sees an "impossible"
  transient). Metric: clock-offset / drift discontinuity from `ObserverDetails`, beyond what an
  oscillator can physically do, gated against a station's own stable-clock baseline (and, on the
  hardware tier, its disciplined TCXO/RTC — `docs/DESIGN.md §3`).
- **Cross-constellation contradiction.** A spoofer that targets only GPS L1 leaves the Galileo /
  GLONASS / BeiDou solutions disagreeing with the GPS one. Metric: divergence of the broadcast
  inter-system time offsets and per-constellation PVT beyond their normal agreement
  (`docs/MATH.md §8` offsets; a receiver whose GPS-only solution contradicts its multi-GNSS
  solution is suspect).
- **Measured-vs-model ionosphere.** A counterfeit constellation rarely reproduces a physically
  consistent dual-frequency ionospheric delay. The measured slant iono (`docs/MATH.md §7.4`) that
  diverges from the broadcast model *incoherently with the real space-weather picture the rest of
  the fleet sees* is a spoofing tell — one more independent physics gate the network gets for free.
- **Receiver SEC-SIG verdict.** The receiver's own spoofing/jamming flags are ingested and
  **weighted, not trusted**: a SEC-SIG spoofing flag *raises the prior*, and when it agrees with
  one or more independent physics gates above the event is confirmed; alone it is recorded but not
  alarmed (§5 — a black-box flag can miss a cold-boot spoof and can false-positive on multipath).

**Fusion rule.** No single gate fires a spoofing alarm. A confirmed `spoofing_suspected` event
requires **≥2 independent gates** agreeing at one station within the debounce window (or one gate
corroborated by a *neighbouring* station seeing the same anomaly on the same SVs — cross-receiver
corroboration is the strongest evidence, since a genuine broadcast is identical for every receiver
in view). This mirrors the broadcast-agreement logic in `docs/INTEGRITY.md §6`.

---

## 4. The detector, thresholds, and events (extending INTEGRITY)

These metrics flow through the **same debounced detector state machine** as the broadcast-integrity
metrics (`docs/INTEGRITY.md §4`) — per (station, metric) provisional→confirmed with a debounce
window — and emit through the **same event contract** (`docs/OUTPUT.md §3`). New event types,
added to the vocabulary in `docs/INTEGRITY.md §5`:

| `event_type` | Fires when | Severity |
|---|---|---|
| `jamming_detected` | AGC departure + CW/noise corroborated at a station, debounced | 1 → 2 on full lock loss |
| `spoofing_suspected` | ≥2 independent physics gates (§3) agree at a station, debounced | 2 |
| `station_rf_degraded` | a single RF metric departs baseline (fault-or-early-warning, not an attack claim) | 1 |
| `antenna_fault` | MON-RF antenna status open/short, or C/N₀ collapse with no jamming signature | 1 |

**Thresholds are observational in v1 — like the measured-iono feature, we publish the metrics and
accumulate per-station quiet-time baselines before freezing alert thresholds.** A jamming/spoofing
false positive that cries wolf is worse than a slightly delayed true positive; the thresholds land
in `docs/INTEGRITY.md §2` (the single source of threshold truth) once real distributions exist, per
the authority rule there. Severity encoding is the shared `0 info · 1 warning · 2 critical`.

Constellation/station scope: these are **station-scoped** events (the threat is at a receiver's
antenna), unlike the SV-scoped broadcast events. The event's `params` carry the station id, the RF
band, and the corroborating gate set so a client can localize the headline.

---

## 5. Why we recompute instead of trusting the receiver's flag

The receiver's built-in jamming/spoofing detector is valuable but has two structural blind spots we
must design around:

1. **Cold-boot blindness.** Commercial detectors are optimized to catch the *transition* into a
   degraded environment. A receiver powered on *inside* an already-spoofed environment has no
   genuine baseline to compare against and may report "clean." Our defense against this is
   **cross-receiver corroboration** and **fleet baselines**: a station that boots into a spoofed
   RF field disagrees with its neighbours and with its own historical quiet baseline, which we hold
   centrally and it cannot.
2. **Black-box opacity.** We cannot audit the vendor's thresholds, and they change across firmware.
   By deriving our own metrics from the raw MON-RF/RAWX numbers we get an explainable, versioned,
   fleet-consistent detector — the same reason we decode raw nav frames centrally instead of
   trusting the receiver's computed PVT (`docs/DESIGN.md §0`).

The receiver flag is therefore **one weighted input** into the fusion rule (§3), never the verdict.

---

## 6. Persistence & output

- **RF telemetry is stored like any observation** (`docs/OUTPUT.md §4`): the `JammingStats` /
  `ReceptionData` records land in the raw telemetry hypertable with their decoded projection, so a
  detector improvement can be **replayed over history** — the same re-decodability guarantee the
  nav frames get. A jamming/spoofing incident is then reconstructable after the fact.
- **Station RF health surfaces in the `observers` feed** (`docs/OUTPUT.md §1.3`): per-station,
  per-band `agc_departure`, `cw_suppression`, `noise_floor_db`, `jam_state`, and a `rf_trust`
  scalar (how much this station's votes are currently down-weighted). Confirmed events go to the
  SSE stream and `gnss_events` like every other integrity event.
- **Baselines are state, not config**: the per-station quiet-time AGC/C/N₀/clock baselines live in
  the live state and the historian, learned continuously — never hand-tuned constants.

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
