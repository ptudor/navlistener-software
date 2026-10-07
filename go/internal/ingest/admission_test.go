package ingest

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

func TestPolicyAdmissionFencesAllSessionsAndLateHandoffs(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		out := make(chan *RawFrame, 8)
		p := newPushServer("", &tls.Config{}, out, nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
		old := identity.NewPrivateContext("observer", identity.CredentialToken)
		old.OrganizationID = "old-org"
		old.CollectionIDs = []string{"old-fleet"}
		old.Publication.Revision = "v1"
		var canceled atomic.Int32
		a, _ := p.admit(context.Background(), old, 0, func() { canceled.Add(1) })
		b, _ := p.admit(context.Background(), old, 0, func() { canceled.Add(1) })
		if a == nil || b == nil {
			t.Fatal("legitimate simultaneous sessions rejected")
		}
		// Represents a frame suspended immediately before send and buffered zstd
		// records decoded later. Both retain exactly the same immutable admission.
		held := []*RawFrame{{Observer: old, Admission: a}, {Observer: old, Admission: b}}
		generation := p.policyGeneration(old.ObserverID)
		next := old
		next.OrganizationID = "new-org"
		next.CollectionIDs = []string{"new-fleet"}
		next.Publication.Revision = "v2"
		if revoked {
			p.changeAdmission(context.Background(), a, identity.ObserverContext{})
		} else {
			if a, _ := p.admit(context.Background(), next, generation, func() {}); a == nil {
				t.Fatal("new policy not admitted")
			}
		}
		marker := <-out
		if marker.ScopeRevocation == nil || marker.ScopeRevocation.Previous.OrganizationID != "old-org" {
			t.Fatal("wrong reset boundary")
		}
		if canceled.Load() != 2 {
			t.Fatal("old producers were not stopped")
		}
		for _, frame := range held {
			out <- frame
			late := <-out
			if late.Admission.Current() {
				t.Fatal("late DATA retained projection authority")
			}
			if late.Observer.OrganizationID != "old-org" {
				t.Fatal("forensic provenance was rewritten")
			}
		}
		p.changeAdmission(context.Background(), b, old)
		select {
		case <-out:
			t.Fatal("late cleanup generated a new reset")
		default:
		}
		if a, _ := p.admit(context.Background(), old, generation, func() {}); a != nil {
			t.Fatal("stale lookup undid transition")
		}
		other := identity.NewPrivateContext("unrelated", identity.CredentialToken)
		if a, _ := p.admit(context.Background(), other, 0, func() {}); a == nil {
			t.Fatal("unrelated admission blocked")
		}
	}
}

func TestNewPolicyCannotOvertakeBlockedReset(t *testing.T) {
	out := make(chan *RawFrame)
	p := newPushServer("", &tls.Config{}, out, nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	old := identity.NewPrivateContext("observer", identity.CredentialToken)
	a, _ := p.admit(context.Background(), old, 0, func() {})
	next := old
	next.Publication.Revision = "v2"
	done := make(chan *Admission, 1)
	go func() { a, _ := p.admit(context.Background(), next, 1, func() {}); done <- a }()
	deadline := time.Now().Add(time.Second)
	for a.Current() {
		if time.Now().After(deadline) {
			t.Fatal("old producer was not fenced")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-done:
		t.Fatal("new policy overtook reset marker")
	default:
	}
	unrelated := identity.NewPrivateContext("unrelated", identity.CredentialToken)
	if a, _ := p.admit(context.Background(), unrelated, 0, func() {}); a == nil {
		t.Fatal("blocked reset stalled unrelated traffic")
	}
	<-out
	select {
	case a := <-done:
		if a == nil || !a.Current() {
			t.Fatal("new policy missing")
		}
	case <-time.After(time.Second):
		t.Fatal("new policy did not resume")
	}
}

// TestBlockedResetMarkerDoesNotHoldThePolicyLock guards a transition whose
// reset marker is stuck behind decode backpressure fences only what it must:
// an old session's release returns at once, a stale lookup of the same
// observer is refused at once, the sweep's snapshot of the policy proceeds, and
// only an equal-policy new session waits — until the marker is in.
func TestBlockedResetMarkerDoesNotHoldThePolicyLock(t *testing.T) {
	out := make(chan *RawFrame) // unbuffered: the marker blocks until read
	p := newPushServer("", &tls.Config{}, out, nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	old := identity.NewPrivateContext("observer", identity.CredentialToken)
	a, _ := p.admit(context.Background(), old, 0, func() {}, policyCredential{"digest", "ubx"})
	next := old
	next.Publication.Revision = "v2"
	transitioned := make(chan *Admission, 1)
	go func() { a, _ := p.admit(context.Background(), next, 1, func() {}); transitioned <- a }()
	deadline := time.Now().Add(time.Second)
	for a.Current() {
		if time.Now().After(deadline) {
			t.Fatal("old producer was not fenced")
		}
		time.Sleep(time.Millisecond)
	}
	promptly := func(name string, f func()) {
		t.Helper()
		finished := make(chan struct{})
		go func() { f(); close(finished) }()
		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Fatalf("%s waited on the blocked reset marker", name)
		}
	}
	promptly("release of the old session", a.release)
	promptly("refusal of a stale lookup", func() {
		if b, reason := p.admit(context.Background(), old, 0, func() {}); b != nil || reason != "stale_lookup" {
			t.Errorf("stale lookup: admission %v, reason %q, want refused as stale_lookup", b, reason)
		}
	})
	promptly("the reconciliation snapshot", func() {
		p.reconcileOnce(context.Background(), reconcileFunc(func(context.Context, string, string, string) (identity.ObserverContext, bool) { return next, true }))
	})
	equal := make(chan *Admission, 1)
	go func() { b, _ := p.admit(context.Background(), next, 2, func() {}); equal <- b }()
	select {
	case <-equal:
		t.Fatal("equal-policy session overtook the reset marker")
	case <-transitioned:
		t.Fatal("transition completed before its marker was read")
	case <-time.After(50 * time.Millisecond):
	}
	marker := <-out
	if marker.ScopeRevocation == nil || marker.ScopeRevocation.Previous.Publication.Revision != "config-private-v1" {
		t.Fatalf("wrong reset marker: %+v", marker.ScopeRevocation)
	}
	for _, waiter := range []chan *Admission{transitioned, equal} {
		select {
		case b := <-waiter:
			if b == nil || !b.Current() {
				t.Fatal("new-policy session missing after the marker")
			}
		case <-time.After(time.Second):
			t.Fatal("new-policy session did not resume after the marker")
		}
	}
	select {
	case m := <-out:
		t.Fatalf("second marker for one transition: %+v", m.ScopeRevocation)
	case <-time.After(20 * time.Millisecond):
	}
}
