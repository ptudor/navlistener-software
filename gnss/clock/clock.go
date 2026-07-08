// Package clock computes the satellite clock correction: the broadcast
// polynomial, the relativistic periodic term, and the group-delay bias, plus the
// broadcast system→UTC offset. Source: docs/MATH.md §4, IS-GPS-200 §20.3.3.3.3.1.
// The relativistic term reuses the eccentric anomaly E from the kepler solve at
// the same epoch — it is not re-derived (docs/MATH.md §4).
package clock

import (
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// Model is one decoded broadcast clock model (coefficients already scaled to SI).
type Model struct {
	ID  gnss.GNSSID // constellation (selects the relativity constant F)
	Af0 float64     // clock bias, seconds
	Af1 float64     // clock drift, seconds/second
	Af2 float64     // clock drift rate, seconds/second²
	Toc float64     // clock reference time, seconds of week
	TGD float64     // group delay for the tracked signal, seconds (already scaled)
}

// Relativistic returns the relativistic clock correction Δtr = F·e·√A·sin E
// (seconds), where F is the constellation's relativity constant and E is the
// eccentric anomaly from the orbit solve. It is zero for a circular orbit (e=0)
// and for GLONASS (F=0, Cartesian model).
func Relativistic(relF, ecc, sqrtA, eccAnom float64) float64 {
	return relF * ecc * sqrtA * math.Sin(eccAnom)
}

// Offset returns the SV clock correction Δtsv (seconds) at time-of-week tow,
// given the eccentric anomaly E, eccentricity, and √A from the orbit solve at the
// same tow. Δtsv = af0 + af1·Δt + af2·Δt² + Δtr − TGD, with Δt half-week corrected
// against the clock reference toc (docs/MATH.md §4). The group delay carried in
// the model is the one for the tracked signal; a dual-frequency ionosphere-free
// user cancels it (pass TGD=0).
func Offset(c Model, tow, ecc, sqrtA, eccAnom float64) float64 {
	dt := gnsstime.EphAge(tow, c.Toc)
	var relF float64
	if p, ok := physconst.For(c.ID); ok {
		relF = p.RelF
	}
	rel := Relativistic(relF, ecc, sqrtA, eccAnom)
	return c.Af0 + c.Af1*dt + c.Af2*dt*dt + rel - c.TGD
}

// OffsetFor is the convenience form: it solves the orbit at tow to obtain E, then
// returns the clock correction. The ephemeris and clock model must be for the
// same SV. It returns the kepler solve error on degenerate orbital input.
func OffsetFor(c Model, e kepler.Ephemeris, tow float64) (float64, error) {
	sol, err := kepler.Solve(e, tow)
	if err != nil {
		return 0, err
	}
	return Offset(c, tow, e.Ecc, e.SqrtA, sol.E), nil
}

// L2GroupDelayFactor is γ = (f_L1/f_L2)², the factor by which a single-frequency
// L2 user scales TGD relative to the broadcast (L1-referenced) value
// (docs/MATH.md §4). f_L1 = 1575.42 MHz, f_L2 = 1227.60 MHz.
const L2GroupDelayFactor = (1575.42 / 1227.60) * (1575.42 / 1227.60)

// UTCParams are the broadcast GNSS→UTC parameters (docs/MATH.md §4, §8).
type UTCParams struct {
	A0    float64 // constant term, seconds
	A1    float64 // rate, seconds/second
	Tot   float64 // reference time of the UTC data, seconds of week
	DtLS  float64 // current leap-second count (whole seconds)
	DtLSF float64 // scheduled future leap-second count
}

// UTCOffset returns the system→UTC offset (seconds) at time-of-week tow:
// A0 + A1·(tow − tot) + ΔtLS, half-week corrected (IS-GPS-200 §20.3.3.5.2.4). The
// scheduled leap (DtLSF/WNLSF/DN) governs the pending step and is handled by the
// caller near a leap event; here we apply the current ΔtLS.
func UTCOffset(u UTCParams, tow float64) float64 {
	dt := gnsstime.EphAge(tow, u.Tot)
	return u.A0 + u.A1*dt + u.DtLS
}
