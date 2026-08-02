# `gnss/iono` — broadcast ionospheric models and the dual-frequency measurement

**Headline:** two very different jobs live in one package. One half evaluates the *broadcast*
ionosphere models — what the constellation says the delay should be. The other half computes the
*measured* delay from a dual-frequency receiver's own observations. The gap between them is an
integrity signal, which is the whole reason both are here.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `iono.go` | Broadcast models: the full GPS/QZSS Klobuchar implementation, frequency scaling, and the coefficient carriers for Galileo NeQuick-G and BeiDou BDGIM. |
| `geomfree.go` | The dual-frequency geometry-free measurement: γ, code and carrier-leveled slant delay, the tracking-`Arc` leveler, thin-shell obliquity, and TECU conversion. |
| `iono_test.go` | Klobuchar behavior including a published truth vector, frequency scaling, NeQuick effective ionisation. |
| `geomfree_test.go` | Exact recovery from a synthetic pair, bias sign, arc-leveling noise reduction, maturity/reset, obliquity, TECU conversion, and γ consistency. |
| `README.md` | This file. |

**Zero dependencies** — not even the root `gnss` package or `physconst`. The speed of light is
even re-declared locally in the test rather than importing `physconst`, specifically to keep this
package dependency-free.

---

## Summary

**Broadcast side (`iono.go`).** GPS and QZSS broadcast eight Klobuchar coefficients (α₀–α₃,
β₀–β₃) describing a half-cosine diurnal model. That's implemented in full and returns L1
group delay in seconds. Galileo's NeQuick-G and BeiDou's BDGIM are represented by their
coefficient structs — NeQuick-G's driving scalar is computed, `BDGIM` is a bare carrier — but the
full profile integration and spherical-harmonic evaluation are documented follow-ups, not shipped
math. BeiDou B1I's own distinct Klobuchar-shaped model is also a follow-up. This is stated
plainly rather than half-implemented — see "What's deliberately not here."

**Measured side (`geomfree.go`).** A receiver tracking two frequencies of one satellite measures
the actual first-order slant ionospheric delay on that line of sight, because orbit, clocks, and
troposphere are common to both signals and cancel in the difference. Code (pseudorange) gives an
unbiased but noisy estimate; carrier phase gives a smooth one carrying an unknown per-arc
ambiguity. The classic answer — implemented here — is to level the phase to the code over a
continuous tracking arc.

**Units convention:** broadcast-side delay is **seconds** of L1-equivalent group delay.
Measured-side slant delay is **metres**. That's not sloppiness — it matches how each side is
consumed — but it does mean you should read the signature before assuming.

---

## Details — the broadcast side

### `Klobuchar`

```go
func Klobuchar(alpha, beta [4]float64, userLat, userLon, az, el, gpsTOW float64) float64
```

Follows IS-GPS-200N §20.3.3.5.2.5 (`docs/MATH.md §7.1`). Latitudes, longitudes, and elevation
work in **semicircles** internally; azimuth stays in **radians**. Mixing those up is the classic
Klobuchar bug, so the conversion happens once at the top and the units are named in the code.

The sequence: earth-centred angle ψ → ionospheric pierce point latitude φ_I (clamped to ±0.416
semicircles) → IPP longitude λ_I → geomagnetic latitude φ_M → local time at the IPP (mod 86400)
→ amplitude and period as cubics in φ_M (amplitude floored at 0, period floored at 72000 s) →
the half-cosine phase term → the obliquity factor F = 1 + 16(0.53 − E)³, with E the elevation
**in semicircles**.

Outside |x| < 1.57 the model returns just the 5 ns night-time floor times obliquity.

**The negative-elevation clamp** : the model is defined for non-negative elevation. At an
elevation of −0.11π rad (−19.8°, i.e. −0.11 semicircles, where the `e + 0.11` denominator
vanishes) the earth-centred-angle term divides by zero outright, and any negative elevation gives
an out-of-validity obliquity. `geo.AzEl` can legitimately produce a negative elevation,
so the input is **clamped, not rejected** — matching this package's no-error guard style. A
below-horizon SV is a caller bug elsewhere; it's not something a pure math function should panic
or error over.

**Scope:** GPS L1 and QZSS only. BeiDou B1I broadcasts a materially different model
(BDS-SIS-B1I-3.0 §5.2.4.7) and must not be run through this function — even though its α/β
coefficients are decoded and carried on `frame.BeiDouSubframe`.

### `ScaleDelay` and the carrier frequencies

```go
L1Hz = 1575.42e6
L2Hz = 1227.60e6
L5Hz = 1176.45e6
func ScaleDelay(delayL1, fHz float64) float64  // × (f_L1/f)²
```

Ionospheric delay is dispersive and scales as (f_L1/f)². `ScaleDelay` converts an L1 delay to any
other carrier.

### `NeQuickG` and `BDGIM` — coefficient carriers

```go
type NeQuickG struct{ A0, A1, A2 float64 }
func (n NeQuickG) EffectiveIonisation(modipDeg float64) float64  // Az = a0 + a1·MODIP + a2·MODIP²

type BDGIM struct{ Alpha [9]float64 }
```

`EffectiveIonisation` computes Az in solar flux units — the ionisation level that *drives* the
NeQuick-G profile — from the modified dip latitude. That much is real. The profile integration
itself is not implemented, and neither is BDGIM's spherical-harmonic evaluation; `BDGIM` is
purely a typed carrier for the nine decoded coefficients.

### What's deliberately not here

Being explicit beats a half-model that quietly returns plausible numbers:

| Model | Status |
|---|---|
| GPS/QZSS Klobuchar | **Implemented in full.** |
| Galileo NeQuick-G | Coefficients decoded; `EffectiveIonisation` implemented; **profile integration is a documented follow-up.** |
| BeiDou B1I Klobuchar-shaped model | **Not implemented** — coefficients are decoded in `gnss/frame` (D1 subframe 1) but there is no evaluator. |
| BeiDou BDGIM | Coefficients decoded and carried; **spherical-harmonic evaluation is a documented follow-up.** |
| NavIC | **Not implemented** — no verified model, matching the deferred NavIC decoder. |

The F/NAV decoder in `gnss/frame` leaves Galileo's ai0/ai1/ai2 undecoded for the same honest
reason: there's no NeQuick model here to consume them yet.

---

## Details — the measured side

### `Gamma` and `SlantFromCode`

```go
func Gamma(f1Hz, f2Hz float64) float64  // (f1/f2)²
func SlantFromCode(p1M, p2M, f1Hz, f2Hz, biasM float64) (float64, bool)
```

Î₁ = (P₂ − P₁ − bias)/(γ − 1), in metres at f1. Pass `biasM = 0` to get the biased (relative)
measurement.

**Satellite and receiver differential code biases are the caller's to remove** — broadcast
TGD/BGD for the satellite, a per-receiver calibration for the receiver (`docs/MATH.md §7.4`).
This package will not guess them.

`ok = false` on a **same-frequency pair** or any non-finite result. That guard is not
hypothetical: γ − 1 is exactly 0 for a same-frequency pair, giving ±Inf, and an ±Inf
`iono_delay_m` is the regression fix feed-freeze generator. regression fix gated `Arc.Slant`; regression fix closed the same
hole in the obvious code-combination entry point.

### `Arc` — carrier-leveling

```go
type Arc struct{ /* unexported: n, meanDiff */ }
func (a *Arc) Add(pGFm, phiGFm float64)                              // one epoch's pair
func (a *Arc) Reset()                                                // loss of lock / cycle slip
func (a *Arc) Count() int
func (a *Arc) Slant(phiGFm, f1Hz, f2Hz, biasM float64) (float64, bool)
const MinArc = 10
```

Feed one (code, phase) geometry-free pair per epoch: `pGF = P₂ − P₁` and `phiGF = Φ₁ − Φ₂`, both
in metres, both equal to I₁·(γ−1) plus their respective biases. `Add` maintains a running mean of
(P_GF − Φ_GF), which *is* the ambiguity estimate. `Slant` then forms (Φ_GF + running mean −
bias)/(γ − 1) — the same conversion `SlantFromCode` does, but over a leveled numerator — so its
noise falls as the arc grows instead of tracking the code noise epoch by epoch.

**Arc continuity is the caller's responsibility.** Do not add samples across a loss of lock or a
cycle slip; call `Reset` first. Upstream, the receiver's lock-time counter going backwards is the
reset signal. `Slant` returns `ok = false` below `MinArc` (10 epochs), because until then the
ambiguity estimate is still code-noise dominated and the smooth phase buys nothing.

The same-frequency guard in `Slant` is defense-in-depth : callers are expected to reject
same-carrier pairs before they get here, but this is the second gate for any future caller that
doesn't.

### Thin-shell mapping and TEC

```go
const ShellHeightM = 350e3  // conventional single-layer ionosphere height
func Obliquity(elRad float64) float64          // M(E) ≥ 1
func TECUToMetres(fHz float64) float64         // 40.308e16 / f²
func VTEC(slantM, fHz, elRad float64) float64  // slant → vertical TEC, TECU
```

The standard single-layer model: the ray pierces a thin shell at 350 km, and sin χ =
R_E/(R_E+h)·cos E gives the obliquity M = 1/cos χ. Vertical = slant / M. R_E is the unexported
`earthRadiusM = 6371 km` mean Earth radius — only `ShellHeightM` is exported.

`TECUToMetres` is the dispersive constant — 0.162 m/TECU at GPS L1, which is the number worth
memorizing as a sanity check.

`Obliquity` takes a *physical* elevation angle. Callers should exclude below-horizon observations
before using the resulting mapping in a measurement feed; unlike `Klobuchar`, this one does not
clamp.

---

## Tests

**Broadcast side (`iono_test.go`):**

| Test | What it pins |
|---|---|
| `TestKlobucharPublishedVector` | The one that matters most — the classic worked example from Klobuchar's own chapter (Parkinson & Spilker Vol. I ch. 12: 40°N, 100°W, az 210°, el 20°, 2000-01-01 20:45 UTC) must produce 23.784 m of slant L1 delay within 20 mm. The expected value is the one Orekit's `KlobucharModelTest` pins to ±1 mm — an independent implementation, so this is an external oracle and a structural error fails by metres. |
| `TestKlobucharZenithPlausible` | Zenith delay lands in a physically sane range. |
| `TestKlobucharLowElevationLarger` | Low elevation gives more delay than high — the obliquity factor working in the right direction. |
| `TestKlobucharNightFloor` | With zero coefficients the amplitude term vanishes and only the 5 ns floor survives, checked at zenith where F ≈ 1 (inside the cosine window, x ≈ −0.63). |
| `TestKlobucharOutsideWindowFloor` | The \|x\| ≥ 1.57 branch: with a nonzero amplitude, deep-night local time (02:00, x ≈ −3.77) returns exactly F·5 ns, while 14:00 local exceeds the floor — the two branches provably differ. |
| `TestKlobucharNegativeElevationGuarded` | negative elevation is clamped, not divided by zero. |
| `TestScaleDelay` | (f_L1/f)² scaling. |
| `TestNeQuickEffectiveIonisation` | Az = a0 + a1·MODIP + a2·MODIP². |

**Measured side (`geomfree_test.go`):**

| Test | What it pins |
|---|---|
| `TestSlantFromCodeRecoversExactly` | A synthetic pair built from a known I₁ round-trips exactly; also the regression fix same-frequency rejection. |
| `TestSlantFromCodeBiasSign` | The bias enters with the correct sign — an easy thing to get backwards. |
| `TestArcLevelingBeatsCodeNoise` | After 300 epochs of 0.5 m code noise the leveled slant lands within 0.20 m of truth — the noise reduction that is the entire justification for `Arc` existing. It is an absolute-error gate, not a head-to-head comparison against the single-epoch code estimate. |
| `TestArcImmatureAndReset` | `MinArc` gating and `Reset` behavior. |
| `TestObliquity` | The thin-shell mapping factor at known elevations, and monotonic from 85° down to 5°. |
| `TestTECUConversion` | 40.308e16/f², cross-checked at L1, plus a `VTEC` round trip at zenith. |
| `TestGammaAgainstScaleDelay` | γ and `ScaleDelay` agree — they're the same physics from two directions. |

Run with `go test ./iono/` from `gnss/`.

---

## Sources

`docs/MATH.md §7` (§7.1 Klobuchar, §7.2 NeQuick-G, §7.4 geometry-free). Primary ICDs:

- **IS-GPS-200N §20.3.3.5.2.5** — Klobuchar.
- **GAL-OS-SIS-ICD-2.2** — the NeQuick-G effective-ionisation coefficients.
- **BDS-SIS-B1I-3.0 §5.2.4.7** — BeiDou's distinct B1I model (not implemented here).
- **BDS-SIS-B1C-1.0 §7.8** (identically BDS-SIS-B2a-1.0 §7.8) — BDGIM coefficients.

See `reference/REFERENCES.md`.
