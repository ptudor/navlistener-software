package ingest

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// feederHello dials, writes the magic and a HELLO, and returns the WELCOME it
// got with the open connection. Errors are returned, not fatal, so it can run
// in a goroutine.
func feederHello(addr string, hello wire.HelloMsg) (*tls.Conn, wire.WelcomeMsg, error) {
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		return nil, wire.WelcomeMsg{}, err
	}
	if err := wire.WriteMagic(conn); err != nil {
		conn.Close()
		return nil, wire.WelcomeMsg{}, err
	}
	if err := wire.WriteHello(conn, hello); err != nil {
		conn.Close()
		return nil, wire.WelcomeMsg{}, err
	}
	ft, payload, err := wire.ReadFrame(conn)
	if err != nil {
		conn.Close()
		return nil, wire.WelcomeMsg{}, err
	}
	if ft != wire.Welcome {
		conn.Close()
		return nil, wire.WelcomeMsg{}, fmt.Errorf("frame %d, want WELCOME", ft)
	}
	wmsg, err := parseWelcome(payload)
	if err != nil {
		conn.Close()
		return nil, wire.WelcomeMsg{}, err
	}
	return conn, wmsg, nil
}

// TestIdleConnectionsDoNotStarveAuthenticatedFeeders guards the pre-auth
// flood: with the fleet budget at four, four raw TCP connections that send
// nothing (no ClientHello, nothing for mTLS to examine) must not hold a single
// fleet slot, and a legitimate feeder must still complete its HELLO within two
// seconds while they are parked.
func TestIdleConnectionsDoNotStarveAuthenticatedFeeders(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *RawFrame, 8)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	const maxConns = 4
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("observer16", "s3cret", "ubx"), 25*time.Millisecond, maxConns,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.serve(ctx, ln) }()
	addr := ln.Addr().String()

	idle := make([]net.Conn, maxConns)
	for i := range idle {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		idle[i] = c
		defer c.Close()
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(srv.preAuth) < maxConns {
		if time.Now().After(deadline) {
			t.Fatalf("pre-auth pool holds %d of the %d idle connections", len(srv.preAuth), maxConns)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(srv.conns) != 0 {
		t.Fatalf("%d fleet slots taken by idle connections, want 0", len(srv.conns))
	}

	type result struct {
		conn    *tls.Conn
		welcome wire.WelcomeMsg
		err     error
	}
	done := make(chan result, 1)
	go func() {
		conn, welcome, err := feederHello(addr, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx", Session: "boot-test"})
		done <- result{conn, welcome, err}
	}()
	select {
	case r := <-done:
		if r.err != nil || !r.welcome.OK {
			t.Fatalf("legitimate feeder: err %v, welcome %+v, want WELCOME ok", r.err, r.welcome)
		}
		defer r.conn.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("legitimate feeder starved by idle connections")
	}
	if len(srv.conns) != 1 {
		t.Fatalf("fleet slots = %d after one authenticated session, want 1", len(srv.conns))
	}
}

// TestAddressGuardBounds guards the per-address pre-auth bounds: at most
// maxPreAuthPerAddress connections in flight, a token bucket of preAuthBurst
// refilled at preAuthRate per second on accepts, and preAuthLogLines warnings
// per preAuthLogWindow; other addresses are unaffected.
func TestAddressGuardBounds(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	g := newAddressGuard(func() time.Time { return now })
	var releases []func()
	for i := 0; i < maxPreAuthPerAddress; i++ {
		release, reason := g.admit("10.0.0.1")
		if reason != "" {
			t.Fatalf("connection %d refused: %s", i+1, reason)
		}
		releases = append(releases, release)
	}
	if _, reason := g.admit("10.0.0.1"); reason != "address_inflight" {
		t.Fatalf("over the in-flight cap: reason %q, want address_inflight", reason)
	}
	if release, reason := g.admit("10.0.0.2"); reason != "" {
		t.Fatalf("another address refused: %s", reason)
	} else {
		release()
	}
	releases[0]()
	if release, reason := g.admit("10.0.0.1"); reason != "" {
		t.Fatalf("released place not reusable: %s", reason)
	} else {
		release()
	}
	for _, release := range releases[1:] {
		release()
	}
	// Quick open/close cycles drain the accept bucket; a second refills it.
	accepted := maxPreAuthPerAddress + 1 // already spent above
	for ; accepted <= preAuthBurst+1; accepted++ {
		release, reason := g.admit("10.0.0.1")
		if reason == "address_rate" {
			break
		}
		if reason != "" {
			t.Fatalf("accept %d refused with %q", accepted, reason)
		}
		release()
	}
	if accepted != preAuthBurst {
		t.Fatalf("rate limit after %d accepts, want exactly the burst of %d", accepted, preAuthBurst)
	}
	now = now.Add(time.Second)
	if release, reason := g.admit("10.0.0.1"); reason != "" {
		t.Fatalf("bucket did not refill after a second: %s", reason)
	} else {
		release()
	}
	for i := 0; i < preAuthLogLines; i++ {
		if !g.allowLog("10.0.0.3") {
			t.Fatalf("log line %d refused", i+1)
		}
	}
	if g.allowLog("10.0.0.3") {
		t.Fatal("log lines not limited")
	}
	if !g.allowLog("10.0.0.4") {
		t.Fatal("another address's log line refused")
	}
	now = now.Add(preAuthLogWindow)
	if !g.allowLog("10.0.0.3") {
		t.Fatal("log bucket did not refill after the window")
	}
}

// lockedBuffer is a bytes.Buffer the connection handlers may write concurrently.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) lines() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Split(bytes.TrimSpace(b.buf.Bytes()), []byte("\n"))
}

// TestPreAuthWarningsAreBoundedAndClipped guards the pre-auth log amplifier:
// a dozen rejected HELLOs from one address carrying a 4000-byte station write
// at most preAuthLogLines warnings, each with the station clipped, while the
// auth-failure metric still counts every attempt.
func TestPreAuthWarningsAreBoundedAndClipped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var logged lockedBuffer
	addr, _ := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"), func(p *PushServer) {
		p.log = slog.New(slog.NewJSONHandler(&logged, nil))
	})
	failuresBefore := testutil.ToFloat64(metrics.PushAuthFailuresTotal)
	station := strings.Repeat("a", 4000)
	const attempts = 12
	for i := 0; i < attempts; i++ {
		conn, welcome, err := feederHello(addr, wire.HelloMsg{Token: "wrong", Station: station, Feed: "ubx", Session: "boot-test"})
		if err != nil {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		conn.Close()
		if welcome.OK {
			t.Fatal("bad token accepted")
		}
	}
	if d := testutil.ToFloat64(metrics.PushAuthFailuresTotal) - failuresBefore; d != attempts {
		t.Fatalf("PushAuthFailuresTotal delta = %v, want %d (the metrics are not rate-limited)", d, attempts)
	}
	rejected := 0
	for _, raw := range logged.lines() {
		var line map[string]any
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		if line["msg"] != "push auth rejected" {
			continue
		}
		rejected++
		if s, _ := line["station"].(string); len(s) > loggedHelloFieldMax+len("…") || !strings.HasSuffix(s, "…") {
			t.Fatalf("logged station %q (%d bytes) is not clipped to %d", s, len(s), loggedHelloFieldMax)
		}
	}
	if rejected == 0 || rejected > preAuthLogLines {
		t.Fatalf("%d auth-rejected lines for %d attempts, want 1..%d", rejected, attempts, preAuthLogLines)
	}
}

// TestObserverSessionCeiling guards one credential cannot hold the fleet: the
// fifth concurrent session of an observer is refused by admission with
// WELCOME{ok:false} and the observer_sessions reason, the observer's up gauge
// never exceeds four, and a slot freed by a closed session is admitted again.
func TestObserverSessionCeiling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))
	upBefore := testutil.ToFloat64(metrics.PushObserversUp.WithLabelValues("observer16"))
	refusedBefore := testutil.ToFloat64(metrics.PushAdmissionRefusedTotal.WithLabelValues("observer_sessions"))
	var conns []*tls.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i <= maxObserverSessions; i++ {
		conn, welcome, err := feederHello(addr, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx", Session: fmt.Sprint("boot-", i)})
		if err != nil {
			t.Fatalf("session %d: %v", i+1, err)
		}
		conns = append(conns, conn)
		if i < maxObserverSessions && !welcome.OK {
			t.Fatalf("session %d refused: %+v", i+1, welcome)
		}
		if i == maxObserverSessions && (welcome.OK || welcome.Error != "observer session limit") {
			t.Fatalf("session %d: welcome %+v, want refused with the observer session limit", i+1, welcome)
		}
		if up := testutil.ToFloat64(metrics.PushObserversUp.WithLabelValues("observer16")) - upBefore; up > maxObserverSessions {
			t.Fatalf("observers_up rose to %v, want at most %d", up, maxObserverSessions)
		}
	}
	if d := testutil.ToFloat64(metrics.PushAdmissionRefusedTotal.WithLabelValues("observer_sessions")) - refusedBefore; d != 1 {
		t.Fatalf("observer_sessions refusals = %v, want 1", d)
	}
	conns[0].Close()
	conns = conns[1:]
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, welcome, err := feederHello(addr, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx", Session: "boot-again"})
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, conn)
		if welcome.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("a closed session's place was never admitted again")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
