package state

import (
	"testing"
	"time"
)

// TestGLONASSPropagateEphAgeCap guards regression fix (the GLONASS twin of regression fix): a
// GLONASS SV whose ephemeris apply time (gloEphAt) is beyond gloPropagateMaxEphAge
// must stop having positions propagated. Before the fix, the frozen PZ-90 state was
// re-integrated and re-stamped fresh (posAt = now) on every tick with no validity
// gate at all — through the km-wrong 0.5–2 h regime and, past 12 h, into the
// EphAgeDay ±half-day wrap, where the served eph_age_m flips negative (un-firing
// the eph_aged detector on a worsening SV) and the RK4 integrates ~12 h backward.
// The fix: positions stop at the cap, the served position expires via
// posStaleBound, and eph_age_m switches to the un-wrappable wall-clock age so
// eph_aged latches and stays latched.
func TestGLONASSPropagateEphAgeCap(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	// One coherent frame: strings 1/2/3 (tb index 45 → 40 500 s of Moscow day),
	// coords large enough for the propagator's degenerate-state guard.
	s.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t0))
	s.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 45, t0.Add(2*time.Second)))
	s.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	// Fresh: position served, day-wrapped age served (bounded by the ±720 min wrap).
	s.Propagate(t0.Add(5 * time.Second))
	sv, ok := s.FeedSVs(t0.Add(5 * time.Second))["R07@0"]
	if !ok {
		t.Fatal("R07@0 missing from svs feed after a coherent triple")
	}
	if sv.XM == nil {
		t.Fatal("fresh GLONASS ephemeris: no position served")
	}
	if sv.EphAgeM == nil {
		t.Fatal("fresh GLONASS ephemeris: eph_age_m absent")
	}

	// Inside the cap (30 min): still served — routine extrapolation is by design.
	t1 := t0.Add(30 * time.Minute)
	s.Propagate(t1)
	if sv = s.FeedSVs(t1)["R07@0"]; sv.XM == nil {
		t.Fatal("30 min old GLONASS ephemeris: position must still be served")
	}

	// Past the cap (2 h): Propagate must skip the SV, so the position expires via
	// posStaleBound instead of a stale state vector serving fresh forever.
	t2 := t0.Add(2 * time.Hour)
	s.Propagate(t2)
	sv = s.FeedSVs(t2)["R07@0"]
	if sv.XM != nil {
		t.Error("2 h stale GLONASS ephemeris still serving a position (the regression fix failure)")
	}
	// eph_age_m must be the monotone wall-clock age, ≥ the true elapsed 120 min.
	if sv.EphAgeM == nil {
		t.Fatal("eph_age_m absent for a 2 h stale GLONASS ephemeris")
	}
	if *sv.EphAgeM < 119 {
		t.Errorf("eph_age_m = %.1f min at 2 h, want ≥ 119 (wall-clock age)", *sv.EphAgeM)
	}

	// Past the ±12 h EphAgeDay wrap horizon (13 h): the day-wrapped age would now
	// read NEGATIVE (~−660 min), flipping eph_aged back to "fresh" while the SV
	// worsens. The wall-clock age must stay monotone instead.
	t3 := t0.Add(13 * time.Hour)
	s.Propagate(t3)
	sv = s.FeedSVs(t3)["R07@0"]
	if sv.XM != nil {
		t.Error("13 h stale GLONASS ephemeris still serving a position")
	}
	if sv.EphAgeM == nil {
		t.Fatal("eph_age_m absent for a 13 h stale GLONASS ephemeris")
	}
	if *sv.EphAgeM < 12*60 {
		t.Errorf("eph_age_m = %.1f min at 13 h, want ≥ %d (monotone wall-clock, not the wrapped/negative day age)",
			*sv.EphAgeM, 12*60)
	}
}
