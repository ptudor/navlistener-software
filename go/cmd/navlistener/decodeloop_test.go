package main

import (
	"fmt"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/identity"
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
	publicLive := state.New(1)
	publicEventsLive := state.New(1)
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

	var lastFrame atomic.Int64
	decodeDone := make(chan struct{})
	go func() {
		defer close(decodeDone)
		decodeLoop(frames, live, publicLive, publicEventsLive, nil, log, &lastFrame, nil, nil, nil)
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
	// the decode funnel must stamp ingest liveness for the /healthz probe.
	if lastFrame.Load() == 0 {
		t.Error("decodeLoop applied frames but never stamped the last-frame liveness signal")
	}
}

func TestDecodeLoopFencesHistoricalReceiptsFromEveryLiveAudience(t *testing.T) {
	for _, kind := range []string{"navigation", "rf", "board"} {
		for _, age := range []string{"fresh", "replay", "unstamped", "future", "queued"} {
			t.Run(kind+"/"+age, func(t *testing.T) {
				now := time.Now()
				c := identity.NewPrivateContext("freshness-observer", identity.CredentialToken)
				c.OrganizationID = "freshness-org"
				c.CollectionIDs = []string{"freshness-fleet"}
				c.Publication.AggregateUse = identity.AggregatePublicAttributed
				c.Publication.StationMetadata = identity.MetadataFull
				c.Publication.EventVisibility = identity.EventsPublic
				f := lifecycleCNAV(10, now, c)
				switch kind {
				case "rf":
					f.Words = nil
					f.RF = &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 100}}}
				case "board":
					f.Words = nil
					f.Details = &ingest.ObserverDetails{UptimeMS: 100}
				}
				switch age {
				case "replay":
					f.Recv = now.Add(-12 * time.Hour)
				case "unstamped":
					f.RecvStamped = false
				case "future":
					f.Recv = now.Add(time.Minute)
				case "queued":
					f.Recv = now.Add(-2 * time.Minute)
					f.RecvLocal = f.Recv
				}
				live, public, events := state.New(1), state.NewProjection(1), state.NewProjection(1)
				registry := audience.NewRegistry(1, nil)
				frames := make(chan *ingest.RawFrame, 1)
				frames <- f
				close(frames)
				var last atomic.Int64
				decodeLoop(frames, live, public, events, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), &last, registry, nil, nil)
				if age == "fresh" {
					if last.Load() == 0 || len(registry.Views()) != 2 {
						t.Fatal("fresh receipt did not reach private projections or liveness")
					}
					return
				}
				if last.Load() != 0 || len(registry.Views()) != 0 {
					t.Fatal("historical receipt refreshed liveness or created private projection")
				}
				for _, s := range []*state.Store{live, public, events} {
					if len(s.MonitoringSatellites(now)) != 0 || len(s.FeedStationRF(now)) != 0 || len(s.FeedStationBoards(now)) != 0 {
						t.Fatal("historical receipt contaminated live state")
					}
				}
			})
		}
	}
}
