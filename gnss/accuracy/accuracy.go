// Package accuracy decodes the broadcast signal-in-space accuracy indices to
// metres and reports whether the value is meaningful (the "no accuracy available"
// sentinels). It backs the sisa_valid / sisa_m feed fields and the SISA-change
// integrity alert. Source: docs/MATH.md §6.
package accuracy

import "math"

// URAMeters decodes a GPS/QZSS/NavIC/BeiDou-B1I user-range-accuracy index N
// (0–15) to metres by the nominal-value formula of IS-GPS-200N §20.3.3.3.1.3:
// N≤6 ⇒ 2^(1+N/2), N≥7 ⇒ 2^(N−2). Index 15 is not a value: per the same section
// it "shall indicate the absence of an accuracy prediction and shall advise the
// standard positioning service user to use that SV at his own risk"
// (valid=false). valid=false is NOT "nothing to report" — the caller
// must surface the sentinel index itself (the feed's acc_index / the detector's
// no_accuracy state), or the one broadcast field that says "do not trust this
// SV's accuracy" silently vanishes.
func URAMeters(n int) (float64, bool) {
	if n < 0 || n >= 15 {
		return 0, false
	}
	if n <= 6 {
		return math.Pow(2, 1+float64(n)/2), true
	}
	return math.Pow(2, float64(n)-2), true
}

// GalileoSISA decodes a Galileo SISA index (0–255) to metres in the four linear
// bands of OS-SIS-ICD §5.1.12. 255 ("NO SISA AVAILABLE") and the 126–254 spare
// range are not meaningful (valid=false).
func GalileoSISA(n int) (float64, bool) {
	switch {
	case n < 0:
		return 0, false
	case n <= 49: // 0–0.49 m, 1 cm steps
		return float64(n) * 0.01, true
	case n <= 74: // 0.5–0.98 m, 2 cm steps
		return 0.5 + float64(n-50)*0.02, true
	case n <= 99: // 1–1.96 m, 4 cm steps
		return 1.0 + float64(n-75)*0.04, true
	case n <= 125: // 2–6 m, 16 cm steps
		return 2.0 + float64(n-100)*0.16, true
	default: // 126–254 spare, 255 = NO SISA AVAILABLE
		return 0, false
	}
}

// glonassFT is the GLONASS F_T accuracy table in metres (GLONASS ICD Ed. 5.1);
// index 15 is "not used" (valid=false).
var glonassFT = [15]float64{1, 2, 2.5, 4, 5, 7, 10, 12, 14, 16, 32, 64, 128, 256, 512}

// GlonassFT decodes a GLONASS F_T index (0–15) to metres. Index 15 (or an
// out-of-range value) is not meaningful (valid=false).
func GlonassFT(n int) (float64, bool) {
	if n < 0 || n >= 15 {
		return 0, false
	}
	return glonassFT[n], true
}
