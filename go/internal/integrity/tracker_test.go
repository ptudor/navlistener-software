package integrity

import (
	"slices"
	"testing"
	"time"
)

var (
	testInfo   = Info{Name: "test_check", Version: 1, Domain: DomainPosition}
	testFilter = FilterPolicy{M: 3, N: 4, RecoveryHold: 5 * time.Minute}
	t0         = time.Unix(1_800_000_000, 0)
)

// feed applies raw states one second apart from start and returns the tracker and the
// instant after the last one.
func feed(t *tracker, start time.Time, states ...State) time.Time {
	at := start
	for _, s := range states {
		t.update(Verdict{State: s}, at, testFilter)
		at = at.Add(time.Second)
	}
	return at
}

func TestTrackerWarmUpThenAssured(t *testing.T) {
	tr := newTracker(testInfo)
	feed(tr, t0, Assured, Assured)
	r := tr.result()
	if r.State != Unavailable || r.Candidate != Unavailable || !slices.Contains(r.Reasons, ReasonWarmingUp) {
		t.Fatalf("after two evaluations = %+v, want unavailable while warming up", r)
	}
	feed(tr, t0.Add(2*time.Second), Assured)
	if r := tr.result(); r.State != Assured || slices.Contains(r.Reasons, ReasonWarmingUp) {
		t.Fatalf("after M evaluations = %+v, want assured at once", r)
	}
}

func TestTrackerSingleOutlierFiltered(t *testing.T) {
	tr := newTracker(testInfo)
	at := feed(tr, t0, Assured, Assured, Assured, Unassured, Assured, Unassured, Assured)
	if r := tr.result(); r.State != Assured {
		t.Fatalf("isolated outliers changed the state: %+v", r)
	}
	feed(tr, at, Unassured, Unassured)
	// Window is now [U, A, U, U]: three of four.
	if r := tr.result(); r.State != Unassured {
		t.Fatalf("three of four unassured = %+v, want unassured", r)
	}
}

func TestTrackerMixedDegradationIsInconsistent(t *testing.T) {
	tr := newTracker(testInfo)
	feed(tr, t0, Assured, Unassured, Inconsistent, Inconsistent)
	if r := tr.result(); r.State != Inconsistent || r.Candidate != Inconsistent {
		t.Fatalf("one unassured and two inconsistent = %+v, want inconsistent", r)
	}
}

func TestTrackerDegradesImmediatelyRecoversAfterHold(t *testing.T) {
	tr := newTracker(testInfo)
	at := feed(tr, t0, Assured, Assured, Assured, Unassured, Unassured, Unassured)
	degradedAt := at.Add(-time.Second)
	if r := tr.result(); r.State != Unassured || r.Since != degradedAt.Unix() {
		t.Fatalf("degradation = %+v, want unassured since %d", r, degradedAt.Unix())
	}
	// The first assured evaluation leaves three unassured in the window; the second
	// makes the candidate assured and starts the hold.
	at = feed(tr, at, Assured, Assured)
	holdStart := at.Add(-time.Second)
	r := tr.result()
	if r.State != Unassured || r.Candidate != Assured || r.RecoveringSince == nil || *r.RecoveringSince != holdStart.Unix() {
		t.Fatalf("recovery start = %+v, want unassured recovering since %d", r, holdStart.Unix())
	}
	for at.Sub(holdStart) < testFilter.RecoveryHold {
		held := at.Sub(holdStart)
		at = feed(tr, at, Assured)
		if tr.result().State != Unassured {
			t.Fatalf("recovered %s into a %s hold", held, testFilter.RecoveryHold)
		}
	}
	feed(tr, holdStart.Add(testFilter.RecoveryHold), Assured)
	if r := tr.result(); r.State != Assured || r.RecoveringSince != nil {
		t.Fatalf("after the hold = %+v, want assured", r)
	}
}

func TestTrackerRecoveryInterruptedRestarts(t *testing.T) {
	tr := newTracker(testInfo)
	at := feed(tr, t0, Unassured, Unassured, Unassured, Assured, Assured)
	at = at.Add(4 * time.Minute)
	at = feed(tr, at, Assured, Unassured, Unassured, Unassured) // back to unassured
	if r := tr.result(); r.State != Unassured || r.RecoveringSince != nil {
		t.Fatalf("relapse = %+v, want unassured with no recovery running", r)
	}
	at = feed(tr, at, Assured, Assured)
	at = at.Add(4 * time.Minute)
	feed(tr, at, Assured)
	if r := tr.result(); r.State != Unassured {
		t.Fatalf("recovered on an interrupted hold: %+v", r)
	}
}

func TestTrackerRecoveryServesWorstCandidateOfHold(t *testing.T) {
	tr := newTracker(testInfo)
	at := feed(tr, t0, Unassured, Unassured, Unassured)
	// Candidates during the hold: inconsistent, then assured.
	at = feed(tr, at, Inconsistent, Inconsistent, Inconsistent, Inconsistent)
	at = feed(tr, at.Add(time.Minute), Assured, Assured, Assured)
	at = feed(tr, at.Add(5*time.Minute), Assured)
	if r := tr.result(); r.State != Inconsistent {
		t.Fatalf("after a hold that saw inconsistent = %+v, want inconsistent", r)
	}
	// The hold toward assured starts only once inconsistent is served.
	at = feed(tr, at, Assured)
	feed(tr, at.Add(4*time.Minute), Assured)
	if r := tr.result(); r.State != Inconsistent || r.RecoveringSince == nil {
		t.Fatalf("inside the second hold = %+v, want inconsistent and recovering", r)
	}
	feed(tr, at.Add(5*time.Minute-time.Second), Assured)
	if r := tr.result(); r.State != Assured {
		t.Fatalf("after a second clean hold = %+v, want assured", r)
	}
}

func TestTrackerUnavailableClearsWindow(t *testing.T) {
	tr := newTracker(testInfo)
	at := feed(tr, t0, Unassured, Unassured, Assured)
	at = feed(tr, at, Unavailable)
	at = feed(tr, at, Unassured) // would make three of four without the reset
	if r := tr.result(); r.Candidate != Unavailable {
		t.Fatalf("candidate after a gap = %+v, want unavailable while the window refills", r)
	}
	feed(tr, at, Unassured, Unassured)
	if r := tr.result(); r.State != Unassured {
		t.Fatalf("refilled window = %+v, want unassured", r)
	}
}

func TestTrackerUnavailableAfterUnassuredIsHeld(t *testing.T) {
	tr := newTracker(testInfo)
	at := feed(tr, t0, Unassured, Unassured, Unassured)
	at = feed(tr, at, Unavailable)
	if r := tr.result(); r.State != Unassured || r.Candidate != Unavailable {
		t.Fatalf("losing input = %+v, want unassured held", r)
	}
	feed(tr, at.Add(5*time.Minute), Unavailable)
	if r := tr.result(); r.State != Unavailable {
		t.Fatalf("after the hold = %+v, want unavailable", r)
	}
}

func TestTrackerExpire(t *testing.T) {
	tr := newTracker(testInfo)
	last := feed(tr, t0, Assured, Assured, Assured).Add(-time.Second)
	if tr.expire(last.Add(5*time.Minute), 5*time.Minute, testFilter) {
		t.Fatal("expired at exactly the stale bound")
	}
	if !tr.expire(last.Add(5*time.Minute+time.Second), 5*time.Minute, testFilter) {
		t.Fatal("did not expire past the stale bound")
	}
	r := tr.result()
	if r.State != Unavailable || !slices.Contains(r.Reasons, ReasonStale) || r.EvaluatedAt != last.Unix() {
		t.Fatalf("expired = %+v, want unavailable/stale evaluated at %d", r, last.Unix())
	}
	if tr.expire(last.Add(time.Hour), 5*time.Minute, testFilter) {
		t.Fatal("expire changed an already unavailable tracker")
	}
}

func TestTrackerExpireHoldsDegradedState(t *testing.T) {
	tr := newTracker(testInfo)
	last := feed(tr, t0, Unassured, Unassured, Unassured).Add(-time.Second)
	tr.update(Verdict{
		State: Unassured, Reasons: []string{"measured_fault"},
		Metrics: map[string]float64{"residual": 12}, Thresholds: map[string]float64{"limit": 10},
	}, last, testFilter)
	stale := last.Add(6 * time.Minute)
	tr.expire(stale, 5*time.Minute, testFilter)
	checkEvidence := func(r Result) {
		t.Helper()
		if r.Metrics["residual"] != 12 || r.Thresholds["limit"] != 10 ||
			!slices.Equal(r.Reasons, []string{"measured_fault", ReasonStale}) || r.EvaluatedAt != last.Unix() {
			t.Fatalf("stale check lost the real evaluation's evidence: %+v", r)
		}
	}
	for _, at := range []time.Time{stale, stale.Add(time.Minute)} {
		tr.expire(at, 5*time.Minute, testFilter)
		r := tr.result()
		if r.State != Unassured {
			t.Fatalf("stale unassured check = %+v, want the degradation held", r)
		}
		checkEvidence(r)
	}
	tr.expire(stale.Add(5*time.Minute), 5*time.Minute, testFilter)
	r := tr.result()
	if r.State != Unavailable {
		t.Fatalf("after the hold = %+v, want unavailable", r)
	}
	checkEvidence(r)
}
