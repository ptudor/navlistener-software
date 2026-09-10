package authorization

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

func TestCacheBoundsAndIndependentExpiry(t *testing.T) {
	var now atomic.Int64
	now.Store(time.Now().UnixNano())
	var allow atomic.Bool
	p := newProvider(10*time.Millisecond, nil, func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		return testContext(), allow.Load(), nil
	})
	p.lookupRead = func(context.Context, string) (identity.ReadPrincipal, bool, error) {
		return identity.ReadPrincipal{ID: "reader", Revision: "v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}}}, allow.Load(), nil
	}
	p.now = func() time.Time { return time.Unix(0, now.Load()) }
	p.cacheLimit = 16
	p.negativeLimit = 16
	for _, granted := range []bool{false, true} {
		allow.Store(granted)
		// denials are now remembered for negativeTTL, so step the
		// fake clock past it before reusing the same tokens with the opposite
		// verdict. Without this the granted pass would legitimately be answered
		// from the negative cache recorded microseconds earlier.
		now.Add(int64(time.Second))
		for i := 0; i < 1000; i++ {
			token := fmt.Sprint(i)
			p.Authenticate(context.Background(), token, "observer-a", "ubx")
			p.AuthorizeRead(context.Background(), token)
			if len(p.observers) > 16 || len(p.readers) > 16 {
				t.Fatal("unbounded positive caches")
			}
			if len(p.deniedObservers) > 16 || len(p.deniedReaders) > 16 {
				t.Fatal("unbounded negative caches")
			}
		}
		if !granted && (len(p.observers) != 0 || len(p.readers) != 0) {
			t.Fatal("denied tokens retained as positive authority")
		}
		if !granted && (len(p.deniedObservers) == 0 || len(p.deniedReaders) == 0) {
			t.Fatal("negative caches not exercised")
		}
	}
	if len(p.observers) != 16 || len(p.readers) != 16 {
		t.Fatal("positive cache not exercised")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.RunInvalidation(ctx) }()
	now.Add(int64(time.Second))
	deadline := time.Now().Add(time.Second)
	for {
		p.mu.Lock()
		empty := len(p.observers) == 0 && len(p.readers) == 0 &&
			len(p.deniedObservers) == 0 && len(p.deniedReaders) == 0
		p.mu.Unlock()
		if empty {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("expiry did not run without token reuse")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sweeper did not stop")
	}
}

func TestConcurrentCacheInvalidation(t *testing.T) {
	p := newProvider(time.Minute, nil, func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		return testContext(), true, nil
	})
	p.lookupRead = func(context.Context, string) (identity.ReadPrincipal, bool, error) {
		return identity.ReadPrincipal{ID: "reader", Revision: "v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}}}, true, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				p.Authenticate(context.Background(), "token", "observer-a", "ubx")
				p.AuthorizeRead(context.Background(), "token")
				p.InvalidateAll()
			}
		})
	}
	wg.Wait()
}

func TestReadInvalidationRacingLookupRejectsStaleResult(t *testing.T) {
	p := newProvider(time.Minute, nil, nil)
	p.lookupRead = func(context.Context, string) (identity.ReadPrincipal, bool, error) {
		p.InvalidateAll()
		return identity.ReadPrincipal{ID: "reader", Revision: "v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}}}, true, nil
	}
	if _, ok := p.AuthorizeRead(context.Background(), "token"); ok {
		t.Fatal("stale read authority admitted")
	}
	if len(p.readers) != 0 {
		t.Fatal("stale read cache repopulated")
	}
}

func TestReconciliationBypassesCachedAuthority(t *testing.T) {
	allowed := true
	p := newProvider(time.Minute, nil, func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		return testContext(), allowed, nil
	})
	if _, ok := p.Authenticate(context.Background(), "token", "observer-a", "ubx"); !ok {
		t.Fatal("initial authority denied")
	}
	allowed = false
	if _, ok := p.ReconcileObserver(context.Background(), "digest", "observer-a", "ubx"); ok {
		t.Fatal("offline recheck used cached authority")
	}
	p.lookupObserver = func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
		p.InvalidateAll()
		return testContext(), true, nil
	}
	if _, ok := p.ReconcileObserver(context.Background(), "digest", "observer-a", "ubx"); ok {
		t.Fatal("racing invalidation returned stale reconciliation")
	}
}
