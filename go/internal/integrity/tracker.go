package integrity

import (
	"maps"
	"slices"
	"time"
)

// Reason codes the tracker itself attaches.
const (
	ReasonWarmingUp = "warming_up" // fewer than M available evaluations since input resumed
	ReasonStale     = "stale"      // no evaluation within the profile's stale bound
)

// tracker holds one check's filter window and hysteresis state.
type tracker struct {
	info Info

	// window holds the most recent available raw states, newest last, at most N.
	// An unavailable evaluation clears it, so a gap must be re-earned.
	window    []State
	candidate State

	served State
	since  time.Time
	// recovery is the hold in progress toward a better state: when it began and
	// the worst candidate seen since, which is what the hold may serve.
	recovering    bool
	recoverySince time.Time
	recoveryFloor State

	last   Verdict
	lastAt time.Time
	seen   bool
}

func newTracker(info Info) *tracker {
	return &tracker{info: info, candidate: Unavailable, served: Unavailable}
}

// update folds one raw evaluation into the filter and the hysteresis.
func (t *tracker) update(v Verdict, now time.Time, f FilterPolicy) {
	if v.State == "" {
		v.State = Unavailable
	}
	if v.State == Unavailable {
		t.window = t.window[:0]
		t.candidate = Unavailable
	} else {
		t.window = append(t.window, v.State)
		if len(t.window) > f.N {
			t.window = t.window[len(t.window)-f.N:]
		}
		t.candidate = filterWindow(t.window, f.M)
		if t.candidate == Unavailable {
			v.Reasons = appendReason(v.Reasons, ReasonWarmingUp)
		}
	}
	t.hold(now, f.RecoveryHold)
	t.last, t.lastAt, t.seen = v, now, true
}

// filterWindow applies the M-of-N rule: the window supports a degraded state when at
// least m of its entries are that bad or worse. Fewer than m entries support nothing
// yet, so the check is still warming up.
func filterWindow(window []State, m int) State {
	if len(window) < m {
		return Unavailable
	}
	unassured, degraded := 0, 0
	for _, s := range window {
		switch s {
		case Unassured:
			unassured++
			degraded++
		case Inconsistent:
			degraded++
		}
	}
	switch {
	case unassured >= m:
		return Unassured
	case degraded >= m:
		return Inconsistent
	}
	return Assured
}

// hold applies one-sided hysteresis to the filtered candidate. A worse candidate is
// served at once. A better one is served only after every candidate for the hold
// period has been better than the served state, and then at the worst of them. A
// check that has merely been unavailable recovers at once: nothing was found wrong.
func (t *tracker) hold(now time.Time, period time.Duration) {
	c := t.candidate
	switch {
	case !t.seen:
		t.served, t.since = c, now
	case c.rank() < t.served.rank():
		t.served, t.since, t.recovering = c, now, false
	case c.rank() == t.served.rank():
		t.recovering = false
	case t.served == Unavailable:
		t.served, t.since, t.recovering = c, now, false
	default: // recovering from a degraded state
		if !t.recovering {
			t.recovering, t.recoverySince, t.recoveryFloor = true, now, c
		} else if c.rank() < t.recoveryFloor.rank() {
			t.recoveryFloor = c
		}
		if now.Sub(t.recoverySince) >= period {
			t.served, t.since, t.recovering = t.recoveryFloor, now, false
		}
	}
}

// expire reports staleness: a check with no evaluation for longer than staleAfter is
// evaluated as unavailable, so a station whose input stopped cannot keep serving a
// state measured long ago. It returns whether it changed anything.
func (t *tracker) expire(now time.Time, staleAfter time.Duration, f FilterPolicy) bool {
	if !t.seen || now.Sub(t.lastAt) <= staleAfter || (t.candidate == Unavailable && t.served == Unavailable) {
		return false
	}
	lastAt := t.lastAt
	t.update(Verdict{
		State: Unavailable, Reasons: appendReason(t.last.Reasons, ReasonStale),
		Metrics: t.last.Metrics, Thresholds: t.last.Thresholds,
	}, now, f)
	t.lastAt = lastAt // staleness is measured from the last real evaluation
	return true
}

// result snapshots the tracker for serving.
func (t *tracker) result() Result {
	r := Result{
		Check: t.info.Name, Version: t.info.Version, Domain: t.info.Domain,
		State: t.served, Candidate: t.candidate, lowerOnly: t.info.LowerOnly,
		Metrics:    maps.Clone(t.last.Metrics),
		Thresholds: maps.Clone(t.last.Thresholds),
		Reasons:    slices.Clone(t.last.Reasons),
	}
	if t.seen {
		r.Since, r.EvaluatedAt = unix(t.since), unix(t.lastAt)
	}
	if t.recovering {
		s := unix(t.recoverySince)
		r.RecoveringSince = &s
	}
	return r
}

func appendReason(reasons []string, r string) []string {
	if slices.Contains(reasons, r) {
		return reasons
	}
	return append(reasons, r)
}
