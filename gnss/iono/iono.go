// Package iono computes broadcast ionospheric delay. The single-frequency
// Klobuchar model is implemented in full and serves GPS L1, QZSS, and NavIC —
// NAVIC-SPS-L5S App. H prescribes the identical algorithm, verified term for
// term against IS-GPS-200N Figure 20-4 (docs/MATH.md §7.1). Galileo NeQuick-G
// and BeiDou's distinct B1I model and BDGIM are documented follow-ups
// (docs/MATH.md §7). Delay is returned in seconds at the coefficient set's
// reference frequency — L1 for GPS/QZSS, L5 for NavIC (App. H: "Tiono is
// referred to the L5 frequency") — and scales to another frequency by
// (f_ref/f)².
package iono

import "math"

// Klobuchar returns the ionospheric group delay (seconds) from the 8 broadcast
// coefficients, for a user at geodetic (userLat, userLon) observing a satellite at
// azimuth az and elevation el (all radians), at time-of-week tow (seconds) in the
// broadcasting constellation's own system time (GPS/QZSS TOW, or IRNSS time for
// NavIC — NAVIC-SPS-L5S App. H). The algorithm follows IS-GPS-200N §20.3.3.5.2.5
// (docs/MATH.md §7.1): latitudes, longitudes, and elevation work in semicircles
// internally; azimuth is used in radians (regression fix — NAVIC-SPS-L5S App. H states
// that reading explicitly: "All the parameters used in trigonometric functions
// are converted to radians before applying the specific operation").
//
// The result is referred to the coefficient set's reference frequency: L1 for
// GPS/QZSS, L5 for NavIC (App. H; an S-band user scales by (f_L5/f_S)²) — the
// side-by-side verification is recorded in docs/MATH.md §7.1. BeiDou B1I uses
// the materially different BDS-SIS-B1I-3.0 §5.2.4.7 model and must not be run
// through this function.
func Klobuchar(alpha, beta [4]float64, userLat, userLon, az, el, tow float64) float64 {
	// the model is defined for el >= 0 (IS-GPS-200N §20.3.3.5.2.5); at
	// el = -0.11π rad (-19.8°) the earth-centred-angle term below divides by
	// zero, and any negative elevation (AzEl can produce one — regression fix) yields an
	// out-of-validity obliquity. Clamped, not rejected, matching this package's
	// no-error guard style (ScaleDelay/EffectiveIonisation take their inputs on
	// faith too) — a below-horizon SV is a caller bug elsewhere, not something
	// this pure math function should panic or error over.
	if el < 0 {
		el = 0
	}
	const rad2semi = 1.0 / math.Pi
	phiU := userLat * rad2semi // user geodetic latitude, semicircles
	lamU := userLon * rad2semi
	e := el * rad2semi // elevation, semicircles

	psi := 0.0137/(e+0.11) - 0.022 // earth-centred angle, semicircles

	phiI := phiU + psi*math.Cos(az) // IPP latitude, semicircles
	if phiI > 0.416 {
		phiI = 0.416
	} else if phiI < -0.416 {
		phiI = -0.416
	}

	lamI := lamU + psi*math.Sin(az)/math.Cos(phiI*math.Pi) // IPP longitude
	phiM := phiI + 0.064*math.Cos((lamI-1.617)*math.Pi)    // geomagnetic latitude

	t := 43200*lamI + tow // local time at the IPP, seconds
	t = math.Mod(t, 86400)
	if t < 0 {
		t += 86400
	}

	amp := horner(phiM, alpha) // amplitude, seconds
	if amp < 0 {
		amp = 0
	}
	per := horner(phiM, beta) // period, seconds
	if per < 72000 {
		per = 72000
	}

	x := 2 * math.Pi * (t - 50400) / per // phase
	f := 1 + 16*math.Pow(0.53-e, 3)      // obliquity (slant) factor

	const night = 5e-9 // night-time floor, seconds
	if math.Abs(x) < 1.57 {
		return f * (night + amp*(1-x*x/2+x*x*x*x/24))
	}
	return f * night
}

// horner evaluates the cubic c0 + c1·x + c2·x² + c3·x³.
func horner(x float64, c [4]float64) float64 {
	return c[0] + x*(c[1]+x*(c[2]+x*c[3]))
}

// L1Hz, L2Hz, L5Hz are the carrier frequencies used for frequency scaling
// (docs/MATH.md §2.2). Ionospheric delay scales as (f_L1/f)².
const (
	L1Hz = 1575.42e6
	L2Hz = 1227.60e6
	L5Hz = 1176.45e6
)

// ScaleDelay converts an L1 delay (seconds) to the delay on frequency fHz, using
// the dispersive (f_L1/f)² relation (docs/MATH.md §7). L1-referred only: NavIC
// Klobuchar output is L5-referred (NAVIC-SPS-L5S App. H) and must be scaled from
// L5, not fed through this helper.
func ScaleDelay(delayL1, fHz float64) float64 {
	r := L1Hz / fHz
	return delayL1 * r * r
}

// NeQuickG holds the three Galileo broadcast effective-ionisation coefficients
// (OS-SIS-ICD). The full NeQuick-G profile integration is a documented follow-up;
// this decodes the coefficients and the effective ionisation level driving it.
type NeQuickG struct {
	A0, A1, A2 float64 // effective ionisation coefficients (sfu, sfu/deg, sfu/deg²)
}

// EffectiveIonisation returns Az = a0 + a1·MODIP + a2·MODIP² (solar flux units),
// the ionisation level that drives the NeQuick-G profile (docs/MATH.md §7.2).
// MODIP is the modified dip latitude in degrees from the modip grid.
func (n NeQuickG) EffectiveIonisation(modipDeg float64) float64 {
	return n.A0 + n.A1*modipDeg + n.A2*modipDeg*modipDeg
}

// BDGIM holds the nine BeiDou Global Ionospheric delay correction Model
// coefficients from B-CNAV (BDS-SIS-B1C-1.0 §7.8; identically BDS-SIS-B2a-1.0
// §7.8 — both ICDs print the same model chapter). The spherical-harmonic
// evaluation is a documented follow-up; this carries the decoded coefficients.
type BDGIM struct {
	Alpha [9]float64
}
