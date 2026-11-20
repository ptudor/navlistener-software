package detect

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

// end is one Septentrio front end with its gain's departure from baseline in dB:
// positive for a cut, negative for a rise.
func end(id int, name string, dep float64) state.StationFrontEnd {
	gain := 40 - int(dep)
	return state.StationFrontEnd{ID: id, Name: name, Locked: true, GainDB: &gain, GainDeparture: &dep}
}

func p3(id string, ends ...state.StationFrontEnd) map[string]state.StationRF {
	return map[string]state.StationRF{id: {ID: id, FrontEnds: ends, RFTrust: 1}}
}

// confirm seeds a station quiet, applies the degraded read model and returns the events
// confirmed once it has held past the onset debounce.
func confirm(d *Detector, quiet, degraded map[string]state.StationRF) []Event {
	t0 := time.Unix(1_700_000_000, 0)
	d.TickStations(t0, quiet)
	d.TickStations(t0.Add(10*time.Second), degraded)
	return d.TickStations(t0.Add(75*time.Second), degraded)
}

var quietP3 = p3("p3", end(0, "GPSL1/E1", 0), end(5, "L5/E5a/B2a", 0.5))

// TestFrontEndGainCutDegrades: a front end's gain cut by the gain departure, with
// nothing corroborating it, is a degradation and not a jamming claim, and the event
// says which front end and by how much.
func TestFrontEndGainCutDegrades(t *testing.T) {
	evs := confirm(New(0), quietP3, p3("p3", end(0, "GPSL1/E1", 5), end(5, "L5/E5a/B2a", 1)))
	if _, ok := find(evs, "jamming_detected"); ok {
		t.Fatalf("a lone 5 dB cut was called jamming: %+v", evs)
	}
	e, ok := find(evs, "station_rf_degraded")
	if !ok || e.NewValue != "degraded" || e.OldValue != "ok" || e.Severity != SevWarning {
		t.Fatalf("station_rf_degraded = %+v (ok=%v)", e, ok)
	}
	if e.Params["gain_cut_db"] != 5.0 || e.Params["gain_cut_front_end"] != "GPSL1/E1" || e.Params["gain_rise_db"] != 0.0 {
		t.Fatalf("params = %+v", e.Params)
	}
	if _, has := e.Params["gain_rise_front_end"]; has {
		t.Fatalf("a rise is named with no front end risen: %+v", e.Params)
	}
	if want := "station p3 RF degraded (gain cut 5.0 dB on GPSL1/E1, gain rise 0.0 dB, cw=false, rx_jam=false, cn0_drop=false)"; e.Message != want {
		t.Fatalf("message %q, want %q", e.Message, want)
	}
	if _, ok := find(evs, "antenna_fault"); ok {
		t.Fatalf("an antenna event from a receiver that reports no antenna status: %+v", evs)
	}
}

// TestFrontEndGainBelowThresholdIsQuiet: the gains of a quiet receiver move a decibel
// or two, and a departure short of the threshold raises nothing.
func TestFrontEndGainBelowThresholdIsQuiet(t *testing.T) {
	if evs := confirm(New(0), quietP3, p3("p3", end(0, "GPSL1/E1", 3.9), end(5, "L5/E5a/B2a", -3.9))); len(evs) != 0 {
		t.Fatalf("departures inside the threshold raised %+v", evs)
	}
}

// TestFrontEndGainCollapseIsJamming: a cut of the severe departure is a gain collapse,
// critical with no corroboration, as a near-total AGC collapse is.
func TestFrontEndGainCollapseIsJamming(t *testing.T) {
	evs := confirm(New(0), quietP3, p3("p3", end(0, "GPSL1/E1", 2), end(5, "L5/E5a/B2a", 11)))
	e, ok := find(evs, "jamming_detected")
	if !ok || e.NewValue != "crit" || e.Severity != SevCritical {
		t.Fatalf("jamming_detected = %+v (ok=%v), want crit", e, ok)
	}
	if e.Params["gain_cut_db"] != 11.0 || e.Params["gain_cut_front_end"] != "L5/E5a/B2a" || e.Params["agc_departure"] != 0.0 {
		t.Fatalf("params = %+v", e.Params)
	}
	if want := "station p3 jamming crit (gain cut 11.0 dB on L5/E5a/B2a, gain rise 0.0 dB)"; e.Message != want {
		t.Fatalf("message %q, want %q", e.Message, want)
	}
	if _, ok := find(evs, "station_rf_degraded"); ok {
		t.Fatalf("a jamming claim was also reported as a degradation: %+v", evs)
	}
}

// TestFrontEndGainCutCorroborated: a lesser cut becomes a jamming warning only with
// corroboration: here a neighbouring station's interference, then a simultaneous drop.
func TestFrontEndGainCutCorroborated(t *testing.T) {
	cut := p3("p3", end(0, "GPSL1/E1", 6))
	withNeighbour := map[string]state.StationRF{"p3": cut["p3"]}
	st := withNeighbour["p3"]
	st.Neighbours, st.NeighbourCount = []string{"near-1"}, 1
	withNeighbour["p3"] = st
	if e, ok := find(confirm(New(0), quietP3, withNeighbour), "jamming_detected"); !ok || e.NewValue != "warn" || e.Severity != SevWarning {
		t.Fatalf("cut with a neighbour = %+v (ok=%v), want warn", e, ok)
	}
	withDrop := map[string]state.StationRF{"p3": cut["p3"]}
	st = withDrop["p3"]
	st.Cn0Drop = true
	withDrop["p3"] = st
	if e, ok := find(confirm(New(0), quietP3, withDrop), "jamming_detected"); !ok || e.NewValue != "warn" {
		t.Fatalf("cut with a simultaneous drop = %+v (ok=%v), want warn", e, ok)
	}
}

// TestFrontEndGainRiseDegrades: a gain that rises means less power arriving. However
// large, and whatever else is going on nearby, it is a degradation and never jamming.
func TestFrontEndGainRiseDegrades(t *testing.T) {
	risen := p3("p3", end(0, "GPSL1/E1", -12), end(5, "L5/E5a/B2a", -11))
	st := risen["p3"]
	st.Neighbours, st.NeighbourCount = []string{"near-1"}, 1
	risen["p3"] = st
	evs := confirm(New(0), quietP3, risen)
	if _, ok := find(evs, "jamming_detected"); ok {
		t.Fatalf("a gain rise was called jamming: %+v", evs)
	}
	e, ok := find(evs, "station_rf_degraded")
	if !ok || e.NewValue != "degraded" || e.Severity != SevWarning {
		t.Fatalf("station_rf_degraded = %+v (ok=%v)", e, ok)
	}
	if e.Params["gain_rise_db"] != 12.0 || e.Params["gain_rise_front_end"] != "GPSL1/E1" || e.Params["gain_cut_db"] != 0.0 {
		t.Fatalf("params = %+v", e.Params)
	}
	if want := "station p3 RF degraded (gain cut 0.0 dB, gain rise 12.0 dB on GPSL1/E1, cw=false, rx_jam=false, cn0_drop=false)"; e.Message != want {
		t.Fatalf("message %q, want %q", e.Message, want)
	}
}

// TestFrontEndGainClears: the degradation clears, as info, once the gain is back within
// the threshold for the station clear dwell.
func TestFrontEndGainClears(t *testing.T) {
	d := New(0)
	if _, ok := find(confirm(d, quietP3, p3("p3", end(0, "GPSL1/E1", 5))), "station_rf_degraded"); !ok {
		t.Fatal("the cut did not confirm")
	}
	t0 := time.Unix(1_700_000_000, 0).Add(80 * time.Second)
	d.TickStations(t0, quietP3)
	var cleared Event
	for at := 15 * time.Second; at <= 20*time.Minute; at += 15 * time.Second {
		if at < StationClearDwell {
			if evs := d.TickStations(t0.Add(at), quietP3); len(evs) != 0 {
				t.Fatalf("recovery confirmed inside the clear dwell: %+v", evs)
			}
			continue
		}
		if e, ok := find(d.TickStations(t0.Add(at), quietP3), "station_rf_degraded"); ok {
			cleared = e
			break
		}
	}
	if cleared.NewValue != "ok" || cleared.OldValue != "degraded" || cleared.Severity != SevInfo {
		t.Fatalf("clear = %+v, want degraded -> ok at info", cleared)
	}
}

// TestFrontEndNoBaselineIsNotDegraded: front ends still learning their baselines, or
// with their PLLs unlocked, have no departure; they are measured and quiet.
func TestFrontEndNoBaselineIsNotDegraded(t *testing.T) {
	gain := 40
	learning := map[string]state.StationRF{"p3": {ID: "p3", RFTrust: 1, FrontEnds: []state.StationFrontEnd{
		{ID: 0, Name: "GPSL1/E1", Locked: true, GainDB: &gain}, {ID: 5, Name: "L5/E5a/B2a"},
	}}}
	if evs := confirm(New(0), learning, learning); len(evs) != 0 {
		t.Fatalf("front ends without a baseline raised %+v", evs)
	}
}

// TestFrontEndMissingTelemetryHoldsAlarm: a station whose front ends have aged out of
// the read model must not recover a confirmed alarm for want of evidence.
func TestFrontEndMissingTelemetryHoldsAlarm(t *testing.T) {
	d := New(0)
	if e, ok := find(confirm(d, quietP3, p3("p3", end(0, "GPSL1/E1", 12))), "jamming_detected"); !ok || e.NewValue != "crit" {
		t.Fatalf("failed to seed the confirmed alarm: %+v", e)
	}
	t0 := time.Unix(1_700_000_000, 0)
	silent := map[string]state.StationRF{"p3": {ID: "p3", RFTrust: 1}}
	for at := 150 * time.Second; at <= 15*time.Minute; at += 15 * time.Second {
		if evs := d.TickStations(t0.Add(at), silent); len(evs) != 0 {
			t.Fatalf("missing front-end evidence recovered the alarm: %+v", evs)
		}
	}
}

// TestBandEventsUnchangedByFrontEnds: a u-blox station's events keep the message and
// the parameters they had before front ends existed.
func TestBandEventsUnchangedByFrontEnds(t *testing.T) {
	evs := confirm(New(0), station("s", band(0, 0, 2, 0)), station("s", band(1200, 0, 2, 0)))
	e, ok := find(evs, "station_rf_degraded")
	if !ok {
		t.Fatalf("no station_rf_degraded: %+v", evs)
	}
	if want := "station s RF degraded (AGC departure 1200, cw=false, rx_jam=false, cn0_drop=false)"; e.Message != want {
		t.Fatalf("message %q, want %q", e.Message, want)
	}
	if len(e.Params) != 5 {
		t.Fatalf("params = %+v, want the five a band event always had", e.Params)
	}
	for _, key := range []string{"gain_cut_db", "gain_rise_db", "gain_cut_front_end", "gain_rise_front_end"} {
		if _, has := e.Params[key]; has {
			t.Fatalf("a band event carries %s: %+v", key, e.Params)
		}
	}
	crit, ok := find(confirm(New(0), station("s", band(0, 0, 2, 0)), station("s", band(2500, 0, 2, 0))), "jamming_detected")
	if !ok || crit.Message != "station s jamming crit (AGC departure 2500)" || len(crit.Params) != 7 {
		t.Fatalf("jamming event = %+v", crit)
	}
}
