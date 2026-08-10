package ingest

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http/httputil"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
)

// dialTimeout bounds a single connection attempt; backoff bounds the retry pace.
const (
	dialTimeout    = 10 * time.Second
	backoffInitial = 1 * time.Second
	backoffMax     = 30 * time.Second
)

// dialIdleTimeout bounds silence on a dial-mode source connection : without
// it, a half-open receiver (cable pull, NAT timeout, crashed peer with no RST)
// blocks the scanner's read forever — SourceUp stays 1, no reconnect fires, a dead
// source reports healthy. Generous enough for the slowest legitimate source (RTCM
// ephemeris messages can be tens of seconds apart) so a live source doesn't thrash
// reconnects; mirrors push.go's idleConn/idleReadTimeout pattern for the dial path.
const dialIdleTimeout = 180 * time.Second

// usefulConnectionDuration bounds how long a connection must survive (absent any
// emitted frame) before it's considered "useful" enough to reset backoff :
// resetting backoff the instant a TCP connect succeeds -- before a single byte of
// data -- lets a peer that accepts and immediately closes retry at ~1 Hz forever
// (connect -> reset -> EOF -> sleep backoffInitial -> repeat, never widening). A
// source that stays connected a few seconds, or emits even one frame sooner, has
// proven itself distinct from an accept-then-close peer.
const usefulConnectionDuration = 3 * time.Second

// scanner reads a receiver stream and emits RawFrames until the stream errors.
type scanner func(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error

// scannerFor returns the stream scanner for a connector type. Load/finalize
// normally rejects unknown types first; nil remains a defensive boundary for
// programmatic callers that construct config.Source values directly.
func scannerFor(typ string) scanner {
	switch typ {
	case "ubx":
		return scanUBX
	case "sbf":
		return scanSBF
	case "rtcm", "ntrip":
		return scanRTCM // ntrip is RTCM3 carried over an NTRIP caster; same frame parser
	default:
		return nil
	}
}

// Manager runs one dial connector per configured ingest source, forwarding every
// decoded RawFrame to out. Receivers are treated as flaky: a connector reconnects
// forever with exponential backoff and never exits on its own (docs/DESIGN.md).
type Manager struct {
	sources []config.Source
	out     chan<- *RawFrame
	log     *slog.Logger
	now     func() time.Time
}

// New builds a Manager. out must be drained by the decode stage; a full channel
// applies backpressure to ingest rather than dropping silently.
func New(sources []config.Source, out chan<- *RawFrame, log *slog.Logger) *Manager {
	return &Manager{sources: sources, out: out, log: log, now: time.Now}
}

// Run starts a goroutine per enabled source and blocks until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, src := range m.sources {
		if src.Disabled {
			m.log.Info("ingest source disabled", "source", src.Name)
			continue
		}
		sc := scannerFor(src.Type)
		if sc == nil {
			m.log.Warn("ingest source type not implemented in this build; skipping",
				"source", src.Name, "type", src.Type)
			continue
		}
		wg.Add(1)
		go func(src config.Source, sc scanner) {
			defer wg.Done()
			m.runSource(ctx, src, sc)
		}(src, sc)
	}
	wg.Wait()
}

// runSource is the reconnect-forever loop for one source.
func (m *Manager) runSource(ctx context.Context, src config.Source, sc scanner) {
	backoff := backoffInitial
	for ctx.Err() == nil {
		conn, err := dialSource(ctx, src)
		if err != nil {
			metrics.SourceUp.WithLabelValues(src.Name, src.Type).Set(0)
			metrics.IngestErrorsTotal.WithLabelValues(src.Name, "dial").Inc()
			m.log.Warn("ingest dial failed; will retry", "source", src.Name, "addr", src.Addr, "error", err, "backoff", backoff)
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		// Close the connection when ctx is cancelled so every post-dial phase,
		// including the NTRIP application handshake, returns immediately.
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-stop:
			}
		}()
		// An NTRIP source needs the caster handshake (GET the mountpoint, basic auth) before
		// the RTCM3 stream flows; a failed handshake reconnects like any other drop.
		ntripChunked := false
		if src.Type == "ntrip" {
			var ntripErr error
			ntripChunked, ntripErr = ntripConnect(conn, src)
			if ntripErr != nil {
				close(stop)
				_ = conn.Close()
				metrics.SourceUp.WithLabelValues(src.Name, src.Type).Set(0)
				metrics.IngestErrorsTotal.WithLabelValues(src.Name, "ntrip_handshake").Inc()
				m.log.Warn("ntrip handshake failed; will retry", "source", src.Name, "mountpoint", src.Mountpoint, "error", ntripErr, "backoff", backoff)
				if !sleep(ctx, backoff) {
					return
				}
				backoff = nextBackoff(backoff)
				continue
			}
		}
		metrics.SourceConnectsTotal.WithLabelValues(src.Name).Inc()
		metrics.SourceUp.WithLabelValues(src.Name, src.Type).Set(1)
		m.log.Info("ingest source connected", "source", src.Name, "addr", src.Addr, "type", src.Type)

		var useful bool
		useful, err = m.runScanner(ctx, sc, conn, src, ntripChunked)
		close(stop)
		_ = conn.Close()
		metrics.SourceUp.WithLabelValues(src.Name, src.Type).Set(0)
		// backoff resets only once the connection has proven useful (ran
		// usefulConnectionDuration or emitted >= 1 frame), not on bare TCP-connect
		// success -- otherwise an accept-then-close peer is retried at ~1 Hz forever.
		if useful {
			backoff = backoffInitial
		}
		if ctx.Err() == nil {
			m.log.Warn("ingest source stream ended; reconnecting", "source", src.Name, "error", err)
			metrics.IngestErrorsTotal.WithLabelValues(src.Name, "disconnect").Inc()
			if !sleep(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
		}
	}
}

// runScanner invokes the source's scanner with a recover() so a parser bug (e.g. an
// out-of-bounds slice on malformed input from an external/untrusted source such as an
// NTRIP caster) reconnects this one source instead of crashing the whole daemon. chunked
//  wraps the idle-timeout-guarded conn in a de-chunking reader before handing it to
// the scanner, for an NTRIP v2 caster that answered with Transfer-Encoding: chunked -- the
// idleConn stays the innermost layer so each physical socket read still gets a deadline,
// with the chunk-framing decode layered on top of that, not the raw conn directly.
//
// useful reports whether the connection proved itself distinct from an
// accept-then-close peer : it ran for at least usefulConnectionDuration,
// or emitted at least one frame, before returning. runSource uses this to decide
// whether to reset backoff rather than resetting on bare TCP-connect success.
//
// a frame-silence watchdog closes the connection when bytes keep arriving
// but no frame has been emitted for src.MaxFrameSilence — the idle timeout only
// covers byte silence, so a receiver reset to NMEA-only output (or a caster
// streaming an HTML error page) otherwise sits at SourceUp=1 with FramesTotal
// frozen forever, indistinguishable from healthy. A watchdog trip surfaces as a
// distinctly-labeled error and a logged reconnect. Note the trip is deliberately
// still "useful" for regression fix backoff purposes (the connection ran for the whole
// window), so the source is re-probed promptly and recovers fast once the
// receiver is fixed.
func (m *Manager) runScanner(ctx context.Context, sc scanner, conn net.Conn, src config.Source, chunked bool) (useful bool, err error) {
	start := m.now()
	var frameCount int
	defer func() {
		if r := recover(); r != nil {
			metrics.IngestErrorsTotal.WithLabelValues(src.Name, "panic").Inc()
			m.log.Error("ingest scanner panicked; reconnecting", "source", src.Name, "panic", r)
			err = fmt.Errorf("scanner panic: %v", r)
		}
		useful = frameCount > 0 || m.now().Sub(start) >= usefulConnectionDuration
	}()
	// lastFrameNs is stamped by the emit wrapper (scanner goroutine) and read by the
	// watchdog goroutine; it starts at connect time so a source that never frames is
	// measured from the connection's start, not from zero.
	var lastFrameNs atomic.Int64
	lastFrameNs.Store(start.UnixNano())
	var watchdogTripped atomic.Bool
	if window := src.MaxFrameSilence; window > 0 {
		watchStop := make(chan struct{})
		defer close(watchStop)
		go func() {
			// Sample at window/4: a trip is detected within 1.25x the window
			// without resetting a timer on every frame of a high-rate source.
			tick := window / 4
			if tick <= 0 {
				tick = window
			}
			t := time.NewTicker(tick)
			defer t.Stop()
			for {
				select {
				case <-watchStop:
					return
				case <-t.C:
					if m.now().Sub(time.Unix(0, lastFrameNs.Load())) > window {
						watchdogTripped.Store(true)
						_ = conn.Close() // unblocks the scanner's read; runSource re-dials
						return
					}
				}
			}
		}()
	}
	var frames io.Reader = &idleConn{Conn: conn, timeout: dialIdleTimeout}
	if chunked {
		frames = httputil.NewChunkedReader(frames)
	}
	baseEmit := m.emit(ctx, src)
	lastFrameGauge := metrics.SourceLastFrameTimestamp.WithLabelValues(src.Name)
	// seed the gauge with the connection start — "last frame = connect
	// time", the same reference lastFrameNs uses above. WithLabelValues alone
	// instantiates the child at 0 (Unix 1970), so the documented alert shape
	// `time() - this while source_up == 1` evaluated to ~56 years for the whole
	// post-connect warm-up of every healthy source, false-firing on each
	// (re)connect until the first frame arrived.
	lastFrameGauge.Set(float64(start.Unix()))
	err = sc(frames, src.Name, m.now, func(f *RawFrame) {
		frameCount++
		// m.now(), not f.Recv: the watchdog must not depend on every scanner
		// stamping Recv (a zero Recv would read as year-1 silence and trip it).
		now := m.now()
		lastFrameNs.Store(now.UnixNano())
		lastFrameGauge.Set(float64(now.Unix()))
		baseEmit(f)
	}, func(kind string) {
		metrics.IngestErrorsTotal.WithLabelValues(src.Name, kind).Inc()
	})
	if watchdogTripped.Load() {
		metrics.IngestErrorsTotal.WithLabelValues(src.Name, "frame_silence").Inc()
		err = fmt.Errorf("no decodable frames for %v (frame-silence watchdog): %w", src.MaxFrameSilence, err)
	}
	return useful, err
}

// emit returns the per-source emit closure: count the frame and hand it to the
// decode stage, honouring shutdown.
func (m *Manager) emit(ctx context.Context, src config.Source) func(*RawFrame) {
	observer := src.ObserverContext
	if observer.ObserverID == "" {
		// Programmatic tests/callers can bypass config.finalize. Preserve the
		// production invariant here too: missing policy becomes private, never public.
		observer = identity.NewPrivateContext(src.Name, identity.CredentialLocalDial)
	}
	return func(f *RawFrame) {
		f.Observer = observer
		if f.Obs == nil && f.RF == nil && f.Words != nil { // byte frames use CapturedOnlyTotal
			metrics.FramesTotal.WithLabelValues(src.Name, strconv.Itoa(int(f.GnssID))).Inc()
		}
		select {
		case m.out <- f:
		case <-ctx.Done():
		}
	}
}

func dialCtx(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, "tcp", addr)
}

func dialSource(ctx context.Context, src config.Source) (net.Conn, error) {
	if src.Type != "ntrip" || src.AllowInsecurePlaintext {
		if src.Type == "ntrip" && src.AllowInsecurePlaintext {
			metrics.SourceSecurityDegraded.WithLabelValues(src.Name, "ntrip_plaintext").Set(1)
		}
		return dialCtx(ctx, src.Addr)
	}
	host, _, err := net.SplitHostPort(src.Addr)
	if err != nil {
		return nil, err
	}
	serverName := src.NTRIPServerName
	if serverName == "" {
		serverName = host
	}
	tc := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if src.NTRIPCAFile != "" {
		pem, err := os.ReadFile(src.NTRIPCAFile)
		if err != nil {
			return nil, fmt.Errorf("ntrip ca_file: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ntrip ca_file: no certificates parsed")
		}
		tc.RootCAs = roots
	}
	d := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	metrics.SourceSecurityDegraded.WithLabelValues(src.Name, "ntrip_plaintext").Set(0)
	return (&tls.Dialer{NetDialer: d, Config: tc}).DialContext(ctx, "tcp", src.Addr)
}

func nextBackoff(b time.Duration) time.Duration {
	b *= 2
	if b > backoffMax {
		b = backoffMax
	}
	return b
}

// sleep waits for d or ctx cancellation; it returns false if ctx was cancelled.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}
