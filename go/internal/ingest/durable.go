package ingest

import (
	"container/heap"
	"sync"
	"time"
)

// DurableTracker computes, per (observer, session), the GNF1 ACK watermark
// under the regression fix contract: the highest sequence N such that every
// sequenced frame ≤ N this collector RECEIVED has been durably resolved —
// committed by the historian (or deduped as a replay of an already-committed
// row), quarantined as unfixable, or classified never-persistable (telemetry,
// malformed body). Before the regression fix, ACK meant "queued in RAM": the feeder then
// pruned its only replay copy while the historian could still fail, so a DB
// outage after ACK permanently erased raw forensic evidence with both sides
// behaving to spec.
//
// Reception gaps are NOT waited for (the regression fix half of the contract that
// survives): a sequence that never arrived is skipped past — only RECEIVED
// frames can hold the watermark back. Keyed by session so a reconnect (same
// session, replayed frames) resumes the same accounting, and a rebooted
// feeder (fresh session, regression fix) starts clean.
//
// The tracker is nil when the collector runs without a historian — live-only
// mode, where stream() acks on receipt exactly as before (documented in
// wire.go's Ack contract).
type DurableTracker struct {
	mu                                                      sync.Mutex
	m                                                       map[durableKey]*sessionDurable
	outstanding, maxOutstanding, maxPerSession, maxSessions int
	nextPrune                                               time.Time
}

type durableKey struct{ source, session string }

type sessionDurable struct {
	highest     uint64         // highest sequence received, any class
	outstanding map[uint64]int // sequence -> index in pending min-heap
	pending     []uint64
	touched     time.Time
}

// Resolved idle sessions can be reclaimed; unresolved holes are never expired.
const durableSessionIdle = 7 * 24 * time.Hour
const (
	maxDurableSessions    = 1024
	maxDurableOutstanding = 65536
	maxDurablePerSession  = 4096
)

// Indexed min-heap: ACK lookup is O(1); receipt and arbitrary commit resolution
// are O(log per-session budget), without scanning other observers' holes.
type pendingHeap struct{ s *sessionDurable }

func (h pendingHeap) Len() int           { return len(h.s.pending) }
func (h pendingHeap) Less(i, j int) bool { return h.s.pending[i] < h.s.pending[j] }
func (h pendingHeap) Swap(i, j int) {
	h.s.pending[i], h.s.pending[j] = h.s.pending[j], h.s.pending[i]
	h.s.outstanding[h.s.pending[i]] = i
	h.s.outstanding[h.s.pending[j]] = j
}
func (h pendingHeap) Push(v any) {
	seq := v.(uint64)
	h.s.outstanding[seq] = len(h.s.pending)
	h.s.pending = append(h.s.pending, seq)
}
func (h pendingHeap) Pop() any {
	n := len(h.s.pending) - 1
	seq := h.s.pending[n]
	h.s.pending = h.s.pending[:n]
	delete(h.s.outstanding, seq)
	return seq
}

// NewDurableTracker builds an empty tracker.
func NewDurableTracker() *DurableTracker {
	return &DurableTracker{m: map[durableKey]*sessionDurable{}, maxSessions: maxDurableSessions, maxOutstanding: maxDurableOutstanding, maxPerSession: maxDurablePerSession}
}

// Received records one sequenced frame's arrival BEFORE it is handed to the
// decode stage (so a resolution can never race its own arrival). persistable
// is false for frames that will never reach the historian — telemetry
// (RF/observables) and the acked-immediately malformed classes — which
// advance the watermark without ever holding it.
// Received returns false before admitting a frame when the tracking budget is full.
// The caller must close the stream before handoff; reconnect can replay already
// tracked holes even at capacity and thereby release budget after commit.
func (t *DurableTracker) Received(source, session string, seq uint64, persistable bool) bool {
	// GNF1 sequences start at 1 (the C feeder's ++seq); 0 is never a valid
	// assigned sequence. Ignoring it here keeps a buggy/hostile seq-0 DATA
	// frame from wedging the watermark at oldest-1 underflow.
	if seq == 0 {
		return true
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	k := durableKey{source, session}
	s := t.m[k]
	if s == nil {
		if len(t.m) >= t.maxSessions && !now.Before(t.nextPrune) {
			t.pruneLocked(now)
			t.nextPrune = now.Add(time.Minute)
		}
		if len(t.m) >= t.maxSessions {
			return false
		}
		// Do not allocate a session for a frame the global budget cannot admit.
		if persistable && t.outstanding >= t.maxOutstanding {
			return false
		}
		s = &sessionDurable{outstanding: map[uint64]int{}}
		t.m[k] = s
	}
	if _, exists := s.outstanding[seq]; persistable && !exists {
		if len(s.pending) >= t.maxPerSession || t.outstanding >= t.maxOutstanding {
			return false
		}
		heap.Push(pendingHeap{s}, seq)
		t.outstanding++
	}
	s.touched = now
	if seq > s.highest {
		s.highest = seq
	}
	return true
}

// Resolved marks one sequenced frame durably resolved (the store's
// post-commit/quarantine notification). Resolving a sequence that was never
// received (or already resolved) is a harmless no-op — the store notifies per
// batch entry and a batch can span a reconnect's replayed duplicates.
func (t *DurableTracker) Resolved(source, session string, seq uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[durableKey{source, session}]
	if s == nil {
		return
	}
	s.touched = time.Now()
	if index, ok := s.outstanding[seq]; ok {
		heap.Remove(pendingHeap{s}, index)
		t.outstanding--
	}
}

// Watermark returns the current ACK value for the session: the highest
// received sequence when nothing is outstanding, else one below the oldest
// outstanding sequence. A replayed low sequence (reconnect) can make the
// value regress transiently; stream() only sends monotonically increasing
// acks per connection, and the feeder additionally clamps (spool_ack ignores
// regressions), so a transient dip is invisible on the wire.
func (t *DurableTracker) Watermark(source, session string) uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.m[durableKey{source, session}]
	if s == nil {
		return 0
	}
	if len(s.outstanding) == 0 {
		return s.highest
	}
	return s.pending[0] - 1
}

// pruneLocked drops only resolved sessions untouched for durableSessionIdle.
// At capacity, new sessions fail closed until these entries expire or restart.
// Unresolved sessions remain available for replay regardless of age. Caller holds mu.
func (t *DurableTracker) pruneLocked(now time.Time) {
	for k, s := range t.m {
		if len(s.pending) == 0 && now.Sub(s.touched) > durableSessionIdle {
			delete(t.m, k)
		}
	}
}
