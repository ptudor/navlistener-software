package gnsstime

import (
	"math"
	"testing"
	"time"
)

func TestEphAgeNoWrap(t *testing.T) {
	// tow just after toe: plain difference, no wrap.
	if got := EphAge(453612, 453600); got != 12 {
		t.Errorf("EphAge = %v, want 12", got)
	}
}

func TestEphAgeForwardWrap(t *testing.T) {
	// Ephemeris near end of week (ref ~604000), measurement early next week
	// (tow ~200): raw tk = 200 − 604000 = −603800 < −HalfWeek → +week.
	got := EphAge(200, 604000)
	want := 200 - 604000 + WeekSeconds
	if got != want {
		t.Errorf("EphAge forward-wrap = %v, want %v", got, want)
	}
	if got <= 0 || got > HalfWeek {
		t.Errorf("wrapped age %v should be a small positive interval", got)
	}
}

func TestEphAgeBackwardWrap(t *testing.T) {
	// Measurement late in week, ref just after rollover: raw tk > HalfWeek → −week.
	got := EphAge(604000, 200)
	want := 604000 - 200 - WeekSeconds
	if got != want {
		t.Errorf("EphAge backward-wrap = %v, want %v", got, want)
	}
}

// TestSOWDelta covers the integer wrapped-delta helper the BeiDou frame
// assemblers use for broadcast adjacency across the weekly rollover.
func TestSOWDelta(t *testing.T) {
	for _, tc := range []struct{ a, b, want int }{
		{106, 100, 6},         // mid-week, no wrap
		{100, 106, -6},        // mid-week, negative
		{0, 604794, 6},        // forward across rollover
		{6, 0, 6},             // adjacent at zero
		{604794, 0, -6},       // backward across rollover
		{0, 604769, 31},       // just outside a ±30 bound, across the boundary
		{302400, 0, 302400},   // exactly half-week keeps its sign (EphAge convention)
		{0, 302400, -302400},  // and mirrors negative
		{1209606, 604794, 12}, // inputs beyond one week still reduce mod 604800
	} {
		if got := SOWDelta(tc.a, tc.b); got != tc.want {
			t.Errorf("SOWDelta(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestEphAgeDay(t *testing.T) {
	// GLONASS day wrap: tb late in day, tod early next day.
	got := EphAgeDay(60, 86300)
	want := 60 - 86300 + DaySeconds
	if got != want {
		t.Errorf("EphAgeDay = %v, want %v", got, want)
	}
}

func TestEphAgeMinutes(t *testing.T) {
	if got := EphAgeMinutes(453900, 453600); math.Abs(got-5) > 1e-9 {
		t.Errorf("EphAgeMinutes = %v, want 5", got)
	}
}

// TestEpochConstants validates the epoch Unix values baked into the GPS-seconds
// table against Go's own calendar, so a mistyped epoch can't slip through.
func TestEpochConstants(t *testing.T) {
	if u := time.Date(1980, 1, 6, 0, 0, 0, 0, time.UTC).Unix(); u != gpsEpochUnix {
		t.Errorf("GPS epoch unix = %d, want %v", u, gpsEpochUnix)
	}
	if u := time.Date(1999, 8, 22, 0, 0, 0, 0, time.UTC).Unix(); u != 935280000 {
		t.Errorf("Galileo epoch unix = %d, want 935280000", u)
	}
	if u := time.Date(2006, 1, 1, 0, 0, 0, 0, time.UTC).Unix(); u != 1136073600 {
		t.Errorf("BeiDou epoch unix = %d, want 1136073600", u)
	}
}

func TestToUnixGPS(t *testing.T) {
	// GPS week 2200, TOW 0 is a week boundary. With ΔtLS = 18, UTC = epoch +
	// 2200 weeks − 18 s. The instant should be 2022-03-06 (a Sunday).
	tm := GNSSTime{Sys: SysGPS, Week: 2200, TOW: 0}
	got, ok := tm.ToUnix(18)
	if !ok {
		t.Fatal("ToUnix not ok for GPS")
	}
	want := gpsEpochUnix + 2200*WeekSeconds - 18
	if got != want {
		t.Errorf("ToUnix GPS = %v, want %v", got, want)
	}
	// GPS time leads UTC by 18 s, so the week boundary lands 18 s before the UTC
	// Sunday midnight — i.e. 2022-03-05 23:59:42 UTC.
	utc := time.Unix(int64(got), 0).UTC()
	if utc.Year() != 2022 || utc.Month() != time.March || utc.Day() != 5 ||
		utc.Hour() != 23 || utc.Minute() != 59 || utc.Second() != 42 {
		t.Errorf("GPS week 2200 TOW 0 → %v; want 2022-03-05 23:59:42 UTC", utc)
	}
}

func TestToUnixBeiDouEpoch(t *testing.T) {
	// BDT week 0, TOW 0 is 2006-01-01T00:00:00Z, where GPS−UTC was 14.
	tm := GNSSTime{Sys: SysBeiDou, Week: 0, TOW: 0}
	got, ok := tm.ToUnix(14)
	if !ok {
		t.Fatal("ToUnix not ok for BeiDou")
	}
	if got != 1136073600 {
		t.Errorf("BDT epoch → unix %v, want 1136073600 (2006-01-01)", got)
	}
}

// TestToUnixGalileoEpoch guards GST(0,0) is 1999-08-21T23:59:47 UTC, 13 s
// *before* the nominal 1999-08-22T00:00:00Z date -- GST was already 13 s ahead of
// UTC at that instant. ToUnix(13) (the GPS-UTC offset at the time) must therefore
// land 13 s before the calendar date's Unix timestamp, not on it.
func TestToUnixGalileoEpoch(t *testing.T) {
	tm := GNSSTime{Sys: SysGalileo, Week: 0, TOW: 0}
	got, ok := tm.ToUnix(13) // GPS−UTC = 13 at 1999-08
	if !ok {
		t.Fatal("ToUnix not ok for Galileo")
	}
	if got != 935279987 {
		t.Errorf("GST epoch → unix %v, want 935279987 (1999-08-21T23:59:47Z, 13s before the nominal date)", got)
	}
}

// TestGalileoEpochAlignsWithGPSWeek1024 guards core claim: GST(0,0) is
// exactly the GPS week-1024 rollover, so a Galileo time and its "GPS week+1024,
// same TOW" twin must denote the identical instant in continuous GPS seconds --
// the property gpsTOW's cross-constellation TOW comparison already relies on.
func TestGalileoEpochAlignsWithGPSWeek1024(t *testing.T) {
	gal := GNSSTime{Sys: SysGalileo, Week: 0, TOW: 0}
	gps, ok := gal.GPSSeconds()
	if !ok {
		t.Fatal("GPSSeconds not ok for Galileo")
	}
	if want := 1024 * WeekSeconds; gps != want {
		t.Errorf("GST(0,0) = %v GPS seconds, want %v (1024 weeks)", gps, want)
	}

	twin := GNSSTime{Sys: SysGPS, Week: 1024, TOW: 12345.5}
	galTwin := GNSSTime{Sys: SysGalileo, Week: 0, TOW: 12345.5}
	gpsTwin, _ := twin.GPSSeconds()
	galTwinSecs, _ := galTwin.GPSSeconds()
	if gpsTwin != galTwinSecs {
		t.Errorf("GPS week 1024 TOW 12345.5 = %v GPS seconds, Galileo week 0 (same TOW) = %v -- want equal", gpsTwin, galTwinSecs)
	}
}

// TestNavICEpochMatchesGalileo guards NavIC half of the fix: IRNWT shares
// Galileo's epoch/convention exactly.
func TestNavICEpochMatchesGalileo(t *testing.T) {
	navic := GNSSTime{Sys: SysNavIC, Week: 3, TOW: 500}
	gal := GNSSTime{Sys: SysGalileo, Week: 3, TOW: 500}
	ns, ok1 := navic.GPSSeconds()
	gs, ok2 := gal.GPSSeconds()
	if !ok1 || !ok2 {
		t.Fatal("GPSSeconds not ok")
	}
	if ns != gs {
		t.Errorf("NavIC GPSSeconds = %v, want equal to Galileo's %v (same epoch/convention)", ns, gs)
	}
}

func TestToUnixGLONASSRejected(t *testing.T) {
	if _, ok := (GNSSTime{Sys: SysGLONASS}).ToUnix(18); ok {
		t.Error("GLONASS is not a continuous-week system; ToUnix should return ok=false")
	}
}

func TestDisambiguateWeek(t *testing.T) {
	// GPS LNAV sends a 10-bit week (mod 1024). Full week 2200 → truncated 152.
	// A wall-clock anywhere in that week must recover 2200.
	full := 2200
	trunc := full % 1024
	approxUnix := gpsEpochUnix + float64(full)*WeekSeconds + 3*DaySeconds // mid-week
	if got := DisambiguateWeek(SysGPS, trunc, 10, approxUnix); got != full {
		t.Errorf("DisambiguateWeek = %d, want %d", got, full)
	}

	// An earlier cycle: full week 1050 → truncated 26.
	full = 1050
	trunc = full % 1024
	approxUnix = gpsEpochUnix + float64(full)*WeekSeconds + DaySeconds
	if got := DisambiguateWeek(SysGPS, trunc, 10, approxUnix); got != full {
		t.Errorf("DisambiguateWeek earlier cycle = %d, want %d", got, full)
	}
}
