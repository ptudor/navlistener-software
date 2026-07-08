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
             health/URA/SISA/OSNMA, corroboration   confirmation → typed events          svs.json fields
```

- **Compute** runs on every new ephemeris / reception; it is pure and stateless-per-input given
  the ephemeris store. Its outputs are the numbers published in `svs.json` (`orbit-disco`,
  `time-disco`, `sisa-m`, `healthissue`, `eph-age-m`, `perrecv.delta_hz`, …).
- **Detect** is a **debounced state machine**: a provisional state change must persist for the
  debounce window before it becomes a confirmed `Event`. This kills the flapping that a single
  noisy frame would otherwise generate.

**Phase note & authority:** in the drop-in phase, intsat's own `detect` package (ported from
`galmonmon.cc`) already consumes our `svs.json`. In the absorb phase, `navlistener` runs this
detector itself, writes `gnss_events`, and fires `pg_notify` — so during the transition the
thresholds must stay numerically identical to intsat's `internal/detect/thresholds.go` (they
were verified identical on 2026-07-07). Going forward **this document is the standard**: intsat
is a consumer, changes land here first, and intsat conforms in lockstep.

---

## 2. The confirmed thresholds (from `galmonmon.cc`, as carried in intsat)

These are the exact constants intsat uses today (**verified 2026-07-07 against
`go/internal/detect/thresholds.go` + `detector.go`**); `navlistener` adopts them verbatim so
the cutover is a no-op for consumers. Change them only in lockstep with intsat.

| Metric | Threshold (constant) | Severity | Notes |
|---|---|---|---|
| **Ephemeris age (Galileo)** | `eph-age-m > 105` (`EphAgeThresholdGalileo`) | **warn** (`eph_aged`) | Galileo refreshes fastest; stale = suspect |
| **Ephemeris age (all non-Galileo)** | `eph-age-m > 140` (`EphAgeThresholdGPS`) | **warn** | despite the name, this constant is intsat's default for *every* non-Galileo constellation; QZSS/NavIC inherit it |
| **Orbit disco** | `> 1.45 m` (`OrbitDiscoThreshold`) → warn | `> 10 m` (`OrbitDiscoSevereThreshold`) → crit | GPS+Galileo only in intsat today; §3 |
| **Time disco (clock jump)** | `> 2.5 ns` (`TimeDiscoThreshold`) → warn | `> 10 ns` (`TimeDiscoSevereThreshold`) → crit | Galileo only in intsat today; `ns/3.335 ≈ m`; §3 |
| **SISA / URA change** | crosses `3.0 m` (`SISAAlertThreshold`) | warn | accuracy degradation |
| **Silent SV** | unseen `> 3600 s` (`SilentThreshold`) | warn (`observation_lost`) | GPS/Galileo (constellations always in view) |
| **Observer offline** | station unseen `> 300 s` (`ObserverOfflineThreshold`) | warn→crit (`station_offline`) | operator-actionable; shorter than SV silence |
| **Fresh-receiver window** | `≤ 60 s` (`FreshReceiverThreshold`) | — | a receiver's vote only counts if it saw the SV this recently |
| **Debounce** | `60 s` (`DebounceDuration`) | — | provisional state must persist this long to confirm |

> **Dead-band caveat.** `thresholds.go` also defines `OrbitDiscoWarningThreshold = 5.0` and
> `TimeDiscoWarningThreshold = 5.0` ("emphasis" middle bands, a galmonmon inheritance), but
> the shipped Go detector **never references them** — only 1.45/10 m and 2.5/10 ns actually
> branch. The real contract is two bands, not three; if intsat ever wires the 5.0 bands in,
> adopt in lockstep.

Severity encoding (the SSE/`gnss_events` contract, `thresholds.go`): `0 = info`,
`1 = warning`, `2 = critical`. Where intsat restricts an event to certain constellations
(orbit_disco: GPS+Galileo; clock_jump: Galileo; observation_lost: GPS+Galileo), we widen to
the full monitored set below — that widening is a documented superset, not a threshold change.

**Monitored signals.** The initial monitoring set covers GPS L1CA (`0,0`), Galileo E1 (`2,1`), BeiDou B1I
(`3,0`), GLONASS L1 (`6,0`). **Regional signal coverage:**
add **QZSS L1CA (`5,0`)** and **NavIC L5 (`7,0`)** as first-class monitored signals, with the
GPS-family thresholds (QZSS) and NavIC-appropriate staleness (its SPS NAV refresh cadence). This
is the integrity-layer half of the CONSTELLATIONS.md decoder work — decoding QZSS/NavIC is
pointless if the monitor ignores them.

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
guard yields the `-1` "unknown" sentinel, never a garbage number that trips an alert.

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
single receiver's oscillator. Published per-receiver in `svs.json.perrecv`.

**RTCM precise-vs-broadcast.** From SSR corrections (RTCM 1057–1068), the magnitude of the
radial/along/cross orbit correction is "how wrong the broadcast orbit is vs. the precise network
orbit" — an independent truth source. Surfaced as `rtcm-eph-delta-cm` (+ components). A broadcast
that diverges from the SSR correction while claiming good SISA is a strong integrity flag.

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
| `health_change` | broadcast health/`healthissue` transition | 2 |
| `eph_aged` | ephemeris age crosses the constellation threshold (§2) | 1 |
| `orbit_disco` | orbit-disco band change (§2) | 1, → 2 above 10 m |
| `clock_jump` | time-disco band change | 1, → 2 above 10 ns |
| `sisa_change` | SISA/URA crosses 3 m | 1 |
| `observation_lost` | SV silent > 3600 s | 1 |
| `station_offline` | observer unseen > 300 s | 1–2 |
| `position_unknown` | a monitored SV has no computable position | 1 |
| `osnma_change` | Galileo OSNMA authentication on↔off | 0 |
| `sbas_health` | SBAS message-type-0 / health change | 0, → 2 on do-not-use |
| `qzss_health` *(ours)* | QZSS health/DC-report (disaster) transition | 1–2 |
| `navic_health` *(ours)* | NavIC SPS health transition | 1–2 |

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

---

## 7. Galileo OSNMA (authentication as an integrity input)

Galileo's Open Service Navigation Message Authentication (OSNMA) lets a receiver cryptographically
verify that the I/NAV data came from Galileo (TESLA-based, delayed-key). We do **not** re-run the
full OSNMA verification chain in v1 (it needs the Merkle root / public-key infrastructure and
tight timing); we **decode and republish the OSNMA status bits** the receiver/frame exposes and
alert on the `osnma_change` transition (authentication present ↔ absent). Full on-box OSNMA
verification is a stretch goal (OS-SIS-ICD OSNMA annex). Note the framing: OSNMA proves the *data*
is genuinely Galileo's — it is provenance, and it still doesn't tell you the *orbit* is good;
orbit-disco does. Both layers, again.

---

## 8. Spoofing & plausibility gates (physics first)

Cheap, always-on gates that catch the common attacks and gross errors without any crypto:

- **No teleports / continuity:** an SV ECEF that jumps beyond orbital dynamics between
  consecutive ephemerides (beyond the disco bands) → flag.
- **Below-horizon impossibility:** a receiver reporting an SV at negative geometric elevation for
  its known position → flag the *receiver*, not the SV.
- **Doppler sanity:** observed Doppler outside the physically possible range for the SV's
  range-rate (docs/MATH.md §2.2) → flag.
- **Valid-range checks:** SV id / PRN in the constellation's assigned range; IOD monotonicity;
  URA/SISA within table bounds; week number consistent with wall-clock.
- **Cross-constellation clock coherence:** the broadcast inter-system offsets (§docs/MATH.md 8)
  should be mutually consistent across receivers; a receiver whose time-offset decode disagrees
  with the fleet is suspect.
- **Jamming context:** u-blox MON-HW/MON-RF jamming/AGC indicators (the GNF1 `JammingStats`
  telemetry type) raise the prior on a receiver's environment being hostile, down-weighting its
  votes.

Every gate is a *plausibility* judgement, cheap and independent of signatures — the failures they
catch (a replayed constellation, a lifted-and-shifted receiver, a bad upload) are exactly the ones
signatures miss.

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

| Signal | `svs.json` field | schema-1.1 field | Event |
|---|---|---|---|
| health | `healthissue`, `health` | `health_code`, `health_issue_level`, `health_subcode` | `health_change`, `qzss_health`, `navic_health`, `sbas_health` |
| ephemeris age | `eph-age-m` | `eph_age_m` | `eph_aged` |
| orbit disco | `orbit-disco`, `orbit-disco-age` | `orbit_disco` | `orbit_disco` |
| clock jump | `time-disco` | `time_disco` | `clock_jump` |
| accuracy | `sisa`, `sisa-m` | `sisa_valid`, `sisa_m` | `sisa_change` |
| per-receiver Doppler | `perrecv.delta_hz(_corr)` | same | (feeds coherent-delta detection) |
| OSNMA | `osnma` | `osnma` | `osnma_change` |
| silence | `last-seen-s` | `last_seen_s` | `observation_lost` (SV), `station_offline` (observer) |
| corroboration | `conf`, `perrecv` | same | — |

All of these are byte-compatible with what intsat consumes today (`docs/OUTPUT.md`), so the
integrity output remains consistent during consumer integration.
