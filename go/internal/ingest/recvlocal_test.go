package ingest

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/wire"
)

// TestRecordToFrameStampsCollectorLocalClock guards push-path frames carry
// the FEEDER's wall-clock stamp in Recv (the forensic reception time, accepted with
// up to recvTimestampSlack of skew), but every staleness/expiry age in live state
// must be an elapsed time on the collector's own clock — so recordToFrame must also
// stamp RecvLocal from time.Now() here, regardless of what the feeder claimed.
func TestRecordToFrameStampsCollectorLocalClock(t *testing.T) {
	before := time.Now()
	stamp := before.Add(-4 * time.Minute) // within recvTimestampSlack: accepted into Recv
	rec := wire.RawRecord{
		RecvUnixNs: stamp.UnixNano(),
		GnssID:     gnss.GPS, SvID: 5, FrameType: 0x10, // GpsLnav
		Raw: make([]byte, 40),
	}
	f := recordToFrame(rec, "ubx", "obs1")
	after := time.Now()

	if f == nil {
		t.Fatal("recordToFrame returned nil for a valid nav record")
	}
	if got := f.Recv.Sub(stamp); got < -time.Second || got > time.Second {
		t.Errorf("Recv = %v, want the feeder stamp %v (forensic time preserved)", f.Recv, stamp)
	}
	if f.RecvLocal.Before(before) || f.RecvLocal.After(after) {
		t.Errorf("RecvLocal = %v, want within [%v, %v] (collector clock)", f.RecvLocal, before, after)
	}
	if !f.LocalRecv().Equal(f.RecvLocal) {
		t.Errorf("LocalRecv() = %v, want RecvLocal %v when RecvLocal is set", f.LocalRecv(), f.RecvLocal)
	}
	// The two clock domains must actually differ here — a regression that copies
	// the feeder stamp into RecvLocal would silently reintroduce the skew bug.
	if f.LocalRecv().Sub(f.Recv) < 3*time.Minute {
		t.Errorf("LocalRecv−Recv = %v, want ≈4m (local clock, not the skewed feeder stamp)", f.LocalRecv().Sub(f.Recv))
	}
}

// TestTelemetryFrameStampsCollectorLocalClock: the RF-telemetry path feeds the
// per-station staleness windows (rfStaleAfter) directly, so it needs the same
// collector-local stamp.
func TestTelemetryFrameStampsCollectorLocalClock(t *testing.T) {
	before := time.Now()
	rec := wire.RawRecord{
		RecvUnixNs: before.Add(-4 * time.Minute).UnixNano(),
		FrameType:  TelemJammingStats,
		Raw:        EncodeJammingStats([]RFBand{{Block: 0, AGC: 2500}}),
	}
	f := recordToFrame(rec, "ubx", "obs1")
	after := time.Now()

	if f == nil || f.RF == nil {
		t.Fatalf("recordToFrame = %+v, want an RF telemetry frame", f)
	}
	if f.RecvLocal.Before(before) || f.RecvLocal.After(after) {
		t.Errorf("telemetry RecvLocal = %v, want within [%v, %v]", f.RecvLocal, before, after)
	}
}

// TestLocalRecvFallsBackToRecvForDialFrames: dial-mode connectors stamp Recv from
// this host's clock and never set RecvLocal — LocalRecv must return Recv there so
// existing dial semantics (and every pre-regression fix test fixture) are unchanged.
func TestLocalRecvFallsBackToRecvForDialFrames(t *testing.T) {
	recv := time.Unix(1_700_000_000, 0)
	f := &RawFrame{Recv: recv}
	if !f.LocalRecv().Equal(recv) {
		t.Errorf("LocalRecv() = %v, want Recv %v when RecvLocal is zero", f.LocalRecv(), recv)
	}
}
