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
	// GST epoch 1999-08-22T00:00:00Z, TAI−UTC = 32 there → GPS−UTC = 13.
	SysGalileo: (935280000 - gpsEpochUnix) + (32 - gpsTAIminusUTC),
	// BDT epoch 2006-01-01T00:00:00Z, TAI−UTC = 33 → GPS−UTC = 14 (BDT = GPST − 14 s).
	SysBeiDou: (1136073600 - gpsEpochUnix) + (33 - gpsTAIminusUTC),
	// IRNWT shares the Galileo epoch (1999-08-22) per docs/MATH.md §1.
	SysNavIC: (935280000 - gpsEpochUnix) + (32 - gpsTAIminusUTC),
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

// DisambiguateWeek recovers a full week number from a truncated broadcast field.
// LNAV sends a 10-bit GPS week (1024-week ambiguity); other messages send wider
// fields. Given the truncated value, the field width in bits, and an approximate
// current Unix time (the ingest wall-clock — "we always know roughly what year it
// is"), it returns the full week nearest that instant. The wall-clock is trusted
// only to pick the rollover cycle, never the low bits (docs/MATH.md §1).
func DisambiguateWeek(sys System, truncated, bits int, approxUnix float64) int {
	off, ok := epochGPSSeconds[sys]
	if !ok || bits <= 0 || bits >= 31 {
		return truncated
	}
	modulus := 1 << bits
	// Approximate full week from the wall-clock, on the same GPS-seconds axis.
	approxGPS := approxUnix - gpsEpochUnix + 18 // ~current GPS−UTC; only picks the cycle
	approxWeek := (approxGPS - off) / WeekSeconds
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
