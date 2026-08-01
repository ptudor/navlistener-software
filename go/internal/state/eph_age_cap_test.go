package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
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

// TestPropagateEphAgeCapReplayBlind guards the regression fix cap gates on the
// collector-local apply time (ephAt), which a push spool drain resets to ≈ now
// when it applies a days-old replayed set — so the cap passed, kepler.Propagate
// evaluated the orbit at a half-week-wrapped tk (a position wrong by thousands
// of km served fresh), and the feed's eph_age_m read the wrapped-small SOW age,
// blinding eph_aged. The forensic reception stamp (ephRecvAt = f.Recv) must
// close both: no position, and a monotone multi-day age.
func TestPropagateEphAgeCapReplayBlind(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	recvAt := now.Add(-5 * 24 * time.Hour) // broadcast/received 5 days ago; drained now

	for _, w := range [][]uint32{sf1Words(85), sf2Words(85, 205075516), sf3Words(85)} {
		s.Apply(&ingest.RawFrame{
			Recv: recvAt, RecvLocal: now, Source: "test",
			GnssID: gnss.GPS, SvID: 5, SigID: 0, Words: w,
		})
	}

	s.Propagate(now)
	sv := s.FeedSVs(now)["G05@0"]
	if sv.XM != nil {
		t.Error("5 d old replayed ephemeris (applied just now) served a position: the serving cap is replay-blind")
	}
	if sv.EphAgeM == nil {
		t.Fatal("eph_age_m absent for a replayed 5 d old ephemeris")
	}
	// The SOW age at 5 d wraps to ≈ −2 d; the served age must instead be the
	// monotone forensic wall-clock age (≈ 7200 min), far past the 140 min alert.
	if *sv.EphAgeM < 71*60 {
		t.Errorf("eph_age_m = %.1f min for a replayed 5 d old set, want ≥ %d (forensic age, not the wrapped SOW age)",
			*sv.EphAgeM, 71*60)
	}
}
