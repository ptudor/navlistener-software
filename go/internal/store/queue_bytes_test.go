package store

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
)

// TestEnqueueByteBudgetDropsBeforeDepth: the writer queue is bounded by bytes
// as well as by frames. Frames that sum past the byte budget are dropped and
// counted long before queueDepth is reached, and a dequeued frame releases its
// reservation so the queue accepts again.
func TestEnqueueByteBudgetDropsBeforeDepth(t *testing.T) {
	s := &Store{in: make(chan *NavFrame, queueDepth), log: quietLog()}
	const payload = 1 << 20
	before := testutil.ToFloat64(metrics.StoreDroppedTotal)
	accepted := 0
	for i := 0; i < 80; i++ {
		s.Enqueue(&NavFrame{SourceID: "obs", Raw: make([]byte, payload)})
		accepted = len(s.in)
		if testutil.ToFloat64(metrics.StoreDroppedTotal) > before {
			break
		}
	}
	dropped := testutil.ToFloat64(metrics.StoreDroppedTotal) - before
	if dropped != 1 {
		t.Fatalf("StoreDroppedTotal advanced by %v, want 1 at the byte budget", dropped)
	}
	// 64 MiB of budget holds 63 frames of 1 MiB plus overhead; the 64th is refused.
	wantAccepted := int(maxQueueBytes / (payload + frameQueueOverhead))
	if accepted != wantAccepted || accepted >= queueDepth {
		t.Fatalf("queue holds %d frames at the byte budget, want %d (far below queueDepth %d)", accepted, wantAccepted, queueDepth)
	}
	if got := s.queueBytes.Load(); got != int64(accepted)*(payload+frameQueueOverhead) {
		t.Fatalf("queue bytes = %d after a refused frame, want %d (the refused frame's reservation must be released)", got, int64(accepted)*(payload+frameQueueOverhead))
	}
	// Taking one frame off the queue, as the writer does, makes room for one.
	f := <-s.in
	s.dequeued(f)
	s.Enqueue(&NavFrame{SourceID: "obs", Raw: make([]byte, payload)})
	if len(s.in) != accepted || testutil.ToFloat64(metrics.StoreDroppedTotal)-before != 1 {
		t.Fatalf("queue did not accept a frame after one was dequeued: len %d, drops %v", len(s.in), testutil.ToFloat64(metrics.StoreDroppedTotal)-before)
	}
	// Board and RF payloads count too.
	small := &Store{in: make(chan *NavFrame, 4), log: quietLog(), queueBytesLimit: 4096}
	small.Enqueue(&NavFrame{SourceID: "obs", Raw: []byte{1}, Board: &BoardSample{Kind: "environment", Data: make([]byte, 4096)}})
	if len(small.in) != 0 {
		t.Fatal("a board sample over the byte budget was queued")
	}
	small.Enqueue(&NavFrame{SourceID: "obs", Raw: []byte{1}, RF: &RFSample{Kind: "jamming", Data: make([]byte, 1024)}})
	if len(small.in) != 1 {
		t.Fatal("an RF sample within the byte budget was refused")
	}
}
