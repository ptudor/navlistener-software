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
	for _, granted := range []bool{false, true} {
		allow.Store(granted)
		for i := 0; i < 1000; i++ {
			token := fmt.Sprint(i)
			p.Authenticate(context.Background(), token, "observer-a", "ubx")
			p.AuthorizeRead(context.Background(), token)
			if len(p.observers) > 16 || len(p.readers) > 16 {
				t.Fatal("unbounded caches")
			}
		}
		if !granted && (len(p.observers) != 0 || len(p.readers) != 0) {
			t.Fatal("denied tokens retained")
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
		empty := len(p.observers) == 0 && len(p.readers) == 0
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
