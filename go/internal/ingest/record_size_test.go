package ingest

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// TestPushRejectsOversizedNavRecords: a navigation record body past
// maxNavRecordBytes never becomes a RawFrame. It is counted under its own
// reason and acked as permanently malformed, so one bad record cannot wedge
// the valid spool records behind it, while a body exactly at the bound is
// forwarded byte-exact.
func TestPushRejectsOversizedNavRecords(t *testing.T) {
	for _, feed := range []string{"ubx", "rtcm"} {
		t.Run(feed, func(t *testing.T) {
			tr := NewDurableTracker()
			cli, out, _ := streamOnPipe(t, feed, tr)
			counter := metrics.PushErrorsTotal.WithLabelValues("obs1", "record_oversize")
			before := testutil.ToFloat64(counter)

			// Sequence 1: exactly at the bound. 2: four bytes over. 3: the 1 MiB
			// body the wire itself still allows.
			for seq, n := range map[uint64]int{1: maxNavRecordBytes, 2: maxNavRecordBytes + 4, 3: wire.MaxFrameLen - 64} {
				if err := wire.WriteFrame(cli, wire.Data, wire.EncodeData(seq, navRecord(n))); err != nil {
					t.Fatalf("write %d-byte body: %v", n, err)
				}
			}
			var f *RawFrame
			select {
			case f = <-out:
			case <-time.After(2 * time.Second):
				t.Fatal("the record at the size bound never reached the decode stage")
			}
			if f.Seq != 1 || len(f.RawBytes()) != maxNavRecordBytes {
				t.Fatalf("forwarded seq %d with %d raw bytes, want seq 1 with %d", f.Seq, len(f.RawBytes()), maxNavRecordBytes)
			}
			expectNoFrame(t, out, "oversized bodies")
			if got := testutil.ToFloat64(counter) - before; got != 2 {
				t.Fatalf("push_errors_total{record_oversize} rose by %v, want 2", got)
			}
			// Both rejected sequences are registered as never-persistable, so
			// resolving the one real record releases all three.
			if got := tr.Watermark("obs1", "boot-a"); got != 0 {
				t.Fatalf("watermark = %d while sequence 1 is outstanding, want 0", got)
			}
			tr.Resolved("obs1", "boot-a", 1)
			if got := tr.Watermark("obs1", "boot-a"); got != 3 {
				t.Fatalf("watermark = %d after resolving the real record, want 3", got)
			}
		})
	}
	// Telemetry is exempt: its codecs bound their own lengths.
	telem := navRecord(maxNavRecordBytes + 4)
	telem.FrameType = TelemJammingStats
	if !navRecordBounded(telem) {
		t.Error("telemetry body refused by the navigation record bound")
	}
}
