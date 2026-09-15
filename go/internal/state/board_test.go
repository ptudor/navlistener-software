package state

import (
	"github.com/ptudor/navlistener/internal/ingest"
	"testing"
	"time"
)

func TestBoardReportLifecycleReplayAndBaseline(t *testing.T) {
	s := New(1)
	now := time.Now()
	f := &ingest.RawFrame{Source: "board", Session: "boot-a", Seq: 1, HasSeq: true, Recv: now, RecvLocal: now, Details: &ingest.ObserverDetails{UptimeMS: 100, Reason: 1}}
	s.Apply(f)
	if s.LiveReceivers(now) != 0 || len(s.FeedSVs(now)) != 0 {
		t.Fatal("environment invented GNSS liveness")
	}
	f.Seq = 2
	f.RecvLocal = now.Add(time.Minute)
	f.Details = &ingest.ObserverDetails{UptimeMS: 60100, Reason: 8, EventCount: 1}
	s.Apply(f)
	b := s.FeedStationBoards(now)["board"]
	if b.LastInterference == nil || b.LastInterference.Before == nil || b.LastInterference.Before.Sequence != 1 || b.Latest.Sequence != 2 || b.Latest.SampleTime != nil {
		t.Fatalf("missing pre-event context: %+v", b)
	}
	f.Seq = 1
	f.RecvLocal = now.Add(2 * time.Minute)
	s.Apply(f)
	if s.FeedStationBoards(now)["board"].Latest.Sequence != 2 {
		t.Fatal("replay replaced latest")
	}
	if !s.FeedStationBoards(now.Add(13 * time.Minute))["board"].Stale {
		t.Fatal("heartbeat never expires")
	}
	f.Session = "boot-b"
	f.Details = &ingest.ObserverDetails{UptimeMS: 1}
	s.Apply(f)
	if s.FeedStationBoards(now)["board"].LastInterference != nil {
		t.Fatal("new boot retained old baseline")
	}
	s.ExpireStations(now.Add(2 * time.Hour))
	if len(s.FeedStationBoards(now)) != 0 {
		t.Fatal("idle entries leak")
	}
	s.Apply(f)
	s.Reset()
	if len(s.FeedStationBoards(now)) != 0 {
		t.Fatal("scope reset leaked board")
	}
}
func TestBoardStampedReplayIsStale(t *testing.T) {
	s := New(1)
	now := time.Now()
	s.Apply(&ingest.RawFrame{Source: "board", Recv: now.Add(-time.Hour), RecvLocal: now, BoardSampleStamped: true, Details: &ingest.ObserverDetails{}})
	b := s.FeedStationBoards(now)["board"]
	if !b.Stale || b.Latest.SampleTime == nil || b.Latest.ReceivedAt != now {
		t.Fatal("replay age concealed")
	}
}
