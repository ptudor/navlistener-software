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
//
// the NavIC claim is not a GPS-shape assumption — the IRNSS SPS ICD
// independently specifies the identical nominal-value formula ("If the value
// of N is 6 or less, X = 2(1 + N/2) … but less than 15, X = 2(N − 2)") and the
// same N=15 no-prediction sentinel (NAVIC-SPS-L5S §6.2.1.4, Table 23). Note
// Table 23's rounding advice for N = 1/3/5 (2.8/5.7/11.3 m) matches
// IS-GPS-200N's, so no NavIC-specific table is needed here.
func URAMeters(n int) (float64, bool) {
	if n < 0 || n >= 15 {
		return 0, false
	}
	if n <= 6 {
		return math.Pow(2, 1+float64(n)/2), true
	}
	return math.Pow(2, float64(n)-2), true
}

// URAEDMeters decodes a CNAV elevation-dependent URA_ED index N (a SIGNED
// two's-complement integer, +15..−16 — IS-GPS-200N §30.3.3.1.1.4) to metres by
// that section's nominal-value formula: −16 < N ≤ 6 ⇒ 2^(1+N/2), 6 ≤ N < 15 ⇒
// 2^(N−2) — the same shape as the LNAV URA formula extended below zero (e.g.
// N=−1 ⇒ ~1.41 m, N=−16 excluded). N = 15 and N = −16 both "indicate the
// absence of an accuracy prediction and shall advise the standard positioning
// service user to use that SV at his own risk" (valid=false; surface the raw
// index per regression fix, as with URAMeters).
//
// regression fix asked for this attribution to be reassigned to LNAV §20.3.3.3.1.3 on
// the premise that §30.3.3.1.1.4 "prints no formula". Re-verified against the
// vendored IS-GPS-200N (01-AUG-2022) and NOT changed: §30.3.3.1.1.4 does print
// it, immediately below its band table and before the §30.3.3.1.2 heading —
// "For each URAED index (N), users may compute a nominal URAED value (X) as
// given by: • If the value of N is 6 or less, but more than -16, X = 2(1+N/2),
// • If the value of N is 6 or more, but less than 15, X = 2(N-2)". The signed
// guard ("but more than -16") is the CNAV section's own wording, so citing
// §30.3.3.1.1.4 is the accurate provenance; re-pointing it at LNAV would be a
// citation regression. residual observation does stand and is worth
// recording: the nominal values do not all sit inside their tabulated bands —
// N = −15 nominally yields 2^−6.5 ≈ 0.011 m against that row's "URAED ≤ 0.01"
// ceiling. That is an ICD-internal rounding artefact at the most-accurate
// index, sub-centimetre, and no consumer of sisa_m distinguishes 0.010 from
// 0.011 m, so the nominal formula (which the ICD instructs users to apply)
// stays authoritative here.
func URAEDMeters(n int) (float64, bool) {
	if n <= -16 || n >= 15 {
		return 0, false
	}
	if n <= 6 {
		return math.Pow(2, 1+float64(n)/2), true
	}
	return math.Pow(2, float64(n)-2), true
}

// GalileoSISA decodes a Galileo SISA index (0–255) to metres in the four linear
// bands of GAL-OS-SIS-ICD-2.2 §5.1.12 Table 91. 255 ("No Accuracy Prediction
// Available (NAPA)") and the 126–254 spare range are not meaningful
// (valid=false). regression fix (the Galileo sibling of URA-15 rule):
// valid=false is NOT "nothing to report" — §5.1.12 states that SISA = NAPA "is
// an indicator of a potential anomalous SIS", a broadcast integrity signal, not
// a missing value. The caller must surface the raw index itself (the feed's
// acc_index / the detector's no_accuracy state) so a consumer can distinguish
// NAPA (255) and the spare range from "no accuracy field decoded yet"; folding
// the sentinel into absence silenced the one broadcast field that disclaims the
// SV's accuracy.
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
	default: // 126–254 spare, 255 = NAPA (Table 91) — sentinel, see doc comment
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
