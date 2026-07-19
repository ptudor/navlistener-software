package detect

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

func band(dep float64, cw, ant, jam int) state.StationRFBand {
	return state.StationRFBand{Block: 0, AGCDeparture: &dep, CWSuppress: cw, AntStatus: ant, JamState: jam}
}

func station(id string, bands ...state.StationRFBand) map[string]state.StationRF {
	return map[string]state.StationRF{id: {ID: id, Bands: bands, RFTrust: 1}}
}

// find returns the first event of a type, or a zero Event.
func find(evs []Event, typ string) (Event, bool) {
	for _, e := range evs {
		if e.Type == typ {
			return e, true
		}
	}
	return Event{}, false
}

// TestRFJammingConfirmed drives a corroborated jamming departure through the debounce: a
// large AGC departure plus a CW spike must confirm as jamming_detected after the window,
// not before.
func TestRFJammingConfirmed(t *testing.T) {
	d := New(0) // 60 s debounce
	t0 := time.Unix(1_700_000_000, 0)
	clear := station("stn1", band(0, 0, 2, 1))
	jam := station("stn1", band(1200, 220, 2, 1)) // AGC departure + CW tone

	d.TickStations(t0, clear) // seed ok
	if evs := d.TickStations(t0.Add(20*time.Second), jam); len(evs) != 0 {
		t.Fatalf("confirmed before debounce: %+v", evs)
	}
	evs := d.TickStations(t0.Add(85*time.Second), jam)
	e, ok := find(evs, "jamming_detected")
	if !ok {
		t.Fatalf("no jamming_detected after debounce; got %+v", evs)
	}
	if e.NewValue != "warn" || e.Severity != SevWarning {
		t.Errorf("jamming event = %+v, want warn/1", e)
	}
}

// TestRFJammingSevereImmediateBand confirms a near-total AGC collapse classifies critical
// regardless of CW corroboration (it is unambiguous).
func TestRFJammingSevereCrit(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 1)))
	d.TickStations(t0.Add(10*time.Second), station("s", band(2500, 0, 2, 0))) // huge departure, no CW
	evs := d.TickStations(t0.Add(75*time.Second), station("s", band(2500, 0, 2, 0)))
	e, ok := find(evs, "jamming_detected")
	if !ok || e.NewValue != "crit" || e.Severity != SevCritical {
		t.Fatalf("severe jamming = %+v (ok=%v), want crit/2", e, ok)
	}
}

// TestRFMissingBandTelemetryHoldsAlarm guards a live station whose MON-RF
// evidence aged out must not recover a confirmed alarm merely because an empty band
// slice initializes every aggregate to zero.
func TestRFMissingBandTelemetryHoldsAlarm(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	clear := station("s", band(0, 0, 2, 0))
	crit := station("s", band(2500, 0, 2, 0))
	d.TickStations(t0, clear)
	d.TickStations(t0.Add(10*time.Second), crit)
	if e, ok := find(d.TickStations(t0.Add(75*time.Second), crit), "jamming_detected"); !ok || e.NewValue != "crit" {
		t.Fatalf("failed to seed confirmed critical state: %+v", e)
	}
	mean, resid := 40.0, 10.0
	missingBands := map[string]state.StationRF{"s": {ID: "s", Cn0Mean: &mean, Cn0Resid: &resid}}
	if evs := d.TickStations(t0.Add(150*time.Second), missingBands); len(evs) != 0 {
		t.Fatalf("missing band evidence emitted a recovery: %+v", evs)
	}
	if evs := d.TickStations(t0.Add(230*time.Second), missingBands); len(evs) != 0 {
		t.Fatalf("missing band evidence confirmed a recovery: %+v", evs)
	}
	// Actual clear evidence should still recover the held machine.
	d.TickStations(t0.Add(240*time.Second), clear)
	e, ok := find(d.TickStations(t0.Add(310*time.Second), clear), "jamming_detected")
	if !ok || e.NewValue != "ok" {
		t.Fatalf("measured clear state did not recover held alarm: %+v", e)
	}
}

// TestRFDegradedNotJamming confirms a lone metric departure (AGC down, but no CW and no
// receiver jam flag) reports as station_rf_degraded, not a jamming attack claim.
func TestRFDegradedNotJamming(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 0)))
	uncorr := station("s", band(1200, 0, 2, 0)) // departure alone, uncorroborated
	d.TickStations(t0.Add(10*time.Second), uncorr)
	evs := d.TickStations(t0.Add(75*time.Second), uncorr)
	if _, ok := find(evs, "jamming_detected"); ok {
		t.Error("uncorroborated departure wrongly called jamming")
	}
	e, ok := find(evs, "station_rf_degraded")
	if !ok || e.NewValue != "degraded" {
		t.Fatalf("station_rf_degraded = %+v (ok=%v), want degraded", e, ok)
	}
}

// TestRFReceiverJamFlagAloneDegraded guards a receiver reporting its own jam flag
// (jamState >= 2) with AGC below the departure threshold and no CW spike must surface as
// station_rf_degraded (DEFENSE-PNT §2's single-metric rule), not classify "ok".
func TestRFReceiverJamFlagAloneDegraded(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 0))) // seed clear
	jamFlag := station("s", band(0, 0, 2, 3))          // receiver jamState=3 alone, no AGC/CW
	d.TickStations(t0.Add(10*time.Second), jamFlag)
	evs := d.TickStations(t0.Add(75*time.Second), jamFlag)
	if _, ok := find(evs, "jamming_detected"); ok {
		t.Error("jamState-alone wrongly called a jamming attack")
	}
	e, ok := find(evs, "station_rf_degraded")
	if !ok || e.NewValue != "degraded" {
		t.Fatalf("station_rf_degraded = %+v (ok=%v), want degraded for jamState-alone", e, ok)
	}
}

// TestRFAntennaFault confirms an antenna open/short surfaces antenna_fault.
func TestRFAntennaFault(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, station("s", band(0, 0, 2, 1)))                     // antenna ok
	d.TickStations(t0.Add(10*time.Second), station("s", band(0, 0, 4, 1))) // open
	evs := d.TickStations(t0.Add(75*time.Second), station("s", band(0, 0, 4, 1)))
	if e, ok := find(evs, "antenna_fault"); !ok || e.NewValue != "fault" {
		t.Fatalf("antenna_fault = %+v (ok=%v)", e, ok)
	}
}

// TestRFSpoofQuorum confirms the fusion rule: one gate is not enough to raise
// spoofing_suspected (the quorum is ≥2), so a lone C/N₀-gate hit stays quiet.
func TestRFSpoofQuorum(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	resid, mean := 0.2, 50.0 // C/N₀ gate tripped: flat, high C/N₀
	spoof := map[string]state.StationRF{"s": {ID: "s", Cn0Resid: &resid, Cn0Mean: &mean, RFTrust: 1}}
	d.TickStations(t0, map[string]state.StationRF{"s": {ID: "s", RFTrust: 1}})
	d.TickStations(t0.Add(10*time.Second), spoof)
	evs := d.TickStations(t0.Add(75*time.Second), spoof)
	if _, ok := find(evs, "spoofing_suspected"); ok {
		t.Error("single gate raised spoofing_suspected; the quorum is ≥2 (DEFENSE-PNT §3)")
	}
	if spoofGates(spoof["s"]) != 1 {
		t.Errorf("expected exactly one gate tripped, got %d", spoofGates(spoof["s"]))
	}
}

// TestWiredSpoofGatesMatchesImplementation guards WiredSpoofGates is the
// published coverage number (the spoof_gates_wired gauge), so it must equal the
// maximum spoofGates can actually return — trip every wired gate at once and
// count. Whoever adds a gate to spoofGates must bump the constant, or this fails.
func TestWiredSpoofGatesMatchesImplementation(t *testing.T) {
	resid, mean := 0.1, 55.0 // everything C/N₀ trips: aggregate AND per-constellation
	all := state.StationRF{
		Cn0Resid: &resid, Cn0Mean: &mean,
		Cn0ByConstellation: map[int]state.Cn0Stats{
			0: {Mean: 55, Resid: 0.1, NumSats: 8},
			2: {Mean: 55, Resid: 0.1, NumSats: 8},
		},
	}
	if got := spoofGates(all); got != WiredSpoofGates {
		t.Fatalf("spoofGates max = %d, WiredSpoofGates = %d — the published coverage number is wrong", got, WiredSpoofGates)
	}
}

// TestSpoofingSuspectedDormantWhileUnderQuorum documents the regression fix posture: with
// WiredSpoofGates < SpoofGateQuorum, spoofing_suspected is arithmetically
// unreachable — every wired gate tripping at once must still not fire it. When a
// second gate lands this test must be REPLACED by a reachability test, not deleted.
func TestSpoofingSuspectedDormantWhileUnderQuorum(t *testing.T) {
	if WiredSpoofGates >= SpoofGateQuorum {
		t.Skip("quorum now reachable; replace this test with a spoofing_suspected reachability test")
	}
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	resid, mean := 0.1, 55.0
	spoof := map[string]state.StationRF{"s": {ID: "s", Cn0Resid: &resid, Cn0Mean: &mean,
		Cn0ByConstellation: map[int]state.Cn0Stats{0: {Mean: 55, Resid: 0.1, NumSats: 8}}, RFTrust: 1}}
	d.TickStations(t0, map[string]state.StationRF{"s": {ID: "s", RFTrust: 1}})
	d.TickStations(t0.Add(10*time.Second), spoof)
	evs := d.TickStations(t0.Add(120*time.Second), spoof)
	if _, ok := find(evs, "spoofing_suspected"); ok {
		t.Fatal("spoofing_suspected fired under quorum — the fusion rule is broken")
	}
}

// A flat GPS group must remain visible beside a genuine Galileo sky.
func TestRFSpoofGateUsesPerConstellationFit(t *testing.T) {
	mean, resid := 42.0, 20.0 // non-tripping all-sky aggregate
	rf := state.StationRF{
		Cn0Mean: &mean, Cn0Resid: &resid,
		Cn0ByConstellation: map[int]state.Cn0Stats{
			0: {Mean: 50, Resid: 0, NumSats: 6},
			2: {Mean: 42, Resid: 8, NumSats: 6},
		},
	}
	if got := spoofGates(rf); got != 1 {
		t.Fatalf("spoof gates = %d, want the flat GPS group to trip one gate", got)
	}
}
