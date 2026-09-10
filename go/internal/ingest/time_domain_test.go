package ingest

import (
	"context"
	"io"
	"math"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
)

// the replay-horizon check used to negate the signed difference
// (`-d <= recvReplayHorizon`). time.Time.Sub saturates at math.MinInt64 for
// differences beyond a Duration's ~292-year range, and negating math.MinInt64
// wraps back to itself — a negative value, always ≤ the positive horizon — so
// every stamp old enough to saturate was ACCEPTED and carried into the historian
// as a genuine received_at.
func TestReceiveTimestampPlausibleBounds(t *testing.T) {
	now := time.Date(2026, 9, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		stamped time.Time
		want    bool
	}{
		{"now", now, true},
		{"upper boundary", now.Add(recvTimestampSlack), true},
		{"one ns past the upper boundary", now.Add(recvTimestampSlack + 1), false},
		{"one ns inside the upper boundary", now.Add(recvTimestampSlack - 1), true},
		{"lower boundary", now.Add(-recvReplayHorizon), true},
		{"one ns past the lower boundary", now.Add(-recvReplayHorizon - 1), false},
		{"one ns inside the lower boundary", now.Add(-recvReplayHorizon + 1), true},
		{"unix epoch", time.Unix(0, 0), false},
		{"earliest positive nanosecond", time.Unix(0, 1), false},
		// Beyond ±292 years the difference saturates; both directions must still
		// be rejected. The past case is the defect: it used to be accepted.
		{"saturated past", time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"saturated future", time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC), false},
		{"far saturated past", time.Date(1, 1, 1, 0, 0, 0, 1, time.UTC), false},
		{"far saturated future", time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), false},
	} {
		if got := receiveTimestampPlausible(tc.stamped, now); got != tc.want {
			t.Errorf("receiveTimestampPlausible(%s: %v) = %v, want %v",
				tc.name, tc.stamped.UTC(), got, tc.want)
		}
	}

	// The exact defect, pinned so a refactor back to the negated form is caught:
	// Sub saturates at math.MinInt64 for a 400-year-old stamp, and negating that
	// wraps straight back to math.MinInt64 — which is less than the positive
	// horizon, so the old `-d <= recvReplayHorizon` accepted it.
	if d := time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC).Sub(now); d != math.MinInt64 {
		t.Fatalf("expected a saturated duration for a 400-year-old stamp, got %v", d)
	} else if -d > recvReplayHorizon {
		t.Fatal("negating math.MinInt64 no longer wraps; this test no longer pins the defect")
	}
}

// the frame-silence watchdog stored now.UnixNano() and rebuilt it
// with time.Unix, both of which strip Go's monotonic reading, so its "elapsed
// silence" was really a wall-clock difference. A host clock step changes what
// UnixNano() reports without changing the monotonic reading — the divergence the
// injected func() time.Time seam cannot express, since a synthesized time.Time
// can never carry a monotonic component. So this exercises the production
// silenceClock two ways: it must store an elapsed DURATION (never an absolute
// instant, which is the representation the fix removed), and the legacy formula
// is reproduced alongside to show exactly how a step corrupted it.
func TestSilenceClockStoresMonotonicElapsed(t *testing.T) {
	const (
		beforeFrame = 50 * time.Millisecond
		silence     = 120 * time.Millisecond
	)
	base := time.Now() // carries a monotonic reading, as m.now() does in production
	frameAt := base.Add(beforeFrame)
	var reading atomic.Int64
	sc := newSilenceClock(base, func() time.Time { return base.Add(time.Duration(reading.Load())) })

	if got := sc.since(); got != 0 {
		t.Fatalf("silence before any frame = %v, want 0 (measured from connect time)", got)
	}
	sc.mark(frameAt)
	if got := sc.last.Load(); got != int64(beforeFrame) {
		t.Fatalf("silenceClock stored %d ns; want the elapsed %d ns. Storing an absolute "+
			"wall-clock instant is exactly the representation regression fix removed",
			got, int64(beforeFrame))
	}
	reading.Store(int64(beforeFrame + silence))
	if got := sc.since(); got != silence {
		t.Fatalf("silence = %v, want %v", got, silence)
	}

	// The pre-fix formula, kept to document what the change removed: a host wall
	// step moves UnixNano() without moving the monotonic clock, and the legacy
	// difference absorbs the whole step.
	monotonicNow := base.Add(beforeFrame + silence)
	for _, step := range []time.Duration{-24 * time.Hour, -time.Hour, time.Hour, 24 * time.Hour} {
		wallNow := time.Unix(0, monotonicNow.UnixNano()+int64(step))
		if legacy := wallNow.Sub(time.Unix(0, frameAt.UnixNano())); legacy != silence+step {
			t.Errorf("step %v: legacy wall-clock formula = %v, want %v", step, legacy, silence+step)
		}
	}
}

// The behavioral half of frames flowing hold the watchdog off for
// well over one window, and the trip comes only after they stop — within the
// documented window/4 sampling tolerance (at most 1.25x the window).
func TestFrameSilenceTripsOnlyAfterFramesStop(t *testing.T) {
	const window = 200 * time.Millisecond
	src := config.Source{Name: "frames-then-silence", MaxFrameSilence: window}
	conn := &closeSignalConn{closed: make(chan struct{})}
	var wentSilent time.Time
	sc := func(_ io.Reader, _ string, _ func() time.Time, emit func(*RawFrame), _ func(string)) error {
		// Three windows' worth of frames: the watchdog must not trip while they flow.
		deadline := time.Now().Add(3 * window)
		for time.Now().Before(deadline) {
			emit(&RawFrame{}) // zero Recv on purpose: the watchdog must not read it
			time.Sleep(window / 5)
		}
		wentSilent = time.Now()
		select {
		case <-conn.closed: // the watchdog tore us down
		case <-time.After(5 * window):
		}
		return io.EOF
	}
	m := New(nil, make(chan *RawFrame, 64), quietManagerLog())
	before := testutil.ToFloat64(metrics.IngestErrorsTotal.WithLabelValues(src.Name, "frame_silence"))
	_, err := m.runScanner(context.Background(), sc, conn, src, false)
	if err == nil || !strings.Contains(err.Error(), "frame-silence watchdog") {
		t.Fatalf("runScanner error = %v, want the frame-silence watchdog error", err)
	}
	tripped, ok := conn.closeTime()
	if !ok {
		t.Fatal("the watchdog never closed the connection")
	}
	// The trip must land strictly after the stream went silent — three windows of
	// frames held it off — and within one window plus the window/4 sampling
	// tolerance, generously padded for a loaded test host.
	if !tripped.After(wentSilent) {
		t.Errorf("watchdog tripped at %v, before the stream went silent at %v", tripped, wentSilent)
	}
	if silent := tripped.Sub(wentSilent); silent > window*5/2 {
		t.Errorf("watchdog tripped %v after the stream went silent, want at most %v", silent, window*5/2)
	}
	if got := testutil.ToFloat64(metrics.IngestErrorsTotal.WithLabelValues(src.Name, "frame_silence")) - before; got != 1 {
		t.Errorf("frame_silence errors rose by %v, want exactly 1", got)
	}
}

// closeSignalConn records when runScanner's watchdog closed it.
type closeSignalConn struct {
	net.Conn
	closed   chan struct{}
	closedAt time.Time
	once     sync.Once
}

func (c *closeSignalConn) SetReadDeadline(time.Time) error { return nil }
func (c *closeSignalConn) Read([]byte) (int, error)        { <-c.closed; return 0, io.EOF }
func (c *closeSignalConn) Close() error {
	c.once.Do(func() { c.closedAt = time.Now(); close(c.closed) })
	return nil
}

// closeTime reads closedAt only through the channel close that publishes it, so
// the value is never read unsynchronized when no close happened.
func (c *closeSignalConn) closeTime() (time.Time, bool) {
	select {
	case <-c.closed:
		return c.closedAt, true
	default:
		return time.Time{}, false
	}
}
