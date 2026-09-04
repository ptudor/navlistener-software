package ingest

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/ptudor/navlistener/internal/identity"
)

// An observer's policy lock orders transitions and their reset markers. It is
// separate from the server map lock so backpressure for one observer never
// locks another observer's authorization lookup or DATA admission.
type observerPolicy struct {
	mu         sync.Mutex
	generation atomic.Uint64
	current    identity.ObserverContext
	sessions   map[*Admission]context.CancelFunc
}

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

func (p *PushServer) admit(ctx context.Context, current identity.ObserverContext, lookedUpAt uint64, cancel context.CancelFunc) *Admission {
	p.authorizationMu.Lock()
	policy := p.policies[current.ObserverID]
	if policy == nil {
		policy = &observerPolicy{sessions: make(map[*Admission]context.CancelFunc)}
		p.policies[current.ObserverID] = policy
	}
	p.authorizationMu.Unlock()
	policy.mu.Lock()
	defer policy.mu.Unlock()
	if ctx.Err() != nil {
		return nil
	}
	generation := policy.generation.Load()
	// A lookup begun before another authoritative transition cannot undo it.
	// Equal-policy simultaneous connections remain legitimate.
	if generation != lookedUpAt && !policy.current.AuthorizationEqual(current) {
		return nil
	}
	if generation == 0 {
		policy.current = current
		policy.generation.Store(1)
	} else if !policy.current.AuthorizationEqual(current) {
		if !p.transitionPolicyLocked(ctx, policy, current) {
			return nil
		}
	}
	a := &Admission{policy: policy, generation: policy.generation.Load()}
	policy.sessions[a] = cancel
	return a
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
