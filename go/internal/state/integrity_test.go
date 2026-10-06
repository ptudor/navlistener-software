package state

import (
	"slices"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
)

var integrityT0 = time.Unix(1_800_000_000, 0)

const integrityTOW0 = 345_600_000

// solutionFrame is a push-shaped receiver solution i seconds into the run, at a
// fixed antenna, with a clock drifting 25 ns/s and the receiver's spoofing state.
func solutionFrame(i int, stamped bool, spoof uint8) *ingest.RawFrame {
	local := integrityT0.Add(time.Duration(i) * time.Second)
	tow := uint32(integrityTOW0 + i*1000)
	utc := local.Add(-250 * time.Millisecond).UTC()
	sol := &ingest.ReceiverSolution{
		PVT: &ingest.SolutionPVT{
			TOWMS: tow, Year: uint16(utc.Year()), Month: uint8(utc.Month()), Day: uint8(utc.Day()),
			Hour: uint8(utc.Hour()), Minute: uint8(utc.Minute()), Second: uint8(utc.Second()), NanoNS: int32(utc.Nanosecond()),
			UTCValid: ingest.UTCValidDate | ingest.UTCValidTime | ingest.UTCValidFullyResolved, TAccNS: 25,
			FixType: 3, FixFlags: ingest.FixFlagOK, NumSV: 15,
			LatE7: 374219000, LonE7: -1220841000, HeightMM: 12500, HAccMM: 1500, VAccMM: 2500, SAccMMS: 90, PDOPx100: 140,
		},
		Clock:  &ingest.SolutionClock{TOWMS: tow, BiasNS: int32(1000 + 25*i), DriftNSS: 25, TAccNS: 25, FAccPSS: 300},
		Status: &ingest.SolutionStatus{TOWMS: tow, FixType: 3, Flags: 0x0d, SpoofState: spoof, SinceStart: uint32(5_000_000 + i*1000)},
	}
	return &ingest.RawFrame{Source: "board-0001-aa", Recv: local, RecvLocal: local, RecvStamped: stamped,
		MsgType: ingest.TelemReceiverSolution, Solution: sol}
}

func integrityResult(t *testing.T, a integrity.Assessment, name string) integrity.Result {
	t.Helper()
	for _, r := range a.Checks {
		if r.Check == name {
			return r
		}
	}
	t.Fatalf("no %s check in %+v", name, a.Checks)
	return integrity.Result{}
}

func fixedSite() integrity.StationProfile {
	return integrity.StationProfile{Mode: integrity.ModeFixed, Position: &integrity.Surveyed{LatDeg: 37.4219, LonDeg: -122.0841, HeightM: 12.5}}
}

func TestStoreIntegrityFromSolutions(t *testing.T) {
	s := New(1)
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{"board-0001-aa": fixedSite()})
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrity(cfg)
	for i := 0; i < 130; i++ {
		s.Apply(solutionFrame(i, true, 1))
	}
	a, ok := s.FeedStationIntegrity(integrityT0.Add(130 * time.Second))["board-0001-aa"]
	if !ok {
		t.Fatal("no assessment for the station")
	}
	if a.Mode != integrity.ModeFixed || !a.Surveyed {
		t.Fatalf("installation not applied: %+v", a)
	}
	for _, name := range []string{integrity.CheckStaticPosition, integrity.CheckStationaryVelocity, integrity.CheckPositionVelocity,
		integrity.CheckClockBiasDrift, integrity.CheckClockDriftRate, integrity.CheckUTCOffset, integrity.CheckReceiverSpoofing} {
		if r := integrityResult(t, a, name); r.State != integrity.Assured {
			t.Errorf("%s = %s %v, want assured", name, r.State, r.Reasons)
		}
	}
	if got := integrityResult(t, a, integrity.CheckStaticPosition).Metrics["horizontal_m"]; got > 0.01 {
		t.Errorf("antenna on its survey is %g m off", got)
	}
}

// TestStoreIntegrityHostStamp: only an observer-stamped push frame (or a dial frame)
// gives the UTC check a reference; an unstamped push arrival time may carry backlog.
func TestStoreIntegrityHostStamp(t *testing.T) {
	s := New(1)
	for i := 0; i < 5; i++ {
		s.Apply(solutionFrame(i, false, 1))
	}
	r := integrityResult(t, s.FeedStationIntegrity(integrityT0.Add(5 * time.Second))["board-0001-aa"], integrity.CheckUTCOffset)
	if r.State != integrity.Unavailable || !slices.Contains(r.Reasons, integrity.ReasonNoHostStamp) {
		t.Fatalf("unstamped push frame: %+v", r)
	}
	dial := New(1)
	for i := 0; i < 5; i++ {
		f := solutionFrame(i, false, 1)
		f.RecvLocal = time.Time{} // dial-mode frames stamp Recv from this host
		dial.Apply(f)
	}
	r = integrityResult(t, dial.FeedStationIntegrity(integrityT0.Add(5 * time.Second))["board-0001-aa"], integrity.CheckUTCOffset)
	if r.State != integrity.Assured {
		t.Fatalf("dial frame: %+v", r)
	}
}

// TestStoreIntegrityStatusPreferredOverBoard: the epoch-rate status block decides the
// receiver's spoofing state; the low-rate board report is used only without it.
func TestStoreIntegrityStatusPreferredOverBoard(t *testing.T) {
	board := func(i int, spoof uint8) *ingest.RawFrame {
		at := integrityT0.Add(time.Duration(i) * time.Second)
		return &ingest.RawFrame{Source: "board-0001-aa", Recv: at, RecvLocal: at, Session: "s1",
			Details: &ingest.ObserverDetails{Version: 1, UptimeMS: uint64(10_000 + i*1000), Receiver: &ingest.BoardReceiver{Spoofing: spoof}}}
	}
	s := New(1)
	for i := 0; i < 10; i++ {
		s.Apply(solutionFrame(i, true, 1))
		s.Apply(board(i, 3))
	}
	if r := integrityResult(t, s.FeedStationIntegrity(integrityT0.Add(10 * time.Second))["board-0001-aa"], integrity.CheckReceiverSpoofing); r.State != integrity.Assured {
		t.Fatalf("board report overrode the status block: %+v", r)
	}
	boardOnly := New(1)
	for i := 0; i < 4; i++ {
		boardOnly.Apply(board(i*60, 3))
	}
	if r := integrityResult(t, boardOnly.FeedStationIntegrity(integrityT0.Add(4 * time.Minute))["board-0001-aa"], integrity.CheckReceiverSpoofing); r.State != integrity.Unassured {
		t.Fatalf("board-only spoofing state: %+v", r)
	}
}

func TestStoreIntegrityFromRF(t *testing.T) {
	s := New(1)
	at := integrityT0
	for i := 0; i < 5; i++ {
		f := &ingest.RawFrame{Source: "dial-1", Recv: at, RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 5000, JamState: 1, AntStatus: 2}}}}
		s.Apply(f)
		sats := make([]ingest.SatCN0, 0, 8)
		for sv := 1; sv <= 8; sv++ {
			sats = append(sats, ingest.SatCN0{GnssID: 0, SvID: sv, Cn0: 30 + sv*2, ElevDeg: 10 + sv*9})
		}
		s.Apply(&ingest.RawFrame{Source: "dial-1", Recv: at, RF: &ingest.RawRF{Sats: sats}})
		at = at.Add(time.Second)
	}
	a := s.FeedStationIntegrity(at)["dial-1"]
	if r := integrityResult(t, a, integrity.CheckAGC); r.State != integrity.Assured {
		t.Fatalf("quiet front end: %+v", r)
	}
	if r := integrityResult(t, a, integrity.CheckCn0Uniformity); r.State != integrity.Assured {
		t.Fatalf("sky with an elevation trend: %+v", r)
	}
	// Lower-only checks alone cannot assure a station.
	if a.State != integrity.Unavailable {
		t.Fatalf("station with only lower-only checks = %s, want unavailable", a.State)
	}
}

func TestStoreIntegrityResetAndEviction(t *testing.T) {
	s := New(1)
	s.Apply(solutionFrame(0, true, 1))
	s.Reset()
	if n := len(s.FeedStationIntegrity(integrityT0)); n != 0 {
		t.Fatalf("Reset kept %d integrity stations", n)
	}
	s.Apply(solutionFrame(0, true, 1))
	if n := len(s.FeedStationIntegrity(integrityT0.Add(integrityEvictAfter))); n != 1 {
		t.Fatalf("evicted at exactly the bound: %d", n)
	}
	if n := len(s.FeedStationIntegrity(integrityT0.Add(integrityEvictAfter + time.Second))); n != 0 {
		t.Fatalf("idle station kept: %d", n)
	}
}

func TestNewIntegrityConfigValidates(t *testing.T) {
	if _, err := NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{"x": {Mode: "orbital"}}); err == nil {
		t.Fatal("invalid installation accepted")
	}
	bad := integrity.DefaultProfile()
	bad.StaleAfter = 0
	if _, err := NewIntegrityConfig(bad, nil); err == nil {
		t.Fatal("invalid profile accepted")
	}
	// The config owns its copy: a caller mutating its map or position afterwards
	// cannot change what stores evaluate.
	site := fixedSite()
	stations := map[string]integrity.StationProfile{"a": site}
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), stations)
	if err != nil {
		t.Fatal(err)
	}
	site.Position.LatDeg = 0
	stations["b"] = site
	if len(cfg.stations) != 1 || cfg.stations["a"].Position.LatDeg != 37.4219 {
		t.Fatalf("config shares caller state: %+v", cfg.stations)
	}
}

// TestSolutionFrameNotCountedAsNavigation guards the routing: a solution frame
// carries no satellite header and must never reach the navigation decoders.
func TestSolutionFrameNotCountedAsNavigation(t *testing.T) {
	s := New(1)
	s.Apply(solutionFrame(0, true, 1))
	if n := len(s.FeedSVs(integrityT0)); n != 0 {
		t.Fatalf("solution frame created %d satellite entries", n)
	}
}
