package state

import (
	"testing"
	"time"
)

// TestPropagateEphAgeCap guards regression fix (the documented regression fix remainder): an SV
// whose ephemeris apply time (ephAt) is beyond propagateMaxEphAge must stop
// having positions propagated — past the ±half-week EphAge wrap the propagation
// is garbage-but-finite, posAt would keep advancing (defeating posStaleBound),
// eph_age_m would wrap back toward zero, and the eph_aged detector would consume
// the wrapped lie: every staleness signal self-defeats at once. Reachable live
// via the regression fix window (RAWX observables keeping an SV fresh with a dead nav
// decode). The fix is the wall-clock gate: positions stop, the served position
// expires via posStaleBound, and eph_age_m switches to the un-wrappable
// wall-clock age so eph_aged stays latched.
func TestPropagateEphAgeCap(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	s.Apply(gpsFrame(sf1Words(85), t0))
	s.Apply(gpsFrame(sf2Words(85, 205075516), t0))
	s.Apply(gpsFrame(sf3Words(85), t0))

	// Fresh: position served, SOW-based age served.
	s.Propagate(t0)
	sv := s.FeedSVs(t0)["G05@0"]
	if sv.XM == nil {
		t.Fatal("fresh ephemeris: no position served")
	}

	// Inside the cap (2 h): still served.
	t1 := t0.Add(2 * time.Hour)
	s.Propagate(t1)
	if sv = s.FeedSVs(t1)["G05@0"]; sv.XM == nil {
		t.Fatal("2 h old ephemeris: position must still be served (extrapolation is by design)")
	}

	// Past the cap AND past the half-week wrap (7 d): Propagate must skip the SV,
	// so the position expires via posStaleBound instead of serving a wrapped,
	// thousands-of-km-wrong solution stamped fresh.
	t2 := t0.Add(7 * 24 * time.Hour)
	s.Propagate(t2)
	sv = s.FeedSVs(t2)["G05@0"]
	if sv.XM != nil {
		t.Error("week-stale ephemeris still serving a position (the regression fix failure)")
	}
	// eph_age_m must not read the wrapped near-zero SOW difference: at 7 d the
	// wrap yields ~0 min, which would hold eph_aged in 'fresh'. The wall-clock
	// fallback reports ≥ the cap (≫ the 140 min alert threshold).
	if sv.EphAgeM == nil {
		t.Fatal("eph_age_m absent for a week-stale ephemeris")
	}
	if *sv.EphAgeM < 72*60 {
		t.Errorf("eph_age_m = %.1f min for a 7 d old ephemeris, want ≥ %d (wall-clock, not the wrapped SOW age)",
			*sv.EphAgeM, 72*60)
	}
}
