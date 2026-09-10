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
}

// Bound the retained reconciliation work, including disconnected contributors.
// At capacity new identities fail closed; retained history is never forgotten
// merely to admit a new observer. A restart establishes a new history epoch.
const maxTrackedObserverPolicies = 1024
const maxPolicyCredentials = 8

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
			p.authorizationMu.Unlock()
			return nil, "observer_ceiling"
		}
		policy = &observerPolicy{sessions: make(map[*Admission]context.CancelFunc)}
		p.policies[current.ObserverID] = policy
		// policies are retained for the process lifetime, so this
		// gauge only rises. It exists so an operator can see the ceiling coming
		// instead of discovering it when a valid new observer is refused.
		metrics.PushObserverPoliciesTracked.Set(float64(len(p.policies)))
	}
	p.authorizationMu.Unlock()
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if ctx.Err() != nil {
		return nil, "canceled"
	}
	generation := policy.generation.Load()
	// A lookup begun before another authoritative transition cannot undo it.
	// Equal-policy simultaneous connections remain legitimate.
	if generation != lookedUpAt && !policy.current.AuthorizationEqual(current) {
		return nil, "stale_lookup"
	}
	if generation == 0 {
		policy.current = current
		policy.generation.Store(1)
	} else if !policy.current.AuthorizationEqual(current) {
		if !p.transitionPolicyLocked(ctx, policy, current) {
			return nil, "reset_blocked"
		}
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
	a := &Admission{policy: policy, generation: policy.generation.Load()}
	policy.sessions[a] = cancel
	return a, ""
}

func (a *Admission) release() {
	a.policy.mu.Lock()
	delete(a.policy.sessions, a)
	a.policy.mu.Unlock()
}

func (p *PushServer) transitionPolicyLocked(ctx context.Context, policy *observerPolicy, current identity.ObserverContext) bool {
	previous := policy.current
	policy.generation.Add(1)
	policy.current = current
	policy.credentials = nil
	for a, cancel := range policy.sessions {
		cancel()
		delete(policy.sessions, a)
	}
	// Hold the per-observer lock until the marker is admitted. New-policy
	// producers cannot start before it; old producers carry obsolete generations.
	return p.enqueueScopeRevocation(ctx, previous)
}

func (p *PushServer) changeAdmission(ctx context.Context, a *Admission, current identity.ObserverContext) {
	a.policy.mu.Lock()
	defer a.policy.mu.Unlock()
	if !a.Current() {
		return
	} // a late old-session watcher is not new authority
	p.transitionPolicyLocked(ctx, a.policy, current)
}
