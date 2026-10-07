package ingest

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/identity"
)

// verifyAnswer is one answer of a scripted AuthorizationVerifier.
type verifyAnswer struct {
	context identity.ObserverContext
	ok      bool
	err     error
}

// scriptedVerifier answers VerifyAuthorization from a script whose last entry
// repeats. With block set, every answer waits for the channel to close first,
// after signalling entered.
type scriptedVerifier struct {
	mu      sync.Mutex
	answers []verifyAnswer
	block   chan struct{}
	entered chan struct{}
}

func (v *scriptedVerifier) set(answers ...verifyAnswer) {
	v.mu.Lock()
	v.answers = answers
	v.mu.Unlock()
}

func (v *scriptedVerifier) VerifyAuthorization(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
	if v.entered != nil {
		v.entered <- struct{}{}
	}
	if v.block != nil {
		<-v.block
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	a := v.answers[0]
	if len(v.answers) > 1 {
		v.answers = v.answers[1:]
	}
	return a.context, a.ok, a.err
}

func (v *scriptedVerifier) Authenticate(ctx context.Context, token, station, feed string) (identity.ObserverContext, bool) {
	c, ok, err := v.VerifyAuthorization(ctx, token, station, feed)
	if err != nil {
		return identity.ObserverContext{}, false
	}
	return c, ok
}

// recheckHarness is a push server with a scripted verifier, one admitted
// session on a pipe, and its authorization watcher running on a fast cadence.
type recheckHarness struct {
	p         *PushServer
	out       chan *RawFrame
	verifier  *scriptedVerifier
	admission *Admission
	client    net.Conn
	cancel    context.CancelFunc
	done      chan struct{}
}

func newRecheckHarness(t *testing.T, verifier *scriptedVerifier, every, ttl time.Duration) *recheckHarness {
	t.Helper()
	out := make(chan *RawFrame, 8)
	p := newPushServer("", &tls.Config{}, out, verifier, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p.reauthorizeEvery, p.authorityTTL = every, ttl
	station := identity.NewPrivateContext("observer", identity.CredentialToken)
	admission, refused := p.admit(context.Background(), station, 0, func() {}, policyCredential{"digest", "local"})
	if admission == nil {
		t.Fatalf("admission refused: %s", refused)
	}
	server, client := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		p.watchAuthorization(ctx, context.Background(), server, "token", "observer", "local", station, commissioning.Result{}, admission)
	}()
	t.Cleanup(func() { cancel(); <-done; client.Close() })
	return &recheckHarness{p: p, out: out, verifier: verifier, admission: admission, client: client, cancel: cancel, done: done}
}

// open reports whether the session's connection is still open from the
// feeder's side: a read times out on an open pipe and fails at once on a
// closed one.
func (h *recheckHarness) open(t *testing.T) bool {
	t.Helper()
	if err := h.client.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		if errors.Is(err, io.ErrClosedPipe) {
			return false // net.Pipe reports a close of either end here
		}
		t.Fatal(err)
	}
	_, err := h.client.Read(make([]byte, 1))
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded):
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrClosedPipe):
		return false
	}
	t.Fatalf("unexpected read result on the session pipe: %v", err)
	return false
}

func (h *recheckHarness) marker(t *testing.T, within time.Duration) *RawFrame {
	t.Helper()
	select {
	case m := <-h.out:
		return m
	case <-time.After(within):
		return nil
	}
}

// TestRecheckUnavailableKeepsSession guards a periodic recheck the control
// plane cannot answer is not a revocation: the session stays open, its policy
// and generation stand and no ScopeRevocation marker is emitted, tick after
// tick; the next authoritative denial then closes it with exactly one marker.
func TestRecheckUnavailableKeepsSession(t *testing.T) {
	verifier := &scriptedVerifier{}
	verifier.set(verifyAnswer{err: errors.New("control plane unreachable")})
	h := newRecheckHarness(t, verifier, 10*time.Millisecond, time.Hour)

	time.Sleep(80 * time.Millisecond) // several unavailable rechecks
	if m := h.marker(t, 0); m != nil {
		t.Fatalf("unavailable recheck emitted a marker: %+v", m.ScopeRevocation)
	}
	if !h.admission.Current() {
		t.Fatal("unavailable recheck advanced the policy generation")
	}
	if !h.open(t) {
		t.Fatal("unavailable recheck closed the session")
	}

	verifier.set(verifyAnswer{ok: false})
	m := h.marker(t, 2*time.Second)
	if m == nil || m.ScopeRevocation == nil || m.ScopeRevocation.Previous.ObserverID != "observer" {
		t.Fatalf("authoritative denial emitted no reset marker (got %+v)", m)
	}
	<-h.done
	if extra := h.marker(t, 20*time.Millisecond); extra != nil {
		t.Fatal("authoritative denial emitted a second marker")
	}
	if h.admission.Current() || h.open(t) {
		t.Fatal("authoritative denial left the session admitted or open")
	}
}

// TestRecheckAfterSessionEndEmitsNoMarker guards the race at a normal
// disconnect: the ticker and the session's cancellation can both be ready, and
// a recheck that then completes — even with an authoritative denial — must not
// turn a healthy disconnect into a policy revocation.
func TestRecheckAfterSessionEndEmitsNoMarker(t *testing.T) {
	verifier := &scriptedVerifier{block: make(chan struct{}), entered: make(chan struct{}, 1)}
	verifier.set(verifyAnswer{ok: false})
	h := newRecheckHarness(t, verifier, 10*time.Millisecond, time.Hour)

	select {
	case <-verifier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("recheck never ran")
	}
	h.cancel()            // the session ends while the check is in flight
	close(verifier.block) // and the check then answers with a denial
	<-h.done
	if m := h.marker(t, 50*time.Millisecond); m != nil {
		t.Fatalf("recheck that completed after the session ended emitted a marker: %+v", m.ScopeRevocation)
	}
	if !h.admission.Current() {
		t.Fatal("recheck after the session ended advanced the policy generation")
	}
}

// TestRecheckUnavailablePastBoundWithdraws guards the documented revocation
// bound still holds under unavailability: once the policy's last confirmation
// is older than cache TTL plus recheck interval, the session is withdrawn with
// exactly one marker and closed.
func TestRecheckUnavailablePastBoundWithdraws(t *testing.T) {
	verifier := &scriptedVerifier{}
	verifier.set(verifyAnswer{err: errors.New("control plane unreachable")})
	h := newRecheckHarness(t, verifier, 10*time.Millisecond, 20*time.Millisecond) // bound 30 ms

	m := h.marker(t, 2*time.Second)
	if m == nil || m.ScopeRevocation == nil || m.ScopeRevocation.Previous.ObserverID != "observer" {
		t.Fatalf("session unverifiable past the bound was not withdrawn (got %+v)", m)
	}
	<-h.done
	if extra := h.marker(t, 20*time.Millisecond); extra != nil {
		t.Fatal("withdrawal emitted a second marker")
	}
	if h.admission.Current() || h.open(t) {
		t.Fatal("withdrawal left the session admitted or open")
	}
}
