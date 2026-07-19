// Package gnsstime handles GNSS time systems: the week/time-of-week
// representation, the mandatory half-week ephemeris-age wrap, GNSS↔UTC
// conversion, and week-number rollover disambiguation. Source: docs/MATH.md §1.
//
// Each constellation runs its own continuous (leap-free) time scale steered near
// UTC — GPS (GPST), Galileo (GST), BeiDou (BDT), QZSS (= GPST), NavIC (IRNWT) —
// except GLONASS, which follows UTC(SU)+3h with leap steps and carries no week or
// full TOW (handled in package glonass). Conversion to UTC uses the broadcast
// leap-second count; this package transcribes, it does not invent, offsets.
package gnsstime

import "math"

// Fundamental week arithmetic constants (docs/MATH.md §1.1).
const (
	WeekSeconds = 604800.0 // seconds in a GNSS week
	HalfWeek    = 302400.0 // half a week; the ephemeris-age wrap threshold
	DaySeconds  = 86400.0  // seconds in a day (the GLONASS wrap threshold)

	// gpsEpochUnix is 1980-01-06T00:00:00Z in Unix seconds — the GPS time epoch.
	gpsEpochUnix = 315964800.0
	// gpsTAIminusUTC is TAI−UTC at the GPS epoch (19 s). GPS−UTC = (TAI−UTC) − 19,
	// so the broadcast leap count ΔtLS (currently 18) is exactly GPS−UTC.
	gpsTAIminusUTC = 19.0
)

// System identifies a constellation's time scale. QZSS shares GPST, so it has no
// separate System.
type System uint8

const (
	SysGPS     System = iota // GPST (also QZSST)
	SysGalileo               // GST
	SysBeiDou                // BDT
	SysNavIC                 // IRNWT
	SysGLONASS               // GLONASST (UTC(SU)+3h, leap-stepped)
)

// EphAge returns the half-week-corrected elapsed time tk = tow − ref (seconds),
// where ref is an ephemeris reference epoch (toe) or clock reference (toc) in
// time-of-week seconds. The ±half-week wrap handles a measurement that straddled
// the week boundary from the reference. This correction is mandatory and appears
// in every Keplerian propagation and clock evaluation (docs/MATH.md §1.1).
func EphAge(tow, ref float64) float64 {
	tk := tow - ref
	if tk > HalfWeek {
		tk -= WeekSeconds
	} else if tk < -HalfWeek {
		tk += WeekSeconds
	}
	return tk
}

// EphAgeDay is the GLONASS analogue of EphAge: elapsed seconds tk = tod − tb
// wrapped around the 86400 s day boundary, since GLONASS has no TOW, only a
// time-of-day (docs/MATH.md §1.1).
func EphAgeDay(tod, tb float64) float64 {
	tk := tod - tb
	if tk > DaySeconds/2 {
		tk -= DaySeconds
	} else if tk < -DaySeconds/2 {
		tk += DaySeconds
	}
	return tk
}

// EphAgeMinutes returns EphAge in minutes — the eph_age_m feed field
// (docs/OUTPUT.md §1.1).
func EphAgeMinutes(tow, ref float64) float64 { return EphAge(tow, ref) / 60.0 }

// SOWDelta returns a − b in integer seconds-of-week, wrapped to ±half-week —
// the integer analogue of EphAge for frame adjacency/staleness checks. Every
// broadcast-adjacency rule that subtracts two raw SOW fields must use this
// wrap, or a legitimate set straddling the weekly rollover (e.g. BeiDou D1
// subframes at 604794 → 0 → 6) is rejected as a splice once per week.
func SOWDelta(a, b int) int {
	const week = int(WeekSeconds)
	d := (a - b) % week
	if d > week/2 {
		d -= week
	} else if d < -week/2 {
		d += week
	}
	return d
}

// GNSSTime is a continuous-scale time as broadcast: system + full week number +
// time-of-week seconds. The week must be the full (disambiguated) week, not a
// truncated broadcast field — use DisambiguateWeek first.
type GNSSTime struct {
	Sys  System
	Week int
	TOW  float64
}

// epoch holds a system's epoch expressed in continuous GPS seconds — the seconds
// from the GPS epoch (1980-01-06) to this system's week-0/TOW-0 instant, on the
// leap-free GPS/TAI rate. Computing everything in GPS seconds folds the
// per-system epoch offset (including BeiDou's 14 s) into one constant.
var epochGPSSeconds = map[System]float64{
	SysGPS: 0,
	// GST(0,0) is 1999-08-21T23:59:47 UTC -- 13 s *before* the nominal
	// 1999-08-22T00:00:00Z UTC date, because GST was already 13 s ahead of UTC at
	// that instant. GST(0,0) is exactly the GPS week-1024 rollover, so GST WN/TOW
	// equals GPS WN/TOW (WN+1024)/TOW with zero offset -- the property gpsTOW's
	// comment and weekFor already rely on. The epoch is 935280000 - gpsEpochUnix
	// (= 1024*WeekSeconds) with no additional leap-second term folded in; the
	// previous "+ (32 - gpsTAIminusUTC)" incorrectly treated 935280000 as a UTC
	// timestamp of GST(0,0) itself rather than of the nominal calendar date 13 s
	// after it.
	SysGalileo: 935280000 - gpsEpochUnix,
	// BDT epoch 2006-01-01T00:00:00Z, TAI−UTC = 33 → GPS−UTC = 14 (BDT = GPST − 14 s).
	SysBeiDou: (1136073600 - gpsEpochUnix) + (33 - gpsTAIminusUTC),
	// IRNWT shares the Galileo epoch/convention (1999-08-22, WN+1024=GPS WN,
	// TOW aligned) per docs/MATH.md §1 -- same correction as SysGalileo above.
	SysNavIC: 935280000 - gpsEpochUnix,
}

// GPSSeconds converts t to continuous seconds since the GPS epoch on the GPS/TAI
// (leap-free) rate. Valid for the continuous systems (GPS/QZSS/Galileo/BeiDou/
// NavIC); GLONASS is not a continuous-week system and is not accepted here.
func (t GNSSTime) GPSSeconds() (float64, bool) {
	off, ok := epochGPSSeconds[t.Sys]
	if !ok {
		return 0, false
	}
	return off + float64(t.Week)*WeekSeconds + t.TOW, true
}

// ToUnix converts t to a Unix timestamp (UTC seconds), given the current
// broadcast GPS−UTC offset in whole leap seconds (ΔtLS, currently 18). Because
// every continuous system is reduced to GPS seconds first, the same leap offset
// applies to all of them. Returns ok=false for GLONASS or an unknown system.
func (t GNSSTime) ToUnix(gpsMinusUTC float64) (float64, bool) {
	gs, ok := t.GPSSeconds()
	if !ok {
		return 0, false
	}
	return gpsEpochUnix + gs - gpsMinusUTC, true
}

// SystemSeconds returns the continuous seconds elapsed since sys's own epoch
// (week-0/TOW-0) at the given Unix instant — the single wall-clock→GNSS-axis
// reduction every derived quantity below builds on : unix is converted
// to GPS seconds with the caller-supplied current GPS−UTC leap offset, then
// shifted by the audited epochGPSSeconds table, so every per-system epoch
// constant (Galileo's week-1024 alignment, BeiDou's 1356-week + 14 s fold,
// regression fix/regression fix/regression fix) lives in exactly ONE place. ok=false for GLONASS (not a
// continuous-week system) or an unknown system.
func SystemSeconds(sys System, unix, gpsMinusUTC float64) (float64, bool) {
	off, ok := epochGPSSeconds[sys]
	if !ok {
		return 0, false
	}
	return unix - gpsEpochUnix + gpsMinusUTC - off, true
}

// WeekAt returns sys's own full (untruncated) week number at the given Unix
// instant, using the caller-supplied current GPS−UTC leap offset (it only
// selects the week, so whole-second accuracy is ample). this is the
// "expected week" side of a broadcast-vs-receiver week cross-check — pair it
// with DisambiguateWeek on the broadcast side, same sys — generalized here so
// the check works on any continuous-week system's own numbering (GST week =
// GPS week − 1024, etc.) instead of hardcoding the GPS axis. ok=false for
// GLONASS (no week number) or an unknown system.
func WeekAt(sys System, unix, gpsMinusUTC float64) (int, bool) {
	s, ok := SystemSeconds(sys, unix, gpsMinusUTC)
	if !ok {
		return 0, false
	}
	return int(math.Floor(s / WeekSeconds)), true
}

// TOWAt returns sys's own time-of-week (seconds, in [0, WeekSeconds)) at the
// given Unix instant  — the wall-clock counterpart of a broadcast
// toe/tow, used as the propagation target. Positive-modulo so instants before
// a week boundary still land in the previous week's tail. ok=false for GLONASS
// (time-of-day system — package glonass / the caller's gloTOD own that axis)
// or an unknown system.
func TOWAt(sys System, unix, gpsMinusUTC float64) (float64, bool) {
	s, ok := SystemSeconds(sys, unix, gpsMinusUTC)
	if !ok {
		return 0, false
	}
	tow := math.Mod(s, WeekSeconds)
	if tow < 0 {
		tow += WeekSeconds
	}
	return tow, true
}

// DisambiguateWeek recovers a full week number from a truncated broadcast field.
// LNAV sends a 10-bit GPS week (1024-week ambiguity); other messages send wider
// fields. Given the truncated value, the field width in bits, an approximate
// current Unix time (the ingest wall-clock — "we always know roughly what year it
// is"), and the current GPS−UTC leap offset, it returns the full week nearest
// that instant. The wall-clock is trusted only to pick the rollover cycle, never
// the low bits (docs/MATH.md §1). gpsMinusUTC is a parameter, not the
// previous compiled-in 18 — the daemon's ΔtLS is settable ([state].leap_seconds,
// regression fix), and a literal here would silently diverge from it after a real leap
// event. The leap term only picks the cycle (it is divided by 604800 s), so even
// a few seconds' error is harmless — but one source of truth is the point.
func DisambiguateWeek(sys System, truncated, bits int, approxUnix, gpsMinusUTC float64) int {
	if bits <= 0 || bits >= 31 {
		return truncated
	}
	approxGPS, ok := SystemSeconds(sys, approxUnix, gpsMinusUTC)
	if !ok {
		return truncated
	}
	modulus := 1 << bits
	approxWeek := approxGPS / WeekSeconds
	base := int(math.Round(approxWeek/float64(modulus))) * modulus
	full := base + truncated
	// Snap to the nearest cycle in case truncated sits just across a boundary.
	if d := full - int(math.Round(approxWeek)); d > modulus/2 {
		full -= modulus
	} else if d < -modulus/2 {
		full += modulus
	}
	return full
}
