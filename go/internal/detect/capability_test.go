package detect

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
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

// TestCapabilityLossEventOrderDeterministic guards (SV, Type) alone is
// not a unique key -- one station losing two demonstrated signals in the same
// tick emits two capability_signal_lost events sharing both SV and Type, and
// run.go's sort must still produce a fixed, repeatable order (the tertiary
// Message key) rather than leaving the tie's relative order unspecified.
func TestCapabilityLossEventOrderDeterministic(t *testing.T) {
	sigs := []state.StationCapability{
		{Gnss: 6, Sig: 0, Count: 50}, // GLONASS
		{Gnss: 0, Sig: 0, Count: 50}, // GPS
	}
	t0 := time.Unix(1_700_000_000, 0)
	for i := range sigs {
		sigs[i].LastSeen = t0.Unix()
	}

	run := func() []Event {
		d := New(0)
		rep0 := capReport("s", t0.Unix(), sigs, nil)
		d.TickCapabilities(t0, rep0)
		t1 := t0.Add(20 * time.Minute)
		rep1 := capReport("s", t1.Unix(), sigs, nil)
		d.TickCapabilities(t1, rep1)
		evs := d.TickCapabilities(t1.Add(70*time.Second), rep1)
		var lost []Event
		for _, e := range evs {
			if e.Type == "capability_signal_lost" {
				lost = append(lost, e)
			}
		}
		return lost
	}

	first := run()
	if len(first) != 2 {
		t.Fatalf("got %d capability_signal_lost events, want 2: %+v", len(first), first)
	}
	if first[0].SV != first[1].SV || first[0].Type != first[1].Type {
		t.Fatalf("expected both events to share (SV, Type): %+v", first)
	}
	if first[0].Message >= first[1].Message {
		t.Errorf("events not in ascending Message order: %+v", first)
	}
	for i := 0; i < 25; i++ {
		got := run()
		if len(got) != 2 || got[0].Message != first[0].Message || got[1].Message != first[1].Message {
			t.Fatalf("iteration %d: order changed run-to-run: %+v vs %+v", i, got, first)
		}
	}
}

// TestCapabilitySignalLostWithLiveRFTelemetry guards a station whose nav
// signal has gone silent must still be judged "alive" (and so still eligible
// for capability_signal_lost) if it keeps producing RF telemetry (MON-RF/
// NAV-SAT) — that combination (all-nav-signal denial + continuing RF) is the
// strongest jamming signature, and the old nav-frame-only stationAlive gate
// suppressed it exactly when it mattered. This drives the real state.Store +
// FeedCapabilityReports path (not the synthetic capReport helper) since the
// fix lives in FeedCapabilityReports's station-liveness computation.
func TestCapabilitySignalLostWithLiveRFTelemetry(t *testing.T) {
	st := state.New(2)
	d := New(0) // 60s debounce
	t0 := time.Unix(1_700_000_000, 0)

	gpsFrame := func(recv time.Time) *ingest.RawFrame {
		w := make([]uint32, 10)
		w[1] = 1 << 8 // HOW subframe id = 1 (a valid id, not the old all-zero sf-id-0)
		return &ingest.RawFrame{Source: "s", GnssID: gnss.GPS, SigID: 0, Recv: recv, Words: w}
	}
	rfFrame := func(recv time.Time) *ingest.RawFrame {
		return &ingest.RawFrame{Source: "s", Recv: recv, RF: &ingest.RawRF{
			Sats: []ingest.SatCN0{{GnssID: 0, SvID: 1, Cn0: 40, ElevDeg: 30}},
		}}
	}

	// Demonstrate GPS L1 (CapMinObservations nav frames) so the signal counts as observed.
	for i := 0; i < CapMinObservations; i++ {
		st.Apply(gpsFrame(t0.Add(time.Duration(i) * time.Second)))
	}
	d.TickCapabilities(t0, st.FeedCapabilityReports(t0))

	// The nav signal goes silent for well past CapSignalLostAfter, but RF
	// telemetry keeps arriving on the same station throughout.
	t1 := t0.Add(CapSignalLostAfter + time.Minute)
	st.Apply(rfFrame(t1))
	if evs := d.TickCapabilities(t1, st.FeedCapabilityReports(t1)); len(evs) != 0 {
		t.Fatalf("confirmed before debounce: %+v", evs)
	}

	t2 := t1.Add(70 * time.Second)
	st.Apply(rfFrame(t2))
	evs := d.TickCapabilities(t2, st.FeedCapabilityReports(t2))
	e, ok := find(evs, "capability_signal_lost")
	if !ok || e.NewValue != "lost" {
		t.Fatalf("capability_signal_lost = %+v (ok=%v), want lost -- RF telemetry alone must keep the station alive", e, ok)
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
