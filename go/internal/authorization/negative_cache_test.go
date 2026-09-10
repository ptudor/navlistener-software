package authorization

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// regression fix. Before this fix, denied credentials were deliberately never
// cached, so one invalid bearer token could force a control-plane query — and a
// connection-pool slot — on every single request, with no single-flight and no
// concurrency ceiling. These tests pin the three bounds that replaced that:
// a short negative cache, per-key single-flight, and a fixed lookup-slot budget.

// deniedProvider returns a provider whose lookups always deny, counting calls
// and blocking on gate so a leader can be parked mid-query.
func deniedProvider(t *testing.T, ttl time.Duration, gate <-chan struct{}) (*Provider, *atomic.Int64, *atomic.Int64) {
	t.Helper()
	var observerCalls, readCalls atomic.Int64
	p := newProvider(ttl, nil, func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		observerCalls.Add(1)
		if gate != nil {
			<-gate
		}
		return identity.ObserverContext{}, false, nil
	})
	p.lookupRead = func(context.Context, string) (identity.ReadPrincipal, bool, error) {
		readCalls.Add(1)
		if gate != nil {
			<-gate
		}
		return identity.ReadPrincipal{}, false, nil
	}
	return p, &observerCalls, &readCalls
}

func TestConcurrentDeniedLookupsIssueOneQuery(t *testing.T) {
	gate := make(chan struct{})
	p, observerCalls, readCalls := deniedProvider(t, time.Minute, gate)

	// Park one leader per API inside its lookup so the remaining callers can only
	// arrive after the flight is registered; without single-flight each of them
	// would issue its own query.
	var leaders sync.WaitGroup
	leaders.Add(2)
	go func() {
		defer leaders.Done()
		if _, ok := p.Authenticate(context.Background(), "bad", "observer-a", "ubx"); ok {
			t.Error("denied observer token admitted")
		}
	}()
	go func() {
		defer leaders.Done()
		if _, ok := p.AuthorizeRead(context.Background(), "bad"); ok {
			t.Error("denied read token admitted")
		}
	}()
	waitFor(t, func() bool { return observerCalls.Load() == 1 && readCalls.Load() == 1 })

	var followers sync.WaitGroup
	for range 99 {
		followers.Add(2)
		go func() {
			defer followers.Done()
			if _, ok := p.Authenticate(context.Background(), "bad", "observer-a", "ubx"); ok {
				t.Error("denied observer token admitted")
			}
		}()
		go func() {
			defer followers.Done()
			if _, ok := p.AuthorizeRead(context.Background(), "bad"); ok {
				t.Error("denied read token admitted")
			}
		}()
	}
	// Give the followers a chance to register as leaders if single-flight is broken.
	time.Sleep(20 * time.Millisecond)
	close(gate)
	leaders.Wait()
	followers.Wait()

	if got := observerCalls.Load(); got != 1 {
		t.Fatalf("100 concurrent denied observer requests issued %d lookups, want 1", got)
	}
	if got := readCalls.Load(); got != 1 {
		t.Fatalf("100 concurrent denied read requests issued %d lookups, want 1", got)
	}

	// And the denial is now remembered: repeats within negativeTTL issue nothing.
	for range 50 {
		if _, ok := p.Authenticate(context.Background(), "bad", "observer-a", "ubx"); ok {
			t.Fatal("denied observer token admitted from negative cache")
		}
		if _, ok := p.AuthorizeRead(context.Background(), "bad"); ok {
			t.Fatal("denied read token admitted from negative cache")
		}
	}
	if got := observerCalls.Load(); got != 1 {
		t.Fatalf("repeated denial issued %d observer lookups, want 1", got)
	}
	if got := readCalls.Load(); got != 1 {
		t.Fatalf("repeated denial issued %d read lookups, want 1", got)
	}
}

func TestNegativeCacheExpiresAndIsInvalidated(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refresh func(p *Provider, clock *atomic.Int64)
	}{
		{"expiry", func(p *Provider, clock *atomic.Int64) {
			// negativeTTL is min(ttl, defaultNegativeCacheTTL); step past it.
			clock.Add(int64(p.negativeTTL + time.Millisecond))
		}},
		{"invalidate_all", func(p *Provider, _ *atomic.Int64) { p.InvalidateAll() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			p, observerCalls, readCalls := deniedProvider(t, time.Second, nil)
			p.now = func() time.Time { return time.Unix(0, clock.Load()) }

			p.Authenticate(context.Background(), "bad", "observer-a", "ubx")
			p.AuthorizeRead(context.Background(), "bad")
			p.Authenticate(context.Background(), "bad", "observer-a", "ubx")
			p.AuthorizeRead(context.Background(), "bad")
			if observerCalls.Load() != 1 || readCalls.Load() != 1 {
				t.Fatalf("denial not cached: observer=%d read=%d", observerCalls.Load(), readCalls.Load())
			}

			tc.refresh(p, &clock)
			p.Authenticate(context.Background(), "bad", "observer-a", "ubx")
			p.AuthorizeRead(context.Background(), "bad")
			if observerCalls.Load() != 2 || readCalls.Load() != 2 {
				t.Fatalf("stale denial reused: observer=%d read=%d", observerCalls.Load(), readCalls.Load())
			}
		})
	}
}

func TestLookupErrorsAreRetriedNotCachedAsDenial(t *testing.T) {
	var observerCalls, readCalls atomic.Int64
	failing := errors.New("control plane unreachable")
	p := newProvider(time.Minute, nil, func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		observerCalls.Add(1)
		return identity.ObserverContext{}, false, failing
	})
	p.lookupRead = func(context.Context, string) (identity.ReadPrincipal, bool, error) {
		readCalls.Add(1)
		return identity.ReadPrincipal{}, false, failing
	}
	for range 5 {
		if _, ok := p.Authenticate(context.Background(), "bad", "observer-a", "ubx"); ok {
			t.Fatal("failed observer lookup admitted")
		}
		if _, ok := p.AuthorizeRead(context.Background(), "bad"); ok {
			t.Fatal("failed read lookup admitted")
		}
	}
	if observerCalls.Load() != 5 || readCalls.Load() != 5 {
		t.Fatalf("database error cached as authoritative denial: observer=%d read=%d",
			observerCalls.Load(), readCalls.Load())
	}
	if len(p.deniedObservers) != 0 || len(p.deniedReaders) != 0 {
		t.Fatal("database error recorded in a negative cache")
	}
}

// A stream of never-repeated invalid tokens misses every cache by construction,
// so the only remaining bounds are the negative-cache budget and the fixed
// lookup-slot ceiling. Both must hold, and the flood must not evict the positive
// authority a legitimate client already established.
func TestUniqueInvalidTokenFloodStaysBounded(t *testing.T) {
	var inflight, peak atomic.Int64
	var slow sync.WaitGroup
	p := newProvider(time.Minute, nil, func(_ context.Context, digest, station, _ string) (identity.ObserverContext, bool, error) {
		n := inflight.Add(1)
		defer inflight.Add(-1)
		for {
			old := peak.Load()
			if n <= old || peak.CompareAndSwap(old, n) {
				break
			}
		}
		// Hold the slot long enough for concurrency to actually build up.
		time.Sleep(time.Millisecond)
		if station == "observer-a" {
			return testContext(), true, nil
		}
		return identity.ObserverContext{}, false, nil
	})
	p.lookupRead = func(context.Context, string) (identity.ReadPrincipal, bool, error) {
		return identity.ReadPrincipal{}, false, nil
	}
	p.negativeLimit = 64

	if _, ok := p.Authenticate(context.Background(), "good", "observer-a", "ubx"); !ok {
		t.Fatal("valid authority denied")
	}

	const workers, perWorker = 32, 200
	for w := range workers {
		slow.Add(1)
		go func() {
			defer slow.Done()
			for i := range perWorker {
				token := fmt.Sprintf("invalid-%d-%d", w, i)
				if _, ok := p.Authenticate(context.Background(), token, "observer-b", "ubx"); ok {
					t.Error("invalid token admitted")
				}
				p.AuthorizeRead(context.Background(), token)
			}
		}()
	}
	slow.Wait()

	if got := peak.Load(); got > int64(cap(p.lookupSlots)) {
		t.Fatalf("peak concurrent control-plane lookups %d exceeds the %d-slot budget", got, cap(p.lookupSlots))
	}
	if got := peak.Load(); got < 2 {
		t.Fatalf("flood never overlapped (peak %d); the concurrency bound was not exercised", got)
	}
	p.mu.Lock()
	denied, deniedReads, positives := len(p.deniedObservers), len(p.deniedReaders), len(p.observers)
	p.mu.Unlock()
	if denied > p.negativeLimit || deniedReads > p.negativeLimit {
		t.Fatalf("negative caches unbounded: observers=%d readers=%d limit=%d", denied, deniedReads, p.negativeLimit)
	}
	if positives != 1 {
		t.Fatalf("positive authority cache holds %d entries after the flood, want the 1 valid grant", positives)
	}
	if _, ok := p.Authenticate(context.Background(), "good", "observer-a", "ubx"); !ok {
		t.Fatal("valid authority evicted by a flood of invalid tokens")
	}
}

// Distinct (station, feed) pairs must not collide in the folded negative-cache
// key: a denial for one feed cannot be allowed to deny another.
func TestObserverFoldedKeyDistinguishesStationAndFeed(t *testing.T) {
	seen := map[string]string{}
	for _, key := range []observerCacheKey{
		{tokenSHA256: "d", station: "ab", feed: "c"},
		{tokenSHA256: "d", station: "a", feed: "bc"},
		{tokenSHA256: "d", station: "a", feed: "b"},
		{tokenSHA256: "d", station: "", feed: "ab"},
		{tokenSHA256: "e", station: "a", feed: "b"},
	} {
		folded := observerFoldedKey(key)
		label := fmt.Sprintf("%+v", key)
		if prior, ok := seen[folded]; ok {
			t.Fatalf("folded key collision between %s and %s", prior, label)
		}
		seen[folded] = label
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
