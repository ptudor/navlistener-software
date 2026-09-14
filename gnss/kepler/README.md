# `gnss/kepler` — the generic Keplerian ECEF propagator

**Headline:** one propagator, five constellations. GPS, Galileo, BeiDou (MEO/IGSO), QZSS, and
NavIC all broadcast the same Keplerian element set and all resolve to ECEF through the same
algorithm — the only things that vary are the physical constants (from `physconst`) and one
genuine exception, the BeiDou GEO rotation. This package is that algorithm, plus velocity and
predicted Doppler on top of it.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `kepler.go` | `Ephemeris`, `Solution`, `Solve`, `Propagate`, the BeiDou-GEO branch, `Velocity`, `PredictedDoppler`, and the input guards. |
| `kepler_test.go` | Analytic checks, realistic-orbit property tests, the three BeiDou-GEO tests, the error-guard matrix, determinism, and Doppler plausibility. |
| `README.md` | This file. |

Imports: the root `gnss` package (for `ECEF`/`GNSSID`), `gnsstime` (for `EphAge` and
`HalfWeek`), and `physconst` (for μ, ωe, and c). Nothing else — no I/O, no logging, no mutable
state. The datum in `physconst.Params` is never read here; propagation is pure ECEF.

---

## Summary

Feed it a decoded broadcast ephemeris and a time-of-week; get back an ECEF position in metres.
That's the contract. Everything else in the package is either a convenience wrapper around it
(`Propagate`, `Velocity`, `PredictedDoppler`) or a guard that keeps a bad input from becoming a
plausible-looking wrong answer.

The library-wide rule matters most here: **this package returns errors, never NaN.** A NaN
position doesn't crash anything — it propagates into the live feed, freezes a map marker, and
poisons an integrity threshold three layers away with no stack trace. So every degenerate input
gets a sentinel error at the boundary, and the final result is checked for finiteness before it's
allowed to leave.

---

## Details

### `Ephemeris` — the input

One decoded broadcast ephemeris, with elements **already scaled to SI** (radians, seconds,
metres). The frame decoders in `gnss/frame` fill it; this package consumes it.

```go
type Ephemeris struct {
    ID   gnss.GNSSID // selects the physical constants
    SVID int         // PRN within the constellation (BeiDou GEO detection)

    SqrtA     float64 // √A at reference, √metres
    ADot      float64 // Ȧ, m/s — CNAV/B-CNAV family; 0 for LNAV/D1/I/NAV
    Ecc       float64 // eccentricity
    M0        float64 // mean anomaly at reference, rad
    DeltaN    float64 // Δn₀, rad/s
    DeltaNDot float64 // Δṅ₀, rad/s² — CNAV/B-CNAV family; else 0
    I0        float64 // inclination at reference, rad
    IDot      float64 // IDOT, rad/s
    Omega0    float64 // longitude of ascending node, rad
    OmegaDot  float64 // Ω̇, rad/s
    Omega     float64 // argument of perigee, rad
    Cuc, Cus  float64 // argument-of-latitude harmonic corrections, rad
    Crc, Crs  float64 // radius corrections, m
    Cic, Cis  float64 // inclination corrections, rad
    Toe       float64 // reference time of ephemeris, seconds of week
}
```

`ADot` and `DeltaNDot` are the CNAV-generation additions (GPS/QZSS CNAV, BeiDou B-CNAV2). A
legacy LNAV/D1/I/NAV ephemeris simply leaves them zero, which makes the time-varying terms
vanish — so one struct and one code path serve both message generations without a flag.

### `Solve` and `Propagate` — the algorithm

```go
func Solve(e Ephemeris, tow float64) (Solution, error)
func Propagate(e Ephemeris, tow float64) (gnss.ECEF, error)
```

`Solve` returns the full `Solution{Pos, E, Tk}`; `Propagate` is the position-only convenience
form. The eccentric anomaly `E` is returned deliberately: `gnss/clock` needs it for the
relativistic term Δtr = F·e·√A·sin E, and re-deriving it there would mean solving Kepler's
equation twice at the same epoch and hoping both solves agree.

The sequence follows IS-GPS-200N §20.3.3.4.3.1 Table 20-IV — which every other Kepler-family ICD
reproduces near-verbatim:

1. **A** = (√A)², and n₀ = √(μ/A³) with μ from `physconst`.
2. **tk** = `gnsstime.EphAge(tow, toe)` — half-week corrected, always.
3. **CNAV time-varying terms:** A(tk) = A₀ + Ȧ·tk, and n = n₀ + Δn₀ + ½Δṅ₀·tk — both rate terms
   are zero for legacy messages, and n₀ itself stays computed from A₀, not A(tk).
   (IS-GPS-705J / BDS-SIS-B2a-1.0 Table 7-9.)
4. **M** = M₀ + n·tk.
5. **Kepler's equation** M = E − e·sin E by Newton–Raphson, seeded at E₀ = M, tolerance 1e-12
   rad, 15 iterations max.
6. **ν** (true anomaly) and **Φ** = ν + ω (argument of latitude).
7. **Second-harmonic corrections** at 2Φ: δu, δr, δi from the six C-coefficients.
8. **u, r, i** corrected — r = A(tk)·(1 − e·cos E) + δr, the one place the Ȧ rate reaches the
   geometry; in-plane x′ = r·cos u, y′ = r·sin u.
9. **Node and rotation into ECEF:** Ω = Ω₀ + (Ω̇ − ωe)·tk − ωe·toe, then the standard rotation.

Step 9 is where the BeiDou GEO exception splits off — see below.

### The guards, and why each one exists

Every one of these is a real bug that was found and closed, not speculative hardening. All of the
sentinels are package-private — outside `kepler` they are `error` values to check against `nil`,
not to match with `errors.Is`:

| Guard | Trigger | Why |
|---|---|---|
| `errNoParams` | GLONASS, or a constellation with no `physconst` entry | GLONASS broadcasts a Cartesian state, never Kepler elements. Refuse explicitly rather than integrate garbage. |
| `errBadSemiAxis` | A ≤ 0 or NaN, before **and** after the Ȧ·tk correction | A rate term can drive a valid A₀ negative over a long tk. |
| `errBadEcc` | e < 0, e ≥ `eccMax` (0.25), or NaN | See below. |
| `errNoConverge` | \|dE\| still ≥ 1e-12 after 15 iterations | regression fix. The loop only breaks *early* on convergence; without this check a non-converging ephemeris silently returns the last iterate as if it were a solution. |
| `errNaN` | Any non-finite component of the final position — and, in `PredictedDoppler`, a zero/non-finite range or a non-finite result | The last line of defense on the errors-never-NaN contract. |
| `errHalfWeekStraddle` | `Velocity` called when tk is within 1.0 s of the ±half-week wrap | See `Velocity` below. |
| `errBadFreq` / `errBadReceiver` | `PredictedDoppler` given a non-finite/non-positive frequency or a non-finite receiver ECEF | regression fix. These are public inputs the propagation path never validates, so without the check a NaN sails through to the final multiplication and returns non-finite with `err == nil`. |

**`eccMax = 0.25` deserves the long explanation**. The obvious guard is `e < 1` — that's
the mathematically degenerate bound. But every real GNSS orbit has e < ~0.02–0.03, and even the
outlier — QZSS's QZO, the highest eccentricity any live constellation broadcasts — is only
≈0.075 (`gnss/truth_test.go`), while Newton–Raphson seeded at E₀ = M is only *guaranteed* to
converge for modest eccentricity. A malformed or spoofed ephemeris with e near 1 and M near π
can fail to converge and return a wrong-but-finite position. This library feeds an anti-spoof
integrity monitor that ingests untrusted broadcasts by design, so "plausible but wrong" is the worst
possible failure mode. 0.25 leaves generous room above any legitimate broadcast while staying far
from where convergence gets risky.

### The BeiDou GEO exception

BeiDou's GEO satellites (C01–C05, C59–C63) use a different final rotation
(BDS-SIS-B1I-3.0 §5.2.4.12, `docs/MATH.md §2.1`):

- The node angle is **Ω_GEO = Ω₀ + Ω̇·tk − ωe·toe** — note this is *not* the standard
  (Ω̇ − ωe)·tk form. The two differ by exactly ωe·tk, so using the wrong one puts the node off by
  ~15°/hour of ephemeris age — thousands of kilometres at GEO radius within the first hour.
- The result is then rotated `[X,Y,Z]ᵀ = Rz(ωe·tk)·Rx(−5°)·[X_GK,Y_GK,Z_GK]ᵀ` — **Rx first, Rz
  last.**

Detection is by SV id, never by inclination. Two caveats are recorded in-code and both are
currently latent :

1. The PRN list is an *operational* fact, not an ICD constant. A future GEO outside C01–C05 /
   C59–C63 would silently get MEO math once a D2 decoder lands.
2. The −5° rotation is B1I-sourced, while the B2a ICD defines no GEO branch at all (Table 7-9 is
   MEO/IGSO only). A hypothetical GEO ephemeris arriving via B-CNAV2 would receive a rotation
   that ICD never prescribes. Unreachable today — BDS-3 GEOs don't broadcast B2a.

The planned fix is carrying the broadcast SatType (B2a Table 7-8) into `Ephemeris` to override
the PRN list and alert on disagreement, scheduled with the first D2/B-CNAV1/B-CNAV3 decoder. Read
`docs/MATH.md §2.1 "Provenance"` before relying on a B2a GEO position.

### `Velocity` — central difference, and the wrap it refuses

```go
func Velocity(e Ephemeris, tow float64) (gnss.ECEF, error)
```

Velocity comes from a central finite difference at tow ± 0.5 s, which is accurate to mm/s and a
great deal simpler than the analytic derivative (`docs/MATH.md §2.2`). Since the interval is
exactly 1.0 s, the difference *is* the velocity — no division needed.

The interesting part is what it refuses. Each `Propagate` call re-derives its own half-week-
wrapped tk independently. When tow − toe sits within 0.5 s of ±half-week, **only one of the two
shifted instants crosses the wrap threshold** — so the "velocity" you'd get is the position delta
across a full ~604,800 s jump, not 1 s. That's only reachable with a ~3.5-day-stale ephemeris
(`HalfWeek` = 302,400 s), but the library's contract is to refuse a degenerate result rather than
emit one, so it returns `errHalfWeekStraddle` whenever tk lands within 1.0 s of the wrap
— twice the ±0.5 s difference offset, for margin. `TestVelocityRejectsHalfWeekStraddle` pins it.

### `PredictedDoppler`

```go
func PredictedDoppler(e Ephemeris, tow, freqHz float64, recv gnss.ECEF) (float64, error)
```

f_D = −f·ρ̇/c, with range-rate ρ̇ = v_sv·û along the receiver→SV unit vector, for a **static**
receiver (v_recv = 0). This is the "predicted" half of the delta-Hz integrity signal
(`docs/INTEGRITY.md §3`): compare it against the receiver's measured Doppler and a persistent
disagreement is a spoofing indicator.

The distance check is `dist == 0 || IsNaN || IsInf` rather than a bare `== 0`, because a NaN
distance sails straight past an equality test.

---

## Tests

| Test | What it pins |
|---|---|
| `TestCircularEquatorialAnalytic` | A circular equatorial orbit against a closed-form position — the base-case correctness check. |
| `TestRealisticGPSRadiusAndSpeed` | A realistic GPS ephemeris stays in the right radius window at the right speed. |
| `TestBeiDouGEOBranch` | The GEO path is taken for GEO PRNs and not for MEO ones. |
| `TestBeiDouGEOStationarity` | regression fix. A GEO traces only a small analemma (bounded at 3000 km over an hour here); a flipped `Rz`/`Rx` rotation sign would sweep it ~22 000 km, which the ≥1 km branch-switch assertion above cannot see. |
| `TestBeiDouGEOTiltDirection` | regression fix. The Rx(−5°) rotation tilts the right way — analytically, at tk = 0 with i = 0 and x′ = 0; changing the tilt to +5° flips Z. |
| `TestVelocityRejectsHalfWeekStraddle` | regression fix, above — rejection exactly at toe ± `HalfWeek`, and a sane speed a full day clear of it. |
| `TestErrorGuards` | Zero √A, e = 1.5, a spoof-shaped e = 0.9 at M₀ = π, SBAS, and GLONASS (`errNoParams`) all return errors — while a legitimate e = 0.03 still propagates. |
| `TestDeterministic` | Same input, same output, every time — no map-iteration or float-accumulation nondeterminism. |
| `TestDopplerPlausible` / `TestDopplerRejectsInvalidInputs` | Doppler magnitude is physically sane; bad frequency/receiver inputs error out. |

**The external check that matters most lives one directory up.** `gnss/truth_test.go`
(`TestKeplerTruthVectors`) propagates a real BKG broadcast ephemeris for GPS, Galileo, BeiDou,
and QZSS and compares against the ESA/ESOC MGEX precise orbit (SP3) at the same instant, with
8–12 m tolerances. Those vectors come from entirely outside this codebase, so a swapped harmonic
coefficient (Cus/Cuc, Crc/Crs), a Φ-vs-2Φ argument error, or a δi sign flip — each invisible to
the property tests here — fails there by hundreds of metres to kilometres.

Run with `go test ./kepler/` (and `go test .` for the truth vectors) from `gnss/`.

---

## Sources

`docs/MATH.md §2`, §2.1 (BeiDou GEO), §2.2 (velocity and Doppler). Primary ICDs:

- **IS-GPS-200N §20.3.3.4.3.1, Table 20-IV** — the canonical algorithm.
- **IS-GPS-705J** — the CNAV ΔA/Ȧ/Δṅ₀ parameterization.
- **GAL-OS-SIS-ICD-2.2**, **BDS-SIS-B1I-3.0 §5.2.4.12** (GEO), **BDS-SIS-B2a-1.0 Table 7-9**,
  **QZSS-PNT-006**, **NAVIC-SPS-L5S** — the same algorithm restated per constellation.

See `reference/REFERENCES.md`.
