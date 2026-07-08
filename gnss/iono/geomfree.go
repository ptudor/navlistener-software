// Geometry-free dual-frequency ionosphere measurement (docs/MATH.md §7.4).
//
// A receiver tracking two frequencies of one SV measures the actual first-order
// slant ionospheric delay on that line of sight: orbit, clocks, and troposphere
// are common to both signals and cancel in the difference. The code (pseudorange)
// combination is unbiased but noisy; the carrier-phase combination is smooth but
// carries an unknown per-arc ambiguity, so it is leveled to the code over a
// continuous tracking arc. Satellite and receiver differential code biases are
// the caller's to remove (broadcast TGD/BGD for the satellite, a per-receiver
// calibration for the receiver — docs/MATH.md §7.4).
package iono

import "math"

// Gamma returns γ = (f1/f2)², the dispersive ratio between two carriers.
func Gamma(f1Hz, f2Hz float64) float64 {
	r := f1Hz / f2Hz
	return r * r
}

// SlantFromCode returns the slant ionospheric delay at f1 (metres) from one
// code geometry-free sample: Î₁ = (P₂ − P₁ − biasM)/(γ − 1). biasM is the sum
// of the satellite and receiver differential code biases in metres for this
// signal pair; pass 0 to obtain the biased (relative) measurement.
func SlantFromCode(p1M, p2M, f1Hz, f2Hz, biasM float64) float64 {
	return (p2M - p1M - biasM) / (Gamma(f1Hz, f2Hz) - 1)
}

// Arc carrier-levels the phase geometry-free combination to the code one over a
// continuous tracking arc. Feed it one (code, phase) geometry-free pair per
// epoch; reset on loss of lock or cycle slip (the receiver's lock-time counter
// going backwards is the reset signal upstream). The leveled slant uses the
// smooth phase with the running mean of (P_GF − Φ_GF) as the ambiguity estimate,
// so its noise falls as the arc grows instead of tracking the code noise.
type Arc struct {
	n        int
	meanDiff float64 // running mean of (pGF − phiGF), metres
}

// Add accumulates one epoch's geometry-free pair: pGF = P₂−P₁, phiGF = Φ₁−Φ₂
// (both metres; both equal I₁·(γ−1) plus their respective biases).
func (a *Arc) Add(pGFm, phiGFm float64) {
	a.n++
	a.meanDiff += (pGFm - phiGFm - a.meanDiff) / float64(a.n)
}

// Reset discards the arc (loss of lock / cycle slip).
func (a *Arc) Reset() { *a = Arc{} }

// Count reports how many epochs the current arc has accumulated.
func (a *Arc) Count() int { return a.n }

// MinArc is the number of epochs an arc must accumulate before its leveled
// output is published; below this the ambiguity estimate is still code-noise
// dominated and the smooth phase buys nothing.
const MinArc = 10

// Slant returns the carrier-leveled slant delay at f1 (metres) for the current
// epoch's phase geometry-free value, and whether the arc is mature (n ≥ MinArc).
func (a *Arc) Slant(phiGFm, f1Hz, f2Hz, biasM float64) (float64, bool) {
	if a.n < MinArc {
		return 0, false
	}
	return (phiGFm + a.meanDiff - biasM) / (Gamma(f1Hz, f2Hz) - 1), true
}

// Thin-shell mapping (docs/MATH.md §7.4): the slant ray pierces a single-layer
// ionosphere at ShellHeightM; the obliquity factor maps slant to vertical.
const (
	earthRadiusM = 6371e3 // mean Earth radius for the thin-shell model
	ShellHeightM = 350e3  // conventional single-layer ionosphere height
)

// Obliquity returns the thin-shell mapping factor M(E) ≥ 1 for elevation elRad:
// sin χ = R_E/(R_E+h)·cos E, M = 1/cos χ. Vertical = slant / M.
func Obliquity(elRad float64) float64 {
	sinChi := earthRadiusM / (earthRadiusM + ShellHeightM) * math.Cos(elRad)
	return 1 / math.Sqrt(1-sinChi*sinChi)
}

// TECUToMetres returns the group delay in metres per TECU at frequency fHz:
// I[m] = 40.308×10¹⁶·TEC/f². At GPS L1 this is 0.162 m/TECU.
func TECUToMetres(fHz float64) float64 {
	return 40.308e16 / (fHz * fHz)
}

// VTEC maps a slant delay at fHz (metres) and elevation to vertical TEC (TECU).
func VTEC(slantM, fHz, elRad float64) float64 {
	return slantM / Obliquity(elRad) / TECUToMetres(fHz)
}
