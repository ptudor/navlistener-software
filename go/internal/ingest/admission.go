package ingest

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
)

// An observer's policy lock orders transitions and their reset markers. It is
// separate from the server map lock so backpressure for one observer never
// locks another observer's authorization lookup or DATA admission.
type observerPolicy struct {
	mu         sync.Mutex
	generation atomic.Uint64
	current    identity.ObserverContext
	sessions   map[*Admission]context.CancelFunc
	// credentials retains the digests that admitted data under this policy so
	// disconnected contributors can be reconciled, keyed to their
	// last admission time. Bounded by maxPolicyCredentials: at capacity the
	// least recently admitted digest is evicted rather than the session refused
	// — any one retained credential detects a withdrawal, whereas refusing
	// silently locked out a token-rotating observer (whose token tier has no
	// fingerprint, so rotation is not a policy transition) around its third
	// rotation until restart (astra-6 verification of regression fix).
	credentials map[policyCredential]time.Time
	// verifiedAt is when the control plane last confirmed this policy's
	// authority: at an admission, at a live session's periodic recheck, or in
	// a reconciliation sweep. A recheck the control plane could not answer
	// leaves the policy as it is; a live session is withdrawn only once the
	// time since verifiedAt exceeds the documented revocation bound, and the
	// sweep does not query a policy verified within the authority TTL.
	verifiedAt time.Time
	// lastAdmitted is when a session of this observer was last admitted. A
	// policy with no live session and no admission within the policy
	// retention window is reclaimed when the table is at its ceiling: nothing
	// it admitted is still visible to withdraw, and holding it only turned a
	// churned fleet (replaced boards, bench stations, renamed observers) into
	// a lockout of the next new identity until restart.
	lastAdmitted time.Time
	// transition is non-nil while a policy transition's reset marker is still
	// being enqueued, and is closed once the marker is in the decode channel.
	// admit waits on it — never on mu — so no new-generation producer starts
	// before the marker, while release and the reconciliation snapshot take mu
	// for their bookkeeping without waiting behind decode backpressure.
	transition chan struct{}
}

// Bound the retained reconciliation work, including disconnected contributors.
// At capacity, policies with no live session and no admission within the
// retention window are reclaimed first; only then do new identities fail
// closed. A policy with a live session is never forgotten. A restart
// establishes a new history epoch.
const maxTrackedObserverPolicies = 1024
const maxPolicyCredentials = 8

// defaultPolicyRetention is how long a policy with no live session is kept
// for reconciliation after its last admission when no raw-retention horizon
// is configured (SetPolicyRetention): the historian's default raw retention,
// past which nothing the observer contributed remains to withdraw.
const defaultPolicyRetention = 7 * 24 * time.Hour

// maxObserverSessions bounds the sessions one observer may hold at once. A
// credential legitimately runs a few — the C feeder replays each recovered
// spool on its own connection, one after another, and a reconnect briefly
// overlaps the session it replaces until the old one times out — but never
// the fleet's worth: one stolen token must not occupy every slot, each with
// its own zstd window.
const maxObserverSessions = 4

type policyCredential struct{ digest, feed string }

// Admission binds immutable receipt provenance to a policy generation. A
// canceled producer can have already handed off DATA (including buffered zstd
// records); the decoder checks this generation after forensic persistence and
// before any audience projection. Disconnect alone does not invalidate receipts.
type Admission struct {
	policy     *observerPolicy
	generation uint64
}

func (a *Admission) Current() bool {
	return a == nil || a.policy.generation.Load() == a.generation
}

// verified records that the control plane confirmed this admission's policy
// at now (the unavailability bound and the sweep's skip rule read it).
func (a *Admission) verified(now time.Time) {
	a.policy.mu.Lock()
	if now.After(a.policy.verifiedAt) {
		a.policy.verifiedAt = now
	}
	a.policy.mu.Unlock()
}

// verifiedAt is when the control plane last confirmed this admission's policy.
func (a *Admission) verifiedAt() time.Time {
	a.policy.mu.Lock()
	defer a.policy.mu.Unlock()
	return a.policy.verifiedAt
}

type admissionContextKey struct{}

func (p *PushServer) policyGeneration(observer string) uint64 {
	p.authorizationMu.Lock()
	policy := p.policies[observer]
	p.authorizationMu.Unlock()
	if policy == nil {
		return 0
	}
	return policy.generation.Load()
}

// admit returns nil and a reason label (PushAdmissionRefusedTotal) when the
// session cannot be admitted under current policy.
func (p *PushServer) admit(ctx context.Context, current identity.ObserverContext, lookedUpAt uint64, cancel context.CancelFunc, credentials ...policyCredential) (*Admission, string) {
	p.authorizationMu.Lock()
	policy := p.policies[current.ObserverID]
	if policy == nil {
		if len(p.policies) >= maxTrackedObserverPolicies {
			p.reclaimIdlePoliciesLocked(time.Now())
		}
		if len(p.policies) >= maxTrackedObserverPolicies {
			p.authorizationMu.Unlock()
			return nil, "observer_ceiling"
		}
		policy = &observerPolicy{sessions: make(map[*Admission]context.CancelFunc)}
		p.policies[current.ObserverID] = policy
		// The gauge is the table's size, so an operator can see the ceiling
		// coming instead of discovering it when a valid new observer is refused.
		metrics.PushObserverPoliciesTracked.Set(float64(len(p.policies)))
	}
	p.authorizationMu.Unlock()
	policy.mu.Lock()
	defer policy.mu.Unlock()
	for {
		if ctx.Err() != nil {
			return nil, "canceled"
		}
		generation := policy.generation.Load()
		// A lookup begun before another authoritative transition cannot undo it.
		// Equal-policy simultaneous connections remain legitimate. Decided
		// before waiting on any pending marker: a refusal produces no DATA.
		if generation != lookedUpAt && !policy.current.AuthorizationEqual(current) {
			return nil, "stale_lookup"
		}
		if pending := policy.transition; pending != nil {
			// Another session's transition is still enqueueing its reset
			// marker: no new-generation producer may start before it. Wait
			// off the lock, so releases and bookkeeping proceed meanwhile.
			if !awaitPending(&policy.mu, pending, ctx) {
				return nil, "canceled"
			}
			continue
		}
		if generation == 0 {
			policy.current = current
			policy.generation.Store(1)
			break
		}
		if policy.current.AuthorizationEqual(current) {
			break
		}
		previous, pending := p.beginTransitionLocked(policy, current)
		policy.mu.Unlock()
		enqueued := p.completeTransition(ctx, policy, previous, pending)
		policy.mu.Lock()
		if !enqueued {
			return nil, "reset_blocked"
		}
		// The marker is in. The policy is this session's unless a later
		// transition moved it again, which the next pass reports as stale.
	}
	if len(policy.sessions) >= maxObserverSessions {
		return nil, "observer_sessions"
	}
	now := time.Now()
	for _, credential := range credentials {
		if policy.credentials == nil {
			policy.credentials = make(map[policyCredential]time.Time)
		}
		if _, exists := policy.credentials[credential]; !exists && len(policy.credentials) >= maxPolicyCredentials {
			var oldest policyCredential
			first := true
			for c, at := range policy.credentials {
				if first || at.Before(policy.credentials[oldest]) {
					oldest, first = c, false
				}
			}
			delete(policy.credentials, oldest)
		}
		policy.credentials[credential] = now
	}
	// The handshake that reached admission verified this policy just now.
	if now.After(policy.verifiedAt) {
		policy.verifiedAt = now
	}
	policy.lastAdmitted = now
	a := &Admission{policy: policy, generation: policy.generation.Load()}
	policy.sessions[a] = cancel
	return a, ""
}

// reclaimIdlePoliciesLocked forgets every policy with no live session whose
// last admission is older than the policy retention window, and resets the
// capacity gauge. A live policy keeps its generation; an observer that
// returns after reclamation starts a fresh one. Caller holds authorizationMu.
func (p *PushServer) reclaimIdlePoliciesLocked(now time.Time) {
	for observer, policy := range p.policies {
		policy.mu.Lock()
		idle := len(policy.sessions) == 0 && now.Sub(policy.lastAdmitted) > p.policyRetention
		policy.mu.Unlock()
		if idle {
			delete(p.policies, observer)
		}
	}
	metrics.PushObserverPoliciesTracked.Set(float64(len(p.policies)))
}

// SetPolicyRetention sets how long a policy with no live session is kept for
// offline reconciliation after its last admission — the historian's raw
// retention, past which nothing the observer contributed remains to withdraw.
func (p *PushServer) SetPolicyRetention(d time.Duration) {
	if d > 0 {
		p.policyRetention = d
	}
}

// awaitPending releases mu while it waits for a transition's marker to be
// enqueued (pending closes) or for ctx to end, then re-acquires mu. It reports
// whether the marker was enqueued.
func awaitPending(mu *sync.Mutex, pending <-chan struct{}, ctx context.Context) bool {
	mu.Unlock()
	defer mu.Lock()
	select {
	case <-pending:
		return true
	case <-ctx.Done():
		return false
	}
}

// release ends a session's admission. It takes only the policy lock, which no
// transition holds across its marker send, so an ending session never waits
// behind decode backpressure.
func (a *Admission) release() {
	a.policy.mu.Lock()
	delete(a.policy.sessions, a)
	a.policy.mu.Unlock()
}

// beginTransitionLocked records a policy transition under policy.mu: the
// generation advances, the context is replaced, every older session is
// cancelled and the retained credentials are dropped. The reset marker is not
// yet enqueued; the returned channel is closed by completeTransition once it
// is, and admit waits on it so no new-generation DATA precedes the marker.
func (p *PushServer) beginTransitionLocked(policy *observerPolicy, current identity.ObserverContext) (previous identity.ObserverContext, pending chan struct{}) {
	previous = policy.current
	policy.generation.Add(1)
	policy.current = current
	policy.credentials = nil
	for a, cancel := range policy.sessions {
		cancel()
		delete(policy.sessions, a)
	}
	pending = make(chan struct{})
	policy.transition = pending
	return previous, pending
}

// completeTransition enqueues the transition's reset marker — off the policy
// lock, since the decode channel may be backpressured — then clears the pending
// state and wakes every admission waiting on it. It reports whether the marker
// was enqueued, which fails only when ctx ended first (shutdown). Exactly one
// marker per transition: beginTransitionLocked hands out one pending channel,
// and this is its only consumer.
func (p *PushServer) completeTransition(ctx context.Context, policy *observerPolicy, previous identity.ObserverContext, pending chan struct{}) bool {
	enqueued := p.enqueueScopeRevocation(ctx, previous)
	policy.mu.Lock()
	if policy.transition == pending {
		policy.transition = nil
	}
	policy.mu.Unlock()
	close(pending)
	return enqueued
}

func (p *PushServer) changeAdmission(ctx context.Context, a *Admission, current identity.ObserverContext) {
	a.policy.mu.Lock()
	if !a.Current() {
		a.policy.mu.Unlock()
		return
	} // a late old-session watcher is not new authority
	previous, pending := p.beginTransitionLocked(a.policy, current)
	a.policy.mu.Unlock()
	p.completeTransition(ctx, a.policy, previous, pending)
}
