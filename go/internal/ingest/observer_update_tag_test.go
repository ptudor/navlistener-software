package ingest

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// tlv appends one observer-details component.
func tlv(report []byte, tag byte, value []byte) []byte {
	report = append(report, tag, byte(len(value)>>8), byte(len(value)))
	return append(report, value...)
}

// validTiming is the smallest tag 8 value decodeBoardTiming accepts: version
// and layout 1, every channel idle.
func validTiming() []byte {
	v := make([]byte, 196)
	v[0], v[1] = 1, 1
	return v
}

// updateStatus is a decodable tag 9 value (manual policy, stable channel,
// idle) that mutate may then push outside the strict contract.
func updateStatus(mutate func(p []byte)) []byte {
	p := make([]byte, 140)
	p[0], p[1], p[2] = 1, 1, 1
	if mutate != nil {
		mutate(p)
	}
	return p
}

// TestUndecodableUpdateStatusTagIsSkipped: tag 9 is skippable by design, so a
// value this collector cannot decode — a state newer firmware added, or
// received past total after a failed download — must not discard the timing,
// firmware and environment tags beside it. The update stays absent.
func TestUndecodableUpdateStatusTagIsSkipped(t *testing.T) {
	base := observerGolden(t)
	for name, bad := range map[string]func(p []byte){
		"unknown state":         func(p []byte) { p[3] = 12 },
		"unknown profile":       func(p []byte) { p[5] = byte(len(wire.TrustProfiles)) },
		"unknown error domain":  func(p []byte) { binary.BigEndian.PutUint16(p[72:], 8); binary.BigEndian.PutUint16(p[74:], 1) },
		"reserved security bit": func(p []byte) { p[4] = 32 },
		"received past total": func(p []byte) {
			binary.BigEndian.PutUint32(p[40:], 5000)
			binary.BigEndian.PutUint32(p[44:], 4096)
		},
	} {
		t.Run(name, func(t *testing.T) {
			report := tlv(tlv(append([]byte(nil), base...), 8, validTiming()), 9, updateStatus(bad))
			d, err := decodeObserverDetails(report)
			if err != nil {
				t.Fatalf("report discarded for an undecodable update tag: %v", err)
			}
			if d.Timing == nil || d.Firmware != "test-v1" || d.Environment == nil {
				t.Fatalf("components beside the rejected tag were lost: %+v", d)
			}
			if d.Update != nil || !d.UpdateStatusRejected {
				t.Fatalf("rejected update status recorded: update=%v rejected=%t", d.Update, d.UpdateStatusRejected)
			}
		})
	}
	// A decodable tag still lands.
	good := tlv(tlv(append([]byte(nil), base...), 8, validTiming()), 9, updateStatus(nil))
	d, err := decodeObserverDetails(good)
	if err != nil {
		t.Fatal(err)
	}
	if d.Update == nil || d.UpdateStatusRejected || d.Update.State != "idle" || d.Timing == nil {
		t.Fatalf("valid update status not decoded: %+v", d)
	}
	// The length rule is part of the strict contract: a 139-byte tag is
	// skipped as well, never read as a shorter layout.
	short := tlv(append([]byte(nil), base...), 9, updateStatus(nil)[:139])
	if d, err := decodeObserverDetails(short); err != nil || d.Update != nil || !d.UpdateStatusRejected {
		t.Fatalf("short update tag: err=%v update=%v rejected=%t", err, d.Update, d != nil && d.UpdateStatusRejected)
	}
	// A report whose only component is an undecodable tag 9 carries nothing
	// known, exactly as a report with only unknown tags does.
	only := tlv(append([]byte(nil), base[:24]...), 9, updateStatus(func(p []byte) { p[3] = 12 }))
	if _, err := decodeObserverDetails(only); err == nil {
		t.Fatal("a report with no decodable component was accepted")
	}
}

// TestUndecodableUpdateStatusDeliversTheReport pushes a report with a valid
// timing tag and an undecodable update tag through the GNF1 session: the
// frame reaches the pipeline with its timing, the rejection is counted per
// observer, and the update coordinator is never handed a status.
func TestUndecodableUpdateStatusDeliversTheReport(t *testing.T) {
	tr := NewDurableTracker()
	cli, out, _ := streamOnPipe(t, "ubx", tr)
	before := testutil.ToFloat64(metrics.PushUpdateStatusRejectedTotal.WithLabelValues("obs1"))
	report := tlv(tlv(observerGolden(t), 8, validTiming()), 9, updateStatus(func(p []byte) { p[3] = 12 }))
	rec := wire.RawRecord{FrameType: TelemObserverDetails, Raw: report}
	if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}
	select {
	case f := <-out:
		if f.Details == nil || f.Details.Timing == nil || f.Details.Update != nil || !f.Details.UpdateStatusRejected {
			t.Fatalf("delivered report: %+v", f.Details)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("observer report with an undecodable update tag was not delivered")
	}
	if got := testutil.ToFloat64(metrics.PushUpdateStatusRejectedTotal.WithLabelValues("obs1")); got != before+1 {
		t.Fatalf("rejected counter = %v, want %v", got, before+1)
	}
}
