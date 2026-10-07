package authorization

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// fakeNotifier is a LISTEN connection whose subscription fails a set number of
// times before holding; Wait then blocks until the listener is stopped.
type fakeNotifier struct {
	failListens int32
	listens     atomic.Int32
	// generations records the cache generation seen at each LISTEN attempt,
	// so a test can show the failed attempts changed nothing.
	mu          sync.Mutex
	generations []uint64
	provider    *Provider
	subscribed  chan struct{}
	once        sync.Once
}

func (n *fakeNotifier) Listen(context.Context) error {
	n.provider.mu.Lock()
	generation := n.provider.generation
	n.provider.mu.Unlock()
	n.mu.Lock()
	n.generations = append(n.generations, generation)
	n.mu.Unlock()
	if n.listens.Add(1) <= n.failListens {
		return errors.New("LISTEN refused by the pooler")
	}
	n.once.Do(func() { close(n.subscribed) })
	return nil
}

func (n *fakeNotifier) Wait(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}

func (n *fakeNotifier) Release() {}

// TestInvalidationOncePerOutage guards the invalidation listener clears the
// caches once per outage — when its LISTEN comes back — and not on each
// reconnect attempt: a control plane that accepts connections but fails the
// LISTEN three times must leave positive entries younger than the TTL in
// place, and the successful subscription that ends the outage must then
// invalidate exactly once.
func TestInvalidationOncePerOutage(t *testing.T) {
	p := newProvider(time.Minute, nil, func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		return testContext(), true, nil
	})
	if _, ok := p.Authenticate(context.Background(), "token", "observer-a", "ubx"); !ok {
		t.Fatal("seed lookup denied")
	}
	p.mu.Lock()
	before, cached := p.generation, len(p.observers)
	p.mu.Unlock()
	if cached != 1 {
		t.Fatalf("seed entry not cached (%d entries)", cached)
	}
	fake := &fakeNotifier{failListens: 3, provider: p, subscribed: make(chan struct{})}
	p.acquireNotifier = func(context.Context) (notifier, error) { return fake, nil }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.RunInvalidation(ctx) }()
	select {
	case <-fake.subscribed:
	case <-time.After(10 * time.Second):
		t.Fatal("listener never subscribed after the failed attempts")
	}
	fake.mu.Lock()
	attempts := append([]uint64(nil), fake.generations...)
	fake.mu.Unlock()
	if len(attempts) != 4 {
		t.Fatalf("LISTEN attempted %d times, want 4 (three failures, then success)", len(attempts))
	}
	for i, generation := range attempts {
		if generation != before {
			t.Errorf("attempt %d saw cache generation %d, want %d: a failed or not-yet-subscribed attempt must not invalidate", i+1, generation, before)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		after, remaining := p.generation, len(p.observers)
		p.mu.Unlock()
		if after == before+1 && remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("generation %d with %d entries after the LISTEN succeeded, want %d and an empty cache (invalidated exactly once)", after, remaining, before+1)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
}

// TestVerdictsKeepTheirProvenance guards VerifyAuthorization and
// ReconcileObserver tell a check the control plane could not answer (an error
// wrapping ErrUnavailable, nothing cached) apart from its own denial (ok=false,
// nil error, remembered in the negative cache), while Authenticate still fails
// closed on both.
func TestVerdictsKeepTheirProvenance(t *testing.T) {
	var answer atomic.Pointer[error]
	allowed := atomic.Bool{}
	p := newProvider(time.Minute, nil, func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		if err := answer.Load(); err != nil {
			return identity.ObserverContext{}, false, *err
		}
		return testContext(), allowed.Load(), nil
	})
	down := errors.New("control plane unreachable")
	answer.Store(&down)
	if _, ok, err := p.VerifyAuthorization(context.Background(), "token", "observer-a", "ubx"); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("lookup failure: (ok %v, err %v), want (false, ErrUnavailable)", ok, err)
	}
	if _, ok := p.Authenticate(context.Background(), "token", "observer-a", "ubx"); ok {
		t.Fatal("Authenticate granted on a lookup failure")
	}
	if _, ok, err := p.ReconcileObserver(context.Background(), "digest", "observer-a", "ubx"); ok || !errors.Is(err, ErrUnavailable) {
		t.Fatalf("reconcile on lookup failure: (ok %v, err %v), want (false, ErrUnavailable)", ok, err)
	}
	p.mu.Lock()
	cached := len(p.observers) + len(p.deniedObservers)
	p.mu.Unlock()
	if cached != 0 {
		t.Fatalf("%d entries cached from a failed lookup, want none (the next attempt must retry)", cached)
	}

	answer.Store(nil)
	if _, ok, err := p.VerifyAuthorization(context.Background(), "token", "observer-a", "ubx"); ok || err != nil {
		t.Fatalf("authoritative denial: (ok %v, err %v), want (false, nil)", ok, err)
	}
	if _, ok, err := p.ReconcileObserver(context.Background(), "digest", "observer-a", "ubx"); ok || err != nil {
		t.Fatalf("reconcile on authoritative denial: (ok %v, err %v), want (false, nil)", ok, err)
	}
	p.mu.Lock()
	denied := len(p.deniedObservers)
	p.mu.Unlock()
	if denied != 1 {
		t.Fatalf("authoritative denial not remembered (%d negative entries)", denied)
	}

	allowed.Store(true)
	p.InvalidateAll()
	if _, ok, err := p.VerifyAuthorization(context.Background(), "token", "observer-a", "ubx"); !ok || err != nil {
		t.Fatalf("grant: (ok %v, err %v), want (true, nil)", ok, err)
	}
}
