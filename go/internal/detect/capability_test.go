package detect

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

func capReport(id string, stationLastSeen int64, obs []state.StationCapability, decl []state.CapSignal) map[string]state.StationCapReport {
	return map[string]state.StationCapReport{id: {ID: id, StationLastSeen: stationLastSeen, Observed: obs, Declared: decl}}
}

// TestCapabilitySignalLost drives a demonstrated signal that goes silent while the station is
// otherwise alive: it must confirm capability_signal_lost after the debounce, not before.
func TestCapabilitySignalLost(t *testing.T) {
	d := New(0) // 60 s debounce
	t0 := time.Unix(1_700_000_000, 0)
	// Galileo E5a F/NAV, demonstrated (count 50), last seen at t0.
	sig := state.StationCapability{Gnss: 2, Sig: 3, Count: 50, FirstSeen: t0.Add(-time.Hour).Unix(), LastSeen: t0.Unix()}

	// Seed: signal fresh, station alive → "present".
	d.TickCapabilities(t0, capReport("s", t0.Unix(), []state.StationCapability{sig}, nil))

	// 20 min later the signal hasn't been seen (LastSeen still t0) but the station is alive
	// (it produced other frames at t1) → the signal is "lost".
	t1 := t0.Add(20 * time.Minute)
	rep := capReport("s", t1.Unix(), []state.StationCapability{sig}, nil)
	if evs := d.TickCapabilities(t1, rep); len(evs) != 0 {
		t.Fatalf("confirmed before debounce: %+v", evs)
	}
	rep2 := capReport("s", t1.Add(70*time.Second).Unix(), []state.StationCapability{sig}, nil)
	evs := d.TickCapabilities(t1.Add(70*time.Second), rep2)
	e, ok := find(evs, "capability_signal_lost")
	if !ok || e.NewValue != "lost" || e.Severity != SevWarning {
		t.Fatalf("capability_signal_lost = %+v (ok=%v), want lost/warning", e, ok)
	}
	if e.Params["gnss"] != 2 || e.Params["sig"] != 3 {
		t.Errorf("event params = %+v, want gnss 2 sig 3", e.Params)
	}
}

// TestCapabilityStationOfflineNotLost confirms a signal going quiet because the WHOLE station
// went offline does not fire capability_signal_lost (that is the observation/offline detectors'
// job) — the loss gate requires the station to be otherwise alive.
func TestCapabilityStationOfflineNotLost(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	sig := state.StationCapability{Gnss: 2, Sig: 3, Count: 50, LastSeen: t0.Unix()}
	d.TickCapabilities(t0, capReport("s", t0.Unix(), []state.StationCapability{sig}, nil))

	// 20 min later BOTH the signal and the station are stale (a full outage).
	t1 := t0.Add(20 * time.Minute)
	rep := capReport("s", t0.Unix(), []state.StationCapability{sig}, nil) // StationLastSeen stays t0
	d.TickCapabilities(t1, rep)
	if evs := d.TickCapabilities(t1.Add(70*time.Second), rep); len(evs) != 0 {
		t.Fatalf("full outage wrongly fired a capability event: %+v", evs)
	}
}

// TestCapabilityImpossible drives a node reporting a signal outside its declared silicon
// capability — the strong spoofing/misconfig signature — and checks it confirms critical.
func TestCapabilityImpossible(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	decl := []state.CapSignal{{Gnss: 0, Sig: 0}, {Gnss: 2, Sig: 0}} // GPS L1 + Galileo I/NAV
	ok := []state.StationCapability{{Gnss: 0, Sig: 0, Count: 5, LastSeen: t0.Unix()}}
	bad := append([]state.StationCapability{}, ok...)
	bad = append(bad, state.StationCapability{Gnss: 7, Sig: 0, Count: 5, LastSeen: t0.Unix()}) // NavIC — impossible

	d.TickCapabilities(t0, capReport("s", t0.Unix(), ok, decl)) // seed: no impossible signal
	t1 := t0.Add(10 * time.Second)
	if evs := d.TickCapabilities(t1, capReport("s", t1.Unix(), bad, decl)); len(evs) != 0 {
		t.Fatalf("confirmed before debounce: %+v", evs)
	}
	rep := capReport("s", t1.Add(70*time.Second).Unix(), bad, decl)
	evs := d.TickCapabilities(t1.Add(70*time.Second), rep)
	e, found := find(evs, "capability_impossible")
	if !found || e.NewValue != "impossible" || e.Severity != SevCritical {
		t.Fatalf("capability_impossible = %+v (found=%v), want impossible/critical", e, found)
	}
	sigs, _ := e.Params["signals"].([]string)
	if len(sigs) != 1 || sigs[0] != "7:0" {
		t.Errorf("offending signals = %v, want [7:0]", sigs)
	}
}

// TestCapabilityNoDeclaredNoImpossible confirms that with no declared set, the impossible gate
// never fires (there is nothing to contradict).
func TestCapabilityNoDeclaredNoImpossible(t *testing.T) {
	d := New(0)
	t0 := time.Unix(1_700_000_000, 0)
	obs := []state.StationCapability{{Gnss: 7, Sig: 0, Count: 5, LastSeen: t0.Unix()}}
	d.TickCapabilities(t0, capReport("s", t0.Unix(), obs, nil))
	evs := d.TickCapabilities(t0.Add(70*time.Second), capReport("s", t0.Add(70*time.Second).Unix(), obs, nil))
	if _, ok := find(evs, "capability_impossible"); ok {
		t.Error("capability_impossible fired with no declared set")
	}
}
