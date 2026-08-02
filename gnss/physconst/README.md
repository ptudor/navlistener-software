# `gnss/physconst` — the per-constellation physical constants

**Headline:** every number that describes the *world* rather than a *satellite* lives here, and
each constellation gets its own set. Nothing in this package is averaged, shared "because
they're close," or typed in from memory. If a propagator needs μ, ωe, a relativity constant, or
a reference ellipsoid, it asks `physconst.For(id)` and gets the values that constellation's own
ICD specifies.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `physconst.go` | The whole package: `Ellipsoid`, `Params`, the per-constellation table, the accessors, and the GLONASS-specific kilometre constants. |
| `physconst_test.go` | The guard tests — including regression tests for per-constellation μ values. |
| `README.md` | This file. |

The package imports only the root `gnss` package (for `GNSSID`). It is the bottom of the
dependency graph: `kepler`, `clock`, `geo`, `glonass`, and `frame` all import it, and it imports
none of them.

---

## Summary

Broadcast ephemerides are just numbers until you pair them with the constants the broadcasting
system assumed when it generated them. Use the wrong gravitational parameter and the orbit you
compute is subtly, silently wrong — not wrong enough to look broken, just wrong enough to
poison an integrity threshold. That's the whole reason this package exists as its own compilation
unit instead of a handful of `const` blocks scattered through the propagators.

Four things live here:

1. **`Params`** — the Keplerian constant set (μ, ωe, the relativity constant F, and the datum),
   keyed by constellation.
2. **`Ellipsoid`** — the three reference ellipsoids (WGS-84, PZ-90.11, CGCS2000) with derived
   flattening and first eccentricity squared.
3. **The universal constants** — the speed of light, and the ICD's mandated value of π.
4. **The GLONASS kilometre constants** — a separate set, because the GLONASS equations of motion
   are specified in kilometres and converting them would just introduce a rounding step nobody
   asked for.

---

## Details

### `Params` and the per-constellation table

```go
type Params struct {
    Mu     float64   // gravitational parameter GM, m³/s²
    OmegaE float64   // Earth rotation rate ωe, rad/s
    RelF   float64   // relativity constant F = −2√μ/c², s/√m (0 for GLONASS)
    Datum  Ellipsoid // reference ellipsoid for ECEF↔geodetic
}
```

The table as built:

| Constellation | μ (m³/s²) | ωe (rad/s) | F (s/√m) | Datum |
|---|---|---|---|---|
| GPS | 3.986005e14 | 7.2921151467e-5 | −4.442807633e-10 | WGS-84 |
| QZSS | 3.986005e14 | 7.2921151467e-5 | −4.442807633e-10 | WGS-84 |
| NavIC | 3.986005e14 | 7.2921151467e-5 | −4.442807633e-10 | WGS-84 |
| Galileo | 3.986004418e14 | 7.2921151467e-5 | −4.442807309e-10 | WGS-84 |
| BeiDou | 3.986004418e14 | 7.2921150e-5 | −4.442807309e-10 | CGCS2000 |
| GLONASS | 3.986004418e14 | 7.2921150e-5 | 0 | PZ-90.11 |

Read that table carefully, because three of its rows are the entire point of the package:

- **Galileo and BeiDou do NOT use the GPS μ.** GPS specifies 3.986005e14; Galileo and
  BeiDou specify 3.986004418e14. The difference shows up in the eighth digit
  of the relativity constant F — which is exactly the scale the 2.5 ns clock-disco integrity
  threshold operates at. This is the single most consequential constant in the package, and
  `TestGalileoBeidouMuDiffersFromGPS` exists solely to fail if anyone ever "simplifies" it back.
- **QZSS and NavIC DO share the GPS values,** deliberately. Both are GPS-compatible by design
  (GPS-compatible time scale, GPS-compatible datum), so sharing is correct here and the same test
  asserts it in the opposite direction — it fails if someone splits them out on the assumption
  that "different constellation" always means "different constants."
- **GLONASS has no F term.** It broadcasts a Cartesian state vector, not Keplerian elements, and
  the relativistic correction is already folded into the broadcast τn. Setting `RelF: 0` is not a
  placeholder; it's the correct value, and `TestGlonassHasNoRelativityF` pins it.

BeiDou's F deserves its own note, because it looks unsourced and isn't. The BDS ICD publishes no
rounded digit string for F — it defines F = −2μ^½/C² with μ = 3.986004418e14 and C = 2.99792458e8
and leaves you to compute it (BDS-SIS-B1I-3.0 §5.2.4.9). The value in the table is that
computation, which lands on the same digits as Galileo's because the two ICDs specify the same μ.
That's a coincidence of shared inputs, not a copy-paste, and it's documented in-file so nobody
"fixes" it later.

### Accessors: `For` vs `MustFor`

```go
func For(id gnss.GNSSID) (Params, bool)   // ok=false for SBAS, IMES, unknown ids
func MustFor(id gnss.GNSSID) Params       // panics on the same inputs
```

`For` is the runtime accessor and the one to reach for by default — SBAS and IMES have no
Keplerian parameter set at all (SBAS GEOs broadcast an ECEF state in their own message format,
IMES is never emitted), so a constellation id arriving from an untrusted frame must be allowed to
miss. `MustFor` is for compile-time-known constellations, mostly in tests and table setup. The
propagators use `For` and turn `ok=false` into a typed error rather than a panic; that's part of
the library-wide "errors, never NaN, never panic" contract.

### `Ellipsoid` and the three datums

```go
type Ellipsoid struct {
    A    float64 // semi-major axis, metres
    InvF float64 // 1/f
}
func (e Ellipsoid) F() float64   // flattening f
func (e Ellipsoid) E2() float64  // first eccentricity squared, e² = 2f − f²
```

| Name | A (m) | 1/f | Used by |
|---|---|---|---|
| `WGS84` | 6378137 | 298.257223563 | GPS, QZSS, NavIC, Galileo |
| `PZ90` | 6378136 | 298.25784 | GLONASS (PZ-90.11) |
| `CGCS2000` | 6378137 | 298.257222101 | BeiDou |

The three differ at the centimetre level, which is below anything this system alerts on. We carry
them separately anyway — partly because it costs nothing, and partly because "close enough"
reasoning about datums is how a centimetre becomes a metre three refactors later. `geo` takes the
ellipsoid as an explicit argument for the same reason: the caller has to say which datum it means.

### `SpeedOfLight` and `Pi`

`SpeedOfLight = 299792458.0` is exact by definition and shared by every constellation.

`Pi = 3.1415926535898` is **not** `math.Pi`, and that's on purpose. The ICDs mandate this
13-digit value for converting broadcast angles from semicircles to radians (radians = semicircles
× Pi). Using it rather than the full-precision `math.Pi` makes decoded angles match the ICD
bit-for-bit. The frame decoders apply it at decode time — every `* semi` you see in
`gnss/frame/*.go` is this constant. If you ever see `math.Pi` used to scale a broadcast angle,
that's a bug.

### The GLONASS kilometre constants

```go
GloMuKm   = 398600.4418   // GM, km³/s²
GloAeKm   = 6378.136      // PZ-90 equatorial radius aₑ, km
GloJ2     = 1.0826257e-3  // second zonal harmonic (the ICD writes C₂₀ = −J₂)
GloOmegaE = 7.2921150e-5  // PZ-90 Earth rotation rate, rad/s
```

These are separate from the `Params` table because the GLONASS equations of motion in the ICD's
Appendix are written in kilometres, and `gnss/glonass` integrates in those units and converts to
metres only at the boundary. Note the sign convention on J₂: the ICD publishes C₂₀ = −J₂, so the
value here is the *positive* J₂ and the propagator's `deriv` uses it accordingly. That sign is the
kind of thing that produces a plausible-looking orbit that's wrong by kilometres, so it's called
out in both places.

---

## Tests

| Test | What it pins |
|---|---|
| `TestRelativityConstant` | Each constellation's F is consistent with its own μ via F = −2√μ/c² (1e-5 relative tolerance, because the ICD values are rounded). Catches pairing one constellation's F with another's μ. |
| `TestGalileoBeidouMuDiffersFromGPS` | Galileo and BeiDou must **not** carry the GPS μ; QZSS and NavIC **must**. Both directions asserted. |
| `TestEllipsoidE2` | WGS-84 e² equals the standard 0.0066943799901; PZ-90's semi-major axis differs from WGS-84's. |
| `TestForUnknown` | SBAS has no Keplerian params; GPS does. |
| `TestGlonassHasNoRelativityF` | GLONASS F is 0 and its datum is PZ-90.11. |

Run them with `go test ./physconst/` from `gnss/`.

---

## Sources

Everything here traces to `docs/MATH.md §0`, which in turn cites:

- **IS-GPS-200N** — GPS μ, ωe, F, WGS-84.
- **GAL-OS-SIS-ICD-2.2** — Galileo μ and F.
- **BDS-SIS-B1I-3.0 §5.2.4.9** — BeiDou μ, ωe, the F *definition* (not a printed value), CGCS2000.
- **GLO-ICD-5.1** — PZ-90.11, GM, aₑ, C₂₀/J₂, ωe.
- **QZSS-PNT-006**, **NAVIC-SPS-L5S** — the GPS-compatibility statements.

See `reference/REFERENCES.md` for the documents and redistribution terms. Every
constant must have a source citation; mark unresolved values as unverified.
