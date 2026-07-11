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
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// idleReadTimeout bounds silence on a feeder connection: feeders stream steadily (or PING),
// so a longer gap means a dead peer. idleConn refreshes this deadline on every underlying
// read, so a wrapping zstd decoder — which pulls bytes outside the per-frame loop — still
// honours the timeout.
const idleReadTimeout = 120 * time.Second

// writeTimeout bounds every WriteFrame on a push connection (WELCOME/ACK/PONG). A
// feeder that stops reading must not be able to wedge the writer (and, via
// connWriter's mutex, every other writer on the same connection) past this deadline.
const writeTimeout = 30 * time.Second

// acceptBackoffInitial/acceptBackoffMax bound the accept loop's retry pace on a
// persistent Accept error (EMFILE/ENFILE, regression fix) -- without backoff, Accept
// fails instantly and the loop hot-spins at 100% CPU while flooding the log at
// line rate, exactly when the process is already in trouble (out of file
// descriptors, possibly from the regression fix pre-auth flood or the regression fix FD leak).
const (
	acceptBackoffInitial = 5 * time.Millisecond
	acceptBackoffMax     = 1 * time.Second
)

// zstdMaxWindow bounds the per-connection zstd decoder's window/memory.
// navfeeder.c's compressor sets only ZSTD_c_compressionLevel=3 (no explicit
// windowLog, no pledged source size), whose default window is 2 MiB
// (ZSTD_WINDOWLOG_LIMIT_DEFAULT-class level-3 table entry) — 16 MiB leaves a wide
// margin for that while still bounding a hostile-but-authenticated feeder that
// declares a large window from allocating an unbounded amount per connection.
const zstdMaxWindow = 16 << 20 // 16 MiB

// idleConn refreshes the read deadline on every Read, so a decoder reading through it (zstd)
// can't outlive the idle timeout even though it reads outside wire.ReadFrame's frame loop.
type idleConn struct {
	net.Conn
	timeout time.Duration
}

func (c *idleConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(p)
}

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
	// The token is the identity; station is a misconfiguration guard  — a
	// feeder pointed at the wrong station id (valid token, wrong presented name)
	// is rejected rather than silently accepted under the token's canonical
	// identity. handshake()'s caller already logs the rejected station/feed.
	if station != o.Station {
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
	// conns bounds concurrent in-flight connections : a buffered semaphore
	// acquired before spawning a handler goroutine, released in its defer. Without
	// mTLS (ClientCA unset), this is the only cap between the internet and
	// unbounded goroutine/FD growth from a pre-auth connection flood.
	conns chan struct{}
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
	return newPushServer(cfg.Addr, tc, out, auth, cfg.AckInterval, cfg.MaxConns, log), nil
}

// newPushServer builds a PushServer from a ready TLS config (the file-loading
// NewPushServer wraps it; tests construct an in-memory config directly).
func newPushServer(addr string, tc *tls.Config, out chan<- *RawFrame, auth Authenticator, ack time.Duration, maxConns int, log *slog.Logger) *PushServer {
	if ack <= 0 {
		ack = time.Second
	}
	if maxConns <= 0 {
		maxConns = 512
	}
	return &PushServer{addr: addr, tlsConfig: tc, auth: auth, out: out, ackInterval: ack, log: log, conns: make(chan struct{}, maxConns)}
}

// Run listens until ctx is cancelled, handling each feeder connection concurrently.
func (p *PushServer) Run(ctx context.Context) error {
	ln, err := p.Listen()
	if err != nil {
		return err
	}
	return p.Serve(ctx, ln)
}

// Listen binds the configured TLS listener. Startup calls this synchronously
// before any producer or historian goroutine is started.
func (p *PushServer) Listen() (net.Listener, error) {
	ln, err := tls.Listen("tcp", p.addr, p.tlsConfig)
	if err != nil {
		return nil, fmt.Errorf("push listen %s: %w", p.addr, err)
	}
	return ln, nil
}

// Serve runs an already-bound push listener until cancellation or a terminal
// listener error.
func (p *PushServer) Serve(ctx context.Context, ln net.Listener) error {
	p.log.Info("push endpoint listening", "addr", p.addr, "mtls", p.tlsConfig.ClientAuth != tls.NoClientCert)
	return p.serve(ctx, ln)
}

// serve runs the accept loop on an established listener until ctx is cancelled,
// then waits for in-flight connections to drain (tests supply their own listener).
func (p *PushServer) serve(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); _ = ln.Close() }()

	var wg sync.WaitGroup
	backoff := acceptBackoffInitial
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				wg.Wait()
				return nil // clean shutdown
			}
			if ne, ok := err.(net.Error); !ok || !ne.Temporary() {
				wg.Wait()
				return fmt.Errorf("push accept: %w", err)
			}
			p.log.Warn("push accept failed", "error", err, "backoff", backoff)
			if !sleep(ctx, backoff) {
				wg.Wait()
				return nil // ctx cancelled during the backoff sleep; clean shutdown
			}
			backoff *= 2
			if backoff > acceptBackoffMax {
				backoff = acceptBackoffMax
			}
			continue
		}
		backoff = acceptBackoffInitial
		select {
		case p.conns <- struct{}{}:
		case <-ctx.Done():
			_ = conn.Close()
			wg.Wait()
			return nil // clean shutdown; don't block admission on a full semaphore
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-p.conns }()
			p.handle(ctx, conn)
		}()
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
	_ = w.c.SetWriteDeadline(time.Now().Add(writeTimeout))
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

	// The magic + handshake are plaintext and time-boxed; the DATA stream that follows
	// refreshes its own deadline through idleConn below.
	if err := conn.SetReadDeadline(time.Now().Add(30 * time.Second)); err != nil {
		return
	}
	if err := wire.ReadMagic(conn); err != nil {
		p.log.Warn("push bad handshake", "remote", remote, "error", err)
		return
	}
	w := &connWriter{c: conn}
	observer, feed, useZstd, ok := p.handshake(conn, w, remote)
	if !ok {
		return
	}
	metrics.PushConnectsTotal.WithLabelValues(observer).Inc()
	metrics.PushObserversUp.WithLabelValues(observer).Inc()
	defer metrics.PushObserversUp.WithLabelValues(observer).Dec()
	p.log.Info("push feeder authenticated", "observer", observer, "feed", feed, "remote", remote, "zstd", useZstd)

	// Everything after the handshake is read through idleConn (deadline discipline); when the
	// feeder negotiated zstd, the DATA stream is decompressed first. ACKs/PONGs back to the
	// feeder stay plaintext (written straight to conn via connWriter).
	var frames io.Reader = &idleConn{Conn: conn, timeout: idleReadTimeout}
	if useZstd {
		zr, err := zstd.NewReader(frames,
			zstd.WithDecoderConcurrency(1), // one connection, one stream: no goroutine fan-out
			zstd.WithDecoderMaxWindow(zstdMaxWindow),
			zstd.WithDecoderMaxMemory(zstdMaxWindow),
		)
		if err != nil {
			p.log.Warn("push zstd reader init failed", "observer", observer, "error", err)
			return
		}
		defer zr.Close()
		frames = zr
	}

	p.stream(ctx, frames, w, observer, feed)
}

// helloMaxLen caps the pre-auth HELLO frame length far below wire.MaxFrameLen
// : a real HELLO is ~150 bytes and navfeeder.c never sends one over 1024
// bytes, but ReadFrame's normal 1 MiB cap would let any unauthenticated
// connection pin up to 1 MiB before a single byte is verified -- a
// per-connection amplifier for the regression fix pre-auth flood. This is a
// reception-side policy, not a wire change: DATA-phase reads (in stream, after
// authentication) keep the full MaxFrameLen.
const helloMaxLen = 4096

// handshake reads and authenticates the HELLO, replying WELCOME. It returns the
// canonical observer id, feed, and whether the DATA stream is zstd-compressed
// (confirmed only when the feeder requested it) on success.
func (p *PushServer) handshake(conn net.Conn, w *connWriter, remote string) (observer, feed string, useZstd, ok bool) {
	ft, payload, err := wire.ReadFrameMax(conn, helloMaxLen)
	if err != nil || ft != wire.Hello {
		p.log.Warn("push expected HELLO", "remote", remote, "frame", ft, "error", err)
		return "", "", false, false
	}
	h, err := wire.ParseHello(payload)
	if err != nil {
		p.log.Warn("push bad HELLO json", "remote", remote, "error", err)
		return "", "", false, false
	}
	obs, authed := p.auth.Authenticate(h.Token, h.Station, h.Feed)
	if !authed {
		metrics.PushAuthFailuresTotal.Inc()
		metrics.PushAuthFailuresByReasonTotal.WithLabelValues("token_or_grant").Inc()
		_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "unauthorized"}))
		p.log.Warn("push auth rejected", "remote", remote, "station", h.Station, "feed", h.Feed)
		return "", "", false, false
	}
	if p.tlsConfig.ClientAuth != tls.NoClientCert {
		tlsConn, isTLS := conn.(*tls.Conn)
		if !isTLS {
			metrics.PushAuthFailuresTotal.Inc()
			metrics.PushAuthFailuresByReasonTotal.WithLabelValues("certificate_identity").Inc()
			return "", "", false, false
		}
		state := tlsConn.ConnectionState()
		if err := matchPeerIdentity(state.PeerCertificates, obs); err != nil {
			metrics.PushAuthFailuresTotal.Inc()
			metrics.PushAuthFailuresByReasonTotal.WithLabelValues("certificate_identity").Inc()
			_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "certificate identity mismatch"}))
			p.log.Warn("push certificate identity rejected", "remote", remote, "observer", obs, "error", err)
			return "", "", false, false
		}
	}
	if scannerFor(h.Feed) == nil {
		_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "unsupported feed"}))
		return "", "", false, false
	}
	if err := w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{
		OK: true, AckIntervalMS: int(p.ackInterval / time.Millisecond), Zstd: h.Zstd,
	})); err != nil {
		return "", "", false, false
	}
	return obs, h.Feed, h.Zstd, true
}

// matchPeerIdentity binds an mTLS-authenticated leaf to the token's canonical
// observer. GNF1 uses exactly one DNS SAN as the identity field. Legacy CN-only
// certificates are deliberately rejected; enabling them requires an explicit
// future migration setting rather than an implicit fallback.
func matchPeerIdentity(chain []*x509.Certificate, observer string) error {
	if len(chain) == 0 {
		return errors.New("verified client certificate missing")
	}
	if !asciiObserverID(observer) {
		return errors.New("canonical observer id is not valid ASCII")
	}
	names := chain[0].DNSNames
	if len(names) != 1 {
		return fmt.Errorf("client certificate must contain exactly one DNS SAN, got %d", len(names))
	}
	if !asciiObserverID(names[0]) || names[0] != observer {
		return fmt.Errorf("DNS SAN does not exactly match canonical observer")
	}
	return nil
}

func asciiObserverID(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(c >= '0' && c <= '9') || c == '.' || c == '-' {
			continue
		}
		return false
	}
	return true
}

// stream reads DATA/PING frames, forwards decoded records to the decode stage, and
// acks the highest sequence received on the configured cadence. The feeder assigns
// monotonically increasing global sequences and, on every reconnect, replays all
// frames past the last ack it received (docs/DESIGN.md §2). A fresh connection
// therefore resumes at an arbitrary sequence, not 1, so the collector must ack the
// highest seq seen this connection — acking "contiguous from zero" would never
// advance past a replay and the feeder's spool would grow without bound. Frames are
// forwarded to decode unconditionally (nav frames are idempotent, so a replayed
// duplicate is harmless); the sequence governs only spool pruning.
func (p *PushServer) stream(ctx context.Context, frames io.Reader, w *connWriter, observer, feed string) {
	var (
		mu      sync.Mutex
		highest uint64 // highest sequence received this connection
		acked   uint64
	)
	ackTicker := time.NewTicker(p.ackInterval)
	defer ackTicker.Stop()
	ackDone := make(chan struct{})
	quit := make(chan struct{})
	go func() {
		defer close(ackDone)
		for {
			select {
			case <-quit:
				return
			case <-ackTicker.C:
				mu.Lock()
				last, prev := highest, acked
				mu.Unlock()
				if last == prev {
					continue
				}
				if err := w.write(wire.Ack, wire.EncodeAck(last)); err != nil {
					// an ack-write failure means this connection's write side is
					// dead (e.g. a broken TLS session with the read side still delivering).
					// Close it so the read loop's blocked ReadFrame errors out too --
					// otherwise frames are consumed forever, never acked, and the feeder's
					// spool fills and eventually drops them permanently while this
					// connection looks alive.
					_ = w.c.Close()
					return
				}
				mu.Lock()
				acked = last
				mu.Unlock()
			}
		}
	}()

	for {
		// The idle-timeout deadline is refreshed by idleConn on every underlying read, so a
		// stalled feeder (or a stalled zstd stream) still trips it here.
		ft, payload, err := wire.ReadFrame(frames)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				p.log.Info("push feeder disconnected", "observer", observer, "error", err)
			} else {
				p.log.Warn("push feeder idle timeout", "observer", observer)
			}
			close(quit)
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
			f := recordToFrame(rec, feed, observer)
			if f == nil {
				metrics.PushErrorsTotal.WithLabelValues(observer, "bad_telemetry").Inc()
				// The body is malformed; a retransmit cannot fix it, so this sequence is
				// acked (matches the pre-existing behaviour for this branch, regression fix).
				mu.Lock()
				if seq > highest {
					highest = seq
				}
				mu.Unlock()
				continue
			}
			f.Seq, f.HasSeq = seq, true // historian dedup key : this connection may be a replay
			if f.RF == nil {            // FramesTotal counts nav-frame throughput per constellation, not telemetry
				metrics.FramesTotal.WithLabelValues(observer, fmt.Sprint(int(f.GnssID))).Inc()
			}
			select {
			case p.out <- f:
				mu.Lock()
				if seq > highest {
					highest = seq
				}
				mu.Unlock()
			case <-ctx.Done(): // daemon teardown; frame is unacked, feeder replays on reconnect
				close(quit)
				<-ackDone
				return
			}
		case wire.Ping:
			// same rationale as the ack-writer above -- a dead write side must
			// tear down the connection, not just silently drop the PONG.
			if err := w.write(wire.Pong, nil); err != nil {
				_ = w.c.Close()
			}
		default:
			metrics.PushErrorsTotal.WithLabelValues(observer, "unexpected_frame").Inc()
		}
	}
}

// recordToFrame reconstructs a RawFrame from a GNF1 raw record. A telemetry record
// (frame_type < 0x10, docs/CONSTELLATIONS.md §6.2) decodes to an RF sample; word-oriented
// feeds (ubx/sbf nav frames) carry the broadcast words big-endian in Raw; rtcm carries the
// message bytes. The reception time falls back to now when the feeder did not stamp it. A
// malformed telemetry body returns nil (the caller counts and drops it).
// recvTimestampSlack bounds how far a feeder-supplied reception timestamp may
// diverge from collector wall-clock before it's rejected as implausible :
// a receiver/feeder clock can legitimately drift by a little, but a stamp minutes
// away in either direction is not a reception time, it's a bug or a malfunctioning
// clock, and must not flow into the historian's time-based partitioning/integrity
// math uncorrected.
const recvTimestampSlack = 5 * time.Minute

func recordToFrame(rec wire.RawRecord, feed, source string) *RawFrame {
	recv := time.Now()
	if rec.RecvUnixNs > 0 {
		if stamped := time.Unix(0, rec.RecvUnixNs); withinSlack(stamped, recv, recvTimestampSlack) {
			recv = stamped
		} else {
			metrics.PushErrorsTotal.WithLabelValues(source, "recv_ts_implausible").Inc()
		}
	}
	if IsTelemetryType(int(rec.FrameType)) {
		return telemetryToFrame(rec, source, recv)
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
		// the GNF1 frame_type byte is 8 bits and cannot carry an RTCM message
		// number (12 bits, e.g. 1019/1020); derive it from the payload the same way
		// scanRTCM does (rtcm.go) rather than trusting the feeder-supplied frame_type.
		if len(rec.Raw) >= 2 {
			f.MsgType = int(rec.Raw[0])<<4 | int(rec.Raw[1])>>4
		}
	} else {
		f.Words = bytesToWords(rec.Raw)
	}
	return f
}

// telemetryToFrame decodes a GNF1 telemetry record's body into a station-scoped RF sample
// (docs/CONSTELLATIONS.md §6.2), the fleet-push counterpart of the dial-mode MON-RF/NAV-SAT
// parsers. The sample is tagged with the authenticated observer id — the same station key
// applyRF uses — so the PNT-defense detector sees push and dial stations alike. An
// unrecognised telemetry type or malformed body returns nil. RF frames carry no nav words
// and are never written to the raw-nav historian (main.decodeLoop skips them).
func telemetryToFrame(rec wire.RawRecord, source string, recv time.Time) *RawFrame {
	rf := &RawRF{}
	switch int(rec.FrameType) {
	case TelemJammingStats:
		bands, err := decodeJammingStats(rec.Raw)
		if err != nil {
			return nil
		}
		rf.Bands = bands
	case TelemReceptionData:
		sats, err := decodeReceptionData(rec.Raw)
		if err != nil {
			return nil
		}
		rf.Sats = sats
	default:
		return nil // a telemetry type we don't transport yet
	}
	return &RawFrame{Recv: recv, Source: source, RF: rf}
}

// withinSlack reports whether stamped is within slack of now in either direction.
func withinSlack(stamped, now time.Time, slack time.Duration) bool {
	d := stamped.Sub(now)
	if d < 0 {
		d = -d
	}
	return d <= slack
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
