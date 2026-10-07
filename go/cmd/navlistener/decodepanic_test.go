package main

import (
	"bytes"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/state"
)

// TestDecodePanicCountedEveryTimeLoggedOnce guards a systematically-
// panicking decoder (same SV re-broadcasting the offending bit pattern) must
// increment navlistener_decode_panics_total on EVERY recurrence — the alertable
// signal decode_errors_total never carries — while the ERROR log line is
// rate-limited per (gnssid, svid, sigid) so months of recurrence
// cannot flood the logfile at ingest rate.
func TestDecodePanicCountedEveryTimeLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	lim := &panicLogLimiter{}
	f := &ingest.RawFrame{GnssID: gnss.GPS, SvID: 7, SigID: 0, MsgType: 0x10, Source: "obs1"}
	boom := func(fr *ingest.RawFrame) {
		defer recoverDecodePanic(fr, log, lim)
		panic("offending bit pattern")
	}

	before := testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues("0"))
	for i := 0; i < 5; i++ {
		boom(f)
	}
	if d := testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues("0")) - before; d != 5 {
		t.Errorf("decode_panics_total delta = %v, want 5 (every recurrence counted)", d)
	}
	if n := strings.Count(buf.String(), "decode panic recovered"); n != 1 {
		t.Errorf("logged %d times for one repeating signal, want 1 (rate-limited)", n)
	}
	if !strings.Contains(buf.String(), "svid=7") || !strings.Contains(buf.String(), "source=obs1") {
		t.Errorf("panic log line lacks the SV identity needed to pull the raw frame: %s", buf.String())
	}

	// A different SV panicking is a distinct signal and gets its own first line.
	f2 := &ingest.RawFrame{GnssID: gnss.GPS, SvID: 8, SigID: 0, MsgType: 0x10, Source: "obs1"}
	boom(f2)
	if n := strings.Count(buf.String(), "decode panic recovered"); n != 2 {
		t.Errorf("distinct SV's first panic not logged (got %d lines, want 2)", n)
	}
}

// TestDecodeLoopRecoversScopeRevocationPanic guards the scope-revocation
// marker path runs inside the per-frame recover like every other frame: a panic
// in the revoke callback drops that marker and counts a panic instead of taking
// the process down, is not filed under GPS, and the loop goes on applying the
// frames behind it.
func TestDecodeLoopRecoversScopeRevocationPanic(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	frames := make(chan *ingest.RawFrame, 2)
	revoked := 0
	revoke := func(*ingest.ScopeRevocation) {
		revoked++
		panic("registry reset failed")
	}
	beforeUnlabelled := testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues(""))
	beforeGPS := testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues("0"))
	frames <- &ingest.RawFrame{ScopeRevocation: &ingest.ScopeRevocation{
		Previous: identity.ObserverContext{ObserverID: "obs1"}, ChangedAt: time.Now()}}
	frames <- &ingest.RawFrame{Recv: time.Now(), Source: "st1", RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 100}}}}
	close(frames)

	var lastFrame atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		decodeLoop(frames, state.New(1), state.New(1), state.New(1), nil, log, &lastFrame, nil, revoke, nil)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("decodeLoop did not return after the frames channel closed")
	}
	if revoked != 1 {
		t.Fatalf("revoke called %d times, want 1", revoked)
	}
	if d := testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues("")) - beforeUnlabelled; d != 1 {
		t.Errorf("decode_panics_total without a constellation delta = %v, want 1", d)
	}
	if d := testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues("0")) - beforeGPS; d != 0 {
		t.Errorf("a control-frame panic was filed under GPS (delta %v)", d)
	}
	if !strings.Contains(buf.String(), "kind=control") || !strings.Contains(buf.String(), "source=obs1") {
		t.Errorf("panic log line lacks the frame kind and the observer: %s", buf.String())
	}
	if lastFrame.Load() == 0 {
		t.Error("the frame behind the panicking marker was not applied")
	}
}

// TestExpireTickFor guards the SV-expiry sweep cadence must follow the
// configured TTL instead of silently quantizing a fast-expiry configuration to
// the unrelated 30 s constant.
func TestExpireTickFor(t *testing.T) {
	cases := []struct {
		ttl, want time.Duration
	}{
		{2 * time.Hour, 30 * time.Second},   // default: the established sweep
		{5 * time.Minute, 30 * time.Second}, // still capped
		{20 * time.Second, 10 * time.Second},
		{time.Second, time.Second}, // floored
		{200 * time.Millisecond, time.Second},
	}
	for _, tc := range cases {
		if got := expireTickFor(tc.ttl); got != tc.want {
			t.Errorf("expireTickFor(%v) = %v, want %v", tc.ttl, got, tc.want)
		}
	}
}

// TestPanicLogLimiterReopensAfterInterval: the limiter is a cadence, not a
// once-ever latch — after panicLogEvery the same signal logs again so a
// long-running incident stays visible in the logfile. The third key component is
// the SIGNAL id : sigid is what selects the panicking decoder and is
// domain-checked to ~10 values, while push-path msg_type is an unvalidated wire
// byte that would inflate the never-evicted key space ~24×.
func TestPanicLogLimiterReopensAfterInterval(t *testing.T) {
	lim := &panicLogLimiter{}
	t0 := time.Unix(1_700_000_000, 0)
	if !lim.allow(0, 7, 0 /*sigid L1C/A*/, t0) {
		t.Fatal("first occurrence must be allowed")
	}
	if lim.allow(0, 7, 0, t0.Add(panicLogEvery/2)) {
		t.Error("recurrence inside the interval must be suppressed")
	}
	if !lim.allow(0, 7, 0, t0.Add(panicLogEvery+time.Second)) {
		t.Error("recurrence after the interval must be allowed again")
	}
	if !lim.allow(0, 7, 3 /*sigid L2C*/, t0) {
		t.Error("a different sigid is a distinct decoder path and must be allowed")
	}
	if !lim.allow(2, 7, 0, t0) {
		t.Error("a different constellation is a distinct key and must be allowed")
	}
}

// TestPanicLogLimiterIgnoresMsgType pins key choice: two frames of the
// same (gnssid, svid, sigid) that differ only in msg_type run the SAME decoder, so
// the second must be suppressed — msg_type must not multiply the never-evicted map
// (a push-path wire byte with ~240 unvalidated values).
func TestPanicLogLimiterIgnoresMsgType(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	lim := &panicLogLimiter{}
	boom := func(fr *ingest.RawFrame) {
		defer recoverDecodePanic(fr, log, lim)
		panic("offending bit pattern")
	}
	boom(&ingest.RawFrame{GnssID: gnss.GPS, SvID: 9, SigID: 0, MsgType: 0x10, Source: "obs1"})
	boom(&ingest.RawFrame{GnssID: gnss.GPS, SvID: 9, SigID: 0, MsgType: 0x11, Source: "obs1"})
	if n := strings.Count(buf.String(), "decode panic recovered"); n != 1 {
		t.Errorf("logged %d times for one (gnssid, svid, sigid) signal, want 1 (msg_type must not key the limiter)", n)
	}
}
