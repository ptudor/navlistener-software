package ingest

import (
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
	mu sync.Mutex
	m  map[durableKey]*sessionDurable
}

type durableKey struct{ source, session string }

type sessionDurable struct {
	highest     uint64              // highest sequence received, any class
	outstanding map[uint64]struct{} // received persistable frames not yet durably resolved
	touched     time.Time
}

// durableSessionIdle bounds how long an untouched session's accounting is
// kept. Sessions are per feeder boot : after a week of silence the
// feeder either reconnected (touching it) or rebooted into a new session, so
// the entry can only be dead weight. Generous versus the feeders' minutes-
// scale reconnect ladder.
const durableSessionIdle = 7 * 24 * time.Hour

// NewDurableTracker builds an empty tracker.
func NewDurableTracker() *DurableTracker {
	return &DurableTracker{m: map[durableKey]*sessionDurable{}}
}

// Received records one sequenced frame's arrival BEFORE it is handed to the
// decode stage (so a resolution can never race its own arrival). persistable
// is false for frames that will never reach the historian — telemetry
// (RF/observables) and the acked-immediately malformed classes — which
// advance the watermark without ever holding it.
func (t *DurableTracker) Received(source, session string, seq uint64, persistable bool) {
	// GNF1 sequences start at 1 (the C feeder's ++seq); 0 is never a valid
	// assigned sequence. Ignoring it here keeps a buggy/hostile seq-0 DATA
	// frame from wedging the watermark at oldest-1 underflow.
	if seq == 0 {
		return
	}
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	k := durableKey{source, session}
	s := t.m[k]
	if s == nil {
		if len(t.m) >= 128 {
			t.pruneLocked(now) // opportunistic; the fleet is dozens of sessions, not thousands
		}
		s = &sessionDurable{outstanding: map[uint64]struct{}{}}
		t.m[k] = s
	}
	s.touched = now
	if seq > s.highest {
		s.highest = seq
	}
	if persistable {
		s.outstanding[seq] = struct{}{}
	}
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
	delete(s.outstanding, seq)
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
	oldest := uint64(0)
	first := true
	for seq := range s.outstanding {
		if first || seq < oldest {
			oldest, first = seq, false
		}
	}
	return oldest - 1
}

// pruneLocked drops sessions untouched for durableSessionIdle. Caller holds mu.
func (t *DurableTracker) pruneLocked(now time.Time) {
	for k, s := range t.m {
		if now.Sub(s.touched) > durableSessionIdle {
			delete(t.m, k)
		}
	}
}
