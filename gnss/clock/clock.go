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
	// TGD is the group delay for the tracked signal, seconds, ALREADY SCALED by
	// the caller: the broadcast value is L1-referenced, so a single-frequency L2
	// user must multiply it by L2GroupDelayFactor (γ) before populating this
	// field (IS-GPS-200N §20.3.3.3.3.2), and a dual-frequency ionosphere-free
	// user passes 0 — nothing in this package applies γ automatically.
	TGD float64
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
// (IS-GPS-200N §20.3.3.3.3.2; docs/MATH.md §4). f_L1 = 1575.42 MHz, f_L2 =
// 1227.60 MHz. this constant is the CALLER'S tool for producing the
// already-scaled Model.TGD — no code path in this library applies it
// automatically (the daemon tracks L1, and its computeDisco zeroes TGD on both
// sides, so γ never enters); an L2-only consumer passing the broadcast TGD
// unscaled would carry a (γ−1)·TGD ≈ 0.65·TGD bias.
const L2GroupDelayFactor = (1575.42 / 1227.60) * (1575.42 / 1227.60)

// E5aGroupDelayFactor is (f_E1/f_E5a)², the Galileo Eq. 19 factor by which a
// single-frequency E5a user scales the broadcast BGD(E1,E5a) before applying it
// to the F/NAV (E1,E5a) clock (GAL-OS-SIS-ICD-2.2 §5.1.5: Eq. 18 is the f1 = E1
// user, no scaling; Eq. 19 is the f2 user, ×(f1/f2)²; Table 71 fixes the F/NAV
// clock as the (E1,E5a) pair and its service as single-frequency E5a). Carriers
// per Table 2: f_E1 = 1575.420 MHz, f_E5a = 1176.450 MHz, so the factor is
// ≈ 1.7933. like L2GroupDelayFactor above (the regression fix rule), this is
// the CALLER'S tool for producing the already-scaled Model.TGD — nothing in
// this package applies it automatically. The frame package's F/NAV assembler is
// that caller: the daemon's E##@3 entries track E5a, the f2 signal.
const E5aGroupDelayFactor = (1575.420 / 1176.450) * (1575.420 / 1176.450)

// UTCParams are the broadcast GNSS→UTC parameters (docs/MATH.md §4, §8).
// The two-term A0/A1 set is the IS-GPS-200 LNAV shape; the CNAV-generation
// messages (and BeiDou B-CNAV2 MT34, BDS-SIS-ICD-B2a v1.0 Table 7-20 — regression fix)
// add the A2 drift-rate term and carry full (untruncated) reference weeks.
// Zero-valued extras are harmless: a two-term source simply leaves them 0.
type UTCParams struct {
	A0    float64 // constant term, seconds
	A1    float64 // rate, seconds/second
	A2    float64 // drift-rate term, seconds/second² (0 for two-term sources)
	Tot   float64 // reference time of the UTC data, seconds of week
	WNot  int     // reference week number of the UTC data (system week; 0 if the source set carries none)
	DtLS  float64 // current (or past, pre-event) leap-second count (whole seconds)
	DtLSF float64 // leap-second count after the WNLSF/DN event (current or future)
	WNLSF int     // leap event reference week number
	DN    int     // leap event day number within WNLSF (BeiDou: 0–6, Table 7-20; GPS LNAV: 1–7)
}

// UTCOffset returns the system→UTC offset (seconds) at time-of-week tow:
// A0 + A1·(tow − tot) + A2·(tow − tot)² + ΔtLS, half-week corrected
// (IS-GPS-200 §20.3.3.5.2.4; the A2 term per BDS-SIS-ICD-B2a v1.0 Eq. 7-25 is
// zero for two-term sources). The scheduled leap (DtLSF/WNLSF/DN) governs the
// pending step and is handled by the caller near a leap event; here we apply
// the current ΔtLS.
//
// regression fix (recorded deviation, accepted): the ICD forms the A1 term over the
// true week-spanning difference tE − tot + 604800·(WN − WNt); this
// implementation substitutes the ±half-week wrap (EphAge) and carries no WNt,
// which is identical within ±half a week of the reference and diverges beyond.
// A1 is spec-bounded near 1e-15 s/s, so the divergence is sub-nanosecond
// against the 2.5 ns integrity threshold — leave-as-is is the disposition.
// UTCParams now carries WNot/WNLSF/DN, so a caller with an absolute
// epoch in hand can evaluate the exact week-spanning form and the
// leap-transition arm itself (the navlistener feed does); this
// tow-only convenience stays on the wrapped axis and the current ΔtLS.
func UTCOffset(u UTCParams, tow float64) float64 {
	dt := gnsstime.EphAge(tow, u.Tot)
	return u.A0 + u.A1*dt + u.A2*dt*dt + u.DtLS
}
