package detect

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

func ptrF(v float64) *float64 { return &v }

// gps builds a minimal healthy GPS FeedSV with a position, at the given health.
// The issue level is derived the way state.healthFor pairs them : code 1
// → level 0, code 2 → level 1 (marginal/warning), code 3 → level 2 (error).
func gps(name string, svid, health int) state.FeedSV {
	level := 0
	switch health {
	case 2:
		level = 1
	case 3:
		level = 2
	}
	return state.FeedSV{
		Name: name, GnssID: 0, SvID: svid, SigID: 0,
		HealthCode: health, HealthIssueLevel: level, XM: ptrF(1), YM: ptrF(2), ZM: ptrF(3),
	}
}

// TestSeedNoEvent confirms the first sighting of a metric seeds the state silently —
// no phantom transition (docs/INTEGRITY.md §3 guard).
func TestSeedNoEvent(t *testing.T) {
	d := New(time.Minute)
	now := time.Unix(1_000_000, 0)
	evs := d.Tick(now, map[string]state.FeedSV{"G05@0": gps("G05", 5, 1)}, nil)
	if len(evs) != 0 {
		t.Fatalf("first tick emitted %d events, want 0 (seed only)", len(evs))
	}
}

// TestHealthUnknownNoPhantomEvent guards health_code 0 ("unknown", e.g. iono-only
// RAWX tracking or pre-word-5 Galileo) must not be classified. An SV that enters unknown,
// then decodes to OK past the debounce, must fire NO event (the machine seeds on the first
// DECODED health); and a 1→0→1 excursion through unknown must likewise stay silent.
func TestHealthUnknownNoPhantomEvent(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(1_000_000, 0)

	// Unknown → decoded OK past debounce: no phantom health_change 0→1.
	unknown := map[string]state.FeedSV{"G05@0": gps("G05", 5, 0)}
	d.Tick(t0, unknown, nil) // health unknown: not classified, nothing seeded
	ok := map[string]state.FeedSV{"G05@0": gps("G05", 5, 1)}
	d.Tick(t0.Add(10*time.Second), ok, nil) // seeds OK silently (first decoded health)
	if evs := d.Tick(t0.Add(120*time.Second), ok, nil); len(evs) != 0 {
		t.Fatalf("unknown→OK fired %d events, want 0", len(evs))
	}

	// OK → unknown (RAWX-only) → OK, each held past debounce: no spurious 1→0 or 0→1.
	if evs := d.Tick(t0.Add(200*time.Second), unknown, nil); len(evs) != 0 {
		t.Fatalf("OK→unknown fired %d events, want 0", len(evs))
	}
	if evs := d.Tick(t0.Add(400*time.Second), ok, nil); len(evs) != 0 {
		t.Fatalf("unknown→OK reacquire fired %d events, want 0", len(evs))
	}
}

// TestHealthDebounce confirms a health change is emitted only after the provisional
// state persists for the full debounce window, and reverts silently before it.
func TestHealthDebounce(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(1_000_000, 0)
	svs := map[string]state.FeedSV{"G05@0": gps("G05", 5, 1)} // healthy
	d.Tick(t0, svs, nil)                                      // seed OK

	// Unhealthy: this tick starts the provisional window (no event yet).
	bad := map[string]state.FeedSV{"G05@0": gps("G05", 5, 2)}
	if evs := d.Tick(t0.Add(30*time.Second), bad, nil); len(evs) != 0 {
		t.Fatalf("emitted %d events when provisional started, want 0", len(evs))
	}
	// Still inside the window (< 60 s since the provisional began): no event.
	if evs := d.Tick(t0.Add(60*time.Second), bad, nil); len(evs) != 0 {
		t.Fatalf("emitted %d events before debounce elapsed, want 0", len(evs))
	}
	// Past the window (≥ 60 s since the provisional began): confirmed transition.
	// code 2 is the ICD's "marginal" class (issue level 1), so the event is
	// a WARNING — the old unconditional critical manufactured alerts for routine
	// component codes.
	evs := d.Tick(t0.Add(95*time.Second), bad, nil)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	e := evs[0]
	if e.Type != "health_change" || e.Severity != SevWarning {
		t.Errorf("event = %s/%d, want health_change/1 (marginal → warning, regression fix)", e.Type, e.Severity)
	}
	if e.OldValue != "1" || e.NewValue != "2" {
		t.Errorf("transition %s→%s, want 1→2", e.OldValue, e.NewValue)
	}

	// A do-not-use transition (code 3, issue level 2) stays CRITICAL.
	worse := map[string]state.FeedSV{"G05@0": gps("G05", 5, 3)}
	d.Tick(t0.Add(120*time.Second), worse, nil)
	evs = d.Tick(t0.Add(200*time.Second), worse, nil)
	if len(evs) != 1 || evs[0].Type != "health_change" || evs[0].Severity != SevCritical {
		t.Fatalf("do-not-use transition = %+v, want one health_change/2", evs)
	}
}

// TestRevertBeforeConfirm confirms a provisional change that reverts to the current
// state before the window elapses fires nothing (the flap filter).
func TestRevertBeforeConfirm(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(2_000_000, 0)
	ok := map[string]state.FeedSV{"G05@0": gps("G05", 5, 1)}
	bad := map[string]state.FeedSV{"G05@0": gps("G05", 5, 2)}
	d.Tick(t0, ok, nil)
	d.Tick(t0.Add(20*time.Second), bad, nil) // provisional unhealthy
	d.Tick(t0.Add(40*time.Second), ok, nil)  // reverts before 60 s
	if evs := d.Tick(t0.Add(90*time.Second), ok, nil); len(evs) != 0 {
		t.Fatalf("flap emitted %d events, want 0", len(evs))
	}
}

// TestOrbitDiscoBands confirms the orbit-disco warn/crit banding and event type.
func TestOrbitDiscoBands(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(3_000_000, 0)
	sv := gps("G05", 5, 1)
	sv.OrbitDiscoM = ptrF(0.5) // ok
	d.Tick(t0, map[string]state.FeedSV{"G05@0": sv}, nil)

	sv.OrbitDiscoM = ptrF(12.0) // crit
	m := map[string]state.FeedSV{"G05@0": sv}
	d.Tick(t0.Add(10*time.Second), m, nil)
	evs := d.Tick(t0.Add(80*time.Second), m, nil)
	var found *Event
	for i := range evs {
		if evs[i].Type == "orbit_disco" {
			found = &evs[i]
		}
	}
	if found == nil {
		t.Fatal("no orbit_disco event")
	}
	if found.NewValue != "crit" || found.Severity != SevCritical {
		t.Errorf("orbit_disco = %s/%d, want crit/2", found.NewValue, found.Severity)
	}
}

// TestSISAHysteresisDampensQuantizedDwell guards URA/SISA are quantized
// (e.g. this codebase's GPS URA table steps directly from 2.8284 m (N=1) to
// 4.0 m (N=2) around the 3.0 m SISAAlertThreshold, per gnss/accuracy.URAMeters
// -- there is no intermediate value), so a real SV legitimately dwells on both
// steps for minutes at a time, longer than the debounce window, and a plain
// threshold produces a confirmed sisa_change pair on every such dwell. The
// asymmetric exit band (clear only below SISAExitThreshold, 2.5 m) must absorb
// that: dwelling back at the lower quantized step (2.8284 m, which is still
// >= the exit threshold) must NOT clear the degraded state, while a genuine
// drop further below the exit threshold must.
func TestSISAHysteresisDampensQuantizedDwell(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(4_000_000, 0)
	sv := gps("G05", 5, 1)

	tick := func(sisaM float64, at time.Time) []Event {
		sv.SISAM = ptrF(sisaM)
		return d.Tick(at, map[string]state.FeedSV{"G05@0": sv}, nil)
	}

	// Seed ok at the lower quantized URA step (N=1, 2.8284 m).
	tick(2.8284, t0)

	// Dwell at the upper quantized step (N=2, 4.0 m -- degraded) for 90 s, past
	// the debounce: a genuine confirmed transition.
	t1 := t0.Add(90 * time.Second)
	tick(4.0, t0.Add(10*time.Second))
	evs := tick(4.0, t1)
	e, ok := find(evs, "sisa_change")
	if !ok || e.NewValue != "degraded" {
		t.Fatalf("first sisa_change = %+v (ok=%v), want degraded", e, ok)
	}

	// Dwell back at the lower quantized step (2.8284 m) for another 90 s.
	// Without hysteresis this clears (2.8284 < SISAAlertThreshold) and fires a
	// second sisa_change pair on every such dwell; with the exit band it must
	// NOT clear, since 2.8284 >= SISAExitThreshold (2.5 m).
	t2 := t1.Add(90 * time.Second)
	tick(2.8284, t1.Add(10*time.Second))
	if evs := tick(2.8284, t2); len(evs) != 0 {
		if _, ok := find(evs, "sisa_change"); ok {
			t.Fatalf("sisa flapped back to ok while dwelling at %.4f m (>= exit threshold %.1f m): %+v",
				2.8284, SISAExitThreshold, evs)
		}
	}

	// A genuine drop below the exit threshold DOES clear it.
	t3 := t2.Add(90 * time.Second)
	tick(1.0, t2.Add(10*time.Second))
	evs = tick(1.0, t3)
	e, ok = find(evs, "sisa_change")
	if !ok || e.NewValue != "ok" {
		t.Fatalf("sisa clear = %+v (ok=%v), want ok once genuinely below the exit threshold", e, ok)
	}
}

// TestNoAccuracySentinelClassifies guards an SV whose broadcast accuracy
// index is the "no accuracy prediction — use at own risk" sentinel serves no
// sisa_m (there is no metres value) but does serve acc_index; the sisa classifier
// must treat that as its own confirmed state instead of being structurally
// skipped, so the transition fires a sisa_change.
func TestNoAccuracySentinelClassifies(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(6_000_000, 0)
	sv := gps("G05", 5, 1)
	sv.SISAM = ptrF(2.0) // normal accuracy: seeds "ok"
	d.Tick(t0, map[string]state.FeedSV{"G05@0": sv}, nil)

	// URA flips to index 15: sisa_m vanishes, acc_index carries the sentinel.
	sv.SISAM = nil
	idx := 15
	sv.AccIndex = &idx
	bad := map[string]state.FeedSV{"G05@0": sv}
	d.Tick(t0.Add(10*time.Second), bad, nil)
	evs := d.Tick(t0.Add(80*time.Second), bad, nil)
	e, ok := find(evs, "sisa_change")
	if !ok || e.NewValue != "no_accuracy" || e.Severity != SevWarning {
		t.Fatalf("sisa_change = %+v (ok=%v), want confirmed no_accuracy/1", e, ok)
	}

	// An SV with NO accuracy field decoded at all (both absent) is not classified —
	// absence stays "unknown", only the sentinel is a state.
	d2 := New(time.Minute)
	blank := gps("G07", 7, 1)
	d2.Tick(t0, map[string]state.FeedSV{"G07@0": blank}, nil)
	if evs := d2.Tick(t0.Add(120*time.Second), map[string]state.FeedSV{"G07@0": blank}, nil); len(evs) != 0 {
		if _, ok := find(evs, "sisa_change"); ok {
			t.Fatal("accuracy-less SV classified a sisa state")
		}
	}
}

// TestQZSSHealthType confirms QZSS health transitions carry the qzss_health type,
// not health_change (docs/INTEGRITY.md §5, the Japan extension).
func TestQZSSHealthType(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(4_000_000, 0)
	j := state.FeedSV{Name: "J03", GnssID: 5, SvID: 3, HealthCode: 1, XM: ptrF(1), YM: ptrF(1), ZM: ptrF(1)}
	d.Tick(t0, map[string]state.FeedSV{"J03@0": j}, nil)
	j.HealthCode, j.HealthIssueLevel = 3, 2 // do-not-use
	bad := map[string]state.FeedSV{"J03@0": j}
	d.Tick(t0.Add(10*time.Second), bad, nil)
	evs := d.Tick(t0.Add(80*time.Second), bad, nil)
	if len(evs) != 1 || evs[0].Type != "qzss_health" || evs[0].Severity != SevCritical {
		t.Fatalf("got %+v, want one qzss_health/2", evs)
	}
}

// TestSBASDoNotUse confirms an SBAS PRN going do-not-use fires a critical sbas_health.
func TestSBASDoNotUse(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(5_000_000, 0)
	ok := map[string]state.SBASEntry{"131": {Provider: "WAAS", HealthCode: 1}}
	d.Tick(t0, nil, ok)
	bad := map[string]state.SBASEntry{"131": {Provider: "WAAS", HealthCode: 3}}
	d.Tick(t0.Add(10*time.Second), nil, bad)
	evs := d.Tick(t0.Add(80*time.Second), nil, bad)
	if len(evs) != 1 || evs[0].Type != "sbas_health" || evs[0].Severity != SevCritical {
		t.Fatalf("got %+v, want one sbas_health/2", evs)
	}
	if evs[0].SV != "S131" {
		t.Errorf("subject = %s, want S131", evs[0].SV)
	}
}
