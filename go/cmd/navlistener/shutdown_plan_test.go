package main

import (
	"testing"
	"time"
)

// shutdown used one deadline consumed in order, so a slow
// producer, a stalled decode, or an SSE client that never disconnects could
// leave the historian no time at all — losing dial-mode frames permanently —
// while the process still logged a graceful shutdown and returned success. The
// plan exists so the historian's share cannot be spent by anything else.
func TestPlanShutdownReservesPersistenceBudget(t *testing.T) {
	for _, total := range []time.Duration{
		15 * time.Second, // the configured default
		time.Second,
		100 * time.Millisecond,
		time.Nanosecond, // degenerate, but must not produce a zero-length phase
		0,               // unset: falls back to the documented default
		-time.Second,
	} {
		p := planShutdown(total)
		if p.pipeline <= 0 || p.api <= 0 || p.store <= 0 || p.metrics <= 0 {
			t.Errorf("planShutdown(%v) produced a zero-length phase: %+v", total, p)
		}
		// The store's reservation must be a real share, not a remainder.
		if total >= time.Second && p.store < total/4 {
			t.Errorf("planShutdown(%v) reserved only %v for the historian", total, p.store)
		}
		// The serial path (the API phase runs concurrently with the pipeline) must
		// stay inside the configured bound.
		effective := total
		if effective <= 0 {
			effective = 15 * time.Second
		}
		// Only meaningful once every share clears the 1ms floor; below that the
		// floor deliberately wins, because a zero-length phase is worse than a
		// slightly overrunning one.
		if effective >= 10*time.Millisecond {
			if serial := p.pipeline + p.store + p.metrics; serial > effective {
				t.Errorf("planShutdown(%v): serial phases total %v, over the configured bound %v",
					total, serial, effective)
			}
		}
		// The API phase must fit inside the window it overlaps, so a stuck SSE
		// stream is force-closed well before the historian is even asked to drain.
		if p.api > p.pipeline {
			t.Errorf("planShutdown(%v): API phase %v exceeds the pipeline phase %v it overlaps",
				total, p.api, p.pipeline)
		}
	}
}

// Every phase must get the same share of a given budget regardless of how much
// an earlier phase consumed — that is what "non-consumable reservation" means.
func TestPlanShutdownPhasesAreIndependent(t *testing.T) {
	const total = 15 * time.Second
	a := planShutdown(total)
	b := planShutdown(total)
	if a != b {
		t.Fatalf("planShutdown is not deterministic: %+v vs %+v", a, b)
	}
	// Doubling the budget must scale the historian's reservation with it, rather
	// than handing the extra time to whichever phase runs first.
	d := planShutdown(2 * total)
	if d.store != 2*a.store {
		t.Errorf("historian reservation did not scale with the configured bound: %v vs %v",
			d.store, a.store)
	}
}
