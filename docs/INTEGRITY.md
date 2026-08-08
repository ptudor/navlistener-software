# navlistener — integrity monitoring

**Status: design (2026-07-07).** How `navlistener` turns decoded navigation messages into
integrity signals and alerts. The *math* that produces the raw metrics is in `docs/MATH.md §9`;
this doc is the *detection* layer — thresholds, debouncing, severity, corroboration, and the
event contract — plus the spoofing/plausibility gates that are the reason the map is called
*Integrity*.

> The design rule, applied to orbits: **"verify physics, not signatures."** A cryptographic
> signature (OSNMA, our own ATECC feeder auth) proves *provenance* — who sent the frame and that
> it wasn't altered in flight. It cannot prove the *signal* was honest: a receiver fed a spoofing
> transmitter, or a satellite broadcasting a bad ephemeris, emits perfectly-authentic wrong data.
> Integrity is the second, independent layer: does the broadcast agree with physics, with the
> previous broadcast, and with what *other* receivers heard? Provenance + plausibility, both.

---

## 1. Where integrity is computed vs. detected

Two responsibilities are kept separate within `navlistener`:

```
  DECODE ──▶ COMPUTE (internal/state)          ──▶ DETECT (internal/state/detect)   ──▶ EMIT
             derive raw metrics per SV:             debounced state machine:             SSE events,
             orbit-disco, time-disco, delta-Hz,     threshold + hysteresis + 60 s        gnss_events,
             health/URA/SISA/OSNMA, corroboration   confirmation → typed events          svs feed fields
```

- **Compute** runs on every new ephemeris / reception; it is pure and stateless-per-input given
  the ephemeris store. Its outputs are the numbers published in the svs feed (`orbit_disco_m`,
  `time_disco_ns`, `sisa_m`, `health_issue_level`, `eph_age_m`, `perrecv.delta_hz`, …).
- **Detect** is a **debounced state machine**: a provisional state change must persist for the
  debounce window before it becomes a confirmed `Event`. This kills the flapping that a single
  noisy frame would otherwise generate.

**Authority:** **`navlistener` runs this detector itself** — computes the metrics, detects the
transitions, writes `gnss_events`, fires `pg_notify`. This document is the standard for all of
it. intsat today carries its own legacy detector (a galmonmon port); that code is retired when
intsat repoints to us — it is a consumer, not a designer. Its constants were cross-checked once
against this table (2026-07-07); from here on, changes are designed here and consumers align.

---

## 2. The thresholds (the standard)

These thresholds are **defined here.** The numeric values are retained from galmonmon/intsat
operational experience because they are proven operating points — cross-checked 2026-07-07
against intsat's as-built `go/internal/detect/{thresholds,detector}.go` — not because
compatibility demands them. Changes are designed in this table first; intsat and every other
consumer align to it.

| Metric | Threshold (constant) | Severity | Notes |
|---|---|---|---|
| **Ephemeris age (Galileo)** | `eph-age-m > 105` (`EphAgeThresholdGalileo`) | **warn** (`eph_aged`) | Galileo refreshes fastest; stale = suspect |
| **Ephemeris age (all non-Galileo)** | `eph-age-m > 140` (`EphAgeThresholdGPS`) | **warn** | despite the name, this constant is intsat's default for *every* non-Galileo constellation; QZSS/NavIC inherit it |
| **Orbit disco** | `> 1.45 m` (`OrbitDiscoThreshold`) → warn | `> 10 m` (`OrbitDiscoSevereThreshold`) → crit | GPS+Galileo only in intsat today; §3 |
| **Time disco (clock jump)** | `> 2.5 ns` (`TimeDiscoThreshold`) → warn | `> 10 ns` (`TimeDiscoSevereThreshold`) → crit | Galileo only in intsat today; `ns/3.335 ≈ m`; §3 |
| **SISA / URA change** | crosses `3.0 m` (`SISAAlertThreshold`) | warn | accuracy degradation |
| **Silent SV** | unseen `> 3600 s` (`SilentThreshold`), classified only with `≥ 4` live receivers (`SilenceMinReceivers`) | warn (`observation_lost`) | **precondition : "always in view" is true of the constellation as seen by a globally distributed fleet, not of one station's sky** — from a sub-footprint fleet every MEO/IGSO SV sets once per orbital pass and the classifier manufactured a false warning pair per pass. Below the floor the classifier is suppressed (machines hold, regression fix); constellation-outage coverage there comes from `eph_aged`/`position_unknown`/`capability_signal_lost`. The floor is a necessary-not-sufficient proxy until the observer-geometry pass lands the real gate (propagated elevation > mask from ≥ 1 live station) |
| **Silent SBAS GEO** | PRN unseen `> 300 s` (`SBASSilentThreshold`) | warn (`sbas_lost`) | no visibility caveat — geostationary, ~1 Hz broadcast, never rise/set noise. Mirrors `sbasStaleAfter` (the feed's own staleness drop), so the event confirms one debounce after the entry leaves the sbas feed. Classified from the detector's **unfiltered** store view (`SBASDetect`); while a PRN is unseen past `SBASHealthCurrentWindow` (60 s, the regression fix MT0-latch horizon) its `sbas_health` machine holds rather than reading the decayed latch as a fabricated recovery |
| **Observer offline** | station unseen `> 300 s` (`ObserverOfflineThreshold`) | warn (`station_offline`) | operator-actionable; shorter than SV silence. Wired per regression fix (regression fix defined it but nothing emitted it): classified from the unfiltered caps ∪ rf liveness map, so a dark station stays observable indefinitely. The crit escalation tier is reserved until a second threshold is defined |
| **Fresh-receiver window** | `≤ 60 s` (`FreshReceiverThreshold`) | — | a receiver's vote only counts if it saw the SV this recently |
| **Debounce** | `60 s` (`DebounceDuration`) | — | provisional state must persist this long to confirm |

> **Dead-band note.** intsat's `thresholds.go` also defines `OrbitDiscoWarningThreshold = 5.0`
> and `TimeDiscoWarningThreshold = 5.0` ("emphasis" middle bands, a galmonmon inheritance), but
> its shipped detector never references them — only 1.45/10 m and 2.5/10 ns actually branch.
> **The standard is two bands (warn/crit); the 5.0 constants are not part of it.**

> **Pending calibration decision — regression fix (2026-08-08, not yet the standard).** The first
> live GLONASS calibration (125 routine tb changeovers, single site/session;
> measured with `go/cmd/gloreplay`) measured routine orbit-disco
> p95 1.75 m / max 4.13 m with **15.2% of routine changeovers ≥ the 1.45 m warn band**, and
> routine time-disco p95 1.86 ns / max **9.99 ns — 0.1% below the 10 ns crit band**. Proposed
> GLONASS-specific pair, awaiting ratification: orbit warn 1.45 → **5 m** (crit 10 m keeps),
> time crit 10 → **25 ns** (warn 2.5 ns keeps). Until this table changes, the detector
> applies the shared bands above to GLONASS unchanged.

Severity encoding (the SSE/`gnss_events` contract): `0 = info`, `1 = warning`, `2 = critical`.
The standard applies each event type across the **full monitored set** below. intsat's as-built
restrictions (orbit_disco: GPS+Galileo; clock_jump: Galileo-only; observation_lost:
GPS+Galileo) are consumer limitations to be lifted, not part of the standard.

**Monitored signals.** navlistener monitors GPS L1CA (`0,0`), Galileo E1 (`2,0` — E1-B is
u-blox sigId 1 on the wire but the feed keys it at the primary sigid 0, so cross-references use
`2,0`/`E##@0`, regression fix), BeiDou B1I (`3,0`), GLONASS L1 (`6,0`), **QZSS L1CA (`5,0`)**, and
**NavIC L5 (`7,0`)** — with the GPS-family thresholds (QZSS) and NavIC-appropriate staleness
(its SPS NAV refresh cadence). This is the integrity-layer counterpart of the CONSTELLATIONS.md
decoder coverage.

---

## 3. Orbit-disco and time-disco: computing the discontinuity

The two headline metrics are **discontinuities across an ephemeris changeover** — the thing a
single receiver cannot see but a historian can, because it remembers the *previous* ephemeris.

**Orbit-disco.** When a new IOD ephemeris `E_new` arrives for an SV that already had `E_old`:

```
t*        = E_new.toe                          // the changeover reference epoch
X_old     = Propagate(E_old, t*)               // docs/MATH.md §2 or §3
X_new     = Propagate(E_new, t*)
orbit-disco = |X_new − X_old|                  // metres
```

Guards: only record when **both** propagations
succeed and are non-zero/non-NaN, **both** ephemerides are `ephAge < 4 h` old, and this is **not**
the first-ever ephemeris for the SV (no `E_old` ⇒ no disco, not a huge phantom jump). A violated
guard leaves the field **absent** (unknown), never a garbage number or a magic sentinel that
trips an alert.

**Time-disco.** The clock-offset jump at the same changeover:

```
time-disco = |ClockOffset(E_new, t*) − ClockOffset(E_old, t*)|   // ns
```

with `ClockOffset` = `af0 + af1·Δt + af2·Δt² + Δtr` (docs/MATH.md §4), the relativistic term
computed consistently for both models. A legitimate ephemeris update has sub-nanosecond,
sub-metre discontinuities; a real orbit/clock event, an upload error, or a spoofer swapping the
broadcast shows up as metres / nanoseconds. This discontinuity measure is also used in galmon's Galileo integrity reporting.

**delta-Hz** (per receiver): observed Doppler − ephemeris-predicted Doppler (docs/MATH.md §2.2),
optionally clock-corrected (`delta_hz_corr`). A *coherent* delta-Hz across independent receivers
points at the broadcast (orbit/clock) or a wide-area spoofer; an *incoherent* one points at a
single receiver's oscillator. Published per-receiver in the svs feed's `perrecv`.

**RTCM precise-vs-broadcast.** From SSR corrections (RTCM 1057–1068), the magnitude of the
radial/along/cross orbit correction is "how wrong the broadcast orbit is vs. the precise network
orbit" — an independent truth source. Surfaced as `rtcm_eph_delta_cm` (+ components). A broadcast
that diverges from the SSR correction while claiming good SISA is a strong integrity flag.

**Measured-vs-model ionosphere `iono_resid_m`** (per receiver × SV): the carrier-leveled
geometry-free dual-frequency measurement minus the broadcast-model prediction (MATH.md §7.4).
The discriminator is coherence, same as delta-Hz: a residual that moves **coherently across
receivers and satellites** is an ionospheric storm or a broadcast-model failure — a space-
weather sensor the network gets for free; a **single receiver** diverging is local
multipath/interference and down-weights that receiver's votes rather than raising an SV alarm.
v1 is observational — publish the residuals, accumulate baselines; alert thresholds and an
event type are added to §2/§5 only once quiet-time distributions are known (constants land in
this document first, per §authority).

---

## 4. The detector state machine

Per `(SV, metric)` the detector holds `{current_state, provisional_state, provisional_since}`:

```
on each update(metric_value):
    s = classify(metric_value)                 // e.g. healthy/unhealthy, or disco-band
    if s == current_state:
        provisional_state = none               // reset any pending change
    else if s == provisional_state:
        if now − provisional_since ≥ 60 s:     // DebounceDuration
            emit Event(old=current_state, new=s, severity, message, params)
            current_state = s
            provisional_state = none
    else:
        provisional_state = s                   // start a new pending change
        provisional_since = now
```

`classify` applies §2's thresholds with **hysteresis** where a metric is continuous (e.g. SISA
must cross the band by a margin to flip and flip back, so a value dithering on the boundary
doesn't alternate). Each emitted `Event` carries `params` (the interpolation values — SV,
constellation, magnitudes, discriminators) so clients localize the headline; `message` is the
English fallback. This mirrors intsat's `Event`/`params` contract exactly (`docs/OUTPUT.md §3`).

---

## 5. Event types (the SSE / `gnss_events` contract)

The baseline vocabulary and severities below are **verified against intsat's shipped detector
(2026-07-07)**; the two `*_health` types are our extensions.

| `event_type` | Fires when | Severity |
|---|---|---|
| `health_change` | broadcast health transition (`health_code`/`health_issue_level`) | 1–2 (severity follows `health_issue_level` — marginal component codes warn, do-not-use/nav-data-bad is critical) |
| `eph_aged` | ephemeris age crosses the constellation threshold (§2) | 1 |
| `orbit_disco` | orbit-disco band change (§2) | 1, → 2 above 10 m |
| `clock_jump` | time-disco band change | 1, → 2 above 10 ns |
| `sisa_change` | SISA/URA crosses 3 m, or the broadcast index decodes to the "no accuracy prediction — use at own risk" sentinel (`new_value` `no_accuracy`, regression fix). BeiDou `C##@8` (packed B-CNAV2 SISAI, OUTPUT.md §1.1 `acc_index`): no metres decode exists, so transitions carry `raw_<packed>` values at **info** severity instead of the 3 m band  | 1 |
| `observation_lost` | SV silent > 3600 s — classified only while `≥ SilenceMinReceivers` stations are live (below the fleet-footprint floor, silence is orbital mechanics, not an outage; §2) | 1 |
| `station_offline` | observer unseen > 300 s — wired per regression fix (definition, from the unfiltered station-liveness map) | 1 |
| `sbas_lost` | SBAS PRN unseen > 300 s  — the augmentation mirror of `observation_lost`, with no visibility caveat (GEOs never set); subject `S<prn>` | 1 |
| `position_unknown` | a monitored SV has no computable position | 1 |
| `osnma_change` | Galileo OSNMA authentication on↔off | 0 |
| `sbas_health` | SBAS message-type-0 / health change (do-not-use is latched on MT0 recency — `sbasType0Hold` — not the last message, so a test-mode MT0/2 interleave reads do-not-use, as a DO-229 receiver would) | sev 0 (info), → sev 2 (critical) on do-not-use — these are event *severities*; the served SBAS `health_code` itself is only ever 1 (OK) or 3 (do-not-use), per the §OUTPUT 2.2 enum  |
| `qzss_health` | QZSS navigation health transition; L1S DC-report enrichment is planned | 1–2 |
| `navic_health` *(planned)* | NavIC SPS health transition; unavailable until the NavIC decoder lands | 1–2 |
| `jamming_detected` | station AGC/CW/noise evidence confirms jamming (`DEFENSE-PNT.md §4`) | 1, → 2 on severe/full-lock-loss evidence |
| `spoofing_suspected` | ≥2 independent station physics gates agree (`DEFENSE-PNT.md §3–4`). **Dormant in v1 : only 1 station-fusion gate is wired, so this cannot fire — see §8's dormancy disclosure and the `spoof_gates_wired`/`spoof_gate_quorum` gauges** | 2 |
| `station_rf_degraded` | one station RF metric departs its baseline; early warning, not an attack claim | 1 |
| `antenna_fault` | receiver antenna status reports open/short or equivalent confirmed fault | 1 |
| `capability_signal_lost` | a demonstrated `(gnssId,sigId)` is unseen past `CapSignalLostAfter` while its station remains alive | 1 |
| `capability_impossible` | an observed signal lies outside the station's declared tudorgps capability set | 2 |
| `ura_alert` | GPS/QZSS URA-alert flag transition : LNAV HOW bit 18 / CNAV bit 38 — the SV's own "use at own risk" declaration (IS-GPS-200N §20.3.3.2, a §6.4.6.3 marginal condition) | 1 on raise, 0 on clear |
| `wn_mismatch` | broadcast week number (LNAV 10-bit / CNAV 13-bit, rollover-disambiguated) disagrees with the collector wall-clock week  — the cheapest time-domain anomaly: upload error, SV clock fault, or a replayed/spoofed signal carrying a wrong week | 2 on mismatch, 0 on recovery |
| `bds_integrity_flag` | a BeiDou B-CNAV2 SV's broadcast per-signal integrity flags (DIF/SIF/AIF, BDS-SIS-ICD-B2a Table 7-23) change state  — the constellation's own real-time integrity channel, e.g. DIF=1 "the error of message parameters broadcasted in this signal exceeds the predictive accuracy". Warning on any raise (the ICD defers the flags' numeric thresholds), info on clear | 1 on raise, 0 on clear |
| `xsig_divergence` | one physical SV's two independently-decoded signals (Galileo E1-B I/NAV `E##@0` vs E5a F/NAV `E##@3`) broadcast diverging ephemerides under one IODnav  — a signal-selective fault or spoof; the cross-signal agreement evidence F/NAV was wired to provide. Subject is the physical SV name (`E14`, no `@sig`); Galileo-only (same-IODnav I/NAV and F/NAV carry the same CED per GAL-OS-SIS-ICD §5.1.9.2, so agreeing signals differ by exactly 0 m — GPS LNAV-vs-CNAV / BDS D1-vs-B-CNAV2 are independent curve fits needing their own tolerance analysis). Warning in v1: a single-collector observation corroborates rather than convicts | 1 on divergence, 0 on clear |
| `leap_mismatch` | an SV's broadcast current leap-second count (BeiDou B-CNAV2 MT34's BDT-UTC ΔtLS today, via the fixed BDT = GPST − 14 s alignment) disagrees with the collector's configured GPS−UTC count (regression fix — the regression fix cross-check): a stale config after a real leap event, or a bogus broadcast. GNSS-side axes are unaffected; UTC-facing output is shifted by whole seconds | 1 on mismatch, 0 on recovery |

The two `*_health` types cover QZSS and NavIC; the
QZSS one is doubly interesting because QZSS L1S carries **DC Report** disaster/crisis messages
(earthquake/tsunami), which are themselves a broadcast-integrity-relevant stream for Japan.

---

## 6. Corroboration & trust (the "Integrity" namesake)

Provenance and plausibility combine into a per-observation trust picture, reusing radiolistener's
tiers (`Source.Tier()`): our own dialed RF > authenticated edge push (ATECC > software cert >
token) > unauthenticated LAN > third-party. But GNSS adds cross-receiver corroboration that is
uniquely strong because *every* receiver in view should hear the *same* broadcast:

- **Broadcast agreement.** The navigation message for a given SV/IOD is identical for every
  receiver on Earth in view. If two authenticated receivers decode the *same* SV/IOD to
  *different* ephemeris bits, one is faulty or one is being spoofed — a hard integrity alarm that
  needs no external truth. (Store raw frames from all receivers; compare the decoded element sets,
  not just positions.)
- **Coverage plausibility.** An SV that *should* be visible to N receivers (by geometry) but is
  reported by only one is suspect; an SV reported by a receiver from a position where it is below
  the horizon is impossible (a physics gate).
- **Capability plausibility (tudorgps).** Each observer's decoded-signal capability is known from
  its `tudorgps` fingerprint (`docs/DESIGN.md §3`). A node whose silicon *cannot* track L5
  reporting L5 frames is a forged/proxied feed; a node that *can* track E6/L5 but never does is a
  config/antenna fault. The fingerprint sets the expectation; deviation is the signal.
- **Confidence** (`conf` in the feeds) = number of independent authenticated chains corroborating
  an SV's state — a render cue in the map (solid vs. hollow/dimmed/badged), exactly as
  radiolistener does for aircraft/ships.

**As-built status :** `conf` is served on every `svs` entry and stamped into every
**satellite×signal (svs-subject) event's** `params` — the station-subject, SBAS-subject
(`S##`), and cross-signal (`xsig_divergence`) event families deliberately carry no `conf`
(their subjects are not satellite×signal keys; regression fix) — counted from per-source
decoded-nav-frame recency inside the 60 s
fresh-receiver window (§2), so consumers can tell "five stations agree" from "one station said
so". The **broadcast-agreement divergence detector** (same SV/IOD decoded to different bits by
different receivers → hard alarm) additionally needs per-source element hashes and is tracked
P7 work — it only becomes meaningful once the fleet converts to `navfeeder` push with per-observer
identity. Until then every event's `conf` makes the corroboration level explicit rather than
implied.

---

## 7. Galileo OSNMA (authentication as an integrity input)

Galileo's Open Service Navigation Message Authentication (OSNMA) lets a receiver cryptographically
verify that the I/NAV data came from Galileo (TESLA-based, delayed-key). We do **not** re-run the
full OSNMA verification chain in v1 (it needs the Merkle root / public-key infrastructure and
tight timing); we **decode and republish the OSNMA status bits** the receiver/frame exposes and
alert on the `osnma_change` transition (OSNMA protocol data present ↔ absent — presence, not
verification, per the regression fix contract note in OUTPUT.md §1.1). Full on-box OSNMA
verification is a stretch goal (OS-SIS-ICD OSNMA annex). Note the framing: OSNMA proves the *data*
is genuinely Galileo's — it is provenance, and it still doesn't tell you the *orbit* is good;
orbit-disco does. Both layers, again.

---

## 8. Spoofing & plausibility gates (physics first)

Cheap, always-on gates that catch the common attacks and gross errors without any crypto. **Each
gate below carries its as-built status ** — this section is the design set, not a claim
of coverage:

- **No teleports / continuity** — *implemented as the per-SV disco detectors* (`orbit_disco`/
  `clock_jump`, §3): an SV ECEF that jumps beyond orbital dynamics between consecutive
  ephemerides → flag.
- **Below-horizon impossibility** — *planned; blocked on station positions (the
  observer-geometry pass)*: a receiver reporting an SV at negative geometric elevation for its
  known position → flag the *receiver*, not the SV.
- **Doppler sanity / coherent delta-Hz** — *planned; blocked on station positions for the
  predicted range-rate*: observed Doppler outside the physically possible range for the SV's
  range-rate (docs/MATH.md §2.2) → flag. This is the named NEXT station-fusion gate.
- **Valid-range checks** — *partially implemented as per-SV classifiers and ingest gates*:
  SV id / PRN envelopes, week number vs wall-clock (`wn_mismatch`, regression fix/
  regression fix), URA/SISA table-bounded decode. IOD monotonicity — *planned*.
- **Cross-constellation clock coherence** — *planned; input decode partial*: the broadcast
  inter-system offsets (docs/MATH.md §8) should be mutually consistent across receivers.
  Galileo GGTO and BeiDou BDT-UTC now decode; the coherence detector itself
  is tracked P6+ work.
- **Jamming context** — *implemented* (`jamming_detected`/`station_rf_degraded`, DEFENSE-PNT
  §2/§4): u-blox MON-HW/MON-RF jamming/AGC indicators raise the prior on a receiver's
  environment being hostile, down-weighting its votes (`rf_trust`).
- **Cross-signal broadcast agreement** — *implemented for Galileo (regression fix,
  `xsig_divergence`)*: one SV's independently-decoded I/NAV and F/NAV positions compared under
  one IODnav at one propagation epoch — the intra-SV analogue of §6's cross-receiver
  broadcast-agreement check, catching a signal-selective fault/spoof.

Every gate is a *plausibility* judgement, cheap and independent of signatures — the failures they
catch (a replayed constellation, a lifted-and-shifted receiver, a bad upload) are exactly the ones
signatures miss.

> **Dormancy disclosure.** The station-level *fusion* (`spoofing_suspected`) requires
> `SpoofGateQuorum` (2) independent gates agreeing, and exactly **one** station-fusion gate is
> wired today (C/N₀-vs-elevation, `WiredSpoofGates = 1`) — so `spoofing_suspected` is
> **arithmetically unreachable** in v1. That is a deliberate conservative posture (a single gate
> is a degradation signal, not an attack claim — `station_rf_degraded` carries it), made visible
> rather than implied: the daemon exports `navlistener_spoof_gates_wired` and
> `navlistener_spoof_gate_quorum` gauges (alert on `wired < quorum`) and logs the dormancy at
> startup. The per-SV plausibility gates above (`wn_mismatch`, discos, envelopes) fire
> independently of the quorum.

---

## 9. Hardening (the C1–C11 discipline)

Integrity code parses untrusted broadcast and receiver data. Its security
requirements cover bounded reads and allocations, safe serialization, and
synchronized access to shared state:

- Every frame decoder is **bounds-checked** and **fuzzed** (`go test -fuzz`); a short/oversized
  frame is dropped, never read past.
- All shared integrity state is **race-clean** (Go race detector in CI; counters are
  `atomic` or mutex-guarded).
- Feed serialization **sanitizes** any string field (receiver-supplied `remark`/`owner`/vendor)
  before JSON encoding — no untrusted bytes reach the encoder.
- The push endpoint validates every length prefix against `MaxFrameLen` before allocating.

---

## 10. What we report and where

| Signal | Feed field (`docs/OUTPUT.md §1`) | Event |
|---|---|---|
| health | `health_code`, `health_issue_level`, `health_subcode` | `health_change`, `qzss_health`, `navic_health`, `sbas_health` |
| ephemeris age | `eph_age_m` | `eph_aged` |
| orbit disco | `orbit_disco_m`, `orbit_disco_age_s` | `orbit_disco` |
| clock jump | `time_disco_ns` | `clock_jump` |
| accuracy | `sisa_valid`, `sisa_m`, `acc_index` | `sisa_change` |
| per-receiver Doppler | `perrecv.delta_hz(_corr)` | (feeds coherent-delta detection) |
| OSNMA | `osnma` | `osnma_change` |
| silence | `last_seen_s` | `observation_lost` (SV), `station_offline` (observer), `sbas_lost` (SBAS PRN; regression fix) |
| corroboration | `conf`, `perrecv` | — |

**Scope :** this table maps the core per-SV integrity signals to their primary feed
fields; it is deliberately **not** the full event vocabulary. The authoritative, complete event
list — including `ura_alert`, `wn_mismatch`, `leap_mismatch`, `bds_integrity_flag`,
`position_unknown`, `xsig_divergence`, the two capability events, and the four station-RF
events — is §5's table, which is kept matched to the shipped `Type:` literals.

Field names and event vocabulary are defined once, in `docs/OUTPUT.md`; the integrity layer
emits to that standard and consumers read it from there.
