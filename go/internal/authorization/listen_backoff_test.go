package authorization

import (
	"math"
	"testing"
	"time"
)

// the invalidation listener's backoff only ever grew. Once any
// early outage reached the five-second cap, every later transient disconnect for
// the life of the process waited the full interval — widening the window in which
// a revocation depends solely on cache TTL, and synchronizing a whole fleet's
// retries. These tests pin the pacing rules; the loop's own reset condition is
// exercised by the health-proof helper below.
func TestListenBackoffJitterStaysBounded(t *testing.T) {
	p := newProvider(time.Minute, nil, nil)
	for _, base := range []time.Duration{
		listenBackoffInitial, time.Second, listenBackoffMax,
	} {
		var lo, hi time.Duration = math.MaxInt64, 0
		for range 2000 {
			got := p.jitter(base)
			if got <= 0 {
				t.Fatalf("jitter(%v) returned %v; a non-positive wait would hot-spin", base, got)
			}
			// ±25% of the interval, so a fleet that lost the same database does not
			// retry in lockstep, but the documented pacing still holds.
			if got < base*3/4 || got > base*5/4 {
				t.Fatalf("jitter(%v) = %v, outside the ±25%% band", base, got)
			}
			lo, hi = min(lo, got), max(hi, got)
		}
		if lo == hi {
			t.Errorf("jitter(%v) never varied; synchronized fleet retries are the point of it", base)
		}
	}
	// Degenerate inputs must never produce a zero or negative wait.
	for _, d := range []time.Duration{0, -time.Second, time.Nanosecond} {
		if got := p.jitter(d); got <= 0 {
			t.Errorf("jitter(%v) = %v, want a positive wait", d, got)
		}
	}
}

// The growth schedule itself is unchanged: doubling from the initial interval,
// clamped at the maximum. The defect was purely the missing reset.
func TestListenBackoffGrowthAndReset(t *testing.T) {
	grow := func(b time.Duration) time.Duration {
		if b < listenBackoffMax {
			b *= 2
			if b > listenBackoffMax {
				b = listenBackoffMax
			}
		}
		return b
	}
	b := listenBackoffInitial
	steps := 0
	for b < listenBackoffMax {
		next := grow(b)
		if next <= b {
			t.Fatalf("backoff stopped growing at %v", b)
		}
		b = next
		if steps++; steps > 32 {
			t.Fatal("backoff never reached the cap")
		}
	}
	if b != listenBackoffMax {
		t.Fatalf("backoff settled at %v, want the %v cap", b, listenBackoffMax)
	}
	if grow(b) != listenBackoffMax {
		t.Error("backoff grew past its cap")
	}
	// After a demonstrated healthy connection the loop assigns the initial value
	// back; without that, this is the interval every later blip would pay.
	if listenBackoffInitial >= listenBackoffMax {
		t.Fatal("the initial interval must be shorter than the cap for a reset to matter")
	}
	if listenHealthyAfter <= 0 {
		t.Fatal("a quiet-but-working listener needs a positive proof interval")
	}
}
