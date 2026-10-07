package ingest

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// reconcileFunc is an authoritative reconciler: every answer is a verdict.
type reconcileFunc func(context.Context, string, string, string) (identity.ObserverContext, bool)

func (f reconcileFunc) ReconcileObserver(ctx context.Context, d, s, feed string) (identity.ObserverContext, bool, error) {
	c, ok := f(ctx, d, s, feed)
	return c, ok, nil
}

// reconcileStatusFunc is a reconciler that can also answer "could not verify".
type reconcileStatusFunc func(context.Context, string, string, string) (identity.ObserverContext, bool, error)

func (f reconcileStatusFunc) ReconcileObserver(ctx context.Context, d, s, feed string) (identity.ObserverContext, bool, error) {
	return f(ctx, d, s, feed)
}

// sweptServer is a push server whose sweep queries every retained policy (no
// authority TTL skip), for tests of the sweep's verdict handling.
func sweptServer(out chan *RawFrame) *PushServer {
	p := newPushServer("", &tls.Config{}, out, nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.authorityTTL = 0
	return p
}

func TestDisconnectedContributorReconciliation(t *testing.T) {
	for _, change := range []string{"private", "transfer", "disabled"} {
		t.Run(change, func(t *testing.T) {
			out := make(chan *RawFrame, 4)
			p := sweptServer(out)
			old := identity.NewPrivateContext("observer", identity.CredentialToken)
			old.OrganizationID = "old-org"
			old.Publication.AggregateUse = identity.AggregatePublicAnonymous
			a, _ := p.admit(context.Background(), old, 0, func() {}, policyCredential{"digest", "ubx"})
			a.release()
			other := identity.NewPrivateContext("unrelated", identity.CredentialToken)
			other.OrganizationID = "unrelated-org"
			b, _ := p.admit(context.Background(), other, 0, func() {}, policyCredential{"other-digest", "ubx"})
			b.release()
			next := old
			if change == "private" {
				next.Publication.AggregateUse = identity.AggregatePrivate
			}
			if change == "transfer" {
				next.OrganizationID = "new-org"
			}
			p.reconcileOnce(context.Background(), reconcileFunc(func(_ context.Context, d, station, feed string) (identity.ObserverContext, bool) {
				if station == "unrelated" {
					return other, true
				}
				if d != "digest" || feed != "ubx" {
					t.Error("reconciliation identity changed")
				}
				return next, change != "disabled"
			}))
			marker := <-out
			if marker.ScopeRevocation.Previous.ObserverID != "observer" {
				t.Fatal("wrong observer withdrawn")
			}
			if a.Current() || !b.Current() {
				t.Fatal("wrong generation fenced")
			}
			p.reconcileOnce(context.Background(), reconcileFunc(func(context.Context, string, string, string) (identity.ObserverContext, bool) { return other, true }))
			select {
			case <-out:
				t.Fatal("retired credential produced repeated reset")
			default:
			}
		})
	}
}

func TestReconciliationRetainsAllCredentialContributorsAndBoundsAdmission(t *testing.T) {
	p := sweptServer(make(chan *RawFrame, 8))
	old := identity.NewPrivateContext("observer", identity.CredentialToken)
	for i := 0; i < maxPolicyCredentials; i++ {
		a, _ := p.admit(context.Background(), old, p.policyGeneration("observer"), func() {}, policyCredential{fmt.Sprint(i), "ubx"})
		if a == nil {
			t.Fatal("credential unexpectedly refused")
		}
		a.release()
		time.Sleep(time.Millisecond) // distinct admission times: eviction is least-recent
	}
	// A further credential (a rotated token) evicts the least recently admitted
	// digest rather than refusing the session: any one retained credential
	// detects a withdrawal, and refusing locked a rotating observer out.
	a, _ := p.admit(context.Background(), old, 1, func() {}, policyCredential{"overflow", "ubx"})
	if a == nil {
		t.Fatal("credential rotation refused at the retention bound")
	}
	a.release()
	policy := p.policies["observer"]
	policy.mu.Lock()
	retained := len(policy.credentials)
	_, evictedStillThere := policy.credentials[policyCredential{"0", "ubx"}]
	_, newestKept := policy.credentials[policyCredential{"overflow", "ubx"}]
	policy.mu.Unlock()
	if retained != maxPolicyCredentials || evictedStillThere || !newestKept {
		t.Fatalf("credential retention: %d retained, oldest evicted=%v, newest kept=%v", retained, !evictedStillThere, newestKept)
	}
	seen := make(chan string, maxPolicyCredentials)
	p.reconcileOnce(context.Background(), reconcileFunc(func(_ context.Context, d, _, _ string) (identity.ObserverContext, bool) { seen <- d; return old, true }))
	if len(seen) != maxPolicyCredentials {
		t.Fatal("disconnected credential forgotten")
	}
	for i := 1; i < maxTrackedObserverPolicies; i++ {
		c := identity.NewPrivateContext(fmt.Sprint(i), identity.CredentialToken)
		a, _ := p.admit(context.Background(), c, 0, func() {})
		if a == nil {
			t.Fatal("observer unexpectedly refused")
		}
		a.release()
	}
	if a, _ := p.admit(context.Background(), identity.NewPrivateContext("overflow", identity.CredentialToken), 0, func() {}); a != nil {
		t.Fatal("unbounded policy retention")
	}
}

// TestReconcileUnverifiedIsSkippedNotWithdrawn guards a sweep that cannot
// verify a contributor — the reconciler reports unavailability, or the sweep
// budget runs out while it blocks — leaves that contributor's policy exactly as
// it was: no marker, no generation change, no closed session.
func TestReconcileUnverifiedIsSkippedNotWithdrawn(t *testing.T) {
	for name, reconciler := range map[string]reconcileStatusFunc{
		"unavailable": func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
			return identity.ObserverContext{}, false, errors.New("control plane unreachable")
		},
		"budget expired": func(ctx context.Context, _, _, _ string) (identity.ObserverContext, bool, error) {
			<-ctx.Done() // blocks past the sweep budget
			return identity.ObserverContext{}, false, ctx.Err()
		},
	} {
		t.Run(name, func(t *testing.T) {
			out := make(chan *RawFrame, 4)
			p := sweptServer(out)
			p.reconcileBudget = 30 * time.Millisecond
			old := identity.NewPrivateContext("observer", identity.CredentialToken)
			var canceled atomic.Int32
			a, _ := p.admit(context.Background(), old, 0, func() { canceled.Add(1) }, policyCredential{"digest", "ubx"})
			started := time.Now()
			p.reconcileOnce(context.Background(), reconciler)
			if took := time.Since(started); took > time.Second {
				t.Fatalf("sweep took %v, want it bounded by the %v budget", took, p.reconcileBudget)
			}
			select {
			case m := <-out:
				t.Fatalf("unverified contributor withdrawn: %+v", m.ScopeRevocation)
			default:
			}
			if !a.Current() || canceled.Load() != 0 {
				t.Fatal("unverified contributor's generation advanced or session cancelled")
			}
		})
	}
}

// TestReconcileSweepIsRateLimited guards the sweep's load on the control plane:
// with 1024 retained policies of 8 credentials each and a reconciler that takes
// 10 ms per call, one sweep queries at most maxReconcileTasksPerSweep pairs,
// finishes well inside its budget, withdraws nobody, and the next sweep moves
// on to policies the first one did not reach; policies confirmed within the
// authority TTL are not queried at all.
func TestReconcileSweepIsRateLimited(t *testing.T) {
	p := sweptServer(make(chan *RawFrame, 16))
	for i := 0; i < maxTrackedObserverPolicies; i++ {
		c := identity.NewPrivateContext(fmt.Sprint("observer-", i), identity.CredentialToken)
		credentials := make([]policyCredential, 0, maxPolicyCredentials)
		for j := 0; j < maxPolicyCredentials; j++ {
			credentials = append(credentials, policyCredential{fmt.Sprint(i, "/", j), "ubx"})
		}
		a, refused := p.admit(context.Background(), c, 0, func() {}, credentials...)
		if a == nil {
			t.Fatalf("observer %d refused: %s", i, refused)
		}
		a.release()
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var calls atomic.Int64
	reconciler := reconcileStatusFunc(func(_ context.Context, _, station, _ string) (identity.ObserverContext, bool, error) {
		time.Sleep(10 * time.Millisecond)
		calls.Add(1)
		mu.Lock()
		seen[station]++
		mu.Unlock()
		c := identity.NewPrivateContext(station, identity.CredentialToken)
		return c, true, nil
	})
	sweep := func() map[string]int {
		started := time.Now()
		p.reconcileOnce(context.Background(), reconciler)
		if took := time.Since(started); took > reconciliationBudget {
			t.Fatalf("sweep took %v, over the %v budget", took, reconciliationBudget)
		}
		mu.Lock()
		defer mu.Unlock()
		swept := seen
		seen = map[string]int{}
		return swept
	}
	first := sweep()
	if n := calls.Swap(0); n != maxReconcileTasksPerSweep {
		t.Fatalf("first sweep issued %d queries, want exactly %d", n, maxReconcileTasksPerSweep)
	}
	second := sweep()
	if n := calls.Swap(0); n != maxReconcileTasksPerSweep {
		t.Fatalf("second sweep issued %d queries, want exactly %d", n, maxReconcileTasksPerSweep)
	}
	for station := range second {
		if first[station] != 0 {
			t.Fatalf("station %s swept twice while others waited", station)
		}
	}
	if len(p.out) != 0 {
		t.Fatalf("%d markers emitted by sweeps that verified everyone", len(p.out))
	}
	p.authorityTTL = time.Hour
	sweep()
	if n := calls.Load(); n != 0 {
		t.Fatalf("sweep queried %d policies confirmed within the authority TTL, want 0", n)
	}
}
