# `gnss/glonass` — PZ-90 numerical propagation and the analytic almanac model

**Headline:** GLONASS is the constellation that doesn't play along. It broadcasts a Cartesian
state vector instead of Keplerian elements, works in kilometres instead of metres, uses
sign-magnitude instead of two's complement, runs on a time-of-day instead of a time-of-week, and
takes leap seconds. This package owns all of that — a Runge-Kutta integrator for the immediate
ephemeris, and a separate analytic model for the almanac.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `glonass.go` | The immediate ephemeris: the `Ephemeris` struct, `Propagate`, and the RK4 integrator with the PZ-90 rotating-frame equations of motion. |
| `almanac.go` | The long-term almanac model: the `Almanac` struct, `PropagateAlmanacECEF`, the C20 perturbation formulae, and the fixed-point Kepler solver. |
| `glonass_test.go` | Propagation guards, radius/reversibility/determinism properties, velocity magnitude. |
| `almanac_test.go` | The ICD's own worked numerical example, ground-track and continuity checks, the cycle-boundary wrap. |
| `kepler_convergence_test.go` | The fixed-point solver's convergence domain and its unconverged-rejection behavior. |
| `README.md` | This file. |

Imports the root `gnss` package (for `ECEF`) and `physconst` (for the kilometre constants and
`Pi`). Notably it does **not** import `gnsstime` — the caller computes tk with
`gnsstime.EphAgeDay` and passes it in.

---

## Summary

Two propagators, sharing nothing but a datum:

**`Propagate` — the immediate ephemeris (30-minute validity).** GLONASS broadcasts position,
velocity, and luni-solar acceleration in PZ-90.11 at a reference time tb. To get the position at
another epoch you numerically integrate the equations of motion — gravity, the J₂ oblateness
term, centrifugal, Coriolis, and the broadcast luni-solar acceleration — with classic 4th-order
Runge-Kutta. The integration happens **directly in the PZ-90 rotating frame**, which is why the
centrifugal and Coriolis terms are there and why no inertial↔rotating transform is needed.

**`PropagateAlmanacECEF` — the almanac (long-life, coarse).** A completely different, analytic
model: iterate the semi-major axis against the draconitic period, apply the secular and periodic
perturbations of the C20 zonal harmonic, then solve a perturbed Keplerian orbit. This is what
feeds the all-SV acquisition-grade almanac feed (`docs/OUTPUT.md §1.4`), including GLONASS
satellites currently out of ephemeris view.

Both refuse degenerate input with a typed error rather than returning a NaN, same as the rest of
the library.

---

## Details — the immediate ephemeris (`glonass.go`)

### `Ephemeris`

```go
type Ephemeris struct {
    X, Y, Z    float64 // position at tb, km (PZ-90.11)
    Vx, Vy, Vz float64 // velocity at tb, km/s
    Ax, Ay, Az float64 // luni-solar acceleration, km/s² (≈constant over the span)

    Tb       float64 // reference time, seconds of day
    TodKnown bool    // true once string 2 anchored the time-of-day

    TauN       float64 // τn(tb): SV-time → GLONASS-time correction, s
    GammaN     float64 // γn(tb): relative frequency deviation, dimensionless
    DeltaTauN  float64 // Δτn: L2−L1 group-delay difference, s
    ClockKnown bool    // a same-frame string 4 contributed τn/Δτn

    FreqCh int // FDMA channel k = freqId − 7
    Slot   int // slot number n
}
```

**Units are the ICD's**: kilometres, not metres. `Propagate` multiplies by 1000 on the way out.
Getting this wrong produces a position off by a factor of 1000, which at least fails loudly.

**`TodKnown` is required.** Time-of-day must be anchored before `tb` can be
trusted. `Propagate` returns `errTimeless` until that condition is met.

**`ClockKnown` distinguishes "clockless" from "zero clock."** γn rides string 3 with the rest of
the immediate data so it's always present on an assembled set, but τn and Δτn come from string 4,
which can be lost. An ephemeris that assembled without string 4 is perfectly usable for position
math — it just has no clock terms, and `ClockKnown = false` says so rather than letting a
legitimate τn = 0 be indistinguishable from a missing one.

### `Propagate`

```go
func Propagate(e Ephemeris, tk float64) (gnss.ECEF, error)
```

`tk` is the **signed** interval from tb, in seconds, obtained by the caller via
`gnsstime.EphAgeDay(targetTOD, tb)` — the day-wrapped analogue of the half-week wrap.

Three guards, in order:

1. **`errTimeless`** — `TodKnown` false.
2. **`errTkDomain`** — tk is NaN, Inf, or |tk| > 2 days. This is the GLONASS side of the
   regression fix/regression fix "public inputs bypass validation" class. A huge tk doesn't just give a wrong
   answer: it runs millions of RK4 steps (seconds to CPU-*days* of work) and then returns a
   finite, physically meaningless position with `err == nil`. The GLONASS ephemeris is only valid
   over its ~30-minute fit interval and daemon callers are wrapped to ±43,200 s, so two days is a
   generous ceiling that only catches out-of-domain library misuse.
3. **`errZero`** — the position vector is under 1 km, i.e. effectively zero. An uninitialized or
   all-zeros state would otherwise integrate straight into a division by r³ = 0.

And `errNaN` at the end if the integration somehow produced a non-finite component anyway.

### The integrator

Step size is capped at **60 s** (`maxStep`, per `docs/MATH.md §3`: h = ±30…60 s). `integrate`
computes `n = ceil(|tk|/60)` sub-steps of uniform size `h = tk/n`, so a backward propagation just
uses a negative h — no special case.

`deriv` is the equations of motion, in km:

```
r² = x²+y²+z², ρ² = (aₑ/r)², c = −μ/r³, k = 1.5·J₂·ρ²

ax = c·x·(1 + k(1 − 5z²/r²)) + ωe²·x + 2ωe·vy + jx
ay = c·y·(1 + k(1 − 5z²/r²)) + ωe²·y − 2ωe·vx + jy
az = c·z·(1 + k(3 − 5z²/r²))                    + jz
```

Three things to notice. The **z equation has a 3, not a 1**, in the J₂ bracket — that's the
oblateness term's structure, not a typo. The **ωe² terms are centrifugal** and the **±2ωe terms
are Coriolis**, present because we integrate in the rotating frame; their signs are opposite
between x and y and that asymmetry is correct. And **there is no centrifugal or Coriolis term in
z**, because the rotation axis *is* z.

None of those signs can be caught by a radius-window test or a reversibility test — flip the
Coriolis sign and the orbit still has the right radius and still reverses cleanly. That's exactly
why the external truth vector one directory up exists (see Tests).

---

## Details — the almanac (`almanac.go`)

### `Almanac`

```go
type Almanac struct {
    NA        int     // calendar day number within the four-year interval
    Lambda    float64 // GREENWICH longitude of ascending node at tλ, rad
    Tlambda   float64 // first ascending-node passage within day NA, s
    DeltaI    float64 // correction to the mean inclination (i_avg = 63°), rad
    DeltaT    float64 // correction to the mean Draconian period (T_avg = 43200 s), s
    DeltaTdot float64 // rate of change of the Draconian period, s per period
    Ecc       float64 // eccentricity
    Omega     float64 // argument of perigee, rad
    Slot      int
    FreqCh    int
}
```

Authored from **GLONASS ICD Ed. 5.1 (2008), Appendix 3, §A.3.2.2** — "Algorithm of calculation of
satellite motion parameters using almanac" — and validated against that appendix's own worked
numerical example in §A.3.2.3.

### The frame subtlety — "the F0 bug"

This is the part to read before changing anything (`docs/MATH.md §3.1`).

The almanac's node longitude λ is a **Greenwich** longitude. So the PZ-90 ECEF node at time t is:

**Ω = λ − ωe·(t − tλk) + δΩ**

Earth rotation folded in directly, with **no sidereal-time term**. Expressing it this way makes
the result ECEF with no astronomical-almanac input at all, and it passes a multi-time-of-day
ground-track check that a single-instant test cannot — which is precisely the trap §3.1 warns
about. A single-epoch test will happily pass with a wrong frame convention.

The package keeps both conventions internally via `nodeConvention`: `ecefNode` (the rotating
Greenwich frame, what `PropagateAlmanacECEF` uses) and `inertialNode` (the ICD's absolute OXaYaZa
frame with s = S₀ + ωe(tλk − 3h), used only by the §A.3.2.3 example test). Having both is what
lets one test compare against the ICD's published numbers while the shipped path stays ECEF.

### Two wraps and a velocity correction, each from a real bug

**regression fix — the four-year cycle boundary.** GLONASS day numbers NA/NT run 1..1461 within a four-year
interval (3×365 + 366). An almanac referenced to NA = 1461 but evaluated on day 1 of the next
cycle has a true age of about one day, not −1460. Without wrapping `(n0 − NA)` across the
1461-day boundary, that term injects thousands of spurious orbital periods and the ΔṪ·w² term
alone diverges.

**regression fix — τ must carry the day count.** The elapsed time since the k-th node passage is
`tau = tStar − dtNode`, **not** `ti − tLambdaK`. `tLambdaK` is wrapped mod 86400, so it discards
which calendar day the passage fell on; whenever the containing passage was on the previous day
relative to ti, `ti − tLambdaK` comes out ~86400 s wrong, corrupting the mean anomaly, the node
longitude, and every perturbation argument by a full period's worth of along-track motion.
`tLambdaK` is still kept — but only for the `inertialNode` sidereal term S(tλk), which the ICD's
own worked example explicitly defines against the wrapped time-of-day.

**regression fix — the ECEF velocity needs the frame-rotation term.** For `ecefNode`, Ω folds in Earth
rotation (dΩ/dt = −ωe), so the *position* is already correct ECEF. But the velocity computed from
the orbital elements is only Rz(Ω)·d(r_orbital)/dt — the orbital velocity as seen in a frame with
Ω momentarily frozen. It misses d/dt[Rz(Ω(t))]·r_orbital = (dΩ/dt)·(ẑ×r). Adding that back is
`vel.X += ωe·pos.Y; vel.Y −= ωe·pos.X`, which is a **~1.9 km/s correction** at GLONASS altitude
— not a rounding detail. `inertialNode` has no such t-dependence and is untouched.

### `solveKeplerEcc` and its residual check

Fixed-point iteration E = M + e·sin E, 20 iterations, 1e-12 stopping delta — tighter than the
ICD's 1e-8 rad requirement.

The interesting part is **why it returns an error instead of the last iterate**.
Broadcast almanacs can't reach the failure state: εnA is a 15-bit field at 2⁻²⁰, so decoded e ≤
~0.031 and fixed-point iteration converges at rate ≈ e in well under 10 steps. The exposure is
the *exported API* — `propagateAlmanac` accepts any e ∈ [0,1), and above e ≈ 0.75 twenty
iterations from a cold start can't reach 1e-12, which used to return a finite, plausible-looking,
silently wrong anomaly. A plausible-but-wrong number out of the reusable math library is the
worst failure class in this codebase.

`keplerResidualTol = 1e-9` is explicitly **not a spec value**. It's a systems threshold chosen
between the two numbers that *are* pinned: the 1e-12 stopping delta below it and the ICD's 1e-8
rad accuracy requirement above it. A converged solve's residual is bounded by e·|Eₙ₊₁ − Eₙ| <
1e-12, so 1e-9 rejects only genuinely unconverged results with four orders of margin against
float noise. At GLONASS radius, 1e-9 rad is ~2.5e-5 m of along-track position.

Checking the *residual* rather than the iteration count is deliberate: it validates the answer,
not the method, so a future switch to Newton or Halley inherits the guarantee for free.

Note also that the convergence guard applies to `epsI` — the **perturbed** eccentricity
|(h+δh, l+δl)| — not the broadcast εnA, because that's the value actually being solved for.

---

## Tests

**Immediate ephemeris (`glonass_test.go`):**

| Test | What it pins |
|---|---|
| `TestPropagateZeroReturnsState` | tk = 0 returns the input state unchanged. |
| `TestRadiusSaneOverSpan` | The orbit stays on-shell (~25,510 km) across the propagation span. |
| `TestReversibility` | Forward then backward returns to the start — the integrator's self-consistency. |
| `TestGuardTimeless` / `TestGuardZeroState` / `TestGuardTkDomain` | The three refusals, including domain ceiling. |
| `TestDeterministic` | Same input, same output. |
| `TestVelocityMagnitude` | Orbital speed is physically right. |

**Almanac (`almanac_test.go`, `kepler_convergence_test.go`):**

| Test | What it pins |
|---|---|
| `TestAlmanacICDExample` | **The anchor.** The ICD §A.3.2.3 worked example — NA 615, ti 33300 s — must reproduce X/Y/Z = 10947.021572 / 13078.978287 / 18922.063362 km within 2 m and the velocity within 10 mm/s. The Appendix 3 coefficients actually land within ~0.14 m and ~0.7 mm/s, far tighter than the almanac's own acquisition-grade accuracy. |
| `TestAlmanacECEFGroundTrack` | The multi-time-of-day check the F0 bug fails and a single-instant test would pass. |
| `TestAlmanacECEFMatchesRotatedICDExample` | The ECEF path agrees with the inertial ICD example once rotated — the two conventions are consistent. |
| `TestAlmanacECEFVelocityMatchesFiniteDifference` | the analytic ECEF velocity matches a finite difference of the position, which it would not without the ωe×r term. |
| `TestAlmanacCycleBoundaryWrap` / `TestPropagateAlmanacDayCountInvariant` / `TestPropagateAlmanacMidnightContinuity` | regression fix and regression fix — day counting across the cycle boundary and across midnight. |
| `TestAlmanacTargetDayIsN0` | The target day is n0, not NA. |
| `TestSolveKeplerBroadcastDomainConverges` / `TestSolveKeplerRejectsUnconverged` / `TestSolveKeplerInvariant` / `TestPropagateAlmanacRejectsUnconvergedKepler` | regression fix — converges over the whole broadcast domain, errors rather than lying outside it. |

**The external check lives one directory up.** `gnss/truth_test.go`'s `TestGLONASSTruthVector`
integrates a real R09 PZ-90 state vector (tb 12:15:00 UTC) forward to a 12:30:00 GPST SP3 epoch
and requires agreement with the ESA/ESOC precise orbit within 12 m. That is what externally pins
the J₂ term and the centrifugal/Coriolis signs — the ones reversibility and radius-window tests
structurally cannot distinguish from their flipped variants.

Run with `go test ./glonass/` (and `go test .` for the truth vector) from `gnss/`.

---

## Sources

`docs/MATH.md §3` (numerical propagation) and §3.1 (the almanac and the F0 frame bug).

- **GLO-ICD-5.1** — §4 (the nav message), the Appendix algorithm J.1/J.2 (equations of motion),
  Appendix 3 §A.3.2.1–§A.3.2.3 (almanac elements, the algorithm, and the worked example),
  Table 4.5 (τn/γn/Δτn), Table 4.8 (almanac accuracy), Tables 4.9/4.10/4.11 (almanac coding).
- Constants come from `gnss/physconst` (`GloMuKm`, `GloAeKm`, `GloJ2`, `GloOmegaE`); the almanac
  file carries its own ICD-§A.3.2.2 constant block in the ICD's units.

The bit-level decode of GLONASS strings and almanac pairs is `gnss/frame/glonass_string.go` —
see that package's README. See `reference/REFERENCES.md` for the ICD itself (local-only; no
redistribution grant stated).
