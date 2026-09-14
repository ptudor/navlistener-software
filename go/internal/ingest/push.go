package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
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
	"github.com/ptudor/navlistener/internal/identity"
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

// Authenticator validates an edge feeder's HELLO. It returns the complete
// server-resolved observer context (identity, ownership, enrollment and
// publication policy), not just the source tag. Implementations must be safe for
// concurrent use. Ordinary DATA can never override this result.
type Authenticator interface {
	Authenticate(context.Context, string, string, string) (identity.ObserverContext, bool)
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

func (a *configAuth) Authenticate(_ context.Context, token, station, feed string) (identity.ObserverContext, bool) {
	sum := sha256.Sum256([]byte(token))
	o, ok := a.byHash[hex.EncodeToString(sum[:])]
	if !ok {
		return identity.ObserverContext{}, false
	}
	// The token is the identity; station is a misconfiguration guard — a
	// feeder pointed at the wrong station id (valid token, wrong presented name)
	// is rejected rather than silently accepted under the token's canonical
	// identity. handshake()'s caller already logs the rejected station/feed.
	if station != o.Station {
		return identity.ObserverContext{}, false
	}
	for _, f := range o.Feeds {
		if f == feed {
			ctx := o.ObserverContext
			if ctx.ObserverID == "" { // tests/programmatic callers may bypass config.finalize
				ctx = identity.NewPrivateContext(o.Station, identity.CredentialToken)
			}
			// Preserve the complete server-side receipt evidence even for
			// programmatic callers that bypass config.finalize.
			ctx.FeedGrants = append([]string(nil), o.Feeds...)
			if len(ctx.DeclaredCapabilities) == 0 {
				for _, capability := range o.CapDecl {
					ctx.DeclaredCapabilities = append(ctx.DeclaredCapabilities, identity.Signal{
						GnssID: capability.Gnss, SigID: capability.Sig,
					})
				}
			}
			ctx, err := ctx.Normalize()
			if err != nil || ctx.ObserverID != o.Station {
				return identity.ObserverContext{}, false
			}
			return ctx, true
		}
	}
	return identity.ObserverContext{}, false // authenticated, but not granted this feed
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

	// durable, when non-nil, switches ACK to the regression fix durability
	// watermark (see DurableTracker). nil = live-only mode (no historian):
	// ACK on receipt, the pre-regression fix semantics. Set via SetDurableTracker
	// before Run; main wires it exactly when the historian is enabled.
	durable *DurableTracker

	// reauthorizeEvery bounds how long an active feeder can retain authority
	// after a credential/enrollment/policy revocation. The provider's cache TTL
	// is the other half of the documented bound.
	reauthorizeEvery time.Duration

	// collectorInstanceID is this deployment's stable realm. A control-plane
	// row for another instance is rejected even if its credential otherwise
	// verifies, preventing one database/view mistake from crossing CA realms.
	collectorInstanceID string

	authorizationMu sync.Mutex
	policies        map[string]*observerPolicy
}

// SetDurableTracker installs the regression fix durability watermark source. Must be
// called before Run/Serve (connections read the field without a lock).
func (p *PushServer) SetDurableTracker(t *DurableTracker) { p.durable = t }

// SetReauthorizationInterval changes the active-session authorization cadence.
// Config validation requires a positive bounded value.
func (p *PushServer) SetReauthorizationInterval(every time.Duration) {
	if every > 0 {
		p.reauthorizeEvery = every
	}
}

// SetCollectorInstance binds every admitted observer to this collector realm.
// Configuration validation supplies a normalized non-empty value.
func (p *PushServer) SetCollectorInstance(instanceID string) {
	if identity.ValidScopeID(instanceID) {
		p.collectorInstanceID = instanceID
	}
}

// NewPushServer builds the listener from config. It loads the server certificate and,
// if a client CA is configured, requires and verifies client certificates (mTLS).
func NewPushServer(cfg config.Push, out chan<- *RawFrame, auth Authenticator, log *slog.Logger) (*PushServer, error) {
	cert, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey)
	if err != nil {
		return nil, fmt.Errorf("push tls keypair: %w", err)
	}
	if len(cert.Certificate) == 0 {
		return nil, fmt.Errorf("push tls keypair: certificate chain is empty")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("push tls leaf certificate: %w", err)
	}
	cert.Leaf = leaf
	observePushServerCertificate(cfg.TLSCert, leaf, time.Now(), log)
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

func observePushServerCertificate(path string, leaf *x509.Certificate, now time.Time, log *slog.Logger) {
	metrics.PushServerCertNotAfterSeconds.Set(float64(leaf.NotAfter.Unix()))
	const certWarnHorizon = 30 * 24 * time.Hour
	if now.Before(leaf.NotBefore) {
		log.Warn("push server certificate is not yet valid", "path", path, "not_before", leaf.NotBefore)
	} else if !now.Before(leaf.NotAfter) {
		log.Warn("push server certificate has expired", "path", path, "not_after", leaf.NotAfter)
	} else if leaf.NotAfter.Sub(now) <= certWarnHorizon {
		log.Warn("push server certificate expires soon", "path", path, "not_after", leaf.NotAfter)
	}
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
	return &PushServer{addr: addr, tlsConfig: tc, auth: auth, out: out, ackInterval: ack,
		reauthorizeEvery: 30 * time.Second, collectorInstanceID: identity.LocalCollectorInstance,
		policies: make(map[string]*observerPolicy),
		log:      log, conns: make(chan struct{}, maxConns)}
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

	// handlers exit when their context is cancelled (the stop goroutine in handle()
	// closes the conn so a blocked read returns), but the PARENT ctx is cancelled only AFTER
	// this function returns — main wires the returned terminal error into reportFatal ->
	// cancel(). On a non-temporary Accept error with the parent ctx still alive, a bare
	// wg.Wait() would therefore block forever on fleet feeders that stream + PING indefinitely:
	// Serve never returns, the fatal is never reported, and /healthz stays OK (the half-alive
	// state the terminal-error path exists to prevent, regression fix). Give the handlers a
	// child context this function can cancel itself, so wg.Wait() completes on that path.
	hctx, hcancel := context.WithCancel(ctx)
	defer hcancel()
	reconciled := make(chan struct{})
	go func() { defer close(reconciled); p.runReconciliation(hctx) }()
	defer func() { hcancel(); <-reconciled }()

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
				hcancel() // tear down in-flight handlers so wg.Wait() can complete
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
			p.handle(hctx, conn)
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

	// recover so a future parser edge case in the push path (wire.DecodeData,
	// decodeJammingStats, decodeReceptionData, bytesToWords, rtcm) reconnects this one
	// authenticated feeder instead of crashing the whole collector — symmetric with the dial
	// path's runScanner recover. decodeLoop's per-frame recover only covers the far side.
	var observer string // set after handshake; labels the panic metric below
	defer func() {
		if r := recover(); r != nil {
			metrics.PushErrorsTotal.WithLabelValues(observer, "panic").Inc()
			p.log.Error("push handler panicked; closing connection", "observer", observer, "remote", remote, "panic", r)
		}
	}()

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
	sessionCtx, sessionCancel := context.WithCancel(ctx)
	defer sessionCancel()
	authorized, ok := p.handshake(sessionCtx, conn, w, remote)
	if !ok {
		return
	}
	observerContext, feed, session, useZstd := authorized.observer, authorized.feed, authorized.session, authorized.useZstd
	observer = observerContext.ObserverID
	metrics.PushConnectsTotal.WithLabelValues(observer).Inc()
	metrics.PushObserversUp.WithLabelValues(observer).Inc()
	defer metrics.PushObserversUp.WithLabelValues(observer).Dec()
	p.log.Info("push feeder authenticated", "observer", observer, "feed", feed, "remote", remote, "session", session, "zstd", useZstd)

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

	// Admit this session under the observer's policy generation : a
	// context that differs from the observer's current policy — a transfer or
	// revocation that happened while the feeder was disconnected — transitions
	// the policy, cancels every older session and enqueues the ordered reset
	// marker before a single newly-authorized DATA record can enter live state.
	// The credential digest is retained (never the token) so a later
	// withdrawal can be reconciled without a reconnect.
	digest := sha256.Sum256([]byte(authorized.token))
	admission, refused := p.admit(ctx, observerContext, authorized.policyGeneration, func() { sessionCancel(); _ = conn.Close() }, policyCredential{digest: hex.EncodeToString(digest[:]), feed: feed})
	if admission == nil {
		// WELCOME{ok:true} has already been written, so the feeder sees a
		// successful handshake followed by a close: say why on this side.
		metrics.PushAdmissionRefusedTotal.WithLabelValues(refused).Inc()
		p.log.Warn("push session refused by policy admission after WELCOME", "observer", observer, "feed", feed, "reason", refused)
		return
	}
	defer admission.release()
	sessionCtx = context.WithValue(sessionCtx, admissionContextKey{}, admission)
	go p.watchAuthorization(sessionCtx, ctx, conn, authorized.token, observer, feed, observerContext, admission)
	p.stream(sessionCtx, frames, w, observerContext, feed, session)
	sessionCancel()
}

// maxConsecutiveUnforwarded bounds how many frames in a row a connection may
// produce without a single record reaching the decode stage. The DATA
// path self-throttles (p.out <- f blocks, TCP backpressure does the rest), but
// every other outcome — unexpected frame type, short_record, bad_telemetry —
// looped straight back to wire.ReadFrame with only a metric increment. Behind
// zstd that is an amplifier: a few KB compressing a long run of zeros parses as
// an endless sequence of type=0/len=0 frames (ReadFrameMax returns
// FrameType(0), empty payload for five zero bytes), each landing in the
// default: branch — one collector core pinned per connection, x MaxConns, with
// idleConn never tripping because the compressed reads keep succeeding. Past
// this bound the connection is torn down (the regression fix misbehaving-feeder
// pattern; no wire/ack contract change — unacked frames replay on reconnect).
// 256 is far above anything a correct feeder produces consecutively (feeders
// send DATA + PING only; a corrupt spool record among flowing DATA resets the
// count at every delivered frame) while ending a hostile spin within
// microseconds. PING neither increments nor resets: it is valid protocol, but
// resetting on it would let junk+periodic-ping streams evade the bound.
const maxConsecutiveUnforwarded = 256

// helloMaxLen caps the pre-auth HELLO frame length far below wire.MaxFrameLen
// : a real HELLO is ~150 bytes and navfeeder.c bounds its own to exactly
// this value (its HELLO_CAP, regression fix — it previously stopped at 1024 while
// silently truncating any token past 512 bytes), but ReadFrame's normal 1 MiB cap would let any unauthenticated
// connection pin up to 1 MiB before a single byte is verified -- a
// per-connection amplifier for the regression fix pre-auth flood. This is a
// reception-side policy, not a wire change: DATA-phase reads (in stream, after
// authentication) keep the full MaxFrameLen.
const helloMaxLen = 4096

type authorizedHello struct {
	policyGeneration uint64
	observer         identity.ObserverContext
	feed             string
	session          string
	token            string
	useZstd          bool
}

// handshake reads and authenticates the HELLO, replying WELCOME. The bearer
// token remains session-local only so active authorization can be rechecked.
func (p *PushServer) handshake(ctx context.Context, conn net.Conn, w *connWriter, remote string) (authorizedHello, bool) {
	ft, payload, err := wire.ReadFrameMax(conn, helloMaxLen)
	if err != nil || ft != wire.Hello {
		p.log.Warn("push expected HELLO", "remote", remote, "frame", ft, "error", err)
		return authorizedHello{}, false
	}
	h, err := wire.ParseHello(payload)
	if err != nil {
		p.log.Warn("push bad HELLO json", "remote", remote, "error", err)
		return authorizedHello{}, false
	}
	policyGeneration := p.policyGeneration(h.Station)
	observerContext, authed, authErr := p.authorize(ctx, conn, h.Token, h.Station, h.Feed)
	if !authed {
		metrics.PushAuthFailuresTotal.Inc()
		reason := "token_or_grant"
		if authErr != nil {
			reason = "certificate_identity"
		}
		metrics.PushAuthFailuresByReasonTotal.WithLabelValues(reason).Inc()
		_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "unauthorized"}))
		p.log.Warn("push auth rejected", "remote", remote, "station", h.Station, "feed", h.Feed, "error", authErr)
		return authorizedHello{}, false
	}
	obs := observerContext.ObserverID
	// regression fix/regression fix defense-in-depth: push has an explicit wire-contract allow-list,
	// independent of what a future non-config Authenticator may accidentally grant.
	// Dial-only sbf/ntrip must never reach recordToFrame's ubx/rtcm branches.
	if h.Feed != "ubx" && h.Feed != "rtcm" {
		_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "unsupported feed"}))
		return authorizedHello{}, false
	}
	// the session is the boot-identity half of the replay-dedup key
	// (observer, session, seq) — see wire.HelloMsg.Session for the full contract.
	// Checked after token auth (an unauthenticated probe learns nothing new) and
	// rejected like the feed allow-list above: a feeder that cannot mint a
	// session would silently re-enter the seq-reuse loss regime this field ends.
	if !wire.ValidSession(h.Session) {
		_ = w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{OK: false, Error: "missing or invalid session"}))
		p.log.Warn("push HELLO missing or invalid session", "remote", remote, "observer", obs)
		return authorizedHello{}, false
	}
	if err := w.write(wire.Welcome, mustWelcome(wire.WelcomeMsg{
		OK: true, AckIntervalMS: int(p.ackInterval / time.Millisecond), Zstd: h.Zstd,
	})); err != nil {
		return authorizedHello{}, false
	}
	return authorizedHello{observer: observerContext, feed: h.Feed, session: h.Session, token: h.Token, useZstd: h.Zstd, policyGeneration: policyGeneration}, true
}

// authorize binds a server-side grant to the proof actually presented on this
// connection. A control-plane hardware label is rejected unless this is mTLS,
// the active leaf fingerprint matches, and manufacturer attestation is present.
func (p *PushServer) authorize(ctx context.Context, conn net.Conn, token, station, feed string) (identity.ObserverContext, bool, error) {
	resolved, ok := p.auth.Authenticate(ctx, token, station, feed)
	if !ok {
		return identity.ObserverContext{}, false, nil
	}
	if resolved.CollectorInstanceID != p.collectorInstanceID {
		return identity.ObserverContext{}, false, errors.New("observer enrollment belongs to a different collector instance")
	}
	feedGranted := false
	for _, granted := range resolved.FeedGrants {
		if granted == feed {
			feedGranted = true
			break
		}
	}
	if !feedGranted {
		return identity.ObserverContext{}, false, errors.New("resolved context does not retain the presented feed grant")
	}
	if p.tlsConfig.ClientAuth == tls.NoClientCert {
		if resolved.CredentialTier == identity.CredentialSoftwareMTLS ||
			resolved.CredentialTier == identity.CredentialHardwareMTLS || resolved.CredentialFingerprint != "" {
			return identity.ObserverContext{}, false, errors.New("control-plane credential requires an mTLS client certificate")
		}
		resolved.CredentialTier = identity.CredentialToken
		resolved.CredentialFingerprint = ""
		resolved, err := resolved.Normalize()
		return resolved, err == nil, err
	}

	tlsConn, isTLS := conn.(*tls.Conn)
	if !isTLS {
		return identity.ObserverContext{}, false, errors.New("verified TLS connection missing")
	}
	state := tlsConn.ConnectionState()
	if err := matchPeerIdentity(state.PeerCertificates, resolved.ObserverID); err != nil {
		return identity.ObserverContext{}, false, err
	}
	leaf := state.PeerCertificates[0]
	fingerprintBytes := sha256.Sum256(leaf.Raw)
	actualFingerprint := hex.EncodeToString(fingerprintBytes[:])
	expectedFingerprint := resolved.CredentialFingerprint
	if (resolved.CredentialTier == identity.CredentialSoftwareMTLS ||
		resolved.CredentialTier == identity.CredentialHardwareMTLS) && expectedFingerprint == "" {
		return identity.ObserverContext{}, false, errors.New("mTLS control-plane credential has no active leaf fingerprint")
	}
	if expectedFingerprint != "" && subtle.ConstantTimeCompare([]byte(expectedFingerprint), []byte(actualFingerprint)) != 1 {
		return identity.ObserverContext{}, false, errors.New("client certificate fingerprint does not match active credential")
	}
	resolved.CredentialFingerprint = actualFingerprint
	if resolved.CredentialTier == identity.CredentialHardwareMTLS {
		if resolved.AttestationTier == identity.AttestationNone {
			return identity.ObserverContext{}, false, errors.New("hardware mTLS credential lacks verified manufacturer attestation")
		}
	} else {
		// A config/bootstrap token plus a CA-verified certificate proves software
		// mTLS, but can never promote itself to hardware mTLS.
		resolved.CredentialTier = identity.CredentialSoftwareMTLS
	}
	resolved, err := resolved.Normalize()
	return resolved, err == nil, err
}

func (p *PushServer) enqueueScopeRevocation(ctx context.Context, previous identity.ObserverContext) bool {
	marker := &RawFrame{Source: previous.ObserverID, ScopeRevocation: &ScopeRevocation{
		Previous: previous, ChangedAt: time.Now(),
	}}
	select {
	case p.out <- marker:
		return true
	case <-ctx.Done():
		return false
	}
}

func (p *PushServer) watchAuthorization(ctx, ingestCtx context.Context, conn net.Conn, token, station, feed string, initial identity.ObserverContext, admission *Admission) {
	if p.reauthorizeEvery <= 0 {
		return
	}
	ticker := time.NewTicker(p.reauthorizeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			current, ok, err := p.authorize(checkCtx, conn, token, station, feed)
			cancel()
			if !ok || !initial.AuthorizationEqual(current) {
				p.log.Warn("push feeder authorization changed; closing active session",
					"observer", initial.ObserverID, "authorized", ok, "error", err)
				if !ok {
					current = identity.ObserverContext{}
				}
				p.changeAdmission(ingestCtx, admission, current)
				_ = conn.Close()
				return
			}
		}
	}
}

// matchPeerIdentity binds an mTLS-authenticated leaf to the token's canonical
// observer. GNF1 uses exactly one DNS SAN as the identity field. Legacy CN-only
// certificates are deliberately rejected; enabling them requires an explicit
// future migration setting rather than an implicit fallback.
func matchPeerIdentity(chain []*x509.Certificate, observer string) error {
	if len(chain) == 0 {
		return errors.New("verified client certificate missing")
	}
	if !config.ValidObserverID(observer) {
		return errors.New("canonical observer id is not a certificate-bindable name")
	}
	names := chain[0].DNSNames
	if len(names) != 1 {
		return fmt.Errorf("client certificate must contain exactly one DNS SAN, got %d", len(names))
	}
	if !config.ValidObserverID(names[0]) || names[0] != observer {
		return fmt.Errorf("DNS SAN does not exactly match canonical observer")
	}
	return nil
}

// stream reads DATA/PING frames, forwards decoded records to the decode stage, and
// acks the DURABLE watermark on the configured cadence (the highest
// durably resolved sequence per the DurableTracker; receipt-based only in the
// documented live-only mode). The feeder assigns monotonically increasing global
// sequences and, on every reconnect, replays all frames past the last ack it
// received (docs/DESIGN.md §2). A fresh connection therefore resumes at an
// arbitrary sequence, not 1, so the watermark tracks sequences seen this
// connection — acking "contiguous from zero" would never advance past a replay and
// the feeder's spool would grow without bound. Frames are forwarded to decode
// unconditionally (nav frames are idempotent, so a replayed duplicate is
// harmless); the sequence governs only spool pruning.
func (p *PushServer) stream(ctx context.Context, frames io.Reader, w *connWriter, observerContext identity.ObserverContext, feed, session string) {
	observer := observerContext.ObserverID
	var (
		mu      sync.Mutex
		highest uint64 // highest sequence received this connection
		acked   uint64
	)
	ackTicker := time.NewTicker(p.ackInterval)
	defer ackTicker.Stop()
	ackDone := make(chan struct{})
	quit := make(chan struct{})
	defer func() { close(quit); <-ackDone }()
	go func() {
		defer close(ackDone)
		for {
			select {
			case <-quit:
				return
			case <-ackTicker.C:
				var last uint64
				if p.durable != nil {
					// ack the durability watermark — the feeder may
					// prune only what the historian has durably resolved. In
					// live-only mode (nil tracker, no historian) ack on receipt,
					// the documented pre-regression fix semantics.
					last = p.durable.Watermark(observer, session)
				} else {
					mu.Lock()
					last = highest
					mu.Unlock()
				}
				mu.Lock()
				prev := acked
				mu.Unlock()
				// Monotone per connection: a reconnect's replayed low sequences can
				// transiently regress the watermark (DurableTracker.Watermark);
				// a regressed ack must never reach the wire.
				if last <= prev {
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

	// unforwarded counts consecutive frames that produced no p.out
	// delivery; see maxConsecutiveUnforwarded for why it exists and its bound.
	unforwarded := 0
	// dropPermanentlyMalformed handles a record a retransmit can never repair.
	// It is counted, registered as never-persistable so it advances the durable
	// watermark instead of holding it — otherwise one bad record would wedge
	// every valid later spool record behind it forever — and dropped without
	// reaching live state or the historian. Returns false when the durability
	// budget is full and the connection must close so the feeder replays.
	dropPermanentlyMalformed := func(seq uint64, reason string) bool {
		metrics.PushErrorsTotal.WithLabelValues(observer, reason).Inc()
		unforwarded++
		if p.durable != nil {
			if !p.durable.Received(observer, session, seq, false) {
				p.log.Warn("durability tracking budget full; closing for replay", "observer", observer)
				return false
			}
		}
		mu.Lock()
		if seq > highest {
			highest = seq
		}
		mu.Unlock()
		return true
	}
	for {
		if ctx.Err() != nil {
			return
		}
		// The idle-timeout deadline is refreshed by idleConn on every underlying read, so a
		// stalled feeder (or a stalled zstd stream) still trips it here.
		ft, payload, err := wire.ReadFrame(frames)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				p.log.Info("push feeder disconnected", "observer", observer, "error", err)
			} else {
				p.log.Warn("push feeder idle timeout", "observer", observer)
			}
			return
		}
		switch ft {
		case wire.Data:
			seq, rec, err := wire.DecodeData(payload)
			if err != nil {
				metrics.PushErrorsTotal.WithLabelValues(observer, "short_record").Inc()
				unforwarded++
			} else if seq == 0 {
				// sequence 0 is outside the assigned sequence space.
				// Both reference feeders emit ++seq, so the first record of a session
				// is 1 (feeder/navfeeder.c disk_put, esp32 spool.c spool_put). A zero
				// can never advance the durable watermark — DurableTracker.Received
				// ignores it rather than underflow Watermark's pending[0]-1 — so
				// before this gate the collector applied and persisted the record on
				// every reconnect while its spool copy could never be retired.
				// This is a protocol error, not a malformed body: a peer emitting it
				// is not speaking GNF1, so the connection is closed rather than acked
				// past. Nothing is registered, forwarded, or acked for it.
				metrics.PushErrorsTotal.WithLabelValues(observer, "seq_zero").Inc()
				p.log.Warn("push feeder sent DATA sequence zero; closing connection",
					"observer", observer, "session", session)
				return
			} else if !IsTelemetryType(int(rec.FrameType)) && !rec.GnssID.Valid() {
				// the GNF1 record's gnssId byte is outside every nav
				// CRC — gate it on the documented 0..7-minus-IMES domain at the
				// boundary (telemetry records don't carry a meaningful gnssId
				// and are exempt), before the frame can mint a
				// FramesTotal{gnssid=<raw>} series or persist a nav_frames row
				// outside the schema's domain. A corrupt id is not fixable by
				// retransmit, so the sequence is acked like bad_telemetry.
				if !dropPermanentlyMalformed(seq, "gnssid_range") {
					return
				}
			} else if !wordRecordWellFormed(rec, feed) {
				// a word-oriented record whose body is empty or not a
				// multiple of four bytes is malformed on the wire. bytesToWords used
				// to silently discard the 1–3 byte remainder, so what the historian
				// stored as immutable raw evidence was not what the authenticated
				// feeder sent — and the sequence was still acked, making the lost
				// suffix unrecoverable from the edge spool. Alignment is a wire
				// invariant no retransmit can repair, so it takes the acked
				// permanent-malformation policy (never the seq-0 close: a single bad
				// record must not wedge the valid spool records behind it), under its
				// own label, distinct from a malformed telemetry body.
				if !dropPermanentlyMalformed(seq, "word_alignment") {
					return
				}
			} else if f := recordToFrame(rec, feed, observer); f == nil {
				// The body is malformed; a retransmit cannot fix it, so this sequence is
				// acked (matches the pre-existing behaviour for this branch, regression fix).
				if !dropPermanentlyMalformed(seq, "bad_telemetry") {
					return
				}
			} else {
				f.Observer = observerContext // trusted handshake result; never record metadata
				f.Admission, _ = ctx.Value(admissionContextKey{}).(*Admission)
				f.Seq, f.HasSeq = seq, true        // historian dedup key : this connection may be a replay
				f.Session = session                // boot-identity half of the dedup key
				if f.RF == nil && f.Words != nil { // byte frames use CapturedOnlyTotal, not gnssid=0
					metrics.FramesTotal.WithLabelValues(observer, fmt.Sprint(int(f.GnssID))).Inc()
				}
				// register receipt BEFORE the decode handoff, so the
				// store's resolution can never race its own arrival. Telemetry
				// (RF/observables) never reaches the historian (decodeLoop skips
				// it), so it advances the watermark without ever holding it.
				if p.durable != nil {
					if !p.durable.Received(observer, session, seq, f.RF == nil && f.Obs == nil) {
						p.log.Warn("durability tracking budget full; closing for replay", "observer", observer)
						return
					}
				}
				select {
				case p.out <- f:
					unforwarded = 0 // a delivered record proves a live, well-formed stream
					mu.Lock()
					if seq > highest {
						highest = seq
					}
					mu.Unlock()
				case <-ctx.Done(): // daemon teardown; frame is unacked, feeder replays on reconnect
					return
				}
			}
		case wire.Ping:
			// same rationale as the ack-writer above -- a dead write side must
			// tear down the connection, not just silently drop the PONG.
			if err := w.write(wire.Pong, nil); err != nil {
				_ = w.c.Close()
			}
		default:
			metrics.PushErrorsTotal.WithLabelValues(observer, "unexpected_frame").Inc()
			unforwarded++
		}
		if unforwarded >= maxConsecutiveUnforwarded {
			metrics.PushErrorsTotal.WithLabelValues(observer, "unforwarded_flood").Inc()
			p.log.Warn("push feeder sent too many consecutive unusable frames; closing connection",
				"observer", observer, "limit", maxConsecutiveUnforwarded)
			return
		}
	}
}

// recordToFrame reconstructs a RawFrame from a GNF1 raw record. A telemetry record
// (frame_type < 0x10, docs/CONSTELLATIONS.md §6.2) decodes to an RF sample; word-oriented
// feeds (ubx/sbf nav frames) carry the broadcast words big-endian in Raw; rtcm carries the
// message bytes. The reception time falls back to now when the feeder did not stamp it. A
// malformed telemetry body returns nil (the caller counts and drops it).
// Future stamps remain tightly bounded as clock errors. Past stamps have a much
// wider horizon because the edge feeder deliberately replays its durable spool
// after an outage; preserving that original reception time is the forensic
// contract. Seven days comfortably covers the 256 MiB spool's hours-to-days
// design while still rejecting absurd/stale clock values.
const (
	recvTimestampSlack = 5 * time.Minute
	recvReplayHorizon  = 7 * 24 * time.Hour
)

func recordToFrame(rec wire.RawRecord, feed, source string) *RawFrame {
	// local is the collector's own clock, monotonic : Recv below may be
	// replaced by the feeder's wall-clock stamp (the forensic reception time),
	// but every staleness/expiry/liveness age in live state must be an elapsed
	// time on ONE clock — time.Unix strips the monotonic reading, so ages built
	// on the feeder stamp silently become cross-machine wall-clock differences.
	local := time.Now()
	recv := local
	switch {
	case rec.RecvUnixNs > 0:
		if stamped := time.Unix(0, rec.RecvUnixNs); receiveTimestampPlausible(stamped, local) {
			recv = stamped
		} else {
			metrics.PushErrorsTotal.WithLabelValues(source, "recv_ts_implausible").Inc()
		}
	case rec.RecvUnixNs < 0:
		// a negative stamp (byte-swapped/sign-flipped clock) is as implausible as a
		// far-future one — count it under the same metric so a misbehaving feeder clock
		// stays visible. == 0 remains the deliberate "unstamped" sentinel (uncounted; falls
		// back to now()).
		metrics.PushErrorsTotal.WithLabelValues(source, "recv_ts_implausible").Inc()
	}
	if IsTelemetryType(int(rec.FrameType)) {
		return telemetryToFrame(rec, source, recv, local)
	}
	f := &RawFrame{
		Recv:      recv,
		RecvLocal: local,
		Source:    source,
		GnssID:    rec.GnssID,
		SvID:      int(rec.SvID),
		SigID:     int(rec.SigID),
		FreqID:    int(rec.FreqID),
		MsgType:   int(rec.FrameType),
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
func telemetryToFrame(rec wire.RawRecord, source string, recv, local time.Time) *RawFrame {
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
	return &RawFrame{Recv: recv, RecvLocal: local, Source: source, RF: rf}
}

// receiveTimestampPlausible applies the asymmetric live-clock/replay contract:
// at most recvTimestampSlack in the future, at most recvReplayHorizon in the
// past.
//
// both bounds are compared against the signed difference
// directly, never by negating it. time.Time.Sub saturates at math.MaxInt64 /
// math.MinInt64 for differences outside a Duration's ~292-year range, and
// negating math.MinInt64 wraps straight back to math.MinInt64 — which is less
// than any positive horizon. The previous `-d <= recvReplayHorizon` form
// therefore *accepted* every stamp old enough to saturate, letting centuries-old
// received_at values into the historian, where they distort chunking, retention,
// replay windows, and every age-based query.
func receiveTimestampPlausible(stamped, now time.Time) bool {
	d := stamped.Sub(now)
	return d <= recvTimestampSlack && d >= -recvReplayHorizon
}

// wordRecordWellFormed enforces the word-feed wire invariant before conversion
// . A word-oriented record — every non-telemetry record on a feed
// other than rtcm — carries whole big-endian 32-bit broadcast words
// (docs/CONSTELLATIONS.md §6.1), so its body must be non-empty and a multiple of
// four bytes. Anything else is malformed on the wire, not merely undecodable:
// silently truncating it would store raw evidence that differs from the bytes
// the authenticated feeder actually sent. Telemetry bodies have their own
// exact-length codecs and rtcm carries byte-oriented messages, so both are
// exempt. Per-signal word-count expectations deliberately stay in the frame
// decoders — this gate asserts only what the wire format itself guarantees.
func wordRecordWellFormed(rec wire.RawRecord, feed string) bool {
	if IsTelemetryType(int(rec.FrameType)) || feed == "rtcm" {
		return true
	}
	return len(rec.Raw) > 0 && len(rec.Raw)%4 == 0
}

// bytesToWords reassembles big-endian 32-bit nav words (the inverse of
// RawFrame.RawBytes). Callers must have accepted the body through
// wordRecordWellFormed first: a trailing partial word is dropped here, which is
// only safe because no such body can reach this point.
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
