# `gnss/gnsstime` — GNSS time systems, week arithmetic, and the half-week wrap

**Headline:** every constellation runs its own leap-free clock with its own epoch and its own
week numbering, and all of them wrap. This package owns the arithmetic that makes those clocks
comparable — most importantly the ±half-week correction that shows up in literally every
propagation and clock evaluation in the library.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `gnsstime.go` | The whole package: week constants, the `System` enum, the ephemeris-age wraps, `GNSSTime` and its conversions, the wall-clock↔GNSS-axis reductions, and week-rollover disambiguation. |
| `gnsstime_test.go` | Epoch-constant pins, wrap behavior in both directions, known-value week/TOW checks, and the GLONASS rejection cases. |
| `README.md` | This file. |

This package has **zero dependencies** — not even the root `gnss` package. It's pure arithmetic
over `float64` and `int`, which is what lets `kepler`, `clock`, and `frame` all import it without
any risk of a cycle.

---

## Summary

Time in GNSS is deceptively hard. Each system defines a continuous, leap-free scale steered close
to UTC:

| System | Scale | Epoch | Relationship |
|---|---|---|---|
| GPS | GPST | 1980-01-06 | the reference axis for everything here |
| QZSS | (GPST) | — | identical to GPST; no separate `System` constant |
| Galileo | GST | 1999-08-21T23:59:47 UTC | GST week = GPS week − 1024, TOW aligned |
| BeiDou | BDT | 2006-01-01 | BDT = GPST − 14 s |
| NavIC | IRNWT | 1999-08-22 (Galileo convention) | same epoch/alignment as GST |
| GLONASS | GLONASST | — | UTC(SU)+3h, **leap-stepped**, no week, no TOW |

GLONASS is the odd one out and is deliberately refused by every function on the continuous-week
axis. It has a time-of-day, not a time-of-week, and it takes leap seconds — so `GPSSeconds`,
`ToUnix`, `SystemSeconds`, `WeekAt`, and `TOWAt` all return `ok=false` for `SysGLONASS` rather
than quietly producing a number. Its day-wrapped analogue, `EphAgeDay`, is right here in this
package; the rest of the GLONASS time story lives in `gnss/glonass` and the daemon's `gloTOD`.

This package **transcribes** offsets — it does not invent them. The GPS−UTC leap count is a
parameter on every function that needs it, never a compiled-in literal.

---

## Details

### The constants

```go
WeekSeconds = 604800.0  // seconds in a GNSS week
HalfWeek    = 302400.0  // the ephemeris-age wrap threshold
DaySeconds  = 86400.0   // the GLONASS wrap threshold
```

Two unexported ones anchor the epoch table: `gpsEpochUnix = 315964800.0` (1980-01-06T00:00:00Z)
and `gpsTAIminusUTC = 19.0` (TAI−UTC at the GPS epoch). Because GPS−UTC = (TAI−UTC) − 19, the
broadcast leap count ΔtLS — currently 18 — *is* exactly GPS−UTC. That identity is why a single
`gpsMinusUTC` parameter threads through the whole package.

### `EphAge` — the function you will use most

```go
func EphAge(tow, ref float64) float64
```

Returns tk = tow − ref, corrected by ±one week when the difference exceeds half a week. `ref` is
an ephemeris reference epoch (toe) or a clock reference (toc), both in seconds-of-week.

This correction is **mandatory**, not defensive. A measurement taken just after a week rollover
against a reference from just before it produces a raw difference of ~604,800 s; without the wrap
you'd propagate an orbit a full week forward and get a position that is wrong by the entire
orbit. Apply this correction consistently, in `kepler.Solve`, `clock.Offset`, `clock.UTCOffset`, and
`kepler.Velocity`'s straddle guard.

Companions:

- **`EphAgeDay(tod, tb)`** — the GLONASS version, wrapping around the 86,400 s day instead,
  because GLONASS broadcasts a time-of-day and nothing wider.
- **`EphAgeMinutes(tow, ref)`** — `EphAge` divided by 60, which is the `eph_age_m` field the feeds
  serve (`docs/OUTPUT.md §1.1`).
- **`SOWDelta(a, b int) int`** — the integer analogue, for frame-adjacency and staleness checks
  that subtract two raw seconds-of-week fields. Every such rule *must* use this. Without it, the
  one legitimate BeiDou D1 subframe set per week that straddles the rollover (604794 → 0 → 6)
  gets rejected as a splice, once a week, forever. `gnss/frame`'s internal `sowDelta`
  wraps this with a domain check so an out-of-range SOW from a corrupt frame fails instead of
  being normalized into an apparently-adjacent delta.

### `GNSSTime` and the conversions

```go
type GNSSTime struct {
    Sys  System
    Week int      // the FULL, disambiguated week — not a truncated broadcast field
    TOW  float64
}
func (t GNSSTime) GPSSeconds() (float64, bool)
func (t GNSSTime) ToUnix(gpsMinusUTC float64) (float64, bool)
```

Everything reduces to continuous GPS seconds first, via a single `epochGPSSeconds` table that
folds each system's epoch offset into one constant. That's why one leap offset applies to all of
them and why there's exactly one place to audit when an epoch is questioned.

The Galileo entry carries a correction worth understanding before you touch it. GST(0,0)
is 1999-08-21T23:59:47 UTC — **thirteen seconds before** the nominal 1999-08-22T00:00:00Z date,
because GST was already 13 s ahead of UTC at that instant. GST(0,0) is exactly the GPS week-1024
rollover, so GST week/TOW maps to GPS week/TOW with zero offset: the epoch constant is
`935280000 − gpsEpochUnix` (= 1024 × 604800) with **no** additional leap term. An earlier version
added `(32 − 19)`, which treated 935280000 as a UTC timestamp of GST(0,0) itself rather than of
the calendar date 13 s after it. NavIC's IRNWT shares the same epoch and convention and got the
same fix.

BeiDou's entry folds its 14 s offset in directly: `(1136073600 − gpsEpochUnix) + (33 − 19)`.

### The wall-clock reductions

```go
func SystemSeconds(sys System, unix, gpsMinusUTC float64) (float64, bool)
func WeekAt(sys System, unix, gpsMinusUTC float64) (int, bool)
func TOWAt(sys System, unix, gpsMinusUTC float64) (float64, bool)
```

`SystemSeconds` is the single wall-clock → GNSS-axis reduction; `WeekAt` and `TOWAt` are built on
it. Keeping it that way means every per-system epoch subtlety above lives in exactly
one place instead of being re-derived at three call sites with three chances to get it wrong.

`WeekAt` gives the "expected week" side of a broadcast-vs-receiver cross-check — pair it with
`DisambiguateWeek` on the broadcast side, same `sys`, and a disagreement is the daemon's
`wn_mismatch` signal. It works on each system's *own* numbering (GST week = GPS week −
1024), not a hardcoded GPS axis.

`TOWAt` uses a positive modulo, so an instant just before a week boundary still lands in the
previous week's tail rather than going negative.

### `DisambiguateWeek` — recovering a full week from a truncated field

```go
func DisambiguateWeek(sys System, truncated, bits int, approxUnix, gpsMinusUTC float64) int
```

LNAV sends a 10-bit GPS week (a 1024-week ambiguity, which is roughly 19.6 years — GPS has
already rolled over twice); other message types send wider fields. Given the truncated value, its
width, an approximate current time, and the leap offset, this returns the full week nearest that
instant.

The design rule: **the wall clock is trusted only to pick the rollover cycle, never the low
bits.** We always know roughly what year it is; we do not assume we know the week. The leap term
only participates in selecting the cycle (it's divided by 604,800), so even a few seconds of
error there is harmless — but `gpsMinusUTC` is still a parameter rather than a literal 18,
because the daemon's ΔtLS is settable (`[state].leap_seconds`, regression fix) and a hardcoded copy would
silently diverge after a real leap event.

**Fallback behavior, and its trap:** invalid field widths (`bits <= 0` or `bits >= 31`) and time
systems with no continuous week axis return `truncated` unchanged. That makes the helper safe to
call on optional metadata, but a caller that *requires* disambiguation must validate those inputs
itself — the fallback is not a full week, and treating it as one would be a silent 1024-week
error.

---

## Tests

`gnsstime_test.go` covers:

- **Wraps** — `TestEphAgeNoWrap`, `TestEphAgeForwardWrap`, `TestEphAgeBackwardWrap`,
  `TestEphAgeDay`, `TestEphAgeMinutes`, `TestSOWDelta`.
- **Epochs** — `TestEpochConstants`, `TestToUnixGPS`, `TestToUnixBeiDouEpoch`,
  `TestToUnixGalileoEpoch`, and two that pin the regression fix corrections specifically:
  `TestGalileoEpochAlignsWithGPSWeek1024` and `TestNavICEpochMatchesGalileo`.
- **GLONASS rejection** — `TestToUnixGLONASSRejected`, `TestTOWAtGLONASSRejected`.
- **Reductions and rollover** — `TestSystemSecondsWeekTOWIdentity`, `TestWeekTOWKnownValues`,
  `TestDisambiguateWeek`.

Run with `go test ./gnsstime/` from `gnss/`.

---

## Sources

`docs/MATH.md §1` is the primary reference and cites: **IS-GPS-200N** (GPS week/TOW, the 10-bit
LNAV WN, the HOW TOW count), **GAL-OS-SIS-ICD-2.2** (GST epoch and week alignment),
**BDS-SIS-B1I-3.0** (BDT epoch and the 14 s offset), **GLO-ICD-5.1** (the time-of-day system and
UTC(SU)+3h), **NAVIC-SPS-L5S** (IRNWT). See `reference/REFERENCES.md`.
