// Package kepler implements the generic Keplerian ECEF propagation shared by
// GPS, Galileo, BeiDou (MEO/IGSO), QZSS, and NavIC, plus the BeiDou-GEO rotation
// exception and finite-difference velocity/Doppler. Source: docs/MATH.md §2,
// IS-GPS-200 §20.3.3.4.3.1 Table 20-IV (and the identical algorithm in each other
// ICD). The only per-constellation inputs are the constants from package
// physconst and the decoded broadcast elements.
package kepler

import (
	"errors"
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/physconst"
)

// Ephemeris is one decoded broadcast ephemeris (elements already scaled to SI:
// radians, seconds, metres) for a Kepler-family constellation. The frame decoders
// fill it; the propagator consumes it. Angles are radians.
type Ephemeris struct {
	ID   gnss.GNSSID // constellation (selects the physical constants)
	SVID int         // PRN within constellation (BeiDou GEO detection)

	SqrtA     float64 // √A at reference time, √metres
	ADot      float64 // semi-major-axis rate Ȧ, m/s (CNAV/B-CNAV family; 0 for LNAV/D1/INAV)
	Ecc       float64 // eccentricity e
	M0        float64 // mean anomaly at reference, rad
	DeltaN    float64 // mean-motion correction Δn₀, rad/s
	DeltaNDot float64 // mean-motion correction rate Δṅ₀, rad/s² (CNAV/B-CNAV family; else 0)
	I0        float64 // inclination at reference i₀, rad
	IDot      float64 // inclination rate IDOT, rad/s
	Omega0    float64 // longitude of ascending node Ω₀, rad
	OmegaDot  float64 // rate of right ascension Ω̇, rad/s
	Omega     float64 // argument of perigee ω, rad
	Cuc, Cus  float64 // argument-of-latitude corrections, rad
	Crc, Crs  float64 // radius corrections, metres
	Cic, Cis  float64 // inclination corrections, rad
	Toe       float64 // reference time of ephemeris, seconds of week
}

// Solution is the result of a propagation: the ECEF position plus the eccentric
// anomaly E and half-week-corrected age tk, so the clock package can reuse E for
// the relativistic term instead of re-deriving it (docs/MATH.md §4).
type Solution struct {
	Pos gnss.ECEF
	E   float64 // eccentric anomaly, rad
	Tk  float64 // t − toe, half-week corrected, seconds
}

var (
	errNoParams    = errors.New("kepler: constellation has no Keplerian parameters")
	errBadSemiAxis = errors.New("kepler: non-positive semi-major axis")
	errBadEcc      = errors.New("kepler: eccentricity out of range [0,1)")
	errNaN         = errors.New("kepler: propagation produced a non-finite value")
)

// Solve propagates ephemeris e to time-of-week tow (seconds) and returns the full
// solution. It returns an error — never a NaN that could poison the feed — on
// degenerate input (unknown constellation, non-positive axis, out-of-range
// eccentricity, or a non-finite result).
func Solve(e Ephemeris, tow float64) (Solution, error) {
	p, ok := physconst.For(e.ID)
	if !ok {
		return Solution{}, errNoParams
	}
	a := e.SqrtA * e.SqrtA
	if a <= 0 || math.IsNaN(a) {
		return Solution{}, errBadSemiAxis
	}
	if e.Ecc < 0 || e.Ecc >= 1 || math.IsNaN(e.Ecc) {
		return Solution{}, errBadEcc
	}

	n0 := math.Sqrt(p.Mu / (a * a * a)) // computed mean motion from A at reference
	tk := gnsstime.EphAge(tow, e.Toe)   // §1.1 half-week corrected
	// CNAV-family time-varying terms (IS-GPS-705 / BDS-SIS-ICD-B2a Table 7-9):
	// A(tk) = A₀ + Ȧ·tk and n = n₀ + Δn₀ + ½Δṅ₀·tk. Zero for LNAV/D1/INAV.
	ak := a + e.ADot*tk
	if ak <= 0 || math.IsNaN(ak) {
		return Solution{}, errBadSemiAxis
	}
	n := n0 + e.DeltaN + 0.5*e.DeltaNDot*tk // corrected mean motion
	m := e.M0 + n*tk                        // mean anomaly

	// Kepler's equation M = E − e·sin E, Newton–Raphson (docs/MATH.md §2).
	ecc := e.Ecc
	ea := m
	for i := 0; i < 15; i++ {
		dE := (m - ea + ecc*math.Sin(ea)) / (1 - ecc*math.Cos(ea))
		ea += dE
		if math.Abs(dE) < 1e-12 {
			break
		}
	}

	sinE, cosE := math.Sincos(ea)
	// True anomaly ν and argument of latitude Φ.
	nu := math.Atan2(math.Sqrt(1-ecc*ecc)*sinE, cosE-ecc)
	phi := nu + e.Omega

	sin2phi, cos2phi := math.Sincos(2 * phi)
	du := e.Cus*sin2phi + e.Cuc*cos2phi
	dr := e.Crs*sin2phi + e.Crc*cos2phi
	di := e.Cis*sin2phi + e.Cic*cos2phi

	u := phi + du
	r := ak*(1-ecc*cosE) + dr
	inc := e.I0 + di + e.IDot*tk

	sinU, cosU := math.Sincos(u)
	xp := r * cosU // in-plane x′
	yp := r * sinU // in-plane y′

	var pos gnss.ECEF
	if isBeiDouGEO(e.ID, e.SVID) {
		pos = beidouGEO(xp, yp, inc, e, p, tk)
	} else {
		// Corrected longitude of ascending node, Earth rotation folded in.
		om := e.Omega0 + (e.OmegaDot-p.OmegaE)*tk - p.OmegaE*e.Toe
		sinOm, cosOm := math.Sincos(om)
		sinI, cosI := math.Sincos(inc)
		pos = gnss.ECEF{
			X: xp*cosOm - yp*cosI*sinOm,
			Y: xp*sinOm + yp*cosI*cosOm,
			Z: yp * sinI,
		}
	}

	if !finite(pos) {
		return Solution{}, errNaN
	}
	return Solution{Pos: pos, E: ea, Tk: tk}, nil
}

// Propagate returns just the ECEF position of ephemeris e at time-of-week tow.
func Propagate(e Ephemeris, tow float64) (gnss.ECEF, error) {
	s, err := Solve(e, tow)
	if err != nil {
		return gnss.ECEF{}, err
	}
	return s.Pos, nil
}

// beidouGEO applies the BeiDou GEO final rotation (docs/MATH.md §2.1,
// BDS-SIS-ICD-B1I §5.2.4.12): compute the position in the inertial-like GK frame
// with Ω_GEO = Ω₀ + Ω̇·tk − ωe·toe (note: NOT (Ω̇ − ωe)·tk), then rotate
// [X,Y,Z]ᵀ = Rz(ωe·tk)·Rx(−5°)·[X_GK,Y_GK,Z_GK]ᵀ (Rx first, Rz last).
func beidouGEO(xp, yp, inc float64, e Ephemeris, p physconst.Params, tk float64) gnss.ECEF {
	om := e.Omega0 + e.OmegaDot*tk - p.OmegaE*e.Toe
	sinOm, cosOm := math.Sincos(om)
	sinI, cosI := math.Sincos(inc)
	gk := gnss.ECEF{
		X: xp*cosOm - yp*cosI*sinOm,
		Y: xp*sinOm + yp*cosI*cosOm,
		Z: yp * sinI,
	}
	// Rx(−5°): rotation about X by −5 degrees.
	phiX := -5.0 * math.Pi / 180.0
	sx, cx := math.Sincos(phiX)
	v := gnss.ECEF{
		X: gk.X,
		Y: cx*gk.Y + sx*gk.Z,
		Z: -sx*gk.Y + cx*gk.Z,
	}
	// Rz(ωe·tk): rotation about Z by ωe·tk (BeiDou ICD sign convention).
	theta := p.OmegaE * tk
	sz, cz := math.Sincos(theta)
	return gnss.ECEF{
		X: cz*v.X + sz*v.Y,
		Y: -sz*v.X + cz*v.Y,
		Z: v.Z,
	}
}

// isBeiDouGEO reports whether a BeiDou SV is a GEO satellite (C01–C05, C59–C63),
// which uses the §2.1 rotation. Detection is by SV id, never by inclination
// (docs/MATH.md §2.1).
func isBeiDouGEO(id gnss.GNSSID, svid int) bool {
	if id != gnss.BeiDou {
		return false
	}
	return (svid >= 1 && svid <= 5) || (svid >= 59 && svid <= 63)
}

// Velocity returns the ECEF velocity (m/s) of ephemeris e at tow via a central
// finite difference at tow ± 0.5 s — accurate to mm/s and simpler than the
// analytic derivative (docs/MATH.md §2.2).
func Velocity(e Ephemeris, tow float64) (gnss.ECEF, error) {
	fwd, err := Propagate(e, tow+0.5)
	if err != nil {
		return gnss.ECEF{}, err
	}
	bwd, err := Propagate(e, tow-0.5)
	if err != nil {
		return gnss.ECEF{}, err
	}
	return fwd.Sub(bwd), nil // divided by 1.0 s
}

// PredictedDoppler returns the ephemeris-predicted Doppler (Hz) on carrier
// frequency freqHz for an SV described by e at time tow, observed by a static
// receiver at ECEF recv. f_D = −f·ρ̇/c with range-rate ρ̇ = v_sv·û
// (docs/MATH.md §2.2). This is the "predicted" half of the delta-Hz integrity
// signal (docs/INTEGRITY.md §3).
func PredictedDoppler(e Ephemeris, tow, freqHz float64, recv gnss.ECEF) (float64, error) {
	pos, err := Propagate(e, tow)
	if err != nil {
		return 0, err
	}
	vel, err := Velocity(e, tow)
	if err != nil {
		return 0, err
	}
	los := pos.Sub(recv)
	dist := los.Norm()
	if dist == 0 {
		return 0, errNaN
	}
	unit := los.Scale(1 / dist)
	rangeRate := vel.Dot(unit) // receiver static ⇒ v_recv = 0
	return -freqHz * rangeRate / physconst.SpeedOfLight, nil
}

func finite(p gnss.ECEF) bool {
	return !(math.IsNaN(p.X) || math.IsInf(p.X, 0) ||
		math.IsNaN(p.Y) || math.IsInf(p.Y, 0) ||
		math.IsNaN(p.Z) || math.IsInf(p.Z, 0))
}
