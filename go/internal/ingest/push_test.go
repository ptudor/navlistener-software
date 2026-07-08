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
	"testing"
	"time"

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
	srv := newPushServer("127.0.0.1:0", tc, out, auth, 25*time.Millisecond,
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
