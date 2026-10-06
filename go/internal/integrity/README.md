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
| `profile.go` | `Profile` (operating points), `StationProfile` (installation), validation, and `ConfigHash`. |
| `*_test.go` | Filter, hold, fusion and profile tests. |
| `README.md` | This file. |

Imports only the standard library. It holds no clocks, goroutines or I/O: every input
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
