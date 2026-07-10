package ingest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"math/big"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/wire"
)

// selfSigned builds an in-memory self-signed server certificate for the test TLS
// listener (no files, no key material on disk).
func selfSigned(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-collector"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startPushServer stands up a PushServer on an ephemeral loopback TLS listener and
// returns its address plus the frame channel it feeds.
func startPushServer(t *testing.T, ctx context.Context, auth Authenticator) (string, chan *RawFrame) {
	t.Helper()
	out := make(chan *RawFrame, 8)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, auth, 25*time.Millisecond, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.serve(ctx, ln) }()
	return ln.Addr().String(), out
}

func tokenAuth(station, token string, feeds ...string) Authenticator {
	sum := sha256.Sum256([]byte(token))
	return NewConfigAuthenticator([]config.PushObserver{
		{Station: station, TokenSHA256: hex.EncodeToString(sum[:]), Feeds: feeds},
	})
}

func dialPush(t *testing.T, addr string) *tls.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteMagic(conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

// TestPushHappyPath drives a full feeder session: magic, authenticated HELLO,
// WELCOME ok, one DATA frame, and asserts the reconstructed RawFrame reaches the
// decode channel with the record's fields, plus an ACK comes back.
func TestPushHappyPath(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx"}); err != nil {
		t.Fatal(err)
	}
	ft, payload, err := wire.ReadFrame(conn)
	if err != nil || ft != wire.Welcome {
		t.Fatalf("welcome frame: ft=%d err=%v", ft, err)
	}
	wmsg, err := parseWelcome(payload)
	if err != nil || !wmsg.OK {
		t.Fatalf("welcome = %+v err=%v, want ok", wmsg, err)
	}

	rec := wire.RawRecord{
		RecvUnixNs: time.Now().UnixNano(),
		GnssID:     gnss.GPS, SvID: 5, SigID: 0,
		Raw: make([]byte, 40), // 10 words
	}
	if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}

	select {
	case f := <-out:
		if f.Source != "observer16" || f.GnssID != gnss.GPS || f.SvID != 5 || len(f.Words) != 10 {
			t.Errorf("frame = %+v, want observer16/GPS/5/10words", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame did not reach the decode channel")
	}

	// The ack ticker acknowledges the last contiguous sequence (1).
	if seq := readAck(t, conn); seq != 1 {
		t.Errorf("ack seq = %d, want 1", seq)
	}
}

// TestPushReplayFromReconnect models a feeder resuming after a disconnect: it
// replays unacked frames starting from a global sequence well past 1. The collector
// must ack the highest sequence it saw this connection (not "contiguous from 1"),
// or the feeder could never prune its spool. This guards the reconnect contract that
// makes the store-and-forward feeder lossless (docs/DESIGN.md §2).
func TestPushReplayFromReconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx"}); err != nil {
		t.Fatal(err)
	}
	if _, payload, err := wire.ReadFrame(conn); err != nil {
		t.Fatal(err)
	} else if wmsg, _ := parseWelcome(payload); !wmsg.OK {
		t.Fatal("handshake rejected")
	}

	// Replay three frames whose sequences resume mid-stream (503, 504, 505).
	for _, seq := range []uint64{503, 504, 505} {
		rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
		if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(seq, rec)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-out:
		case <-time.After(2 * time.Second):
			t.Fatalf("frame seq=%d did not reach decode", seq)
		}
	}

	// The ack must reflect the highest replayed sequence so the feeder can prune.
	if seq := readAck(t, conn); seq != 505 {
		t.Errorf("ack seq = %d, want 505 (highest replayed)", seq)
	}
}

// TestPushZstdStream confirms the collector negotiates and decompresses a zstd DATA
// stream: the feeder requests zstd in HELLO, the collector confirms it in WELCOME, and the
// DATA frames — sent through a zstd stream flushed per frame, exactly as the C feeder does —
// decode correctly on the far side. ACKs stay plaintext. (The C-feeder↔Go-collector zstd
// path is exercised end-to-end in TestNavfeederEndToEnd/zstd; this covers it in pure Go.)
func TestPushZstdStream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx", Zstd: true}); err != nil {
		t.Fatal(err)
	}
	ft, payload, err := wire.ReadFrame(conn)
	if err != nil || ft != wire.Welcome {
		t.Fatalf("welcome frame: ft=%d err=%v", ft, err)
	}
	wmsg, err := parseWelcome(payload)
	if err != nil || !wmsg.OK || !wmsg.Zstd {
		t.Fatalf("welcome = %+v err=%v, want ok+zstd confirmed", wmsg, err)
	}

	// Everything after the handshake is compressed through one zstd stream, flushed per
	// frame so the collector decodes each promptly (the feeder's conn_write does the same).
	enc, err := zstd.NewWriter(conn)
	if err != nil {
		t.Fatal(err)
	}
	rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.Galileo, SvID: 14, SigID: 1, Raw: make([]byte, 32)}
	if err := wire.WriteFrame(enc, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}

	select {
	case f := <-out:
		if f.GnssID != gnss.Galileo || f.SvID != 14 || f.SigID != 1 || len(f.Words) != 8 {
			t.Errorf("frame = %+v, want Galileo/14/1/8words", f)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("zstd DATA frame did not decode on the collector")
	}
	if seq := readAck(t, conn); seq != 1 { // ACK is plaintext
		t.Errorf("ack seq = %d, want 1", seq)
	}
}

// TestPushZstdRejectsOversizedWindow guards a feeder that negotiates zstd
// but declares a window larger than the collector's WithDecoderMaxWindow cap must
// be rejected (the connection dropped, no frame delivered), not silently
// accepted into an outsized allocation.
func TestPushZstdRejectsOversizedWindow(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx", Zstd: true}); err != nil {
		t.Fatal(err)
	}
	ft, payload, err := wire.ReadFrame(conn)
	if err != nil || ft != wire.Welcome {
		t.Fatalf("welcome frame: ft=%d err=%v", ft, err)
	}
	if wmsg, _ := parseWelcome(payload); !wmsg.OK {
		t.Fatal("handshake rejected")
	}

	// zstdMaxWindow is 16 MiB (push.go); declare a window well beyond it.
	enc, err := zstd.NewWriter(conn, zstd.WithWindowSize(64<<20))
	if err != nil {
		t.Fatal(err)
	}
	rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	if err := wire.WriteFrame(enc, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}
	if err := enc.Flush(); err != nil {
		t.Fatal(err)
	}

	select {
	case f := <-out:
		t.Fatalf("frame with an oversized declared window should have been rejected, got %+v", f)
	case <-time.After(500 * time.Millisecond):
		// expected: no frame delivered — the decoder rejected the frame header.
	}
}

// TestPushRejectsBadToken confirms an unknown token gets WELCOME ok=false and no
// frame is admitted.
func TestPushRejectsBadToken(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "wrong", Station: "observer16", Feed: "ubx"}); err != nil {
		t.Fatal(err)
	}
	ft, payload, err := wire.ReadFrame(conn)
	if err != nil || ft != wire.Welcome {
		t.Fatalf("welcome frame: ft=%d err=%v", ft, err)
	}
	wmsg, _ := parseWelcome(payload)
	if wmsg.OK {
		t.Fatal("bad token was accepted")
	}
}

// TestPushRejectsUngrantedFeed confirms a valid token pushing a feed it wasn't
// granted is rejected (the feed_types grant).
func TestPushRejectsUngrantedFeed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	_ = wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "sbf"})
	_, payload, err := wire.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if wmsg, _ := parseWelcome(payload); wmsg.OK {
		t.Fatal("ungranted feed was accepted")
	}
}

// TestPushRejectsStationMismatch guards a valid token presented with a
// station id other than the token's canonical one is a misconfiguration (a feeder
// pointed at the wrong station) and must be rejected, not silently accepted under
// the token's real identity.
func TestPushRejectsStationMismatch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "wrong-station", Feed: "ubx"}); err != nil {
		t.Fatal(err)
	}
	_, payload, err := wire.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if wmsg, _ := parseWelcome(payload); wmsg.OK {
		t.Fatal("station mismatch was accepted")
	}
}

// TestPushMaxConnsBounded guards the accept loop must not spawn more than
// maxConns concurrent handler goroutines, and a slot must free (and admit a
// waiting connection) when a held connection closes.
func TestPushMaxConnsBounded(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *RawFrame, 8)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	const maxConns = 2
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("observer16", "s3cret", "ubx"), 25*time.Millisecond, maxConns,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.serve(ctx, ln) }()
	addr := ln.Addr().String()

	// Open maxConns connections that never send the GNF1 magic — each is accepted
	// and holds a semaphore slot indefinitely (parked reading the handshake).
	held := make([]*tls.Conn, maxConns)
	for i := range held {
		held[i] = dialPush(t, addr) // WriteMagic only; no HELLO, so handle() blocks reading it
	}
	defer func() {
		for _, c := range held {
			if c != nil {
				c.Close()
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		if len(srv.conns) == maxConns {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("semaphore never filled: len=%d, want %d", len(srv.conns), maxConns)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// A full semaphore must not block shutdown: a pending Accept()'s admit attempt
	// (or, as here, an already-parked handler) must not prevent ctx cancellation
	// from draining and returning promptly.
	held[0].Close()
	held[0] = nil

	deadline = time.Now().Add(2 * time.Second)
	for {
		if len(srv.conns) < maxConns {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("semaphore slot was not released after the connection closed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestPushMaxConnsShutdownCompletes guards the fix's shutdown safety: even
// with the semaphore at capacity, cancelling ctx must let serve() return promptly
// (it must not deadlock trying to admit a connection that will never come, nor
// wait forever on an already-parked handler that ctx cancellation itself closes).
func TestPushMaxConnsShutdownCompletes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan *RawFrame, 8)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	const maxConns = 1
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("observer16", "s3cret", "ubx"), 25*time.Millisecond, maxConns,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()

	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()

	conn := dialPush(t, addr)
	defer conn.Close()

	deadline := time.Now().Add(2 * time.Second)
	for len(srv.conns) < maxConns {
		if time.Now().After(deadline) {
			t.Fatal("semaphore never filled")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serve() = %v, want nil on clean shutdown", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("serve() did not return after ctx cancellation with a full semaphore")
	}
}

// TestPushHandleReturnsOnDisconnect guards the per-connection ack goroutine
// must exit (and handle() must return, releasing the conn's goroutines/FD) on a
// quiet client disconnect, not just on a write error. Runs many connect/disconnect
// cycles and asserts the goroutine count settles back down rather than growing.
func TestPushHandleReturnsOnDisconnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, out := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for {
			select {
			case <-out:
			case <-ctx.Done():
				return
			}
		}
	}()

	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		conn := dialPush(t, addr)
		if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx"}); err != nil {
			t.Fatal(err)
		}
		if _, payload, err := wire.ReadFrame(conn); err != nil {
			t.Fatal(err)
		} else if wmsg, _ := parseWelcome(payload); !wmsg.OK {
			t.Fatal("handshake rejected")
		}
		rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
		if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(1, rec)); err != nil {
			t.Fatal(err)
		}
		_ = readAck(t, conn)
		conn.Close() // quiet disconnect: no write error, exercises the regression fix path
	}

	// The read loop notices the close on its next read; give it a moment to unwind.
	deadline := time.Now().Add(3 * time.Second)
	var after int
	for {
		after = runtime.NumGoroutine()
		if after <= before+5 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if after > before+5 {
		t.Errorf("goroutine count grew from %d to %d after 50 connect/disconnect cycles (leak)", before, after)
	}
	cancel()
	<-drainDone
}

// TestPushAckWaitsForBackpressure guards the acked watermark must not
// advance for a frame that has not actually been handed off to the decode stage.
// With a size-1, undrained out channel, frame 2 blocks in the read loop until the
// channel is drained — during that time the ack must stay at 1, not jump to 2 and
// falsely tell the feeder it can prune a frame that was never delivered.
func TestPushAckWaitsForBackpressure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := make(chan *RawFrame, 1)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth("observer16", "s3cret", "ubx"), 25*time.Millisecond, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.serve(ctx, ln) }()
	addr := ln.Addr().String()

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx"}); err != nil {
		t.Fatal(err)
	}
	if _, payload, err := wire.ReadFrame(conn); err != nil {
		t.Fatal(err)
	} else if wmsg, _ := parseWelcome(payload); !wmsg.OK {
		t.Fatal("handshake rejected")
	}

	send := func(seq uint64) {
		rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
		if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(seq, rec)); err != nil {
			t.Fatal(err)
		}
	}
	send(1) // fills the size-1 out channel; nothing drains it yet
	send(2) // the server's read loop now blocks handing this off

	if seq := readAck(t, conn); seq != 1 {
		t.Errorf("ack seq = %d, want 1 (frame 2 not yet delivered, must not be acked)", seq)
	}

	<-out // drain frame 1, unblocking the read loop's send of frame 2
	select {
	case <-out:
	case <-time.After(2 * time.Second):
		t.Fatal("frame 2 was never delivered after the channel drained")
	}
	if seq := readAck(t, conn); seq != 2 {
		t.Errorf("ack seq = %d, want 2 after frame 2 was actually delivered", seq)
	}
}

func readAck(t *testing.T, conn *tls.Conn) uint64 {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		ft, payload, err := wire.ReadFrame(conn)
		if err != nil {
			t.Fatalf("reading ack: %v", err)
		}
		if ft == wire.Ack {
			seq, err := wire.DecodeAck(payload)
			if err != nil {
				t.Fatal(err)
			}
			return seq
		}
	}
}

// parseWelcome decodes a WELCOME payload for the test.
func parseWelcome(payload []byte) (wire.WelcomeMsg, error) {
	var m wire.WelcomeMsg
	err := json.Unmarshal(payload, &m)
	return m, err
}

// TestRecordToFrameClampsImplausibleTimestamp guards a feeder-supplied
// RecvUnixNs far outside recvTimestampSlack of wall-clock (either direction) must
// be rejected in favor of now(), not trusted verbatim into the historian's
// time-based math. A timestamp within slack, and the == 0 fallback, are untouched.
func TestRecordToFrameClampsImplausibleTimestamp(t *testing.T) {
	before := time.Now()
	rec := wire.RawRecord{
		RecvUnixNs: time.Now().Add(48 * time.Hour).UnixNano(), // far future
		GnssID:     gnss.GPS, SvID: 5, Raw: make([]byte, 40),
	}
	f := recordToFrame(rec, "ubx", "obs1")
	if f == nil {
		t.Fatal("recordToFrame returned nil")
	}
	after := time.Now()
	if f.Recv.Before(before) || f.Recv.After(after) {
		t.Errorf("Recv = %v, want clamped to now() (between %v and %v)", f.Recv, before, after)
	}

	// A plausible, recent timestamp is trusted as-is.
	plausible := time.Now().Add(-time.Second)
	rec2 := wire.RawRecord{RecvUnixNs: plausible.UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	f2 := recordToFrame(rec2, "ubx", "obs1")
	if f2 == nil {
		t.Fatal("recordToFrame returned nil")
	}
	if !f2.Recv.Equal(plausible) {
		t.Errorf("Recv = %v, want the plausible feeder timestamp %v unmodified", f2.Recv, plausible)
	}

	// The == 0 fallback (no feeder timestamp at all) is untouched.
	rec3 := wire.RawRecord{GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	f3 := recordToFrame(rec3, "ubx", "obs1")
	if f3 == nil || f3.Recv.Before(before) {
		t.Errorf("Recv = %v, want now() when RecvUnixNs is unset", f3.Recv)
	}
}
