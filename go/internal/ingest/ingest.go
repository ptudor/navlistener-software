package ingest

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
)

// dialTimeout bounds a single connection attempt; backoff bounds the retry pace.
const (
	dialTimeout    = 10 * time.Second
	backoffInitial = 1 * time.Second
	backoffMax     = 30 * time.Second
)

// scanner reads a receiver stream and emits RawFrames until the stream errors.
type scanner func(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error

// scannerFor returns the stream scanner for a connector type, or nil if the type
// is recognised by config but not yet implemented in this build.
func scannerFor(typ string) scanner {
	switch typ {
	case "ubx":
		return scanUBX
	case "sbf":
		return scanSBF
	case "rtcm":
		return scanRTCM
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
		conn, err := dialCtx(ctx, src.Addr)
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
		metrics.SourceConnectsTotal.WithLabelValues(src.Name).Inc()
		metrics.SourceUp.WithLabelValues(src.Name, src.Type).Set(1)
		m.log.Info("ingest source connected", "source", src.Name, "addr", src.Addr, "type", src.Type)
		backoff = backoffInitial

		// Close the connection when ctx is cancelled so a blocked read returns.
		stop := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-stop:
			}
		}()

		err = sc(conn, src.Name, m.now, m.emit(ctx, src), func(kind string) {
			metrics.IngestErrorsTotal.WithLabelValues(src.Name, kind).Inc()
		})
		close(stop)
		_ = conn.Close()
		metrics.SourceUp.WithLabelValues(src.Name, src.Type).Set(0)
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

// emit returns the per-source emit closure: count the frame and hand it to the
// decode stage, honouring shutdown.
func (m *Manager) emit(ctx context.Context, src config.Source) func(*RawFrame) {
	return func(f *RawFrame) {
		metrics.FramesTotal.WithLabelValues(src.Name, strconv.Itoa(int(f.GnssID))).Inc()
		select {
		case m.out <- f:
		case <-ctx.Done():
		}
	}
}

func dialCtx(ctx context.Context, addr string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout}
	return d.DialContext(ctx, "tcp", addr)
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
