# `gnss/geo` — ECEF ↔ geodetic, and topocentric azimuth/elevation

**Headline:** the small, sharp package that turns a satellite's ECEF position into something a
human or a map can use — latitude/longitude/height, and the az/el a given ground station sees.
Every function takes the reference ellipsoid explicitly, so the caller has to say which datum it
means.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `geo.go` | `Geodetic`, `GeodeticToECEF`, `ECEFToGeodetic`, `AzEl`, and the `Deg`/`Rad` helpers. |
| `geo_test.go` | Round-trip and pole/equator checks, the three cardinal az/el cases, and the two NaN-guard regressions. |
| `README.md` | This file. |

Imports the root `gnss` package (for `ECEF`) and `physconst` (for `Ellipsoid`). No I/O, no state.

---

## Summary

Four operations, and they compose:

1. **`GeodeticToECEF`** — lat/lon/height on an ellipsoid → ECEF metres. Closed form, no iteration.
2. **`ECEFToGeodetic`** — the inverse, by the standard fixed-point latitude/height iteration
   (Bowring's closed form is the non-iterative alternative — `docs/MATH.md §5.1`).
3. **`AzEl`** — given an SV's ECEF position and a receiver's geodetic position, the topocentric
   azimuth and elevation. This is what drives the map's sky view and every elevation-gated
   integrity check.
4. **`Deg` / `Rad`** — radian↔degree conversion, because the feeds serve `*_deg` fields while
   everything internal works in radians.

The datum-as-a-parameter choice is the design decision worth stating outright. WGS-84, PZ-90.11,
and CGCS2000 are nearly interchangeable — PZ-90.11's semi-major axis is 1 m shorter than
WGS-84's, which moves geodetic height by about a metre and az/el by effectively nothing, and
CGCS2000 agrees with WGS-84 to a tenth of a millimetre — genuinely below anything this system
alerts on. We could have hardcoded WGS-84 and been fine. We didn't, because a function signature that forces
the caller to name a datum is a function nobody can accidentally misuse, and `physconst` already
carries the right one per constellation on `Params.Datum`.

---

## Details

### `Geodetic`

```go
type Geodetic struct {
    Lat, Lon, Height float64 // radians, radians, metres above the ellipsoid
}
```

Height is **ellipsoidal**, not orthometric — there is no geoid model in this library, so this is
not height above mean sea level. For satellite geometry that distinction never matters; for
anything displaying a ground station's altitude to a person, it does.

### `GeodeticToECEF`

Standard closed form with the prime vertical radius of curvature N = A/√(1 − e²sin²φ):

```
X = (N + h)·cos φ·cos λ
Y = (N + h)·cos φ·sin λ
Z = (N(1 − e²) + h)·sin φ
```

Uses `math.Sincos` for both angle pairs — one call, two results, and it keeps the sin/cos pair
consistent.

### `ECEFToGeodetic`

Longitude is exact and immediate (`atan2(Y, X)`). Latitude and height are the hard part, and
this uses the standard fixed-point iteration (`docs/MATH.md §5.1`) with three pieces of care:

**The pole case.** When the horizontal distance √(X²+Y²) drops below 1e-9 m, latitude is ±90° by
construction and height is |Z| − b, where b = A√(1−e²) is the semi-minor axis. Without this
branch the iteration divides by a vanishing `cos(lat)`.

**Convergence.** Up to 10 iterations, breaking when successive latitudes agree to 1e-12 rad
(roughly 6 µm at the Earth's surface). A ground station converges in one to four passes (a height
of exactly 0 makes the seed exact, so it takes one); the SV-altitude positions the feed actually
converts take about five.

**The final height form is chosen by conditioning, not convenience:**

```go
if hyp > math.Abs(p.Z) {
    h = hyp/cosLat - n
} else {
    h = p.Z/sinLat - n*(1-e2)
}
```

`hyp/cos(lat)` blows up near the poles (cos → 0) and `Z/sin(lat)` blows up near the equator
(sin → 0), so we pick whichever denominator is larger. Either form alone is correct in exact
arithmetic and catastrophic in floating point at its own singularity.

### `AzEl`

```go
func AzEl(sv gnss.ECEF, recv Geodetic, ell physconst.Ellipsoid) (az, el float64)
```

The receiver's geodetic position is converted to ECEF on the same ellipsoid, the receiver→SV
vector `d` is formed, and `d` is projected onto the local ENU basis:

```
east  = (−sin λ,  cos λ, 0)
north = (−sin φ cos λ, −sin φ sin λ, cos φ)
up    = ( cos φ cos λ,  cos φ sin λ, sin φ)
```

Azimuth is `atan2(e, n)`, normalized to [0, 2π) — clockwise from north, the surveying and
astronomy convention. Elevation is in [−π/2, π/2].

**Two degenerate-geometry guards, both from real regressions:**

- **regression fix — coincident points.** If the SV and receiver are at the same ECEF position, `d` has
  zero norm. Physically impossible (no broadcast satellite coincides with a ground receiver), but
  the then-current `asin(u/norm)` divided by zero and silently propagated NaN into the feed, so
  the `norm != 0` branch returns elevation = π/2. Directly overhead is the natural convention for
  zero separation. Since the regression fix replaced the ratio, the same input would now fall through to
  `atan2(0, 0)` = 0 instead — finite, but the horizon, so the branch still earns its keep by
  pinning the convention rather than by preventing a NaN.
- **regression fix — the overhead domain error.** Elevation used to be `asin(u/norm)`. For an SV *exactly*
  overhead, `u = d.Dot(up)` and `norm = d.Norm()` come from different floating-point paths, so
  rounding can push the ratio to 1+ε — outside `asin`'s domain, returning NaN. The fix is
  `atan2(u, hypot(e, n))`, which is mathematically identical (hypot(e,n) *is* the horizontal
  component of d) and has no ratio and no domain restriction. It's unconditionally safe.

Both are pinned by tests, and both are the kind of thing that looks like unnecessary paranoia
until a marker freezes on the map at 03:00.

**A note for callers:** `AzEl` can legitimately return a negative elevation — a satellite below
the horizon. That's correct output, not an error, but downstream models may not accept it.
`iono.Klobuchar` clamps negative elevation to zero for exactly this reason, and
`iono.Obliquity`'s doc tells callers to exclude below-horizon observations before using its
mapping.

---

## Tests

| Test | What it pins |
|---|---|
| `TestGeodeticToECEFEquator` | A known equatorial point maps to the expected ECEF. |
| `TestGeodeticRoundTrip` | Geodetic → ECEF → geodetic returns the input, from the equator to 89.9° and from sea level to 20 000 km (SV altitude). |
| `TestECEFToGeodeticPole` | The polar branch, where the general iteration would divide by zero. |
| `TestAzElOverhead` | An SV directly above gives elevation 90°. |
| `TestAzElNorthHorizon` | An SV due north on the horizon gives az 0°, el 0°. |
| `TestAzElEastHorizon` | Due east gives az 90° — which pins the ENU basis handedness. |
| `TestAzElOverheadNeverNaN` | The regression fix regression: exactly-overhead never yields NaN. |
| `TestAzElCoincidentPoint` | The regression fix regression: coincident SV/receiver returns π/2, not NaN. |

Run with `go test ./geo/` from `gnss/`.

---

## Sources

`docs/MATH.md §5` (§5.1 ECEF↔geodetic and the datums, §5.2 az/el). The ellipsoid parameters
themselves are `gnss/physconst` — see that package's README and `reference/REFERENCES.md`.
