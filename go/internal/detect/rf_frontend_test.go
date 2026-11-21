package detect

import (
	"strings"
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

// clearEvent ticks the detector on quiet read models from at until the given event
// type clears, and returns that event.
func clearEvent(t *testing.T, d *Detector, typ string, at time.Time, quiet func(now time.Time) map[string]state.StationRF) Event {
	t.Helper()
	for step := time.Duration(0); step <= 20*time.Minute; step += 15 * time.Second {
		now := at.Add(step)
		if e, ok := find(d.TickStations(now, quiet(now)), typ); ok {
			return e
		}
	}
	t.Fatalf("%s never cleared", typ)
	return Event{}
}

// TestClearAfterRelearnSaysSo: a departure that ends because its baseline was learned
// again at the level held has not gone away, and the event that clears it says so. One
// that ends because the gain came back carries no such note.
func TestClearAfterRelearnSaysSo(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	relearn := t0.Add(80 * time.Second)
	mark := relearn.Unix()

	// A Septentrio station: the cut held, learning restarted, and the front end has no
	// departure while its new baseline is learned.
	d := New(0)
	if _, ok := find(confirm(d, quietP3, p3("p3", end(0, "GPSL1/E1", 5))), "station_rf_degraded"); !ok {
		t.Fatal("the cut did not confirm")
	}
	gain := 35
	learning := func(time.Time) map[string]state.StationRF {
		return map[string]state.StationRF{"p3": {ID: "p3", RFTrust: 1, BaselineRelearnedAt: &mark,
			FrontEnds: []state.StationFrontEnd{{ID: 0, Name: "GPSL1/E1", Locked: true, GainDB: &gain}}}}
	}
	e := clearEvent(t, d, "station_rf_degraded", relearn, learning)
	if e.NewValue != "ok" || e.Severity != SevInfo || e.Params["baseline_relearned"] != true {
		t.Fatalf("clear after a relearn = %+v", e)
	}
	if want := "station p3 RF degraded (gain cut 0.0 dB, gain rise 0.0 dB, cw=false, rx_jam=false, cn0_drop=false); baseline relearned at the level held"; e.Message != want {
		t.Fatalf("message %q, want %q", e.Message, want)
	}
	if got := e.Time.Sub(relearn); got < StationClearDwell || got > StationClearDwell+30*time.Second {
		t.Fatalf("cleared %v after the relearn, want the clear dwell", got)
	}

	// The same station, but the gain simply came back.
	d = New(0)
	confirm(d, quietP3, p3("p3", end(0, "GPSL1/E1", 5)))
	back := clearEvent(t, d, "station_rf_degraded", relearn, func(time.Time) map[string]state.StationRF { return quietP3 })
	if _, has := back.Params["baseline_relearned"]; has || back.NewValue != "ok" {
		t.Fatalf("clear after the gain returned = %+v", back)
	}
	if strings.Contains(back.Message, "relearned") {
		t.Fatalf("message %q carries a relearn note", back.Message)
	}

	// A u-blox station's collapse, relearned after its hold: the critical event clears
	// with the note too.
	d = New(0)
	if e, ok := find(confirm(d, station("s", band(0, 0, 2, 0)), station("s", band(2500, 0, 2, 0))), "jamming_detected"); !ok || e.NewValue != "crit" {
		t.Fatalf("the collapse did not confirm: %+v", e)
	}
	relearning := func(time.Time) map[string]state.StationRF {
		return map[string]state.StationRF{"s": {ID: "s", RFTrust: 1, BaselineRelearnedAt: &mark,
			Bands: []state.StationRFBand{{Block: 0, AGC: 1500, AntStatus: 2}}}}
	}
	jam := clearEvent(t, d, "jamming_detected", relearn, relearning)
	if jam.NewValue != "ok" || jam.Params["baseline_relearned"] != true ||
		jam.Message != "station s jamming ok (AGC departure 0); baseline relearned at the level held" {
		t.Fatalf("jamming clear after a relearn = %+v", jam)
	}
}

// TestOldRelearnIsNotNoted: a baseline learned again hours ago has nothing to do with a
// departure that ends now.
func TestOldRelearnIsNotNoted(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	old := t0.Add(-3 * time.Hour).Unix()
	with := func(m map[string]state.StationRF) map[string]state.StationRF {
		out := map[string]state.StationRF{}
		for id, st := range m {
			st.BaselineRelearnedAt = &old
			out[id] = st
		}
		return out
	}
	d := New(0)
	onset, ok := find(confirm(d, with(quietP3), with(p3("p3", end(0, "GPSL1/E1", 5)))), "station_rf_degraded")
	if !ok {
		t.Fatal("the cut did not confirm")
	}
	if _, has := onset.Params["baseline_relearned"]; has {
		t.Fatalf("onset three hours after a relearn carries the note: %+v", onset)
	}
	e := clearEvent(t, d, "station_rf_degraded", t0.Add(80*time.Second), func(time.Time) map[string]state.StationRF { return with(quietP3) })
	if _, has := e.Params["baseline_relearned"]; has {
		t.Fatalf("clear three hours after a relearn carries the note: %+v", e)
	}
}

// marked is a read model whose station carries a relearn mark from the given second.
func marked(m map[string]state.StationRF, at int64) map[string]state.StationRF {
	out := map[string]state.StationRF{}
	for id, st := range m {
		st.BaselineRelearnedAt = &at
		out[id] = st
	}
	return out
}

// TestOnsetAfterRelearnIsNotNoted: the note belongs to an event that steps down. A
// departure that begins minutes after another front end's baseline was learned again
// is new evidence, and its event reads as any onset does.
func TestOnsetAfterRelearnIsNotNoted(t *testing.T) {
	recent := time.Unix(1_700_000_000, 0).Add(-time.Minute).Unix()
	for name, c := range map[string]struct {
		typ, state string
		cut        float64
	}{
		"a degradation":   {"station_rf_degraded", "degraded", 5},
		"a gain collapse": {"jamming_detected", "crit", 12},
	} {
		onset, ok := find(confirm(New(0), marked(quietP3, recent), marked(p3("p3", end(0, "GPSL1/E1", 0), end(5, "L5/E5a/B2a", c.cut)), recent)), c.typ)
		if !ok || onset.NewValue != c.state {
			t.Fatalf("%s did not confirm: %+v", name, onset)
		}
		if _, has := onset.Params["baseline_relearned"]; has || strings.Contains(onset.Message, "relearned") {
			t.Fatalf("%s beginning a minute after a relearn carries the note: %+v", name, onset)
		}
	}
}

// TestStepDownAfterRelearnSaysSo: a station with two front ends cut, one of them a
// collapse. When the collapsed one's level is learned as its normal the claim eases to
// the lesser cut that remains, and the event that eases it says why: the collapse did
// not go away.
func TestStepDownAfterRelearnSaysSo(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	relearn := t0.Add(80 * time.Second)
	mark := relearn.Unix()
	both := p3("p3", end(0, "GPSL1/E1", 12), end(5, "L5/E5a/B2a", 5))
	d := New(0)
	if e, ok := find(confirm(d, quietP3, both), "jamming_detected"); !ok || e.NewValue != "crit" {
		t.Fatalf("the collapse did not confirm: %+v", e)
	}
	// L1 is learning its new baseline and has no departure; L5 is still cut by 5 dB,
	// with every signal's C/N₀ down, which is a corroborated departure.
	gain := 28
	eased := func(time.Time) map[string]state.StationRF {
		return map[string]state.StationRF{"p3": {ID: "p3", RFTrust: 0.3, Cn0Drop: true, BaselineRelearnedAt: &mark,
			FrontEnds: []state.StationFrontEnd{{ID: 0, Name: "GPSL1/E1", Locked: true, GainDB: &gain}, end(5, "L5/E5a/B2a", 5)}}}
	}
	e := clearEvent(t, d, "jamming_detected", relearn, eased)
	if e.OldValue != "crit" || e.NewValue != "warn" || e.Params["baseline_relearned"] != true || !strings.HasSuffix(e.Message, "; baseline relearned at the level held") {
		t.Fatalf("the claim easing after a relearn = %+v", e)
	}
}
