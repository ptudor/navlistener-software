package main

import (
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
)

// TestDecodeLoopDrainsAllFramesBeforeClose guards decodeLoop must apply
// every frame a producer sends, including ones sent concurrently right up until
// the frames channel is closed. It no longer has its own shutdown logic (no ctx,
// no drain-until-momentarily-empty) — it simply ranges frames to closure, and the
// owner (run, in main.go) is responsible for closing frames only once every
// producer has confirmed (via ingestWG.Wait()) it will never send again. This
// test plays the producer's part directly: an unbuffered channel means a send
// only returns once decodeLoop has received it, so closing frames right after the
// producer goroutine finishes is exactly the same handoff run() performs, and any
// frame decodeLoop failed to apply would show up as a missing station below.
func TestDecodeLoopDrainsAllFramesBeforeClose(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	live := state.New(1)
	frames := make(chan *ingest.RawFrame)

	const n = 50
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		for i := 0; i < n; i++ {
			frames <- &ingest.RawFrame{
				Recv:   time.Now(),
				Source: fmt.Sprintf("st%d", i),
				RF:     &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 100}}},
			}
		}
	}()

	decodeDone := make(chan struct{})
	go func() {
		defer close(decodeDone)
		decodeLoop(frames, live, nil, log)
	}()

	select {
	case <-producerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("producer never finished sending — test setup broken")
	}
	close(frames) // safe only because producerDone confirms nothing will send again

	select {
	case <-decodeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("decodeLoop did not return after frames was closed")
	}

	rf := live.FeedStationRF(time.Now())
	if len(rf) != n {
		t.Fatalf("live state has %d stations, want %d — some concurrently-emitted frames were never applied", len(rf), n)
	}
}
