package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// TestDecodePanicCountedEveryTimeLoggedOnce guards a systematically-
// panicking decoder (same SV re-broadcasting the offending bit pattern) must
// increment navlistener_decode_panics_total on EVERY recurrence — the alertable
// signal decode_errors_total never carries — while the ERROR log line is
// rate-limited per (gnssid, svid, msg_type) so months of recurrence cannot
// flood the logfile at ingest rate.
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
// long-running incident stays visible in the logfile.
func TestPanicLogLimiterReopensAfterInterval(t *testing.T) {
	lim := &panicLogLimiter{}
	t0 := time.Unix(1_700_000_000, 0)
	if !lim.allow(0, 7, 0x10, t0) {
		t.Fatal("first occurrence must be allowed")
	}
	if lim.allow(0, 7, 0x10, t0.Add(panicLogEvery/2)) {
		t.Error("recurrence inside the interval must be suppressed")
	}
	if !lim.allow(0, 7, 0x10, t0.Add(panicLogEvery+time.Second)) {
		t.Error("recurrence after the interval must be allowed again")
	}
	if !lim.allow(0, 7, 0x11, t0) {
		t.Error("a different msg_type is a distinct key and must be allowed")
	}
}
