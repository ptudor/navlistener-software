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
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

type terminalListener struct{ err error }

func (l terminalListener) Accept() (net.Conn, error) { return nil, l.err }
func (terminalListener) Close() error                { return nil }
func (terminalListener) Addr() net.Addr              { return &net.TCPAddr{} }

func TestPushServeReturnsTerminalAcceptFailure(t *testing.T) {
	p := newPushServer("unused", &tls.Config{}, make(chan *RawFrame), nil, time.Second, 1,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	err := p.serve(context.Background(), terminalListener{err: errors.New("terminal accept failure")})
	if err == nil || !strings.Contains(err.Error(), "terminal accept failure") {
		t.Fatalf("serve error = %v, want terminal accept failure", err)
	}
}

// oneConnThenTerminal returns one connection, then a (non-temporary) terminal error.
type oneConnThenTerminal struct {
	conn net.Conn
	err  error
	done bool
}

func (l *oneConnThenTerminal) Accept() (net.Conn, error) {
	if !l.done {
		l.done = true
		return l.conn, nil
	}
	return nil, l.err
}
func (*oneConnThenTerminal) Close() error   { return nil }
func (*oneConnThenTerminal) Addr() net.Addr { return &net.TCPAddr{} }

// TestPushServeTerminalAcceptWithActiveConn guards on a non-temporary accept error
// with an in-flight connection, serve() must still return (so main can report the fatal and
// tear down) rather than deadlocking in wg.Wait() — the in-flight handler exits only on
// context cancellation, and the parent ctx is cancelled only AFTER serve returns. serve()'s
// child-context cancel breaks that cycle. Before the fix this hangs (until the handler's 30s
// read deadline at best, forever on a live streaming feeder).
func TestPushServeTerminalAcceptWithActiveConn(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() // held open, never writes: the handler blocks reading the magic
	ln := &oneConnThenTerminal{conn: server, err: errors.New("terminal accept failure")}
	p := newPushServer("unused", &tls.Config{}, make(chan *RawFrame), nil, time.Second, 4,
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	done := make(chan error, 1)
	go func() { done <- p.serve(context.Background(), ln) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "terminal accept failure") {
			t.Fatalf("serve error = %v, want terminal accept failure", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve deadlocked on wg.Wait() after a terminal accept error with an active connection ")
	}
}

func TestPushListenFailsWhenAddressOccupied(t *testing.T) {
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	p := newPushServer(occupied.Addr().String(), &tls.Config{}, make(chan *RawFrame), nil,
		time.Second, 1, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if ln, err := p.Listen(); err == nil {
		ln.Close()
		t.Fatal("push Listen succeeded on an occupied address")
	}
}

func TestMatchPeerIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		certs    []*x509.Certificate
		observer string
		ok       bool
	}{
		{"matching DNS SAN", []*x509.Certificate{{DNSNames: []string{"observer16"}}}, "observer16", true},
		{"another observer", []*x509.Certificate{{DNSNames: []string{"observer17"}}}, "observer16", false},
		{"missing certificate", nil, "observer16", false},
		{"legacy CN only", []*x509.Certificate{{Subject: pkix.Name{CommonName: "observer16"}}}, "observer16", false},
		{"ambiguous SAN", []*x509.Certificate{{DNSNames: []string{"observer16", "alias"}}}, "observer16", false},
		{"case alias", []*x509.Certificate{{DNSNames: []string{"OBSERVER16"}}}, "observer16", false},
		{"unicode alias", []*x509.Certificate{{DNSNames: []string{"obsérver16"}}}, "obsérver16", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := matchPeerIdentity(tc.certs, tc.observer)
			if (err == nil) != tc.ok {
				t.Fatalf("error = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// mtlsPKI is an in-memory fleet CA for the end-to-end mTLS binding tests
// : issue() returns a client leaf carrying exactly the given DNS SANs,
// signed by the CA the test server trusts in ClientCAs.
type mtlsPKI struct {
	pool   *x509.CertPool
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

func newMtlsPKI(t *testing.T) *mtlsPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fleet-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return &mtlsPKI{pool: pool, caCert: cert, caKey: key}
}

func (p *mtlsPKI) issue(t *testing.T, cn string, sans ...string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		DNSNames:     sans,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// startMTLSPushServer is startPushServer with client-certificate verification
// against the given fleet CA pool, exercising the same ClientAuth mode
// newPushServer configures when push.client_ca is set.
func startMTLSPushServer(t *testing.T, ctx context.Context, auth Authenticator, pool *x509.CertPool) (string, chan *RawFrame) {
	t.Helper()
	out := make(chan *RawFrame, 8)
	tc := &tls.Config{
		Certificates: []tls.Certificate{selfSigned(t)},
		MinVersion:   tls.VersionTLS12,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
	}
	srv := newPushServer("127.0.0.1:0", tc, out, auth, 25*time.Millisecond, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.serve(ctx, ln) }()
	return ln.Addr().String(), out
}

func dialPushWithCert(t *testing.T, addr string, cert tls.Certificate) *tls.Conn {
	t.Helper()
	conn, err := tls.Dial("tcp", addr, &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cert},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := wire.WriteMagic(conn); err != nil {
		t.Fatal(err)
	}
	return conn
}

// TestPushMTLSBindsCertificateToObserver drives full mTLS handshakes :
// a certificate whose single DNS SAN matches the token's canonical observer is
// admitted and its frames flow; a *different* station's valid fleet certificate
// presented with a stolen victim token is rejected before WELCOME with zero
// frames enqueued; a CN-only legacy certificate is likewise rejected.
func TestPushMTLSBindsCertificateToObserver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pki := newMtlsPKI(t)
	addr, out := startMTLSPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"), pki.pool)

	t.Run("matching SAN admitted", func(t *testing.T) {
		conn := dialPushWithCert(t, addr, pki.issue(t, "observer16", "observer16"))
		defer conn.Close()
		if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx"}); err != nil {
			t.Fatal(err)
		}
		if _, payload, err := wire.ReadFrame(conn); err != nil {
			t.Fatal(err)
		} else if wmsg, _ := parseWelcome(payload); !wmsg.OK {
			t.Fatalf("welcome = %+v, want ok", wmsg)
		}
		rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
		if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(1, rec)); err != nil {
			t.Fatal(err)
		}
		select {
		case f := <-out:
			if f.Source != "observer16" {
				t.Errorf("frame source = %q, want observer16", f.Source)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("frame did not reach the decode channel")
		}
	})

	t.Run("other station's cert with victim token rejected", func(t *testing.T) {
		conn := dialPushWithCert(t, addr, pki.issue(t, "observer17", "observer17"))
		defer conn.Close()
		if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx"}); err != nil {
			t.Fatal(err)
		}
		if _, payload, err := wire.ReadFrame(conn); err != nil {
			t.Fatal(err)
		} else if wmsg, _ := parseWelcome(payload); wmsg.OK {
			t.Fatal("mismatched certificate identity was accepted")
		}
		select {
		case f := <-out:
			t.Fatalf("rejected connection enqueued a frame: %+v", f)
		case <-time.After(100 * time.Millisecond):
		}
	})

	t.Run("legacy CN-only cert rejected", func(t *testing.T) {
		conn := dialPushWithCert(t, addr, pki.issue(t, "observer16"))
		defer conn.Close()
		if err := wire.WriteHello(conn, wire.HelloMsg{Token: "s3cret", Station: "observer16", Feed: "ubx"}); err != nil {
			t.Fatal(err)
		}
		if _, payload, err := wire.ReadFrame(conn); err != nil {
			t.Fatal(err)
		} else if wmsg, _ := parseWelcome(payload); wmsg.OK {
			t.Fatal("CN-only certificate was accepted")
		}
	})
}

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

	// The ack ticker acknowledges the highest sequence received this connection
	//  -- here, just seq 1, since only one frame was sent.
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

// TestPushHelloOversizedRejectedPreAuth guards the pre-auth HELLO read
// must reject a length prefix beyond helloMaxLen immediately, without ever
// attempting to read the declared payload -- otherwise an attacker-declared
// length up to wire.MaxFrameLen (1 MiB) pins that much per pre-auth connection.
func TestPushHelloOversizedRejectedPreAuth(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr, _ := startPushServer(t, ctx, tokenAuth("observer16", "s3cret", "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()

	// Declare a length beyond helloMaxLen (4096) but well under wire.MaxFrameLen
	// (1 MiB) -- then never send that many payload bytes. If the collector only
	// enforced MaxFrameLen it would block waiting for the (never-arriving) rest
	// of the payload and this test would time out; the regression fix cap must reject
	// right after the 5-byte header, before attempting to read any payload.
	var hdr [5]byte
	hdr[0] = byte(wire.Hello)
	binary.BigEndian.PutUint32(hdr[1:], 8192)
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}

	// A short client-side deadline, well under the server's 30s handshake-phase
	// deadline: if the collector still tried to read the full declared payload
	// (i.e. the fix is missing), our own Read here would time out waiting -- which
	// must not be mistaken for the server having rejected and closed the
	// connection. Only a non-timeout error (EOF / connection reset, i.e. the
	// server actually closed it) counts as a pass.
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1)
	_, err := conn.Read(buf)
	if err == nil {
		t.Fatal("expected the connection to be closed after an oversized pre-auth HELLO length, got data instead")
	}
	if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("client read timed out waiting -- the server never closed the connection (it read/blocked on the oversized HELLO instead of rejecting it): %v", err)
	}
}

// TestPushHelloMaxSizeStillAuthenticates guards the other half of fix
// spec: the reception-side cap must not be so tight that a legitimate,
// near-the-feeder's-own-limit HELLO (navfeeder.c never sends one over 1024
// bytes) is rejected.
func TestPushHelloMaxSizeStillAuthenticates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bigToken := strings.Repeat("a", 900) // realistic upper bound per the finding, comfortably under helloMaxLen
	addr, _ := startPushServer(t, ctx, tokenAuth("observer16", bigToken, "ubx"))

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: bigToken, Station: "observer16", Feed: "ubx"}); err != nil {
		t.Fatal(err)
	}
	ft, payload, err := wire.ReadFrame(conn)
	if err != nil || ft != wire.Welcome {
		t.Fatalf("welcome frame: ft=%d err=%v", ft, err)
	}
	wmsg, err := parseWelcome(payload)
	if err != nil || !wmsg.OK {
		t.Fatalf("welcome = %+v err=%v, want ok (a max-size HELLO must still authenticate)", wmsg, err)
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

	// a negative RecvUnixNs (byte-swapped/sign-flipped clock) is as implausible as a
	// far-future stamp — it must increment recv_ts_implausible (not fall through uncounted)
	// while still falling back to now(). == 0 stays uncounted (asserted above by rec3).
	c := metrics.PushErrorsTotal.WithLabelValues("obsNeg", "recv_ts_implausible")
	start := testutil.ToFloat64(c)
	recNeg := wire.RawRecord{RecvUnixNs: -1_000_000, GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	f4 := recordToFrame(recNeg, "ubx", "obsNeg")
	if f4 == nil || f4.Recv.Before(before) {
		t.Errorf("Recv = %v, want now() for a negative stamp", f4.Recv)
	}
	if got := testutil.ToFloat64(c) - start; got != 1 {
		t.Errorf("recv_ts_implausible delta = %v, want 1 for a negative stamp ", got)
	}
}

// TestRecordToFramePushRTCMDerivesMsgTypeFromPayload guards the GNF1
// frame_type byte is 8 bits and cannot carry an RTCM message number (12 bits,
// e.g. 1019); recordToFrame must derive MsgType from the payload's first 12
// bits the same way scanRTCM does, not trust the feeder-supplied frame_type.
func TestRecordToFramePushRTCMDerivesMsgTypeFromPayload(t *testing.T) {
	// Message 1019 (GPS ephemeris): 0x3FB << 4 == 0x3FB0, top byte 0x3F, next
	// nibble 0xB0's high nibble 0xB -- payload[0]=0x3F, payload[1]=0xB0... gives
	// msgNum = 0x3F<<4 | 0xB0>>4 = 0x3F0 | 0xB = 0x3FB = 1019.
	rec := wire.RawRecord{
		FrameType: 0x10, // a frame_type the old code would have used verbatim -- must be ignored
		Raw:       []byte{0x3F, 0xB0, 0x00, 0x00},
	}
	f := recordToFrame(rec, "rtcm", "obs1")
	if f == nil {
		t.Fatal("recordToFrame returned nil")
	}
	if f.MsgType != 1019 {
		t.Errorf("MsgType = %d, want 1019 (derived from payload, not frame_type=0x10)", f.MsgType)
	}
	if f.Bytes == nil || f.Words != nil {
		t.Errorf("rtcm feed must set Bytes (not Words); Bytes=%v Words=%v", f.Bytes, f.Words)
	}

	// Too short to hold a 12-bit message number: MsgType falls back to frame_type
	// rather than indexing out of range.
	short := wire.RawRecord{FrameType: 0x22, Raw: []byte{0xAB}}
	fs := recordToFrame(short, "rtcm", "obs1")
	if fs == nil || fs.MsgType != 0x22 {
		t.Errorf("short rtcm payload: MsgType = %v, want fallback to frame_type 0x22", fs)
	}
}

// tempNetError mimics the shape real EMFILE/ENFILE errors reach the accept loop
// in: a net.Error whose Temporary() is true (syscall.Errno reports EMFILE/ENFILE
// as temporary). serve() classifies on exactly that, so a plain errors.New would
// take the terminal fail-closed branch instead of the regression fix backoff branch and
// make the backoff test vacuous.
type tempNetError struct{}

func (tempNetError) Error() string   { return "accept: too many open files" }
func (tempNetError) Timeout() bool   { return false }
func (tempNetError) Temporary() bool { return true }

// alwaysErrListener is a net.Listener whose Accept always fails with a
// persistent-but-temporary error -- simulating EMFILE/ENFILE.
type alwaysErrListener struct {
	calls int32
}

func (l *alwaysErrListener) Accept() (net.Conn, error) {
	atomic.AddInt32(&l.calls, 1)
	return nil, tempNetError{}
}
func (l *alwaysErrListener) Close() error   { return nil }
func (l *alwaysErrListener) Addr() net.Addr { return &net.TCPAddr{} }

// TestPushAcceptBackoffBoundsCallRate guards a persistent Accept error
// must not hot-spin the loop at 100% CPU -- the fix spec's exact verification
// ("bounded call rate; ctx cancel still returns promptly").
func TestPushAcceptBackoffBoundsCallRate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	srv := newPushServer("127.0.0.1:0", &tls.Config{}, nil, nil, time.Second, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	ln := &alwaysErrListener{}

	done := make(chan error, 1)
	go func() { done <- srv.serve(ctx, ln) }()

	time.Sleep(200 * time.Millisecond)
	calls := atomic.LoadInt32(&ln.calls)
	// Without backoff this would be tens of thousands of calls in 200ms; with a
	// 5ms-1s exponential backoff it's a handful (~6). 60 gives ample margin
	// against scheduling jitter while still catching a hot-spin regression.
	if calls > 60 {
		t.Errorf("Accept called %d times in 200ms, want a bounded (backed-off) rate", calls)
	}
	if calls < 3 {
		t.Errorf("Accept called only %d times in 200ms -- the loop stopped retrying (temporary errors must take the backoff branch, not the terminal one)", calls)
	}

	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("serve did not return promptly after ctx cancel")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("serve took %v to return after cancel, want prompt (ctx must be honored during the backoff sleep)", elapsed)
	}
}

// splitWriteConn wraps a real net.Conn (from net.Pipe, for deterministic
// Read/Close semantics) but makes every Write fail -- simulating a connection
// whose write side is dead while the read side would otherwise keep delivering
// (exact scenario: a broken TLS write with reads still arriving).
type splitWriteConn struct {
	net.Conn
	writeErr error
}

func (c *splitWriteConn) Write([]byte) (int, error) { return 0, c.writeErr }

// TestPushAckWriteFailureClosesConnection guards an ack-write failure
// must close the connection so the read loop's blocked ReadFrame errors out too
// -- otherwise frames are consumed forever, never acked, while the connection
// looks alive (the feeder's spool fills and eventually drops frames permanently).
func TestPushAckWriteFailureClosesConnection(t *testing.T) {
	srvConn, cliConn := net.Pipe()
	defer cliConn.Close()
	sw := &splitWriteConn{Conn: srvConn, writeErr: errors.New("simulated broken write side")}
	w := &connWriter{c: sw}

	out := make(chan *RawFrame, 4)
	p := &PushServer{out: out, ackInterval: 10 * time.Millisecond, log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	done := make(chan struct{})
	go func() {
		defer close(done)
		p.stream(context.Background(), srvConn, w, "observer16", "ubx")
	}()

	// Feed one DATA frame so `highest` advances past `acked` -- otherwise the ack
	// ticker sees highest==acked and never attempts a write at all.
	rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	if err := wire.WriteFrame(cliConn, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}
	select {
	case <-out:
	case <-time.After(time.Second):
		t.Fatal("frame did not reach the decode channel")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stream did not return after an ack-write failure -- the connection was not closed, so the read loop kept blocking indefinitely")
	}
}
