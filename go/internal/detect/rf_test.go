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
