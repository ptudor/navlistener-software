package ingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

type reconcileFunc func(context.Context, string, string, string) (identity.ObserverContext, bool)

func (f reconcileFunc) ReconcileObserver(ctx context.Context, d, s, feed string) (identity.ObserverContext, bool) {
	return f(ctx, d, s, feed)
}

func TestDisconnectedContributorReconciliation(t *testing.T) {
	for _, change := range []string{"private", "transfer", "disabled"} {
		t.Run(change, func(t *testing.T) {
			out := make(chan *RawFrame, 4)
			p := newPushServer("", &tls.Config{}, out, nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	p := newPushServer("", &tls.Config{}, make(chan *RawFrame, 8), nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
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
