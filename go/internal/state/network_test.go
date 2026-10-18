package state

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

func TestNetworkIndependentCadenceReplayAndReboot(t *testing.T) {
	s := New(1)
	now := time.Now()
	apply := func(session string, seq uint64, d *ingest.ObserverDetails, stamp time.Time) {
		s.Apply(&ingest.RawFrame{Source: "board", Session: session, Seq: seq, HasSeq: true, Recv: stamp, RecvLocal: now, BoardSampleStamped: true, Details: d})
	}
	apply("boot-a", 1, &ingest.ObserverDetails{UptimeMS: 100, Firmware: "test"}, now)
	apply("boot-a", 2, &ingest.ObserverDetails{UptimeMS: 200, Timing: &ingest.BoardTiming{}}, now)
	apply("boot-a", 3, &ingest.ObserverDetails{UptimeMS: 300, Network: &ingest.BoardNetwork{EventTotal: 42}}, now.Add(-time.Hour))
	b := s.FeedStationBoards(now)["board"]
	if b.Latest.Sequence != 1 || b.Timing.Sequence != 2 || b.Network.Sequence != 3 || !b.NetworkStale || s.LiveReceivers(now) != 0 {
		t.Fatalf("network damaged board state: %+v", b)
	}
	apply("boot-a", 2, &ingest.ObserverDetails{UptimeMS: 300, Network: &ingest.BoardNetwork{EventTotal: 1}}, now)
	if s.FeedStationBoards(now)["board"].Network.Details.Network.EventTotal != 42 {
		t.Fatal("replay replaced network")
	}
	apply("boot-b", 1, &ingest.ObserverDetails{UptimeMS: 1, Firmware: "test"}, now)
	if s.FeedStationBoards(now)["board"].Network != nil {
		t.Fatal("new boot retained old network counters")
	}
}
