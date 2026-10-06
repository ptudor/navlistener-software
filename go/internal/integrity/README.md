# `internal/integrity` — station assurance checks

**Headline:** turns one station's receiver measurements into explainable assurance states.
Each check reports a candidate state with the values and thresholds behind it; a tracker
filters and holds those states; fusion combines them into a station state and decides
whether the evidence indicates spoofing. The design and its upstream provenance are in
[`docs/proposals/STATION-ASSURANCE.md`](../../../docs/proposals/STATION-ASSURANCE.md).

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `integrity.go` | `State`, `Domain`, `Info`, `Verdict` and `Result`: the check contract and its served shape. |
| `tracker.go` | The per-check M-of-N filter, warm-up, staleness, and one-sided recovery hold. |
| `fuse.go` | `Fuse`: the weighted station level and the domain rules, including the spoofing rule. |
| `profile.go` | `Profile` (operating points and their defaults), `StationProfile` (installation), validation, and `ConfigHash`. |
| `inputs.go` | The receiver-neutral inputs: `Solution`, `ClockSample`, `ReceiverStatus`, `TimingSample`, `Cn0Fit`, `RFSample`. |
| `position.go` | `static_position`, `stationary_velocity`, `motion_bound`, `position_velocity`. |
| `clock.go` | `clock_bias_drift`, `clock_drift_rate`. |
| `timeref.go` | `utc_offset`, `pps_rtc_phase`. |
| `rf.go` | `cn0_uniformity`, `agc`, `receiver_spoofing`, and the station RF defaults `internal/detect` shares. |
| `station.go` | `Station`: routes inputs to checks, gates clock and pulse checks on a valid fix, and assembles the `Assessment`. |
| `*_test.go` | Filter, hold, fusion, profile, per-check and station tests, including determinism and a no-coordinates guard. |
| `README.md` | This file. |

Imports the standard library and `github.com/ptudor/gnss` for geodesy. It holds no clocks, goroutines or I/O: every input
arrives with the collector-local instant it was received, so a replay of stored inputs
evaluates exactly as the live collector did.

---

## Summary

### States

A check or a station is `unavailable` (too little input), `unassured` (input likely
untrustworthy), `inconsistent` (trust cannot be established) or `assured`. These are the
PNT Integrity library's four levels, with its fusion mapping: unassured −1, inconsistent 0,
assured +1.

### Filter and hold

A check evaluates every input and returns a `Verdict`. Its tracker keeps the last N
available raw states (four by default). A degraded state becomes the candidate once M of
them (three) are at least that bad, so one noisy epoch cannot move a check, and a check with
fewer than M evaluations since its input resumed is still `unavailable`.

Hysteresis is one-sided. A worse candidate is served at once. A better one is served only
after every candidate for `RecoveryHold` (five minutes) has been better than the served
state, and then at the worst of those candidates. A check that was merely `unavailable`
becomes `assured` at once, since nothing was found wrong. A check with no input for
`StaleAfter` is evaluated as `unavailable`, which a degraded check must also hold before it
is served.

### Fusion

`Fuse` takes the served results. Every available check takes part in a weighted mean of
levels, mapped to a state at ±0.5. A **lower-only** check takes part only when it is not
assured: its assured state is evidence of absence, so it can lower a station but never
raise one. Then two domain rules apply:

- **Unassured needs a second domain.** A station is `unassured` only when a physics domain
  (signal power, position, receiver clock, time reference) is unassured and another domain
  agrees: a second physics domain, the receiver's own verdict, or the RF environment.
  Otherwise any unassured check holds the station at `inconsistent`.
- **An assured mean never hides an unassured check.**

`SpoofingIndicated` is the stricter spoofing rule: two physics domains, or one together with
the receiver's own spoofing indication. Interference in the RF environment does not count
toward it. Checks in one domain share their measurements and count once.

### Checks

| Check | Domain | Input | Test (defaults) |
|---|---|---|---|
| `static_position` | position | solution, surveyed position | Horizontal and vertical error from the survey; bands 3σ/6σ of the reported accuracy, clamped to 15–100 m and 50–300 m horizontally (25–150 m, 80–450 m vertically). A ten-minute mean offset over 8 m horizontal or 15 m vertical is inconsistent. Fixed stations only. |
| `stationary_velocity` | position | solution | Reported speed; 3σ/6σ of speed accuracy, at least 0.5 and 2 m/s. Fixed stations only. |
| `motion_bound` | position | solution | Distance from the last in-bounds epoch ≤ max speed × elapsed + 3 × both 3D accuracies + 10 m, else unassured. Mobile stations only. |
| `position_velocity` | position | solution | Velocity from a five-second position difference against the reported velocity integrated over the same interval; 3σ/6σ of speed accuracy, at least 1 and 3 m/s. |
| `clock_bias_drift` | receiver clock | clock | \|Δbias − ∫drift\| over 30–40 s, whole-millisecond adjustments removed; 75 and 145 ns. |
| `clock_drift_rate` | receiver clock | clock | \|Δdrift\|/Δt over 60–120 s; 0.05 and 0.2 ns/s². |
| `utc_offset` | time reference | solution | Receiver UTC against the observer's or collector's wall-clock stamp of the record; 2 and 5 s. |
| `pps_rtc_phase` | time reference | timing, solution | RTC-minus-GNSS pulse phase against a line fitted to samples 10–180 s old; 20 and 100 µs. Needs a fresh valid fix and an untrimmed RTC. |
| `cn0_uniformity` | signal power | NAV-SAT fit | The existing C/N₀-vs-elevation gate. Lower-only. |
| `agc` | RF environment | MON-RF | The jamming classification as a state: collapse or corroborated departure is unassured; any single sign, or an antenna fault, is inconsistent. Lower-only. |
| `receiver_spoofing` | receiver verdict | status | The receiver's own spoofing flag. Lower-only. |

Duplicate and out-of-order epochs (by GPS time of week, continuous across the week
rollover) are ignored; a receiver-time gap over 10 s, a lost fix, a receiver restart or an
observer restart resets the affected history. The `profile.go` comments say which defaults
follow an upstream monitor and why some differ.

### Profiles and the configuration hash

`Profile` holds every operating point; `DefaultProfile` returns the standard values.
`StationProfile` is the installation: fixed or mobile, a surveyed position for a fixed
station, a maximum speed for a mobile one. `ConfigHash` digests the engine version, each
check's version, the profile and the station profile, so a served state or event names
exactly what produced it.

---

## Tests

```sh
go test ./internal/integrity/
```

The tracker tests cover warm-up, outlier rejection, mixed degradation, immediate
degradation, the recovery hold and its interruption, the worst-candidate rule, gaps, and
staleness. The fusion table covers every domain rule. The profile tests cover validation
and that every hashed input changes the hash while map order does not.

---

## See also

- `../detect/README.md` — the event machines that report confirmed station changes.
- `../state/README.md` — where station inputs are collected.
- `../../../docs/DEFENSE-PNT.md` — the station PNT defense this extends.
