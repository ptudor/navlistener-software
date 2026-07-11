// Package glonass implements GLONASS ECEF propagation. Unlike the Kepler-family
// constellations, GLONASS broadcasts a Cartesian state vector in PZ-90.11 at a
// reference time tb — position, velocity, and luni-solar acceleration — and the
// position at another epoch is found by numerically integrating the equations of
// motion (with the J₂ oblateness term) directly in the PZ-90 rotating frame.
// Source: docs/MATH.md §3, GLONASS ICD Ed. 5.1 Appendix (algorithm J.1/J.2).
//
// The integration works in kilometres (the ICD's units) and multiplies the result
// by 1000 to return metres. Every decoded field is initialised, a zero/timeless
// state is refused rather than integrated into a NaN, and the time-of-day must be
// anchored (todKnown) before tb is trusted.
package glonass

import (
	"errors"
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/physconst"
)

// Ephemeris is one decoded GLONASS immediate ephemeris: the PZ-90 state at tb.
// Position is km, velocity km/s, luni-solar acceleration km/s² — the ICD units.
type Ephemeris struct {
	X, Y, Z    float64 // position at tb, km (PZ-90.11)
	Vx, Vy, Vz float64 // velocity at tb, km/s
	Ax, Ay, Az float64 // luni-solar acceleration, km/s² (≈constant over the span)

	Tb       float64 // reference time (seconds of day)
	TodKnown bool    // true once string 5 anchored the time-of-day (docs/MATH.md §3)

	// SV clock terms (regression fix; GLONASS ICD Ed. 5.1 Table 4.5), stored exactly as
	// broadcast: TauN is τn(tb), the correction of the SV time scale to GLONASS
	// time at tb, seconds (applied as t_GLO = t_sv + τn − γn·(t_sv − tb));
	// GammaN is γn(tb), the relative frequency deviation, dimensionless;
	// DeltaTauN is Δτn, the L2−L1 group-delay difference, seconds. γn rides
	// string 3 with the rest of the immediate data, so it is always present on
	// an assembled set; ClockKnown reports whether a same-frame string 4
	// contributed τn/Δτn (an ephemeris can assemble clockless when string 4 was
	// lost — position math needs none of these).
	TauN       float64
	GammaN     float64
	DeltaTauN  float64
	ClockKnown bool

	FreqCh int // FDMA channel k = freqId − 7 (identity metadata)
	Slot   int // slot number n
}

var (
	errTimeless = errors.New("glonass: time-of-day not anchored (todKnown false)")
	errZero     = errors.New("glonass: zero/degenerate state vector")
	errNaN      = errors.New("glonass: integration produced a non-finite value")
	errTkDomain = errors.New("glonass: propagation interval out of domain")
)

// maxStep is the RK4 step ceiling in seconds (docs/MATH.md §3: h = ±30…60 s).
const maxStep = 60.0

// maxPropSpan bounds |tk| for Propagate : the GLONASS ephemeris fit interval is
// ~30 min, daemon callers wrap to ±43,200 s; two days is a generous domain ceiling.
const maxPropSpan = 2 * 86400.0

// Propagate integrates the ephemeris forward or backward by tk seconds from tb and
// returns the ECEF position in metres (PZ-90.11). tk is the signed interval — the
// caller obtains it via gnsstime.EphAgeDay(targetTOD, tb). It refuses a state that
// is not time-anchored or is degenerately small, and never returns a NaN.
func Propagate(e Ephemeris, tk float64) (gnss.ECEF, error) {
	if !e.TodKnown {
		return gnss.ECEF{}, errTimeless
	}
	// reject a non-finite or out-of-domain tk before the step-count loop. A huge tk
	// runs millions of RK4 steps (seconds-to-CPU-days of work) and returns a finite but
	// physically meaningless position with err==nil — the "public inputs bypass validation"
	// class regression fix/regression fix closed on the kepler side. The GLONASS ephemeris is valid only over
	// its ~30-min fit interval; daemon callers are wrapped to ±43,200 s, so a couple of days
	// is a generous ceiling that only catches out-of-domain library misuse.
	if math.IsNaN(tk) || math.IsInf(tk, 0) || math.Abs(tk) > maxPropSpan {
		return gnss.ECEF{}, errTkDomain
	}
	if math.Hypot(math.Hypot(e.X, e.Y), e.Z) < 1 { // < 1 km ⇒ effectively zero
		return gnss.ECEF{}, errZero
	}
	s0 := state{e.X, e.Y, e.Z, e.Vx, e.Vy, e.Vz}
	j := accel{e.Ax, e.Ay, e.Az}
	s := integrate(s0, j, tk)
	if !s.finite() {
		return gnss.ECEF{}, errNaN
	}
	return gnss.ECEF{X: s[0] * 1000, Y: s[1] * 1000, Z: s[2] * 1000}, nil
}

// state is the 6-vector (x, y, z, vx, vy, vz) in km / km·s⁻¹.
type state [6]float64

// accel is the constant broadcast luni-solar acceleration (km/s²).
type accel [3]float64

func (s state) finite() bool {
	for _, v := range s {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return false
		}
	}
	return true
}

// integrate RK4-steps s0 by tk seconds under the GLONASS equations of motion,
// choosing enough sub-steps to keep each at or below maxStep.
func integrate(s0 state, j accel, tk float64) state {
	if tk == 0 {
		return s0
	}
	n := int(math.Ceil(math.Abs(tk) / maxStep))
	if n < 1 {
		n = 1
	}
	h := tk / float64(n)
	s := s0
	for i := 0; i < n; i++ {
		s = rk4(s, j, h)
	}
	return s
}

// rk4 advances the state one step of h seconds (classic 4th-order Runge–Kutta).
func rk4(s state, j accel, h float64) state {
	k1 := deriv(s, j)
	k2 := deriv(addScaled(s, k1, h/2), j)
	k3 := deriv(addScaled(s, k2, h/2), j)
	k4 := deriv(addScaled(s, k3, h), j)
	var out state
	for i := 0; i < 6; i++ {
		out[i] = s[i] + h/6*(k1[i]+2*k2[i]+2*k3[i]+k4[i])
	}
	return out
}

// deriv is the time derivative of the GLONASS state: velocity, then the
// gravitational + J₂ + centrifugal + Coriolis + luni-solar acceleration, all in
// the PZ-90 rotating frame so no inertial↔rotating transform is needed
// (docs/MATH.md §3). Works in km.
func deriv(s state, j accel) state {
	x, y, z := s[0], s[1], s[2]
	vx, vy, vz := s[3], s[4], s[5]

	r2 := x*x + y*y + z*z
	r := math.Sqrt(r2)
	r3 := r2 * r

	mu := physconst.GloMuKm
	ae := physconst.GloAeKm
	we := physconst.GloOmegaE
	j2 := physconst.GloJ2

	rho2 := (ae / r) * (ae / r)
	c := -mu / r3
	k := 1.5 * j2 * rho2
	z2r2 := z * z / r2

	ax := c*x*(1+k*(1-5*z2r2)) + we*we*x + 2*we*vy + j[0]
	ay := c*y*(1+k*(1-5*z2r2)) + we*we*y - 2*we*vx + j[1]
	az := c*z*(1+k*(3-5*z2r2)) + j[2]

	return state{vx, vy, vz, ax, ay, az}
}

func addScaled(s state, d state, h float64) state {
	var out state
	for i := 0; i < 6; i++ {
		out[i] = s[i] + d[i]*h
	}
	return out
}
