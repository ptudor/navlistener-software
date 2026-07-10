package ingest

import (
	"context"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
)

func quietManagerLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestEmitExcludesObsAndRFFromFramesTotal guards FramesTotal must count
// only actual nav frames, not RAWX observables or RF telemetry — mirroring the
// push-path guard so dial-mode metrics aren't inflated relative to real nav-frame
// throughput.
func TestEmitExcludesObsAndRFFromFramesTotal(t *testing.T) {
	out := make(chan *RawFrame, 8)
	m := New(nil, out, quietManagerLog())
	ctx := context.Background()
	src := config.Source{Name: "dial-emit-test"}
	emit := m.emit(ctx, src)

	before := testutil.ToFloat64(metrics.FramesTotal.WithLabelValues(src.Name, "0"))

	emit(&RawFrame{GnssID: gnss.GPS, Obs: &RawObs{}}) // observable: must not count
	emit(&RawFrame{GnssID: gnss.GPS, RF: &RawRF{}})   // telemetry: must not count
	emit(&RawFrame{GnssID: gnss.GPS})                 // nav frame: must count

	// Drain what emit() sent to out so the test doesn't depend on channel capacity.
	for i := 0; i < 3; i++ {
		select {
		case <-out:
		case <-time.After(time.Second):
			t.Fatal("emit did not forward a frame to out")
		}
	}

	after := testutil.ToFloat64(metrics.FramesTotal.WithLabelValues(src.Name, "0"))
	if got := after - before; got != 1 {
		t.Errorf("FramesTotal delta = %v, want 1 (only the nav frame counted, not Obs/RF)", got)
	}
}

// fakeDeadlineConn is a minimal net.Conn that records SetReadDeadline calls and
// returns io.EOF from Read, so runScanner's scanner callback can complete
// immediately without a real network round trip.
type fakeDeadlineConn struct {
	net.Conn
	deadlines []time.Time
}

func (c *fakeDeadlineConn) SetReadDeadline(t time.Time) error {
	c.deadlines = append(c.deadlines, t)
	return nil
}
func (c *fakeDeadlineConn) Read([]byte) (int, error) { return 0, io.EOF }
func (c *fakeDeadlineConn) Close() error             { return nil }

// TestRunScannerAppliesIdleTimeout guards runScanner must wrap the dial
// connection in an idle-deadline reader (the idleConn push.go already uses) with
// dialIdleTimeout, so a half-open receiver's blocked Read is bounded instead of
// hanging forever with SourceUp left at 1.
func TestRunScannerAppliesIdleTimeout(t *testing.T) {
	out := make(chan *RawFrame, 1)
	m := New(nil, out, quietManagerLog())
	conn := &fakeDeadlineConn{}

	var sawTimeout time.Duration
	sc := func(r io.Reader, _ string, _ func() time.Time, _ func(*RawFrame), _ func(string)) error {
		ic, ok := r.(*idleConn)
		if !ok {
			t.Fatalf("scanner received %T, want *idleConn", r)
		}
		sawTimeout = ic.timeout
		_, err := r.Read(make([]byte, 1)) // triggers idleConn.Read -> SetReadDeadline -> underlying Read
		return err
	}

	err := m.runScanner(context.Background(), sc, conn, config.Source{Name: "idle-test"})
	if err != io.EOF {
		t.Fatalf("runScanner error = %v, want io.EOF", err)
	}
	if sawTimeout != dialIdleTimeout {
		t.Errorf("idleConn.timeout = %v, want dialIdleTimeout (%v)", sawTimeout, dialIdleTimeout)
	}
	if len(conn.deadlines) != 1 {
		t.Fatalf("SetReadDeadline called %d times, want 1", len(conn.deadlines))
	}
	if slack := time.Until(conn.deadlines[0]) - dialIdleTimeout; slack > time.Second || slack < -time.Second {
		t.Errorf("read deadline %v not within 1s of now+dialIdleTimeout", conn.deadlines[0])
	}
}
