# navlistener — the GNSS math, laid out for the developer

**Status: design (2026-07-07).** This is the complete reference for every calculation
`internal/gnss` performs. It is written so a developer with no GNSS background can implement
each function from this page plus the cited ICD section — **the ICD is the authority; this page
is the map through it.** Every equation carries its source. galmon is used only as a
*differential-test oracle* (§12), never as a source of code.

Conventions: angles in **radians** unless noted; positions in **metres**, ECEF (Earth-Centered
Earth-Fixed) unless noted; time in **seconds** unless noted. Semi-circles (the unit many ICDs
use for broadcast angles) are converted on decode: `radians = semicircles × π`, using the
**ICD-defined** `π = 3.1415926535898`, not the language's `math.Pi`, so results match the ICD
bit-for-bit.

---

## 0. Physical constants (per constellation — do not average them)

`c = 299792458` m/s (exact), shared by all.

| Constellation | Datum | `μ` (GM, m³/s²) | `ωe` (rad/s) | Relativity `F = −2√μ/c²` (s/√m) | ICD |
|---|---|---|---|---|---|
| **GPS** | WGS-84 | `3.986005e14` | `7.2921151467e-5` | `−4.442807633e-10` | IS-GPS-200, §20.3.3.4.3 |
| **QZSS** | (GPS-compat) | `3.986005e14` | `7.2921151467e-5` | `−4.442807633e-10` | IS-QZSS-PNT, §5.2 |
| **NavIC** | WGS-84 | `3.986005e14` | `7.2921151467e-5` | `−4.442807633e-10` | IRNSS SPS ICD v1.1, §6 |
| **Galileo** | GTRF | `3.986004418e14` | `7.2921151467e-5` | `−4.442807309e-10` | OS-SIS-ICD Issue 2.1, §5.1.1 |
| **BeiDou** | CGCS2000 | `3.986004418e14` | `7.2921150e-5` | `−4.442807309e-10` | BDS-SIS-ICD-B1I v3.0, §5.2.4 |
| **GLONASS** | PZ-90.11 | `3.986004418e14` | `7.2921150e-5` | (Cartesian model; no F term) | GLONASS ICD Ed. 5.1, App. |

GLONASS also needs `aₑ = 6378136 m` (PZ-90 equatorial radius) and `J₂ = 1.0826257e-3` (second
zonal harmonic; the ICD writes `C₂₀ = −J₂`).

> **Per-constellation constants.** GPS specifies `μ = 3.986005e14`; the
> Galileo and BeiDou ICDs specify `3.986004418e14`. The choice also changes the
> relativity constant `F`. Use each constellation's specified values and account
> for constant differences when comparing independent implementations (§12).

---

## 1. Time systems, week/TOW, and ephemeris age

Each constellation runs its own time scale, all steered near UTC but differing in epoch, leap
handling, and week rollover:

| System | Epoch | Week length | Leap seconds? | Rollover |
|---|---|---|---|---|
| GPS (GPST) | 1980-01-06 | 1024 (mod) | no (offset from UTC grows) | LNAV WN is 10-bit (1024-wk ambiguity, disambiguate by date); CNAV/CNAV-2 send a 13-bit WN |
| Galileo (GST) | 1999-08-22 | 4096 (mod) | no | 4096 wk |
| BeiDou (BDT) | 2006-01-01 | 8192 (mod) | no | 8192 wk (BDT = GPST − 14 s at epoch) |
| QZSS (QZSST) | = GPST | = GPST | no | shares GPST |
| NavIC (IRNWT) | 1999-08-22 | 1024 (mod) | no | 1024 wk |
| GLONASS | UTC(SU)+3h | *no week/TOW* | **yes (steps with UTC)** | 4-year `N4` + day `NT` |

`navlistener` keeps an internal monotonic **`GNSSTime`** (system + week + tow-seconds, or for
GLONASS `N4/NT/tod`) and converts to Unix/UTC for storage using the broadcast leap-second count
(`ΔtLS`) and the inter-system offsets in §8. Week-number rollover is disambiguated against the
ingest wall-clock (we always know roughly what year it is), never trusted blindly.

### 1.1 Ephemeris age — `ephAge(tow, toe)` — the most-used helper

The time elapsed since the ephemeris reference epoch `toe`:

```
tk = tow − toe
if tk >  half_week:  tk −= week_seconds     // straddled the week boundary forward
if tk < −half_week:  tk += week_seconds     // ...backward
```

with `week_seconds = 604800`, `half_week = 302400`. **This half-week correction is mandatory**
and appears in *every* propagation and clock call (`tk` for Kepler, `Δt` for the clock
polynomial, and the "age" reported to the integrity layer and the feeds as `eph-age-m` =
`tk/60`). For GLONASS,
where there is no TOW, "age" is `(t − tb)` in seconds of the day with the same wrap around the
86400 s day boundary.

`eph-age` also drives **staleness**: an ephemeris older than the per-constellation fit interval
(GPS ~4 h nominal, alert at 140 min; Galileo alert at 105 min — see `docs/INTEGRITY.md`) is
flagged. Discontinuity metrics (§9) are only trusted when `ephAge < 4 h`.

---

## 2. Keplerian ECEF propagation (GPS, Galileo, BeiDou MEO/IGSO, QZSS, NavIC)

These five share one algorithm (the "GPS-like" ephemeris). Implement it **once** as a generic
`kepler.Propagate(e Ephemeris, t GNSSTime) (Point, error)` over an interface that exposes the
broadcast elements; the only per-constellation inputs are the constants from §0 and the element
scalings from the frame decoder. Source: **IS-GPS-200 §20.3.3.4.3.1, Table 20-IV** (and the
identical algorithm in each other ICD).

**Broadcast elements** (after decode + scaling): `√A, e, M₀, Δn, i₀, IDOT, Ω₀, Ω̇, ω, Cuc, Cus,
Crc, Crs, Cic, Cis, toe`. (BeiDou GEO adds a step — §2.1.)

```
A   = (√A)²                                   // semi-major axis
n0  = √(μ / A³)                               // computed mean motion  (μ per §0)
tk  = ephAge(t.tow, toe)                       // §1.1, half-week corrected
n   = n0 + Δn                                  // corrected mean motion
M   = M₀ + n·tk                                // mean anomaly

// Kepler's equation  M = E − e·sin E   — solve for eccentric anomaly E:
E = M
repeat (≤ 15 iters, until |ΔE| < 1e-12):
    ΔE = (M − E + e·sin E) / (1 − e·cos E)     // Newton–Raphson (faster than fixed-point)
    E += ΔE

// true anomaly ν, argument of latitude Φ:
ν   = atan2(√(1−e²)·sin E,  cos E − e)
Φ   = ν + ω

// second-harmonic perturbation corrections:
δu  = Cus·sin 2Φ + Cuc·cos 2Φ                  // argument of latitude
δr  = Crs·sin 2Φ + Crc·cos 2Φ                  // radius
δi  = Cis·sin 2Φ + Cic·cos 2Φ                  // inclination

u   = Φ + δu
r   = A·(1 − e·cos E) + δr
i   = i₀ + δi + IDOT·tk

// position in the orbital plane:
x′  = r·cos u
y′  = r·sin u

// corrected longitude of ascending node (Earth rotation folded in):
Ω   = Ω₀ + (Ω̇ − ωe)·tk − ωe·toe               // ωe per §0  (BeiDou GEO differs — §2.1)

// ECEF:
X = x′·cos Ω − y′·cos i·sin Ω
Y = x′·sin Ω + y′·cos i·cos Ω
Z = y′·sin i
```

The relativistic clock term `Δtr` uses `E` from this solve (§4) — compute `E` once, reuse it.

### 2.1 BeiDou GEO satellites (the one Kepler exception)

BeiDou GEO SVs (C01–C05, C59–C63; PRN ≤ 5 or the ICD's GEO set) use a **different final
rotation**. Compute up to `(x′, y′, i)` as above but with BeiDou's `Ω`:

```
Ω_GEO = Ω₀ + Ω̇·tk − ωe·toe                    // note: NOT (Ω̇ − ωe)·tk
X_GK  = x′·cos Ω_GEO − y′·cos i·sin Ω_GEO
Y_GK  = x′·sin Ω_GEO + y′·cos i·cos Ω_GEO
Z_GK  = y′·sin i
```

then rotate by `−5°` about X and then by `ωe·tk` about Z into CGCS2000 ECEF (BDS-SIS-ICD-B1I
§5.2.4.12: `[X,Y,Z]ᵀ = Rz(ωe·tk)·Rx(−5°)·[X_GK,Y_GK,Z_GK]ᵀ` — Rx applied first, Rz last).
MEO/IGSO BeiDou SVs use the plain §2 algorithm.
Detect GEO by SV id, not by inclination. Getting this wrong puts the GEO belt ~km off — a
constant error a differential test against galmon catches immediately.

### 2.2 Velocity and Doppler (needed for the delta-Hz integrity check, §9)

Analytic velocity is available (IS-GPS-200 has the derivative form), but a **central finite
difference** at `t ± 0.5 s` is simpler and accurate to mm/s:

```
v = (Propagate(e, t+0.5) − Propagate(e, t−0.5)) / 1.0     // ECEF m/s
```

For a receiver at ECEF `R`, the **line-of-sight unit vector** `û = (X_sv − R)/|X_sv − R|`, the
range-rate `ρ̇ = (v_sv − v_recv)·û` (receiver static ⇒ `v_recv = 0`), and the **predicted
Doppler** on carrier frequency `f`:

```
f_D_predicted = −f · ρ̇ / c
```

Compare against the receiver's *observed* Doppler (UBX-RXM-RAWX / SBF MeasEpoch) to get
`delta_hz` — a broadcast-vs-observed integrity signal (§9, `docs/INTEGRITY.md`). Carrier `f` per
signal: GPS/Gal/QZSS/NavIC L1≈1575.42 MHz, L5/E5a≈1176.45, L2≈1227.60, E5b≈1207.14, E6≈1278.75,
B1I≈1561.098, B2a≈1176.45, B3I≈1268.52; GLONASS FDMA L1 = 1602 + k·0.5625 MHz (channel `k`).

---

## 3. GLONASS ECEF propagation (PZ-90, numerical)

GLONASS broadcasts **not** Keplerian elements but a **Cartesian state** in PZ-90.11 at reference
time `tb`: position `(x,y,z)` km, velocity `(ẋ,ẏ,ż)` km/s, and luni-solar acceleration
`(ẍ,ÿ,z̈)` km/s². Propagation is **numerical integration** of the equations of motion with the
J₂ oblateness term. Source: **GLONASS ICD Ed. 5.1, Appendix (algorithms J.1/J.2)**.

Integrate from `tb` to the target time (step `h = ±30…60 s`, Runge–Kutta 4th order) the state
`s = (x, y, z, ẋ, ẏ, ż)` with the derivative:

```
r   = √(x² + y² + z²)             // work in km: μ = 398600.4418 km³/s²
ρ   = aₑ / r                      // aₑ = 6378.136 km

ax = −μ·x/r³ · (1 + 1.5·J₂·ρ²·(1 − 5z²/r²)) + ωe²·x + 2·ωe·ẏ + jx
ay = −μ·y/r³ · (1 + 1.5·J₂·ρ²·(1 − 5z²/r²)) + ωe²·y − 2·ωe·ẋ + jy
az = −μ·z/r³ · (1 + 1.5·J₂·ρ²·(3 − 5z²/r²))                     + jz
```

where `(jx, jy, jz)` are the broadcast luni-solar accelerations (constant over the short
integration span), and the `ωe²` and `2ωe` terms are the centrifugal + Coriolis terms **because
we integrate directly in the PZ-90 rotating (ECEF) frame** — no inertial↔rotating transform is
needed or wanted. RK4 the state to the target epoch; the resulting `(x,y,z)·1000` is metres ECEF.

**Guards:** refuse to integrate a zero or timeless state (`|r|≈0` or `tb`
unknown ⇒ return an error); require parsed time-of-day (`todKnown`) before trusting
`tb`; initialize every decoded field.

### 3.1 GLONASS almanac propagation (coordinate frames)

The almanac gives long-life orbital *elements* (not a state vector): `λ` (longitude of ascending
node), `i`, `ω`, `ε` (eccentricity), `ΔT` (draconic period correction), `tλ` (time of ascending
node). The analytic propagator (GLONASS ICD Appendix 3.2.2 / A.3.2.2) produces PZ-90 ECEF.

> **Frame validation.** Inertial coordinates mislabeled as PZ-90 ECEF produce
> a longitude error that varies with Earth rotation. A single same-day sample
> can mistake this for a constant bias. Express the node longitude directly in
> the rotating Greenwich frame: `Ω = λ − ωe·(t − tλ)`, rather than `λ + GMST`.
> Validate almanac positions at multiple times of day and check that the
> sub-satellite longitude follows the expected ground trace.

GLONASS positions are **absent from `svs.json`** (the legacy API's field layout) and instead delivered through `almanac.json` in **kilometres** — see
`docs/OUTPUT.md`. So this propagator's output feeds the almanac feed, and the numerical §3
propagator (from ephemeris strings) feeds the internal state / az-el / integrity.

---

## 4. Satellite clock correction

The broadcast clock polynomial maps SV time to system time. Source: IS-GPS-200 §20.3.3.3.3.1.

```
Δtsv = af0 + af1·Δt + af2·Δt²  +  Δtr  −  TGD_signal
Δt   = ephAge(tow, toc)                       // half-week corrected against clock ref toc
Δtr  = F · e · √A · sin E                      // relativistic; F per §0, E from §2 solve
```

- `af0, af1, af2` — broadcast, already scaled to seconds / s·s⁻¹ / s·s⁻² by the frame decoder.
- **Relativistic correction `Δtr`** — periodic term from orbital eccentricity; reuse `E` from the
  §2 Kepler solve. Use `F·e·√A·sin E` with the per-constellation `F` (§0). Report as `time-disco` input
  in ns.
- **Group delay `TGD`/`BGD`** — per-signal hardware bias, so the clock correction depends on
  which signal the user tracks:
  - GPS single-freq L1 or L2: subtract `TGD` (L1) or `γ·TGD` with `γ=(f_L1/f_L2)²` (L2).
  - Galileo dual `BGD(E1,E5a)` and `BGD(E1,E5b)` — apply per the tracked pair (OS-SIS-ICD §5.1.5).
  - BeiDou `TGD1`/`TGD2` for B1I/B2I; B-CNAV uses `T_GDB1Cp`, `ISC` terms.
  - NavIC broadcasts `TGD`; QZSS mirrors GPS.
  A dual-frequency ionosphere-free user cancels `TGD`; we record the broadcast `TGD` values and
  apply the correct one per `sigId`.

**`getAtomicOffset` and `getUTCOffset`** (feeds' `delta-gps`, `delta-utc`): the SV-to-system
offset above, and the system-to-UTC offset `a0 + a1·Δt + ΔtLS` (+ scheduled leap `ΔtLSF, WNLSF,
DN`) from the broadcast UTC parameters (IS-GPS-200 §20.3.3.5.2.4).

---

## 5. Geometry: ECEF ↔ geodetic, azimuth, elevation

### 5.1 ECEF → geodetic (lat, lon, height) — Bowring/closed-form

Per datum ellipsoid `(a, f)`: WGS-84 `a=6378137, 1/f=298.257223563`; PZ-90.11 `a=6378136,
1/f=298.25784`; CGCS2000 `a=6378137, 1/f=298.257222101`. The differences are cm-level but we
carry the right ellipsoid per constellation for correctness.

```
e²  = 2f − f²
lon = atan2(Y, X)
p   = √(X² + Y²)
// iterate (or Bowring closed form) for lat, h:
lat = atan2(Z, p·(1 − e²))          // seed
repeat until Δlat < 1e-12:
    N   = a / √(1 − e²·sin²lat)
    h   = p/cos lat − N
    lat = atan2(Z, p·(1 − e²·N/(N+h)))
```

### 5.2 Topocentric azimuth / elevation (for `perrecv` az/el and delta-Hz)

Given SV ECEF `S` and receiver geodetic `(φ, λ, h)` with ECEF `R`, the ENU rotation:

```
d = S − R                                     // ECEF vector receiver→SV
e_hat = [−sin λ,           cos λ,          0      ]
n_hat = [−sin φ·cos λ,  −sin φ·sin λ,   cos φ ]
u_hat = [ cos φ·cos λ,   cos φ·sin λ,   sin φ ]
E = d·e_hat ;  N = d·n_hat ;  U = d·u_hat
azimuth   = atan2(E, N)            // 0=N, clockwise; wrap to [0,2π)
elevation = asin(U / |d|)          // or atan2(U, √(E²+N²))
```

Elevation gates most integrity checks (low-elevation SVs have unreliable Doppler/pseudorange);
a receiver's vote only counts above a configured elevation mask (e.g. `elev > 10°`) — this is
separate from the *freshness* gate (the 60 s fresh-receiver window in `docs/INTEGRITY.md §2`).

---

## 6. Signal-in-space accuracy: URA / SISA / F_T

Each constellation broadcasts a coarse accuracy index; we decode to metres and expose `sisa` /
`sisa-m` (feeds) and use it for integrity (`docs/INTEGRITY.md`, SISA-change alert at 3 m):

- **GPS/QZSS/NavIC URA index `N` (0–15)** → metres by the IS-GPS-200 Table 20-XII step function:
  `N≤6 ⇒ 2^(1+N/2)` (rounded per table), `N≥7 ⇒ 2^(N−2)`; `N=15` = "no accuracy / do not use".
- **Galileo SISA (0–255)** → metres in four linear bands (OS-SIS-ICD §5.1.12): `0–49: 0–0.49 m`
  (1 cm step), `50–74: 0.5–0.98 m` (2 cm), `75–99: 1–1.96 m` (4 cm), `100–125: 2–6 m` (16 cm),
  `126–254` spare, `255` = **"NO SISA AVAILABLE"** (SISA-invalid → `sisa_valid=false`).
- **GLONASS F_T (0–15)** → metres by the ICD F_T table; `NONE`/absent ⇒ `sisa_valid=false`.
- **BeiDou** — B1I URAI table (like GPS), B-CNAV uses SISAoe/SISAoc + a separate accuracy set.

`sisa_valid` (schema-1.1 boolean) captures the "no accuracy available" sentinels so clients
never parse English; galmon emits the strings, we emit both (see `docs/OUTPUT.md`).

---

## 7. Ionospheric delay models

Broadcast ionosphere models support calculating signal delay for clients and
modeling a single-frequency user's expected pseudorange. Delay is per signal frequency `f`:
`delay(f) = delay(L1)·(f_L1/f)²` for the frequency-scaled models.

### 7.1 Klobuchar (GPS L1, QZSS, BeiDou B1I, NavIC) — full algorithm

8 coefficients `α0..α3, β0..β3` (broadcast). Inputs: user geodetic `(φu, λu)` in semicircles,
SV azimuth `A` and elevation `E` (semicircles). Source: IS-GPS-200 §20.3.3.5.2.5.

```
ψ    = 0.0137/(E + 0.11) − 0.022                     // earth-centred angle (semicircles)
φi   = clamp(φu + ψ·cos A, ±0.416)                    // IPP geomag lat
λi   = λu + ψ·sin A / cos(φi·π)
φm   = φi + 0.064·cos((λi − 1.617)·π)                 // geomagnetic lat
t    = 43200·λi + GPS_tow ;  t = mod(t, 86400)        // local time at IPP
AMP  = Σ αn·φmⁿ  (n=0..3) ; if AMP<0: AMP=0
PER  = Σ βn·φmⁿ  (n=0..3) ; if PER<72000: PER=72000
x    = 2π·(t − 50400)/PER
F    = 1 + 16·(0.53 − E)³                             // obliquity
Tiono = F·(5e-9 + AMP·(1 − x²/2 + x⁴/24))   if |x|<1.57
        F·5e-9                                if |x|≥1.57      // seconds of L1 delay
```

BeiDou B1I uses the same 8-parameter form with BeiDou's own broadcast `α/β` (BDS-SIS-ICD-B1I
§5.2.4.7). NavIC broadcasts Klobuchar-style coefficients on its grid.

### 7.2 NeQuick-G (Galileo)

Galileo broadcasts three **effective ionisation** coefficients `a₀, a₁, a₂` (not Klobuchar).
The delay is computed by the full **NeQuick-G** profile integration along the ray, driven by the
effective ionisation level `Az = a₀ + a₁·MODIP + a₂·MODIP²` (MODIP from the modip grid) and
month/solar inputs. This is a substantial model — implement per the **"European GNSS (Galileo)
Open Service NeQuick-G" (JRC, EU 2016)** specification and its reference code description; ship
the modip grid and the ITU-R coefficient tables as data assets. Cross-check against the JRC test
vectors. (For a first cut we may expose the broadcast `a₀..a₂` and defer the full profile integral
behind a feature flag, but the target is the real model.)

### 7.3 BDGIM (BeiDou B-CNAV)

BeiDou's modern signals carry the **BeiDou Global Ionospheric delay correction Model** — 9
broadcast coefficients `α1..α9` over a spherical-harmonic basis with predicted coefficients from
an embedded model. Implement per **BDS-SIS-ICD-B-CNAV1 §7.4**; cross-check against BeiDou's
published examples.

---

## 8. Time-system offsets (feeds' `a0g/a1g/t0g/wn0g`, `global.json` offsets)

Broadcast inter-system and UTC offsets, decoded and republished:

- **GNSS–UTC**: `A0, A1, ΔtLS, tot, WNt, ΔtLSF, WNLSF, DN` → UTC(k) offset + pending leap.
- **GGTO** (Galileo–GPS Time Offset): `A0G, A1G, t0G, WN0G` → `ΔtGGTO = A0G + A1G·(t − t0G)`
  (OS-SIS-ICD §5.1.7). Surfaced as `a0g/a1g/t0g/wn0g` and `gst-gps-offset-ns`.
- **BGTO** (BeiDou–GPS/Galileo), **QZSS–GPS** (≈0 by design), **GLONASS–GPS/UTC(SU)**.
- `global.json` aggregates: `gps-utc-offset-ns`, `gst-utc-offset-ns`, `gst-gps-offset-ns`, and
  `leap-seconds`. These come from the `TimeOffset` decode, not computed by us — we transcribe the
  broadcast values (and flag when receivers disagree, an integrity signal).

---

## 9. Integrity math (summary — full treatment in `docs/INTEGRITY.md`)

The integrity signals are *derived* here and *thresholded/alerted* in INTEGRITY.md:

- **Orbit discontinuity `orbit-disco`** — when a new IOD ephemeris arrives, propagate the **old**
  and **new** ephemeris to the **same epoch** (the changeover TOW) and take
  `orbit-disco = |X_new − X_old|` (metres). Only trusted if `ephAge < 4 h` and both positions are
  non-zero/non-NaN. This is the single most important integrity metric.
- **Clock discontinuity `time-disco`** — the jump in `Δtsv` (§4, evaluated at the changeover, with
  `Δtr` consistent) between old and new clock models, in ns. `ns/3.335` ≈ metres.
- **delta-Hz** — per receiver, observed Doppler − predicted Doppler (§2.2). A coherent nonzero
  delta-Hz across receivers can indicate a clock/orbit error or spoofing.
- **RTCM precise-vs-broadcast** — magnitude of the SSR radial/along/cross correction (RTCM
  1057–1068), i.e. how far the broadcast orbit is from the precise network orbit. Decoded scale:
  radial 0.1 mm, along/cross 0.4 mm (RTCM-3 SSR).
- **Health / URA / OSNMA / SISA** transitions.

---

## 10. Almanac (coarse orbit) propagation

Reduced-precision Kepler for all-SV acquisition data. GPS/QZSS/NavIC/Galileo/BeiDou almanacs are
the §2 algorithm with almanac scalings, `toe = toa` (`toa·2¹²` for GPS). **The broadcast `δi` is
an offset from a per-constellation reference inclination — do not hardcode GPS's:** GPS/NavIC
`i₀ = 0.3 semicircles (54°) + δi`; QZSS QZO uses its own reference per IS-QZSS-PNT (QZO flies
~41–45°, nowhere near 54° — a GPS-hardcoded reference silently corrupts QZSS almanacs); Galileo
references 56° (OS-SIS-ICD almanac §); BeiDou MEO/IGSO reference 0.3 semicircles, GEO 0.
GLONASS almanac uses §3.1. We publish
almanac-derived ECEF in `almanac.json` (km) — this is *also* how the feed supplies GLONASS
positions and how `best-tle`/`alma-dist` cross-checks are computed (`alma-dist` = distance between
the ephemeris ECEF and the almanac/TLE ECEF, a coarse sanity check).

---

## 11. TLE cross-check (`best-tle`, `best-tle-dist`)

As an independent orbit reference we match each SV against public TLEs (CelesTrak GNSS
catalogue), propagate with **SGP4**, and report the SV name of the best match and the distance
`best-tle-dist` between our broadcast-ephemeris ECEF and the SGP4 ECEF. SGP4 is a standard,
independently-implemented algorithm (using a permissively licensed implementation); the TLE match is a coarse gross-error detector, not a precision reference.

---

## 12. Validation strategy (how we know the math is right)

Three independent oracles, in CI:

1. **Published ICD test vectors.** IS-GPS-200, OS-SIS-ICD (Annex), and the NeQuick-G / BDGIM
   reference examples give worked numeric cases. Golden tests assert our propagator reproduces
   them to the ICD tolerance. This is the *authoritative* check — it validates against the spec,
   not against another implementation.
2. **RINEX broadcast navigation (BRDC) files.** Feed daily IGS BRDC ephemerides through our
   propagator and compare the SV positions against the IGS **SP3 precise orbits** at the same
   epochs — the real-world accuracy check (broadcast-vs-precise is a few metres; a bug is
   kilometres). Also re-derive our own decoders' output from RINEX to confirm frame decode.
3. **Independent implementation comparison.** Run galmon and `navlistener` over the **same captured raw
   frame stream** and diff `svs.json`/`sv.json` numeric fields (ECEF x/y/z, clock offset,
   orbit-disco, time-disco). **Agreement cross-validates both implementations**; a disagreement
   is a bug in one of us worth root-causing. Check units, reference frames, signal conventions, and constants
   before interpreting a numerical difference. The comparison uses numerical
   outputs from independently authored implementations.

Every `internal/gnss` decoder is **fuzzed** (`go test -fuzz`) against malformed frames, including
invalid lengths, out-of-bounds reads, and arithmetic underflows,
and every propagator has property tests (energy/momentum sanity, continuity across the week
boundary, non-NaN under degenerate input).

---

## Appendix A — file map

| Package | Responsibility | Key ICD |
|---|---|---|
| `internal/gnss/time` | GNSSTime, leap seconds, `ephAge`, week rollover, inter-system offsets | all §5-time |
| `internal/gnss/kepler` | §2 generic propagator + §2.1 BeiDou GEO + §2.2 velocity | IS-GPS-200 §20.3.3.4.3 |
| `internal/gnss/glonass` | §3 RK4 numerical + §3.1 almanac (rotating-frame-correct) | GLONASS ICD App. |
| `internal/gnss/clock` | §4 clock polynomial + relativity + TGD/BGD/ISC | IS-GPS-200 §20.3.3.3.3 |
| `internal/gnss/geo` | §5 ECEF↔geodetic, az/el, per-datum ellipsoids | — |
| `internal/gnss/accuracy` | §6 URA/SISA/F_T decode | IS-GPS-200 Tbl 20-XII, OS-SIS-ICD §5.1.12 |
| `internal/gnss/iono` | §7 Klobuchar / NeQuick-G / BDGIM | IS-GPS-200 §20.3.3.5.2.5, NeQuick-G, B-CNAV §7.4 |
| `internal/gnss/frame` | raw-frame bit decoders (see `docs/CONSTELLATIONS.md`) | per constellation |
| `internal/gnss/tle` | §11 SGP4 cross-check | — |
