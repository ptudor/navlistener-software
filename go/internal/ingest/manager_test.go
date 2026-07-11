package ingest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
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

	_, err := m.runScanner(context.Background(), sc, conn, config.Source{Name: "idle-test"}, false)
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

// TestRunScannerUsefulOnFrame guards a connection that emits at least one
// frame is "useful" even if it returns almost immediately afterward -- backoff
// must reset for a source that clearly delivered real data, not just one that
// happened to stay connected a while.
func TestRunScannerUsefulOnFrame(t *testing.T) {
	sc := func(r io.Reader, _ string, _ func() time.Time, emit func(*RawFrame), _ func(string)) error {
		emit(&RawFrame{})
		return io.EOF
	}
	m := New(nil, make(chan *RawFrame, 1), quietManagerLog())
	useful, err := m.runScanner(context.Background(), sc, &fakeDeadlineConn{}, config.Source{Name: "useful-test"}, false)
	if err != io.EOF {
		t.Fatalf("runScanner error = %v, want io.EOF", err)
	}
	if !useful {
		t.Error("useful = false, want true (the scanner emitted a frame)")
	}
}

// TestRunScannerNotUsefulOnQuickEmptyDisconnect guards the other half of // zero frames and a near-instant return (the accept-then-close peer's exact
// signature) must not be "useful" -- runSource must be allowed to keep growing
// backoff instead of resetting on bare TCP-connect success.
func TestRunScannerNotUsefulOnQuickEmptyDisconnect(t *testing.T) {
	sc := func(r io.Reader, _ string, _ func() time.Time, _ func(*RawFrame), _ func(string)) error {
		return io.EOF
	}
	m := New(nil, make(chan *RawFrame, 1), quietManagerLog())
	useful, err := m.runScanner(context.Background(), sc, &fakeDeadlineConn{}, config.Source{Name: "not-useful-test"}, false)
	if err != io.EOF {
		t.Fatalf("runScanner error = %v, want io.EOF", err)
	}
	if useful {
		t.Error("useful = true, want false (zero frames, near-instant disconnect)")
	}
}

// TestIngestBackoffGrowsOnAcceptThenClosePeer guards exact fix-spec
// verification: a listener that closes immediately on accept must see inter-dial
// gaps grow toward backoffMax, not stay flat at ~backoffInitial forever. Runs
// runSource against a real loopback listener; each accepted connection is closed
// immediately (0 frames, near-instant disconnect), so every cycle is "not useful"
// and backoff must keep doubling.
func TestIngestBackoffGrowsOnAcceptThenClosePeer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	connects := make(chan time.Time, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			connects <- time.Now()
			_ = c.Close()
		}
	}()

	src := config.Source{Name: "flaky", Type: "ubx", Addr: ln.Addr().String()}
	sc := func(r io.Reader, _ string, _ func() time.Time, _ func(*RawFrame), _ func(string)) error {
		_, err := io.Copy(io.Discard, r) // returns near-instantly: the peer closed right after accept
		return err
	}
	m := New(nil, make(chan *RawFrame, 1), quietManagerLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go m.runSource(ctx, src, sc)

	var times []time.Time
	for i := 0; i < 4; i++ {
		select {
		case tm := <-connects:
			times = append(times, tm)
		case <-time.After(20 * time.Second):
			t.Fatalf("only observed %d/4 reconnect attempts within 20s", len(times))
		}
	}
	gap1 := times[1].Sub(times[0])
	gap2 := times[2].Sub(times[1])
	gap3 := times[3].Sub(times[2])
	if gap2 <= gap1 || gap3 <= gap2 {
		t.Errorf("backoff did not grow across reconnects: gap1=%v gap2=%v gap3=%v, want strictly increasing", gap1, gap2, gap3)
	}
}

// bufDeadlineConn is a minimal net.Conn backed by a fixed byte buffer, for feeding
// runScanner real payload bytes (unlike fakeDeadlineConn, which always returns EOF
// immediately) without a real network round trip.
type bufDeadlineConn struct {
	net.Conn
	r *bytes.Reader
}

func (c *bufDeadlineConn) SetReadDeadline(time.Time) error { return nil }
func (c *bufDeadlineConn) Read(p []byte) (int, error)      { return c.r.Read(p) }
func (c *bufDeadlineConn) Close() error                    { return nil }

// TestRunScannerDechunksNTRIPStream guards an NTRIP v2 caster's
// Transfer-Encoding: chunked stream must be de-chunked before scanRTCM -- otherwise
// hex chunk-size lines and CRLFs interleave into the RTCM3 byte stream and every
// frame spanning a chunk boundary fails CRC. This is the fix spec's exact scenario:
// one valid RTCM message split mid-message across two HTTP chunks must still decode
// to exactly one frame with zero errors.
func TestRunScannerDechunksNTRIPStream(t *testing.T) {
	// A valid RTCM3 message whose first 12 bits are 1019 (same construction as
	// TestScanRTCM).
	payload := make([]byte, 20)
	payload[0] = 0x3F
	payload[1] = 0xB0
	full := []byte{rtcmPreamble, byte(len(payload) >> 8), byte(len(payload))}
	full = append(full, payload...)
	crc := frame.CRC24Q(full)
	full = append(full, byte(crc>>16), byte(crc>>8), byte(crc))

	// Split the message across two HTTP chunks, mid-message.
	split := len(full) / 2
	var chunked bytes.Buffer
	fmt.Fprintf(&chunked, "%x\r\n", split)
	chunked.Write(full[:split])
	chunked.WriteString("\r\n")
	fmt.Fprintf(&chunked, "%x\r\n", len(full)-split)
	chunked.Write(full[split:])
	chunked.WriteString("\r\n0\r\n\r\n")

	conn := &bufDeadlineConn{r: bytes.NewReader(chunked.Bytes())}
	src := config.Source{Name: "ntrip-chunked-test"}
	out := make(chan *RawFrame, 8)
	m := New(nil, out, quietManagerLog())

	_, err := m.runScanner(context.Background(), scanRTCM, conn, src, true)
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Fatalf("runScanner error = %v", err)
	}
	close(out)
	var frames []*RawFrame
	for f := range out {
		frames = append(frames, f)
	}
	if len(frames) != 1 || frames[0].MsgType != 1019 {
		t.Fatalf("want exactly one RTCM 1019 frame, got %+v", frames)
	}
	if got := testutil.ToFloat64(metrics.IngestErrorsTotal.WithLabelValues(src.Name, "rtcm_crc")); got != 0 {
		t.Errorf("rtcm_crc errors = %v, want 0 (the chunk boundary must not corrupt the CRC)", got)
	}
}
