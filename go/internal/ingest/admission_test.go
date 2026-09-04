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
		a := p.admit(context.Background(), old, 0, func() { canceled.Add(1) })
		b := p.admit(context.Background(), old, 0, func() { canceled.Add(1) })
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
			if p.admit(context.Background(), next, generation, func() {}) == nil {
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
		if p.admit(context.Background(), old, generation, func() {}) != nil {
			t.Fatal("stale lookup undid transition")
		}
		other := identity.NewPrivateContext("unrelated", identity.CredentialToken)
		if p.admit(context.Background(), other, 0, func() {}) == nil {
			t.Fatal("unrelated admission blocked")
		}
	}
}

func TestNewPolicyCannotOvertakeBlockedReset(t *testing.T) {
	out := make(chan *RawFrame)
	p := newPushServer("", &tls.Config{}, out, nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	old := identity.NewPrivateContext("observer", identity.CredentialToken)
	a := p.admit(context.Background(), old, 0, func() {})
	next := old
	next.Publication.Revision = "v2"
	done := make(chan *Admission, 1)
	go func() { done <- p.admit(context.Background(), next, 1, func() {}) }()
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
	if p.admit(context.Background(), unrelated, 0, func() {}) == nil {
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
