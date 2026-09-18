package state

import (
	"encoding/json"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/wire"
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

func TestIndependentTimingAndEnvironmentCadence(t *testing.T) {
	s := New(1)
	now := time.Now()
	apply := func(seq uint64, d *ingest.ObserverDetails) {
		s.Apply(&ingest.RawFrame{Source: "board", Session: "boot-a", HasSeq: true, Seq: seq,
			Recv: now, RecvLocal: now, Details: d})
	}
	apply(1, &ingest.ObserverDetails{UptimeMS: 100, Firmware: "test"})
	apply(2, &ingest.ObserverDetails{UptimeMS: 200, Timing: &ingest.BoardTiming{}})
	apply(3, &ingest.ObserverDetails{UptimeMS: 300, EventCount: 1, Reason: 8})
	apply(4, &ingest.ObserverDetails{UptimeMS: 400, Timing: &ingest.BoardTiming{}})
	b := s.FeedStationBoards(now)["board"]
	if b.Latest.Sequence != 3 || b.Timing.Sequence != 4 || b.LastInterference.Before.Sequence != 1 || b.LastInterference.Snapshot.Sequence != 3 {
		t.Fatalf("timing overwrote environment/event baseline: %+v", b)
	}
	apply(3, &ingest.ObserverDetails{UptimeMS: 400, Timing: &ingest.BoardTiming{}})
	if s.FeedStationBoards(now)["board"].Timing.Sequence != 4 {
		t.Fatal("replay replaced timing")
	}
	b = s.FeedStationBoards(now.Add(6 * time.Second))["board"]
	if b.Stale || !b.TimingStale || s.LiveReceivers(now) != 0 {
		t.Fatal("independent freshness/liveness")
	}
	s.Apply(&ingest.RawFrame{Source: "board", Session: "boot-b", RecvLocal: now,
		Details: &ingest.ObserverDetails{UptimeMS: 1, Timing: &ingest.BoardTiming{}}})
	b = s.FeedStationBoards(now)["board"]
	if b.Latest != nil || b.LastInterference != nil || b.Timing == nil || !b.Stale {
		t.Fatal("new timing boot retained old environmental context")
	}
}

// TestBoardSampleCarriesVerifiedHardwareTrust: the board output serves what the
// collector verified for the delivering session beside the device's own update
// report, and says "none" rather than nothing for a source with no evidence.
func TestBoardSampleCarriesVerifiedHardwareTrust(t *testing.T) {
	s := New(1)
	now := time.Now()
	update := &wire.UpdateStatus{Profile: "trusted", Security: 31}
	verified := identity.NewPrivateContext("board", identity.CredentialToken)
	verified.HardwareTrust = identity.HardwareTrustTrusted
	s.Apply(&ingest.RawFrame{Source: "board", Observer: verified, Session: "boot-a", Seq: 1, HasSeq: true,
		Recv: now, RecvLocal: now, Details: &ingest.ObserverDetails{UptimeMS: 100, Update: update}})
	s.Apply(&ingest.RawFrame{Source: "dialled", Session: "boot-a", Seq: 1, HasSeq: true,
		Recv: now, RecvLocal: now, Details: &ingest.ObserverDetails{UptimeMS: 100, Update: update}})
	boards := s.FeedStationBoards(now)
	if got := boards["board"].Update; got == nil || got.HardwareTrust != identity.HardwareTrustTrusted || got.Details.Update.Profile != "trusted" {
		t.Fatalf("verified board update = %+v", got)
	}
	// The same self-report from a source that proved nothing stays a label.
	if got := boards["dialled"].Update; got == nil || got.HardwareTrust != identity.HardwareTrustNone {
		t.Fatalf("unverified board update = %+v", got)
	}
	encoded, err := json.Marshal(boards["board"].Update)
	if err != nil {
		t.Fatal(err)
	}
	var sample struct {
		HardwareTrust string `json:"hardware_trust"`
		Details       struct {
			Update map[string]any `json:"update"`
		} `json:"details"`
	}
	if err := json.Unmarshal(encoded, &sample); err != nil {
		t.Fatal(err)
	}
	if sample.HardwareTrust != "trusted" || sample.Details.Update["trust_profile"] != "trusted" {
		t.Fatalf("served sample = %s", encoded)
	}
	if _, inside := sample.Details.Update["hardware_trust"]; inside {
		t.Fatal("verified trust was served inside the device-reported update")
	}
}
