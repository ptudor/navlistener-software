package ingest

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/reception"
	"github.com/ptudor/navlistener/internal/wire"
)

func TestLiveFreshnessKeepsHistorySeparateFromArrival(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name  string
		stamp int64
		want  string
	}{
		{"fresh", now.UnixNano(), ""},
		{"delayed", now.Add(-30 * time.Second).UnixNano(), ""},
		{"replay", now.Add(-12 * time.Hour).UnixNano(), "stale"},
		{"missing", 0, "unstamped"},
		{"negative", -1, "unstamped"},
		{"too old", now.Add(-8 * 24 * time.Hour).UnixNano(), "unstamped"},
		{"future", now.Add(time.Minute).UnixNano(), "future"},
		{"invalid future", now.Add(time.Hour).UnixNano(), "unstamped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := []wire.RawRecord{
				{RecvUnixNs: tc.stamp, GnssID: gnss.GPS, SvID: 5, FrameType: 0x10, Raw: make([]byte, 40)},
				{RecvUnixNs: tc.stamp, FrameType: TelemJammingStats, Raw: EncodeJammingStats([]RFBand{{Block: 0, AGC: 100}})},
			}
			for _, rec := range records {
				f := recordToFrame(rec, "ubx", "freshness-test")
				if f == nil {
					t.Fatal("receipt discarded instead of retained for history")
				}
				if got := f.LiveRejection(time.Now()); got != tc.want {
					t.Fatalf("live rejection = %q, want %q", got, tc.want)
				}
				if tc.want == "stale" && f.Recv.UnixNano() != tc.stamp {
					t.Fatal("replay's original reception time changed")
				}
			}
		})
	}
	f := &RawFrame{Recv: now, RecvLocal: now, RecvStamped: true}
	if f.LiveRejection(now.Add(LiveMaxAge)) != "" || f.LiveRejection(now.Add(LiveMaxAge+time.Nanosecond)) != "stale" {
		t.Fatal("freshness window does not include queueing time")
	}
	f.Recv = now.Add(time.Second)
	if f.LiveRejection(now) != "" || !f.ObservationTime().Equal(now) {
		t.Fatal("rounding tolerance extended the observation window")
	}
	f.Recv = now.Add(time.Second + time.Nanosecond)
	if f.LiveRejection(now) != "future" {
		t.Fatal("future stamp entered live state")
	}
}

func TestReplayedReceptionReportDoesNotReachLiveCoordinator(t *testing.T) {
	now := time.Now()
	f := &RawFrame{Source: "history", Recv: now.Add(-time.Hour), RecvLocal: now, RecvStamped: true,
		Details: &ObserverDetails{Reception: &reception.Sample{ExpectationID: 1}}}
	checks := &panickingCheck{remaining: 1}
	p := &PushServer{reception: checks}
	if p.checkReception(f, identity.NewPrivateContext("history", identity.CredentialToken), "boot") || checks.remaining != 1 {
		t.Fatal("historical report reached the live reception coordinator")
	}
}

type recordingUpdateCoordinator struct{ reports int }

func (*recordingUpdateCoordinator) BeginSession(identity.ObserverContext, string) {}
func (*recordingUpdateCoordinator) Pending(identity.ObserverContext) []byte       { return nil }
func (c *recordingUpdateCoordinator) Report(identity.ObserverContext, string, uint64, wire.UpdateStatus) error {
	c.reports++
	return nil
}

func TestReplayedUpdateReportDoesNotReachLiveCoordinator(t *testing.T) {
	now := time.Now()
	updates := &recordingUpdateCoordinator{}
	p := &PushServer{updates: updates}
	status := &wire.UpdateStatus{State: "idle"}
	f := &RawFrame{Source: "history", Recv: now.Add(-time.Hour), RecvLocal: now, RecvStamped: true,
		Details: &ObserverDetails{Update: status}}
	if p.reportUpdate(f, identity.NewPrivateContext("history", identity.CredentialToken), "boot", 1) || updates.reports != 0 {
		t.Fatal("historical update report reached the live update coordinator")
	}
	f.Recv = now
	if p.reportUpdate(f, identity.NewPrivateContext("history", identity.CredentialToken), "boot", 2) || updates.reports != 1 {
		t.Fatal("fresh update report did not reach the live update coordinator")
	}
}
