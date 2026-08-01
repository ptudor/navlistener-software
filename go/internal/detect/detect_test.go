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
	evs := d.Tick(now, map[string]state.FeedSV{"G05@0": gps("G05", 5, 1)}, nil, 1)
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
	d.Tick(t0, unknown, nil, 1) // health unknown: not classified, nothing seeded
	ok := map[string]state.FeedSV{"G05@0": gps("G05", 5, 1)}
	d.Tick(t0.Add(10*time.Second), ok, nil, 1) // seeds OK silently (first decoded health)
	if evs := d.Tick(t0.Add(120*time.Second), ok, nil, 1); len(evs) != 0 {
		t.Fatalf("unknown→OK fired %d events, want 0", len(evs))
	}

	// OK → unknown (RAWX-only) → OK, each held past debounce: no spurious 1→0 or 0→1.
	if evs := d.Tick(t0.Add(200*time.Second), unknown, nil, 1); len(evs) != 0 {
		t.Fatalf("OK→unknown fired %d events, want 0", len(evs))
	}
	if evs := d.Tick(t0.Add(400*time.Second), ok, nil, 1); len(evs) != 0 {
		t.Fatalf("unknown→OK reacquire fired %d events, want 0", len(evs))
	}
}

// TestHealthDebounce confirms a health change is emitted only after the provisional
// state persists for the full debounce window, and reverts silently before it.
func TestHealthDebounce(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(1_000_000, 0)
	svs := map[string]state.FeedSV{"G05@0": gps("G05", 5, 1)} // healthy
	d.Tick(t0, svs, nil, 1)                                   // seed OK

	// Unhealthy: this tick starts the provisional window (no event yet).
	bad := map[string]state.FeedSV{"G05@0": gps("G05", 5, 2)}
	if evs := d.Tick(t0.Add(30*time.Second), bad, nil, 1); len(evs) != 0 {
		t.Fatalf("emitted %d events when provisional started, want 0", len(evs))
	}
	// Still inside the window (< 60 s since the provisional began): no event.
	if evs := d.Tick(t0.Add(60*time.Second), bad, nil, 1); len(evs) != 0 {
		t.Fatalf("emitted %d events before debounce elapsed, want 0", len(evs))
	}
	// Past the window (≥ 60 s since the provisional began): confirmed transition.
	// code 2 is the ICD's "marginal" class (issue level 1), so the event is
	// a WARNING — the old unconditional critical manufactured alerts for routine
	// component codes.
	evs := d.Tick(t0.Add(95*time.Second), bad, nil, 1)
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
	d.Tick(t0.Add(120*time.Second), worse, nil, 1)
	evs = d.Tick(t0.Add(200*time.Second), worse, nil, 1)
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
	d.Tick(t0, ok, nil, 1)
	d.Tick(t0.Add(20*time.Second), bad, nil, 1) // provisional unhealthy
	d.Tick(t0.Add(40*time.Second), ok, nil, 1)  // reverts before 60 s
	if evs := d.Tick(t0.Add(90*time.Second), ok, nil, 1); len(evs) != 0 {
		t.Fatalf("flap emitted %d events, want 0", len(evs))
	}
}

// TestOrbitDiscoBands confirms the orbit-disco warn/crit banding and event type.
func TestOrbitDiscoBands(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(3_000_000, 0)
	sv := gps("G05", 5, 1)
	sv.OrbitDiscoM = ptrF(0.5) // ok
	d.Tick(t0, map[string]state.FeedSV{"G05@0": sv}, nil, 1)

	sv.OrbitDiscoM = ptrF(12.0) // crit
	m := map[string]state.FeedSV{"G05@0": sv}
	d.Tick(t0.Add(10*time.Second), m, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), m, nil, 1)
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
		return d.Tick(at, map[string]state.FeedSV{"G05@0": sv}, nil, 1)
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

// TestWNMismatchTransition guards detector half: a confirmed
// broadcast-week anomaly (upload error / SV time fault / replayed signal) is a
// critical wn_mismatch event; recovery back to ok is informational.
func TestWNMismatchTransition(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(8_000_000, 0)
	f := false
	sv := gps("G05", 5, 1)
	sv.WnMismatch = &f
	d.Tick(t0, map[string]state.FeedSV{"G05@0": sv}, nil, 1) // seeds "ok"

	tr := true
	sv.WnMismatch = &tr
	bad := map[string]state.FeedSV{"G05@0": sv}
	d.Tick(t0.Add(10*time.Second), bad, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), bad, nil, 1)
	e, ok := find(evs, "wn_mismatch")
	if !ok || e.NewValue != "mismatch" || e.Severity != SevCritical {
		t.Fatalf("wn_mismatch = %+v (ok=%v), want confirmed mismatch/2", e, ok)
	}
}

// TestURAAlertTransition guards regression fix/detector half: a satellite
// raising its broadcast URA-alert flag ("use at own risk", IS-GPS-200N
// §20.3.3.2) must produce a confirmed ura_alert event — previously the bit was
// never decoded, so a self-declared degraded SV produced no signal at all.
func TestURAAlertTransition(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(7_000_000, 0)
	f := false
	sv := gps("G05", 5, 1)
	sv.Alert = &f
	d.Tick(t0, map[string]state.FeedSV{"G05@0": sv}, nil, 1) // seeds "clear"

	tr := true
	sv.Alert = &tr
	raised := map[string]state.FeedSV{"G05@0": sv}
	d.Tick(t0.Add(10*time.Second), raised, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), raised, nil, 1)
	e, ok := find(evs, "ura_alert")
	if !ok || e.NewValue != "raised" || e.Severity != SevWarning {
		t.Fatalf("ura_alert = %+v (ok=%v), want confirmed raised/1", e, ok)
	}

	// An SV whose alert flag was never decoded (nil) is not classified.
	d2 := New(time.Minute)
	blank := gps("G07", 7, 1)
	d2.Tick(t0, map[string]state.FeedSV{"G07@0": blank}, nil, 1)
	if evs := d2.Tick(t0.Add(120*time.Second), map[string]state.FeedSV{"G07@0": blank}, nil, 1); len(evs) != 0 {
		if _, ok := find(evs, "ura_alert"); ok {
			t.Fatal("undecoded alert flag classified a state")
		}
	}
}

// TestOsnmaChangeClassifies guards the served osnma flag must drive
// its own debounced state machine, so authentication presence going away (or
// arriving) fires the INTEGRITY.md-promised osnma_change — and an SV with no
// OSNMA field observed (nil) is never classified (regression fix absence rule).
func TestOsnmaChangeClassifies(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(6_000_000, 0)
	on := true
	sv := gps("E05", 5, 1)
	sv.GnssID = 2
	sv.Osnma = &on
	d.Tick(t0, map[string]state.FeedSV{"E05@0": sv}, nil, 1) // seed

	off := false
	sv.Osnma = &off
	m := map[string]state.FeedSV{"E05@0": sv}
	d.Tick(t0.Add(10*time.Second), m, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), m, nil, 1)
	e, ok := find(evs, "osnma_change")
	if !ok || e.NewValue != "off" || e.OldValue != "on" || e.Severity != SevInfo {
		t.Fatalf("osnma_change = %+v (ok=%v), want confirmed on→off at info severity", e, ok)
	}

	// nil (no OSNMA field observed): no classification, no phantom event.
	d2 := New(time.Minute)
	blank := gps("E07", 7, 1)
	blank.GnssID = 2
	d2.Tick(t0, map[string]state.FeedSV{"E07@0": blank}, nil, 1)
	if evs := d2.Tick(t0.Add(120*time.Second), map[string]state.FeedSV{"E07@0": blank}, nil, 1); len(evs) != 0 {
		if _, ok := find(evs, "osnma_change"); ok {
			t.Fatal("OSNMA-less SV classified an osnma state")
		}
	}
}

// TestLeapMismatchClassifies guards the served leap_mismatch flag
// (broadcast BDT-UTC ΔtLS vs the collector's configured GPS−UTC count) must
// drive a debounced warning transition, and an SV with no UTC set decoded
// (nil) is never classified (regression fix absence rule).
func TestLeapMismatchClassifies(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(6_000_000, 0)
	okv, dtLS := false, 4
	sv := gps("C24", 24, 1)
	sv.GnssID = 3
	sv.LeapMismatch, sv.DtLS = &okv, &dtLS
	d.Tick(t0, map[string]state.FeedSV{"C24@8": sv}, nil, 1) // seed

	bad := true
	sv.LeapMismatch = &bad
	m := map[string]state.FeedSV{"C24@8": sv}
	d.Tick(t0.Add(10*time.Second), m, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), m, nil, 1)
	e, ok := find(evs, "leap_mismatch")
	if !ok || e.NewValue != "mismatch" || e.OldValue != "ok" || e.Severity != SevWarning {
		t.Fatalf("leap_mismatch = %+v (ok=%v), want confirmed ok→mismatch at warning severity", e, ok)
	}

	// nil (no UTC set decoded): no classification, no phantom event.
	d2 := New(time.Minute)
	blank := gps("C25", 25, 1)
	blank.GnssID = 3
	d2.Tick(t0, map[string]state.FeedSV{"C25@8": blank}, nil, 1)
	if evs := d2.Tick(t0.Add(120*time.Second), map[string]state.FeedSV{"C25@8": blank}, nil, 1); len(evs) != 0 {
		if _, ok := find(evs, "leap_mismatch"); ok {
			t.Fatal("UTC-set-less SV classified a leap state")
		}
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
	d.Tick(t0, map[string]state.FeedSV{"G05@0": sv}, nil, 1)

	// URA flips to index 15: sisa_m vanishes, acc_index carries the sentinel.
	sv.SISAM = nil
	idx := 15
	sv.AccIndex = &idx
	bad := map[string]state.FeedSV{"G05@0": sv}
	d.Tick(t0.Add(10*time.Second), bad, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), bad, nil, 1)
	e, ok := find(evs, "sisa_change")
	if !ok || e.NewValue != "no_accuracy" || e.Severity != SevWarning {
		t.Fatalf("sisa_change = %+v (ok=%v), want confirmed no_accuracy/1", e, ok)
	}

	// An SV with NO accuracy field decoded at all (both absent) is not classified —
	// absence stays "unknown", only the sentinel is a state.
	d2 := New(time.Minute)
	blank := gps("G07", 7, 1)
	d2.Tick(t0, map[string]state.FeedSV{"G07@0": blank}, nil, 1)
	if evs := d2.Tick(t0.Add(120*time.Second), map[string]state.FeedSV{"G07@0": blank}, nil, 1); len(evs) != 0 {
		if _, ok := find(evs, "sisa_change"); ok {
			t.Fatal("accuracy-less SV classified a sisa state")
		}
	}
}

// TestBdsIntegrityFlagClassifies guards a raised B2a integrity flag
// (DIF/SIF/AIF) must fire a debounced bds_integrity_flag warning, each flag
// combination its own state, and clear back at info severity; SVs with no flag
// block decoded (nil) are never classified.
func TestBdsIntegrityFlagClassifies(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(6_000_000, 0)
	f, sm := false, 3
	sv := gps("C27", 27, 1)
	sv.GnssID, sv.SigID = 3, 8
	sv.Dif, sv.Sif, sv.Aif, sv.Sismai = &f, &f, &f, &sm
	d.Tick(t0, map[string]state.FeedSV{"C27@8": sv}, nil, 1) // seeds "ok"

	tr := true
	sv.Dif = &tr
	m := map[string]state.FeedSV{"C27@8": sv}
	d.Tick(t0.Add(10*time.Second), m, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), m, nil, 1)
	e, ok := find(evs, "bds_integrity_flag")
	if !ok || e.NewValue != "dif" || e.OldValue != "ok" || e.Severity != SevWarning {
		t.Fatalf("bds_integrity_flag = %+v (ok=%v), want confirmed ok→dif at warning severity", e, ok)
	}

	// Clear again: back to ok at info severity.
	sv.Dif = &f
	m = map[string]state.FeedSV{"C27@8": sv}
	d.Tick(t0.Add(200*time.Second), m, nil, 1)
	evs = d.Tick(t0.Add(270*time.Second), m, nil, 1)
	if e, ok := find(evs, "bds_integrity_flag"); !ok || e.NewValue != "ok" || e.Severity != SevInfo {
		t.Fatalf("clear transition = %+v (ok=%v), want dif→ok at info severity", e, ok)
	}

	// nil flag block: no classification.
	d2 := New(time.Minute)
	blank := gps("C28", 28, 1)
	blank.GnssID, blank.SigID = 3, 8
	d2.Tick(t0, map[string]state.FeedSV{"C28@8": blank}, nil, 1)
	if evs := d2.Tick(t0.Add(120*time.Second), map[string]state.FeedSV{"C28@8": blank}, nil, 1); len(evs) != 0 {
		if _, ok := find(evs, "bds_integrity_flag"); ok {
			t.Fatal("flag-less SV classified a bds_integrity state")
		}
	}
}

// TestRawAccIndexChangeClassifies guards an accuracy index flagged
// raw-only (BeiDou B-CNAV2 SISAI — no published metres table) must classify
// each index VALUE as its own state, so a broadcast accuracy revision fires a
// sisa_change (info severity, semantics unpublished) instead of collapsing
// every value into one permanent "no_accuracy" state.
func TestRawAccIndexChangeClassifies(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(6_000_000, 0)
	idx := 1386
	sv := gps("C26", 26, 1)
	sv.GnssID, sv.SigID = 3, 8
	sv.AccIndex, sv.AccIndexRawOnly = &idx, true
	d.Tick(t0, map[string]state.FeedSV{"C26@8": sv}, nil, 1) // seed

	// Unchanged index past debounce: no event.
	if evs := d.Tick(t0.Add(120*time.Second), map[string]state.FeedSV{"C26@8": sv}, nil, 1); len(evs) != 0 {
		if _, ok := find(evs, "sisa_change"); ok {
			t.Fatal("unchanged raw index fired a sisa_change")
		}
	}

	idx2 := 36202
	sv.AccIndex = &idx2
	m := map[string]state.FeedSV{"C26@8": sv}
	d.Tick(t0.Add(200*time.Second), m, nil, 1)
	evs := d.Tick(t0.Add(270*time.Second), m, nil, 1)
	e, ok := find(evs, "sisa_change")
	if !ok || e.NewValue != "raw_36202" || e.OldValue != "raw_1386" || e.Severity != SevInfo {
		t.Fatalf("sisa_change = %+v (ok=%v), want confirmed raw_1386→raw_36202 at info severity", e, ok)
	}
}

// TestQZSSHealthType confirms QZSS health transitions carry the qzss_health type,
// not health_change (docs/INTEGRITY.md §5, the Japan extension).
func TestQZSSHealthType(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(4_000_000, 0)
	j := state.FeedSV{Name: "J03", GnssID: 5, SvID: 3, HealthCode: 1, XM: ptrF(1), YM: ptrF(1), ZM: ptrF(1)}
	d.Tick(t0, map[string]state.FeedSV{"J03@0": j}, nil, 1)
	j.HealthCode, j.HealthIssueLevel = 3, 2 // do-not-use
	bad := map[string]state.FeedSV{"J03@0": j}
	d.Tick(t0.Add(10*time.Second), bad, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), bad, nil, 1)
	if len(evs) != 1 || evs[0].Type != "qzss_health" || evs[0].Severity != SevCritical {
		t.Fatalf("got %+v, want one qzss_health/2", evs)
	}
}

// TestEventParamsCarryConf guards every confirmed SV event must carry the
// corroboration count in its params, so a consumer can tell a fleet-corroborated
// event from a single receiver's testimony on the event itself.
func TestEventParamsCarryConf(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(11_000_000, 0)
	sv := gps("G05", 5, 1)
	sv.Conf = 3
	d.Tick(t0, map[string]state.FeedSV{"G05@0": sv}, nil, 1)
	bad := gps("G05", 5, 3)
	bad.Conf = 3
	m := map[string]state.FeedSV{"G05@0": bad}
	d.Tick(t0.Add(10*time.Second), m, nil, 1)
	evs := d.Tick(t0.Add(80*time.Second), m, nil, 1)
	if len(evs) != 1 {
		t.Fatalf("got %d events, want 1", len(evs))
	}
	if got, ok := evs[0].Params["conf"]; !ok || got != 3 {
		t.Fatalf("event params conf = %v (present=%v), want 3", got, ok)
	}
}

// TestSilenceSuppressedBelowFleetFloor guards with fewer than
// SilenceMinReceivers live stations, the per-SV silence classifier must not run
// at all — an SV setting below one station's horizon (LastSeenS past
// SilentThreshold, entry still inside sv_ttl) previously confirmed a false
// observation_lost/recovery warning pair once per orbital pass.
func TestSilenceSuppressedBelowFleetFloor(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(6_000_000, 0)
	fleet := SilenceMinReceivers - 1

	seen := gps("G05", 5, 1)
	d.Tick(t0, map[string]state.FeedSV{"G05@0": seen}, nil, fleet)

	// The SV sets: unseen for over an hour, held there past the debounce.
	silent := gps("G05", 5, 1)
	silent.LastSeenS = int(SilentThreshold) + 100
	m := map[string]state.FeedSV{"G05@0": silent}
	d.Tick(t0.Add(10*time.Second), m, nil, fleet)
	for _, e := range d.Tick(t0.Add(120*time.Second), m, nil, fleet) {
		if e.Type == "observation_lost" {
			t.Fatalf("observation_lost fired with %d live receivers (floor %d): %+v",
				fleet, SilenceMinReceivers, e)
		}
	}
	// Reacquisition must likewise fire no recovery.
	d.Tick(t0.Add(200*time.Second), map[string]state.FeedSV{"G05@0": seen}, nil, fleet)
	for _, e := range d.Tick(t0.Add(400*time.Second), map[string]state.FeedSV{"G05@0": seen}, nil, fleet) {
		if e.Type == "observation_lost" {
			t.Fatalf("observation_lost recovery fired below the fleet floor: %+v", e)
		}
	}
}

// TestSilenceClassifiesAtFleetFloor confirms the silence classifier still works —
// seed, confirm, recover — once the fleet is at the SilenceMinReceivers floor.
func TestSilenceClassifiesAtFleetFloor(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(6_500_000, 0)
	fleet := SilenceMinReceivers

	seen := gps("G05", 5, 1)
	d.Tick(t0, map[string]state.FeedSV{"G05@0": seen}, nil, fleet) // seeds "seen"

	silent := gps("G05", 5, 1)
	silent.LastSeenS = int(SilentThreshold) + 100
	m := map[string]state.FeedSV{"G05@0": silent}
	d.Tick(t0.Add(10*time.Second), m, nil, fleet)
	evs := d.Tick(t0.Add(80*time.Second), m, nil, fleet)
	var found *Event
	for i := range evs {
		if evs[i].Type == "observation_lost" {
			found = &evs[i]
		}
	}
	if found == nil {
		t.Fatalf("no observation_lost at fleet floor %d: %+v", fleet, evs)
	}
	if found.NewValue != "silent" || found.Severity != SevWarning {
		t.Errorf("observation_lost = %s/%d, want silent/1", found.NewValue, found.Severity)
	}
}

// TestSilenceMachineHoldsAcrossFleetDip confirms the regression fix hold-state rule for the
// fleet gate: a machine confirmed "silent" at full fleet neither re-fires nor falsely
// recovers while the fleet is below the floor, and resumes classification after.
func TestSilenceMachineHoldsAcrossFleetDip(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(7_000_000, 0)
	full := SilenceMinReceivers

	seen := gps("G05", 5, 1)
	silent := gps("G05", 5, 1)
	silent.LastSeenS = int(SilentThreshold) + 100

	d.Tick(t0, map[string]state.FeedSV{"G05@0": seen}, nil, full) // seed "seen"
	d.Tick(t0.Add(10*time.Second), map[string]state.FeedSV{"G05@0": silent}, nil, full)
	if evs := d.Tick(t0.Add(80*time.Second), map[string]state.FeedSV{"G05@0": silent}, nil, full); len(evs) != 1 || evs[0].Type != "observation_lost" {
		t.Fatalf("expected the confirmed observation_lost, got %+v", evs)
	}

	// Fleet dips below the floor while the SV is back in view: no false recovery.
	if evs := d.Tick(t0.Add(100*time.Second), map[string]state.FeedSV{"G05@0": seen}, nil, full-1); len(evs) != 0 {
		t.Fatalf("fleet dip emitted %+v, want none (machine must hold)", evs)
	}
	if evs := d.Tick(t0.Add(200*time.Second), map[string]state.FeedSV{"G05@0": seen}, nil, full-1); len(evs) != 0 {
		t.Fatalf("fleet dip emitted %+v, want none (machine must hold)", evs)
	}

	// Fleet recovers: the seen→ recovery classifies and confirms normally.
	d.Tick(t0.Add(220*time.Second), map[string]state.FeedSV{"G05@0": seen}, nil, full)
	evs := d.Tick(t0.Add(300*time.Second), map[string]state.FeedSV{"G05@0": seen}, nil, full)
	if len(evs) != 1 || evs[0].Type != "observation_lost" || evs[0].NewValue != "seen" {
		t.Fatalf("expected the seen recovery after fleet restore, got %+v", evs)
	}
}

// TestProvisionalDoesNotSurviveHold guards the debounce promises a
// CONTINUOUS dwell, so a provisional captured just before a classifier enters a
// hold window must not confirm on the first post-hold observation with its
// pre-hold `since`. The fleet-floor silence skip is the sharpest case — it is
// fleet-wide, so one dip below SilenceMinReceivers holds EVERY SV's silence
// machine at once and a fleet oscillating around the floor could instant-confirm
// whatever was pending at dip time. After the hold the pending change must
// re-earn the full window; a fresh full dwell then confirms normally.
func TestProvisionalDoesNotSurviveHold(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(7_500_000, 0)
	full := SilenceMinReceivers

	seen := gps("G05", 5, 1)
	silent := gps("G05", 5, 1)
	silent.LastSeenS = int(SilentThreshold) + 100
	seenM := map[string]state.FeedSV{"G05@0": seen}
	silentM := map[string]state.FeedSV{"G05@0": silent}

	d.Tick(t0, seenM, nil, full)                         // seeds "seen"
	d.Tick(t0.Add(10*time.Second), silentM, nil, full)   // provisional "silent" at t0+10
	d.Tick(t0.Add(20*time.Second), silentM, nil, full-1) // fleet dips: classifier held
	d.Tick(t0.Add(80*time.Second), silentM, nil, full-1) // still held, past the old dwell

	// Fleet restored. The pre-hold provisional is now 80 s old — older than the
	// debounce — but only ~10 s of that was actually observed, so this tick must
	// only restart the dwell, not confirm.
	if evs := d.Tick(t0.Add(90*time.Second), silentM, nil, full); len(evs) != 0 {
		t.Fatalf("provisional survived the hold and confirmed instantly: %+v", evs)
	}
	// One more tick still inside the fresh window: still nothing.
	if evs := d.Tick(t0.Add(130*time.Second), silentM, nil, full); len(evs) != 0 {
		t.Fatalf("confirmed before the fresh debounce elapsed: %+v", evs)
	}
	// A full post-resume dwell confirms normally — the fix delays, never suppresses.
	evs := d.Tick(t0.Add(160*time.Second), silentM, nil, full)
	if len(evs) != 1 || evs[0].Type != "observation_lost" || evs[0].NewValue != "silent" {
		t.Fatalf("got %+v, want the observation_lost after a fresh full dwell", evs)
	}
}

// TestRecurringHoldDoesNotSuppressConfirmation guards // original unconditional dwell restart meant a classifier held more often than
// once per debounce window (here: the fleet oscillating around the silence
// floor every other 15 s tick) re-stamped `since` on every observation, so a
// GENUINE continuous fault could never confirm — a silent, permanent detector
// outage. A hold shorter than the window must keep the accumulated dwell: two
// consistent observations spanning the full window are real evidence.
func TestRecurringHoldDoesNotSuppressConfirmation(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(7_600_000, 0)
	full := SilenceMinReceivers

	seen := gps("G05", 5, 1)
	silent := gps("G05", 5, 1)
	silent.LastSeenS = int(SilentThreshold) + 100
	seenM := map[string]state.FeedSV{"G05@0": seen}
	silentM := map[string]state.FeedSV{"G05@0": silent}

	d.Tick(t0, seenM, nil, full)                       // seeds "seen"
	d.Tick(t0.Add(15*time.Second), silentM, nil, full) // provisional at t0+15
	// The fleet dips below the floor on every other tick: each hold is a single
	// 15 s round, far shorter than the 60 s window.
	d.Tick(t0.Add(30*time.Second), silentM, nil, full-1) // held
	if evs := d.Tick(t0.Add(45*time.Second), silentM, nil, full); len(evs) != 0 {
		t.Fatalf("confirmed with only 30 s of dwell: %+v", evs)
	}
	d.Tick(t0.Add(60*time.Second), silentM, nil, full-1) // held again
	// t0+75: the provisional has dwelt 60 s across three observations with two
	// sub-window holds between them. It must confirm — never be suppressed.
	evs := d.Tick(t0.Add(75*time.Second), silentM, nil, full)
	if len(evs) != 1 || evs[0].Type != "observation_lost" || evs[0].NewValue != "silent" {
		t.Fatalf("got %+v, want observation_lost confirmed despite recurring sub-window holds", evs)
	}
}

// TestFullWindowHoldStillRestartsDwell pins the regression fix boundary: a hold that
// lasts a full debounce window (or longer — original case) still
// restarts the dwell, because the unobserved span alone could hide a whole
// state excursion.
func TestFullWindowHoldStillRestartsDwell(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(7_700_000, 0)
	full := SilenceMinReceivers

	seen := gps("G05", 5, 1)
	silent := gps("G05", 5, 1)
	silent.LastSeenS = int(SilentThreshold) + 100
	seenM := map[string]state.FeedSV{"G05@0": seen}
	silentM := map[string]state.FeedSV{"G05@0": silent}

	d.Tick(t0, seenM, nil, full)                         // seeds "seen"
	d.Tick(t0.Add(15*time.Second), silentM, nil, full)   // provisional at t0+15
	d.Tick(t0.Add(30*time.Second), silentM, nil, full-1) // held...
	d.Tick(t0.Add(60*time.Second), silentM, nil, full-1) // ...for a full window
	// t0+75: wall dwell since the provisional is 60 s, but the machine was held
	// for exactly one debounce window (t0+15 → t0+75 unobserved). Restart.
	if evs := d.Tick(t0.Add(75*time.Second), silentM, nil, full); len(evs) != 0 {
		t.Fatalf("confirmed across a full-window hold: %+v", evs)
	}
	// A fresh full observed dwell then confirms normally.
	d.Tick(t0.Add(90*time.Second), silentM, nil, full)
	evs := d.Tick(t0.Add(135*time.Second), silentM, nil, full)
	if len(evs) != 1 || evs[0].Type != "observation_lost" {
		t.Fatalf("got %+v, want observation_lost after the fresh post-hold dwell", evs)
	}
}

// TestSBASDoNotUse confirms an SBAS PRN going do-not-use fires a critical sbas_health.
func TestSBASDoNotUse(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(5_000_000, 0)
	ok := map[string]state.SBASEntry{"131": {Provider: "WAAS", HealthCode: 1}}
	d.Tick(t0, nil, ok, 1)
	bad := map[string]state.SBASEntry{"131": {Provider: "WAAS", HealthCode: 3}}
	d.Tick(t0.Add(10*time.Second), nil, bad, 1)
	evs := d.Tick(t0.Add(80*time.Second), nil, bad, 1)
	if len(evs) != 1 || evs[0].Type != "sbas_health" || evs[0].Severity != SevCritical {
		t.Fatalf("got %+v, want one sbas_health/2", evs)
	}
	if evs[0].SV != "S131" {
		t.Errorf("subject = %s, want S131", evs[0].SV)
	}
}

// TestSBASLost guards a dark SBAS GEO — the one always-in-view subject
// class — must fire a warning sbas_lost once unseen past SBASSilentThreshold,
// and a recovery once messages resume. Entries model state.SBASDetect's
// unfiltered view (retained past the served feed's 5 min staleness drop).
func TestSBASLost(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(8_000_000, 0)
	fresh := map[string]state.SBASEntry{"133": {Provider: "WAAS", HealthCode: 1, LastSeenS: 2}}
	d.Tick(t0, nil, fresh, 1) // seeds seen + health OK

	dark := map[string]state.SBASEntry{"133": {Provider: "WAAS", HealthCode: 1, LastSeenS: int(SBASSilentThreshold) + 60}}
	d.Tick(t0.Add(10*time.Second), nil, dark, 1)
	evs := d.Tick(t0.Add(80*time.Second), nil, dark, 1)
	if len(evs) != 1 || evs[0].Type != "sbas_lost" || evs[0].NewValue != "silent" || evs[0].Severity != SevWarning {
		t.Fatalf("got %+v, want one sbas_lost silent/1", evs)
	}
	if evs[0].SV != "S133" {
		t.Errorf("subject = %s, want S133", evs[0].SV)
	}

	// Reacquisition: the recovery confirms, and nothing else phantom-fires.
	d.Tick(t0.Add(200*time.Second), nil, fresh, 1)
	evs = d.Tick(t0.Add(280*time.Second), nil, fresh, 1)
	if len(evs) != 1 || evs[0].Type != "sbas_lost" || evs[0].NewValue != "seen" {
		t.Fatalf("got %+v, want one sbas_lost seen recovery", evs)
	}
}

// TestSBASDarknessDoesNotFakeHealthRecovery guards the regression fix health-hold: a
// test-mode GEO (health latched do_not_use per regression fix) that goes completely dark
// must NOT fire a do_not_use→ok sbas_health "recovery" as the MT0 latch decays for
// lack of input — past SBASHealthCurrentWindow the health machine holds, and the
// only events are the sbas_lost pair.
func TestSBASDarknessDoesNotFakeHealthRecovery(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(9_000_000, 0)
	testMode := map[string]state.SBASEntry{"122": {Provider: "SouthPAN", HealthCode: 3, LastSeenS: 1}}
	d.Tick(t0, nil, testMode, 1) // seeds health do_not_use silently

	// Dark for 2 min: the MT0 latch has decayed (health_code reads 1 at feed
	// build) but the entry is stale past SBASHealthCurrentWindow — hold.
	decayed := map[string]state.SBASEntry{"122": {Provider: "SouthPAN", HealthCode: 1, LastSeenS: 120}}
	d.Tick(t0.Add(10*time.Second), nil, decayed, 1)
	for _, e := range d.Tick(t0.Add(80*time.Second), nil, decayed, 1) {
		if e.Type == "sbas_health" {
			t.Fatalf("stale-latch darkness fired a fabricated sbas_health recovery: %+v", e)
		}
	}

	// Fully silent: only the sbas_lost warning fires.
	dark := map[string]state.SBASEntry{"122": {Provider: "SouthPAN", HealthCode: 1, LastSeenS: int(SBASSilentThreshold) + 30}}
	d.Tick(t0.Add(100*time.Second), nil, dark, 1)
	evs := d.Tick(t0.Add(170*time.Second), nil, dark, 1)
	if len(evs) != 1 || evs[0].Type != "sbas_lost" {
		t.Fatalf("got %+v, want exactly the sbas_lost event", evs)
	}

	// Broadcast resumes still in test mode: no health event (do_not_use → do_not_use),
	// just the silence recovery.
	d.Tick(t0.Add(250*time.Second), nil, testMode, 1)
	evs = d.Tick(t0.Add(330*time.Second), nil, testMode, 1)
	if len(evs) != 1 || evs[0].Type != "sbas_lost" || evs[0].NewValue != "seen" {
		t.Fatalf("got %+v, want only the sbas_lost recovery (health held at do_not_use throughout)", evs)
	}
}

// TestStationOffline guards regression fix wiring: a station unseen past
// ObserverOfflineThreshold fires a warning station_offline from the unfiltered
// liveness map, and recovers when it returns. First sight of an already-offline
// station seeds silently.
func TestStationOffline(t *testing.T) {
	d := New(time.Minute)
	t0 := time.Unix(10_000_000, 0)

	d.TickStationLiveness(t0, map[string]int{"observer16": 5}) // seeds online
	gone := map[string]int{"observer16": int(ObserverOfflineThreshold) + 60}
	d.TickStationLiveness(t0.Add(10*time.Second), gone)
	evs := d.TickStationLiveness(t0.Add(80*time.Second), gone)
	if len(evs) != 1 || evs[0].Type != "station_offline" || evs[0].NewValue != "offline" || evs[0].Severity != SevWarning {
		t.Fatalf("got %+v, want one station_offline offline/1", evs)
	}
	if evs[0].SV != "observer16" {
		t.Errorf("subject = %s, want observer16", evs[0].SV)
	}

	back := map[string]int{"observer16": 3}
	d.TickStationLiveness(t0.Add(200*time.Second), back)
	evs = d.TickStationLiveness(t0.Add(280*time.Second), back)
	if len(evs) != 1 || evs[0].Type != "station_offline" || evs[0].NewValue != "online" {
		t.Fatalf("got %+v, want the online recovery", evs)
	}

	// A station first seen already-offline (daemon restart mid-outage) seeds silently.
	d2 := New(time.Minute)
	dead := map[string]int{"colo9": 4000}
	d2.TickStationLiveness(t0, dead)
	if evs := d2.TickStationLiveness(t0.Add(120*time.Second), dead); len(evs) != 0 {
		t.Fatalf("already-offline first sight fired %+v, want none (seed rule)", evs)
	}
}
