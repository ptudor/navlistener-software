package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// Authenticator validates an edge feeder's HELLO. It returns the canonical observer
// id (the source tag stamped on every frame) and whether the token authorizes this
// station for this feed type. Implementations must be safe for concurrent use. The
// config-backed authenticator here is the bootstrap tier; a Django/DB-backed one
// (the shared AAA plane, docs/DESIGN.md §3) satisfies the same interface later.
type Authenticator interface {
	Authenticate(token, station, feed string) (observerID string, ok bool)
}

// configAuth authenticates against the [[push.observer]] table: it matches the
// SHA-256 of the presented bearer token and checks the feed grant.
type configAuth struct {
	byHash map[string]config.PushObserver
}

// NewConfigAuthenticator builds an Authenticator from the configured observers.
func NewConfigAuthenticator(observers []config.PushObserver) Authenticator {
	m := make(map[string]config.PushObserver, len(observers))
	for _, o := range observers {
		m[normalizeHex(o.TokenSHA256)] = o
	}
	return &configAuth{byHash: m}
}

func (a *configAuth) Authenticate(token, station, feed string) (string, bool) {
	sum := sha256.Sum256([]byte(token))
	o, ok := a.byHash[hex.EncodeToString(sum[:])]
	if !ok {
		return "", false
	}
	for _, f := range o.Feeds {
		if f == feed {
			return o.Station, true
		}
	}
	return "", false // authenticated, but not granted this feed
}

func normalizeHex(s string) string {
	b := make([]byte, 0, len(s))
	for _, r := range []byte(s) {
		if r >= 'A' && r <= 'F' {
			r += 'a' - 'A'
		}
		b = append(b, r)
	}
	return string(b)
}

// PushServer is the authenticated GNF1 push listener (the production fleet ingest
// path). It terminates TLS, authenticates each feeder, and forwards decoded frames
// to the same decode stage the dial connectors feed — one code path from either
// ingest mode.
type PushServer struct {
	addr        string
	tlsConfig   *tls.Config
	auth        Authenticator
	out         chan<- *RawFrame
	ackInterval time.Duration
	log         *slog.Logger
}

// NewPushServer builds the listener from config. It loads the server certificate and,
// if a client CA is configured, requires and verifies client certificates (mTLS).
func NewPushServer(cfg config.Push, out chan<- *RawFrame, auth Authenticator, log *slog.Logger) (*PushServer, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("push tls keypair: %w", err)
	}
	tc := &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12, // collector floor (docs/DESIGN.md §2)
	}
	if cfg.ClientCA != "" {
		pem, err := os.ReadFile(cfg.ClientCA)
		if err != nil {
			return nil, fmt.Errorf("push client_ca: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("push client_ca: no certificates parsed from %s", cfg.ClientCA)
		}
		tc.ClientCAs = pool
		tc.ClientAuth = tls.RequireAndVerifyClientCert
	}
	return newPushServer(cfg.Addr, tc, out, auth, cfg.AckInterval, log), nil
}

// newPushServer builds a PushServer from a ready TLS config (the file-loading
// NewPushServer wraps it; tests construct an in-memory config directly).
func newPushServer(addr string, tc *tls.Config, out chan<- *RawFrame, auth Authenticator, ack time.Duration, log *slog.Logger) *PushServer {
	if ack <= 0 {
		ack = time.Second
	}
	return &PushServer{addr: addr, tlsConfig: tc, auth: auth, out: out, ackInterval: ack, log: log}
}

// Run listens until ctx is cancelled, handling each feeder connection concurrently.
func (p *PushServer) Run(ctx context.Context) error {
	ln, err := tls.Listen("tcp", p.addr, p.tlsConfig)
	if err != nil {
		return fmt.Errorf("push listen %s: %w", p.addr, err)
	}
	p.log.Info("push endpoint listening", "addr", p.addr, "mtls", p.tlsConfig.ClientAuth != tls.NoClientCert)
	return p.serve(ctx, ln)
}

// serve runs the accept loop on an established listener until ctx is cancelled,
// then waits for in-flight connections to drain (tests supply their own listener).
func (p *PushServer) serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); _ = ln.Close() }()

	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil // clean shutdown
			}
			p.log.Warn("push accept failed", "error", err)
			continue
		}
		wg.Add(1)
		go func() { defer wg.Done(); p.handle(ctx, conn) }()
	}
}

// connWriter serializes frame writes to one connection: the read loop (PONG) and the
// ack ticker both write, so every WriteFrame is mutex-guarded.
type connWriter struct {
	mu sync.Mutex
	c  net.Conn
}

func (w *connWriter) write(ft wire.FrameType, payload []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return wire.WriteFrame(w.c, ft, payload)
}

// handle runs one feeder connection: TLS is already up, so read the GNF1 magic,
// authenticate the HELLO, then stream DATA frames into the decode stage with acked,
// in-order sequence tracking.
func (p *PushServer) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()

	// Close the connection when the daemon shuts down so a blocked read returns.
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()

	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err == nil {
		defer conn.SetReadDeadline(time.Time{})
	}
	if err := wire.ReadMagic(conn); err != nil {
		p.log.Warn("push bad handshake", "remote", remote, "error", err)
		return
	}
	w := &connWriter{c: conn}
	observer, feed, ok := p.handshake(conn, w, remote)
	if !ok {
		return
	}
	metrics.PushConnectsTotal.WithLabelValues(observer).Inc()
	metrics.PushObserversUp.WithLabelValues(observer).Inc()
	defer metrics.PushObserversUp.WithLabelValues(observer).Dec()
	p.log.Info("push feeder authenticated", "observer", observer, "feed", feed, "remote", remote)

	p.stream(conn, w, observer, feed)
}

// handshake reads and authenticates the HELLO, replying WELCOME. It returns the
// canonical observer id and feed on success.
func (p *PushServer) handshake(conn net.Conn, w *connWriter, remote string) (observer, feed string, ok bool) {
	ft, payload, err := wire.ReadFrame(conn)
	if err != nil || ft != wire.Hello {
		p.log.Warn("push expected HELLO", "remote", remote, "frame", ft, "error", err)
		return "", "", false
	}
	h, err := wire.ParseHello(payload)
	if err != nil {
		p.log.Warn("push bad HELLO json", "remote", remote, "error", err)
		return "", "", false
	}
	obs, authed := p.auth.Authenticate(h.Token, h.Station, h.Feed)
	if !authed {
		metrics.PushAuthFailuresTotal.Inc()
		_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "unauthorized"}))
		p.log.Warn("push auth rejected", "remote", remote, "station", h.Station, "feed", h.Feed)
		return "", "", false
	}
	if scannerFor(h.Feed) == nil {
		_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "unsupported feed"}))
		return "", "", false
	}
	if err := w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{
		OK: true, AckIntervalMS: int(p.ackInterval / time.Millisecond),
	})); err != nil {
		return "", "", false
	}
	return obs, h.Feed, true
}

// stream reads DATA/PING frames, forwards decoded records to the decode stage, and
// acks the last contiguously-received sequence on the configured cadence.
func (p *PushServer) stream(conn net.Conn, w *connWriter, observer, feed string) {
	var (
		mu     sync.Mutex
		contig uint64 // highest in-order sequence received
		acked  uint64
	)
	ackTicker := time.NewTicker(p.ackInterval)
	defer ackTicker.Stop()
	ackDone := make(chan struct{})
	go func() {
		defer close(ackDone)
		for range ackTicker.C {
			mu.Lock()
			last, prev := contig, acked
			mu.Unlock()
			if last == prev {
				continue
			}
			if err := w.write(wire.Ack, wire.EncodeAck(last)); err != nil {
				return
			}
			mu.Lock()
			acked = last
			mu.Unlock()
		}
	}()

	for {
		// Feeders stream steadily; a long silence means a dead peer. PINGs reset it.
		_ = conn.SetReadDeadline(time.Now().Add(120 * time.Second))
		ft, payload, err := wire.ReadFrame(conn)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				p.log.Info("push feeder disconnected", "observer", observer, "error", err)
			} else {
				p.log.Warn("push feeder idle timeout", "observer", observer)
			}
			ackTicker.Stop()
			<-ackDone
			return
		}
		switch ft {
		case wire.Data:
			seq, rec, err := wire.DecodeData(payload)
			if err != nil {
				metrics.PushErrorsTotal.WithLabelValues(observer, "short_record").Inc()
				continue
			}
			mu.Lock()
			if seq == contig+1 {
				contig = seq // in order
			}
			mu.Unlock()
			f := recordToFrame(rec, feed, observer)
			metrics.FramesTotal.WithLabelValues(observer, fmt.Sprint(int(f.GnssID))).Inc()
			select {
			case p.out <- f:
			default:
				metrics.PushErrorsTotal.WithLabelValues(observer, "queue_full").Inc()
			}
		case wire.Ping:
			_ = w.write(wire.Pong, nil)
		default:
			metrics.PushErrorsTotal.WithLabelValues(observer, "unexpected_frame").Inc()
		}
	}
}

// recordToFrame reconstructs a RawFrame from a GNF1 raw record. Word-oriented feeds
// (ubx/sbf nav frames) carry the broadcast words big-endian in Raw; rtcm carries the
// message bytes. The reception time falls back to now when the feeder did not stamp
// it.
func recordToFrame(rec wire.RawRecord, feed, source string) *RawFrame {
	recv := time.Now()
	if rec.RecvUnixNs > 0 {
		recv = time.Unix(0, rec.RecvUnixNs)
	}
	f := &RawFrame{
		Recv:    recv,
		Source:  source,
		GnssID:  rec.GnssID,
		SvID:    int(rec.SvID),
		SigID:   int(rec.SigID),
		FreqID:  int(rec.FreqID),
		MsgType: int(rec.FrameType),
	}
	if feed == "rtcm" {
		f.Bytes = rec.Raw
	} else {
		f.Words = bytesToWords(rec.Raw)
	}
	return f
}

// bytesToWords reassembles big-endian 32-bit nav words (the inverse of
// RawFrame.RawBytes). A trailing partial word is dropped — nav frames are word
// aligned.
func bytesToWords(b []byte) []uint32 {
	n := len(b) / 4
	if n == 0 {
		return nil
	}
	words := make([]uint32, n)
	for i := 0; i < n; i++ {
		words[i] = binary.BigEndian.Uint32(b[i*4:])
	}
	return words
}

func mustWelcome(m wire.WelcomeMsg) []byte {
	b, _ := wire.MarshalWelcome(m)
	return b
}
