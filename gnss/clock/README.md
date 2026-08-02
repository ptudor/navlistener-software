# `gnss/clock` — satellite clock correction, relativity, group delay, and GNSS→UTC

**Headline:** the orbit tells you where a satellite is; the clock tells you when it thought it
was. This package turns the broadcast clock polynomial, the relativistic periodic term, and the
group-delay bias into a single Δtsv in seconds — and decodes the broadcast system→UTC offset
alongside it.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `clock.go` | `Model`, `Relativistic`, `Offset`, `OffsetFor`, the two group-delay scaling constants, `UTCParams`, and `UTCOffset`. |
| `clock_test.go` | Polynomial and relativity behavior, the E-reuse check, error propagation, the time-disco shape, UTC offset including the A2 term, and both group-delay factors. |
| `README.md` | This file. |

Imports the root `gnss` package, `gnsstime` (the half-week wrap), `kepler` (for `OffsetFor`'s
solve), and `physconst` (the relativity constant F).

---

## Summary

The correction is:

**Δtsv = af0 + af1·Δt + af2·Δt² + Δtr − TGD**

where Δt = `gnsstime.EphAge(tow, toc)` — half-week corrected, same as everywhere else — and
Δtr = F·e·√A·sin E is the relativistic periodic term arising from the orbit's eccentricity.

Two design decisions define this package:

1. **The eccentric anomaly E is passed in, not re-derived.** It comes from the `kepler.Solve` at
   the same epoch. Solving Kepler's equation twice for one output would be wasteful and, worse,
   would create two answers that could drift apart under refactoring.
2. **`Model.TGD` is already scaled by the caller.** Nothing in this package applies a
   frequency-scaling factor automatically. That's a deliberate, documented contract, and the
   reason is below.

---

## Details

### `Model` — the input

```go
type Model struct {
    ID  gnss.GNSSID // selects the relativity constant F
    Af0 float64     // clock bias, s
    Af1 float64     // clock drift, s/s
    Af2 float64     // clock drift rate, s/s²
    Toc float64     // clock reference time, seconds of week
    TGD float64     // group delay for the TRACKED signal — already scaled (see below)
}
```

### The TGD contract — read this before using `Model.TGD`

The broadcast group delay is referenced to a specific signal, and a user tracking a *different*
signal must scale it. **This package does not do that for you**. It exposes the factors
and expects the caller — a frame decoder or the daemon — to hand over an already-scaled value:

| Constant | Value | When it applies |
|---|---|---|
| `L2GroupDelayFactor` | γ = (1575.42/1227.60)² ≈ 1.6469 | A single-frequency **GPS L2** user, scaling the L1-referenced broadcast TGD. IS-GPS-200N §20.3.3.3.3.2. |
| `E5aGroupDelayFactor` | (1575.420/1176.450)² ≈ 1.7933 | A single-frequency **Galileo E5a** user of the F/NAV (E1,E5a) clock. GAL-OS-SIS-ICD-2.2 §5.1.5 Eq. 19. |

And the cases where you pass something else:

- **Dual-frequency ionosphere-free users pass `TGD: 0`** — the combination cancels it.
- **A Galileo E1 user of the I/NAV clock passes BGD(E1,E5b) unscaled** — E1 is the f1 signal of
  the (E1,E5b) pair, so Eq. 18 applies with no factor. Note it's BGD(E1,E5b), *not*
  BGD(E1,E5a); getting that wrong was a real bug.
- **A Galileo E5a user of the F/NAV clock passes BGD(E1,E5a) × `E5aGroupDelayFactor`** — E5a is
  the f2 signal, so Eq. 19 applies. `gnss/frame`'s F/NAV decoder is that caller and does the
  scaling at decode time; before it did, the assembled `@3` clock shipped a zero TGD,
  silently biasing it by the whole group delay and arming the future I/NAV-vs-F/NAV comparison
  with a built-in false clock offset.
- **A BeiDou B-CNAV2 user tracking the B2a data component passes TGD_B2ap + ISC_B2ad** — eq. 7-5,
  not eq. 7-4. TGD_B2ap alone is the *pilot* component's correction, wrong for this stream by
  ISC_B2ad, which is ns-scale — the same order as the 2.5 ns time-disco threshold.

In this system it happens that the daemon tracks L1 and its `computeDisco` zeroes TGD on both
sides, so γ never actually enters the GPS path. The constant is still exported, because an
external consumer of this library — the whole point of `gnss` being its own module — absolutely
might be an L2-only user, and one carrying the unscaled broadcast TGD would eat a (γ−1)·TGD ≈
0.65·TGD bias.

### The functions

```go
func Relativistic(relF, ecc, sqrtA, eccAnom float64) float64
func Offset(c Model, tow, ecc, sqrtA, eccAnom float64) float64
func OffsetFor(c Model, e kepler.Ephemeris, tow float64) (float64, error)
```

- **`Relativistic`** is the bare Δtr = F·e·√A·sin E. Zero for a circular orbit (e = 0) and zero
  for GLONASS (F = 0 — the Cartesian model has no such term, and the broadcast γn is already
  referenced to a predicted carrier frequency that accounts for gravitational and relativistic
  effects, GLO-ICD-5.1 §4.4).
- **`Offset`** is the full polynomial. It looks up F from `physconst` by `Model.ID`; an unknown
  constellation yields relF = 0, which degrades to the polynomial-only correction rather than
  erroring — reasonable, since the polynomial is still valid.
- **`OffsetFor`** is the convenience form: it solves the orbit at `tow` to get E, then calls
  `Offset`. The ephemeris and clock model must be for the same SV — nothing here can check that,
  and pairing across SVs would produce a plausible number that is entirely fictional. It returns
  the `kepler.Solve` error unchanged on degenerate orbital input, so the errors-never-NaN
  contract holds through the composition.

### `UTCParams` and `UTCOffset` — the broadcast UTC relationship

```go
type UTCParams struct {
    A0, A1, A2 float64 // constant / rate / drift-rate terms
    Tot        float64 // reference time of the UTC data, seconds of week
    WNot       int     // reference week (0 if the source carries none)
    DtLS       float64 // current (or pre-event) leap-second count
    DtLSF      float64 // leap count after the WNLSF/DN event
    WNLSF      int     // leap event week
    DN         int     // leap event day within WNLSF (BeiDou 0–6; GPS LNAV 1–7)
}
func UTCOffset(u UTCParams, tow float64) float64
```

The A0/A1 two-term shape is IS-GPS-200N LNAV. The CNAV-generation messages — and BeiDou's
B-CNAV2 MT34 (BDS-SIS-B2a-1.0 Table 7-20, regression fix) — add the A2 drift-rate term and carry full
untruncated reference weeks. A two-term source just leaves the extras zero, so one struct serves
both.

`UTCOffset` returns A0 + A1·Δt + A2·Δt² + ΔtLS with Δt half-week corrected. Note the **recorded
and accepted deviation** : the ICD forms the A1 term over the true week-spanning
difference tE − tot + 604800·(WN − WNt), while this convenience function substitutes the
±half-week wrap and carries no WNt. The two are identical within half a week of the reference and
diverge beyond it — but A1 is spec-bounded near 1e-15 s/s, so the divergence stays sub-nanosecond
against a 2.5 ns integrity threshold. Leave-as-is is the disposition, and it's written down here
and in-code so no future review pass re-litigates it.

`UTCParams` now carries WNot/WNLSF/DN, so a caller holding an absolute epoch can evaluate the
exact week-spanning form and handle the leap-transition arm itself. The navlistener feed does
exactly that; this tow-only helper stays on the wrapped axis with the current ΔtLS.

---

## Tests

| Test | What it pins |
|---|---|
| `TestRelativisticZeroForCircular` | Δtr = 0 at e = 0. |
| `TestRelativisticMagnitude` | Δtr lands in the right order of magnitude for a real orbit (1–100 ns) and comes out negative for positive sin E, pinning F < 0. |
| `TestOffsetPolynomial` / `TestOffsetLinearGrowth` | The polynomial evaluates correctly and grows linearly in Δt where af1 dominates. |
| `TestOffsetForReusesSolveE` | `OffsetFor` really does reuse the solve's E rather than re-deriving it. |
| `TestOffsetForPropagatesError` | A degenerate ephemeris surfaces as an error, not a number. |
| `TestTimeDiscoShape` | The difference between two clock models has the shape the integrity detector expects. |
| `TestUTCOffset` / `TestUTCOffsetQuadraticTerm` | The two-term and three-term forms, including A2. |
| `TestL2GroupDelayFactor` / `TestE5aGroupDelayFactor` | Both constants match their (f1/f2)² values — ≈1.6469 and ≈1.7933 — to 1e-3. |

Run with `go test ./clock/` from `gnss/`.

---

## Sources

`docs/MATH.md §4` (and §8 for time-system offsets). Primary ICDs:

- **IS-GPS-200N §20.3.3.3.3.1** (the polynomial and Δtr), **§20.3.3.3.3.2** (γ and TGD),
  **§20.3.3.5.2.4** (UTC).
- **GAL-OS-SIS-ICD-2.2 §5.1.5** Eq. 18/19, **Table 71** (which clock model pairs with which
  message type and service), and **Table 2** (the carrier frequencies behind the E5a factor).
- **BDS-SIS-B2a-1.0 §7.6.2** eq. 7-4/7-5, **Table 7-20** and eq. 7-25 (BDT-UTC with A2).
- **GLO-ICD-5.1 Table 4.5** — τn/γn/Δτn, decoded in `gnss/frame` and carried on
  `glonass.Ephemeris` rather than through this package's `Model`.

See `reference/REFERENCES.md`.
