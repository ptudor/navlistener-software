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

// setPVTUTC writes at (as UTC) into the solution's date and time fields.
func setPVTUTC(p *ingest.SolutionPVT, at time.Time) {
	utc := at.UTC()
	p.Year, p.Month, p.Day = uint16(utc.Year()), uint8(utc.Month()), uint8(utc.Day())
	p.Hour, p.Minute, p.Second, p.NanoNS = uint8(utc.Hour()), uint8(utc.Minute()), uint8(utc.Second()), int32(utc.Nanosecond())
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

	// A live stamped push frame (received within the no-backlog bound of its
	// stamp) is measured against the collector's own receipt clock, not the
	// observer's stamp, so an observer clock drifting after an NTP outage does
	// not become the time reference; the verdict records which clock it used.
	live := New(1)
	for i := 0; i < 5; i++ {
		f := solutionFrame(i, true, 1)
		f.Recv = f.RecvLocal.Add(-1500 * time.Millisecond) // the observer's clock lags a little
		live.Apply(f)
	}
	r = integrityResult(t, live.FeedStationIntegrity(integrityT0.Add(5 * time.Second))["board-0001-aa"], integrity.CheckUTCOffset)
	if r.State != integrity.Assured || r.Metrics["reference_local"] != 1 {
		t.Fatalf("live stamped frame against the collector clock: %+v", r)
	}

	// A stamp running ahead of the receipt clock is impossible for a genuine
	// record: no reference at all, rather than an unassured vote.
	ahead := New(1)
	for i := 0; i < 5; i++ {
		f := solutionFrame(i, true, 1)
		f.Recv = f.RecvLocal.Add(10 * time.Second)
		ahead.Apply(f)
	}
	r = integrityResult(t, ahead.FeedStationIntegrity(integrityT0.Add(5 * time.Second))["board-0001-aa"], integrity.CheckUTCOffset)
	if r.State != integrity.Unavailable || !slices.Contains(r.Reasons, integrity.ReasonNoHostStamp) {
		t.Fatalf("stamp 10 s ahead of receipt: %+v, want unavailable/no_host_stamp", r)
	}

	// A replayed frame (stamped three hours before it arrived) still uses the
	// observer's stamp: its receipt time says nothing about the record's UTC.
	replay := New(1)
	for i := 0; i < 5; i++ {
		f := solutionFrame(i, true, 1)
		f.Recv = f.RecvLocal.Add(-3 * time.Hour)
		setPVTUTC(f.Solution.PVT, f.Recv.Add(-250*time.Millisecond)) // the receiver's UTC at the record's true instant
		replay.Apply(f)
	}
	r = integrityResult(t, replay.FeedStationIntegrity(integrityT0.Add(5 * time.Second))["board-0001-aa"], integrity.CheckUTCOffset)
	if r.State != integrity.Assured || r.Metrics["reference_local"] != 0 {
		t.Fatalf("replayed stamped frame against the observer stamp: %+v", r)
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
	// Eleven minutes: the AGC baseline needs ten quiet minutes before it is served.
	for i := 0; i < 660; i++ {
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

// navSatFrame is one NAV-SAT epoch of eight used GPS signals, lowered by dropDB.
func navSatFrame(source string, at time.Time, dropDB int) *ingest.RawFrame {
	sats := make([]ingest.SatCN0, 8)
	for i := range sats {
		sats[i] = ingest.SatCN0{GnssID: 0, SvID: i + 1, Cn0: 36 + i - dropDB, ElevDeg: 20 + 8*i, Used: true}
	}
	return &ingest.RawFrame{Source: source, Recv: at, RF: &ingest.RawRF{Sats: sats}}
}

// TestCn0DropCorroborationLastsThroughTheDeparture: a simultaneous C/N₀ drop at the
// onset of an AGC departure keeps corroborating it after the drop itself has passed,
// until the departure ends; a drop that passed before a departure began does not.
func TestCn0DropCorroborationLastsThroughTheDeparture(t *testing.T) {
	const id = "board-0001-aa"
	s := New(1)
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{id: fixedSite()})
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrity(cfg)
	at := integrityT0
	step := func(agc, dropDB int) {
		s.Apply(rfSample(id, 0, agc, 0, 2, at))
		s.Apply(navSatFrame(id, at, dropDB))
		at = at.Add(time.Second)
	}
	for i := 0; i < 11*60; i++ { // the AGC baseline is established
		step(4000+i%5, 0)
	}
	if s.FeedStationRF(at)[id].Cn0Drop {
		t.Fatal("quiet station reports a C/N0 drop")
	}
	for i := 0; i < 10; i++ { // the jammer comes on: AGC down, every signal 5 dB down
		step(3000, 5)
	}
	if !s.FeedStationRF(at)[id].Cn0Drop {
		t.Fatal("onset drop not reported")
	}
	for i := 0; i < 15*60; i++ { // it stays on; C/N0 holds at the lower level
		step(3000, 5)
	}
	if served, _ := s.integrity[id].eval.Served(integrity.CheckCn0Drop, at); served != integrity.Assured {
		t.Fatalf("cn0_drop still served %s fifteen minutes after the step", served)
	}
	rf := s.FeedStationRF(at)[id]
	if !rf.Cn0Drop {
		t.Fatal("corroboration lapsed while the departure continues")
	}
	if r := integrityResult(t, s.FeedStationIntegrity(at)[id], integrity.CheckAGC); r.State != integrity.Unassured || r.Metrics["cn0_drop"] != 1 {
		t.Fatalf("agc = %s %v, want unassured by the corroborated departure", r.State, r.Metrics)
	}
	for i := 0; i < 30; i++ { // the jammer goes away
		step(4000, 5)
	}
	if s.FeedStationRF(at)[id].Cn0Drop {
		t.Fatal("corroboration outlived the departure")
	}
	for i := 0; i < 15*60; i++ { // a later departure without a drop of its own
		step(3000, 5)
	}
	if s.FeedStationRF(at)[id].Cn0Drop {
		t.Fatal("an earlier drop corroborated a later departure")
	}
}

// TestBaselinePairsMatchedEpochs: two paired stations' epochs are matched by GPS time
// and evaluated for both; when one transmitter puts both at one position, both are
// unassured with a collapse. A one-sided pairing is never evaluated.
func TestBaselinePairsMatchedEpochs(t *testing.T) {
	const a, b = "board-0001-aa", "board-0002-bb"
	pair := func(partner string) integrity.StationProfile {
		return integrity.StationProfile{Baseline: &integrity.Baseline{Partner: partner, DistanceM: 50}}
	}
	s := New(1)
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{a: pair(b), b: pair(a)})
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrity(cfg)
	// 50 m east at this latitude, in 1e-7 degrees of longitude.
	const east50 = 5654
	partnerAt := func(i int, eastE7 int32) *ingest.RawFrame {
		f := solutionFrame(i, true, 1)
		f.Source = b
		f.Solution.PVT.LonE7 += eastE7
		return f
	}
	for i := 0; i < 10; i++ {
		s.Apply(solutionFrame(i, true, 1))
		s.Apply(partnerAt(i, east50))
	}
	at := integrityT0.Add(10 * time.Second)
	for _, id := range []string{a, b} {
		r := integrityResult(t, s.FeedStationIntegrity(at)[id], integrity.CheckBaseline)
		if r.State != integrity.Assured || r.Metrics["measured_m"] < 49 || r.Metrics["measured_m"] > 51 {
			t.Fatalf("%s apart = %s %v", id, r.State, r.Metrics)
		}
	}
	for i := 10; i < 16; i++ { // the partner's epoch arrives first now
		s.Apply(partnerAt(i, 0))
		s.Apply(solutionFrame(i, true, 1))
	}
	at = integrityT0.Add(16 * time.Second)
	for _, id := range []string{a, b} {
		r := integrityResult(t, s.FeedStationIntegrity(at)[id], integrity.CheckBaseline)
		if r.State != integrity.Unassured || len(r.Reasons) == 0 || r.Reasons[0] != integrity.ReasonBaselineCollapse {
			t.Fatalf("%s collapsed = %s %v %v", id, r.State, r.Reasons, r.Metrics)
		}
	}
	t.Run("partner at arrival-skew bound", func(t *testing.T) {
		delayed := New(1)
		delayed.SetIntegrity(cfg)
		const lag = 30
		for i := 0; i < lag+16; i++ {
			delayed.Apply(solutionFrame(i, true, 1))
			if i < lag {
				continue
			}
			east := int32(east50)
			if i >= lag+10 {
				east = 0
			}
			f := partnerAt(i-lag, east)
			f.RecvLocal = integrityT0.Add(time.Duration(i) * time.Second)
			delayed.Apply(f)
			if i != lag+9 && i != lag+15 {
				continue
			}
			want := integrity.Assured
			if east == 0 {
				want = integrity.Unassured
			}
			for _, id := range []string{a, b} {
				r := integrityResult(t, delayed.FeedStationIntegrity(f.RecvLocal)[id], integrity.CheckBaseline)
				if r.State != want || r.EvaluatedAt != f.RecvLocal.Unix() {
					t.Fatalf("%s delayed pair = %+v, want %s at %d", id, r, want, f.RecvLocal.Unix())
				}
			}
		}
	})

	oneSided := New(1)
	cfg, err = NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{a: pair(b)})
	if err != nil {
		t.Fatal(err)
	}
	oneSided.SetIntegrity(cfg)
	for i := 0; i < 10; i++ {
		oneSided.Apply(solutionFrame(i, true, 1))
		oneSided.Apply(partnerAt(i, 0))
	}
	if r := integrityResult(t, oneSided.FeedStationIntegrity(integrityT0.Add(10 * time.Second))[a], integrity.CheckBaseline); r.State != integrity.Unavailable || r.EvaluatedAt != 0 {
		t.Fatalf("one-sided pair evaluated: %+v", r)
	}
}

// rfSampleJam is rfSample with the receiver's own jam flag set, so a station shows
// two independent signs of interference (departure + flag) — corroborated evidence.
func rfSampleJam(source string, agc, jam int, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{
		Source: source, Recv: recv,
		RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: agc, AntStatus: 2, JamState: jam}}},
	}
}

// TestNeighbourInterferenceCorroborates: stations within the neighbour radius whose
// own interference evidence is corroborated (two signs, or a severe collapse)
// corroborate each other's departure, by surveyed position or, for a station
// configured mobile, a recent fix; a departed station farther away does not, and a
// neighbour alone corroborates nothing for a quiet station. The gates on the
// corroborator: a lone departure is a degradation and corroborates nothing; a
// station whose own assessment is unassured or indicates spoofing is excluded; a
// fixed installation is never relocated by a reported fix, and a fixed station
// without a survey has no location; a C/N₀ drop counts only within the neighbour
// window of its last evaluation, not for as long as the served state is held.
func TestNeighbourInterferenceCorroborates(t *testing.T) {
	at := func(latDeg float64) *integrity.Surveyed {
		return &integrity.Surveyed{LatDeg: latDeg, LonDeg: -122.0841, HeightM: 12.5}
	}
	s := New(1)
	cfg, err := NewIntegrityConfig(integrity.DefaultProfile(), map[string]integrity.StationProfile{
		"near-a":        {Mode: integrity.ModeFixed, Position: at(37.4219)},
		"near-b":        {Mode: integrity.ModeFixed, Position: at(37.4219 + 0.0899)}, // about 10 km north
		"dropper":       {Mode: integrity.ModeFixed, Position: at(37.4219 + 0.045)},  // about 5 km north
		"lone":          {Mode: integrity.ModeFixed, Position: at(37.4219 + 0.018)},  // about 2 km north
		"far":           {Mode: integrity.ModeFixed, Position: at(37.4219 + 0.899)},  // about 100 km north
		"quiet":         {Mode: integrity.ModeFixed, Position: at(37.4219 + 0.01)},
		"unsurveyed":    {Mode: integrity.ModeFixed}, // fixed, no survey: no location
		"board-0001-aa": {Mode: integrity.ModeMobile, MaxSpeedMPS: 30},
		"rover":         {Mode: integrity.ModeMobile, MaxSpeedMPS: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SetIntegrity(cfg)
	stations := []string{"near-a", "near-b", "dropper", "lone", "far", "quiet", "unsurveyed", "board-0001-aa", "rover"}
	corroborated := map[string]bool{"near-a": true, "near-b": true, "unsurveyed": true, "board-0001-aa": true, "rover": true}
	second := 0
	now := func() time.Time { return integrityT0.Add(time.Duration(second) * time.Second) }
	fixFor := func(source string, spoof uint8, moved bool) {
		f := solutionFrame(second, true, spoof)
		f.Source = source
		if moved {
			f.Solution.PVT.LatE7 += 27_000 // about 300 m north: the static position check trips
		}
		s.Apply(f)
	}
	// jammerOn feeds one second of the jammer: every station but quiet loses gain;
	// the corroborated stations also raise their receiver's jam flag, dropper shows
	// a C/N₀ drop while dropping is set, lone and far show the departure alone.
	jammerOn := func(dropping bool) {
		for _, id := range stations {
			switch {
			case id == "quiet":
				s.Apply(rfSample(id, 0, 4000, 0, 2, now()))
			case corroborated[id]:
				s.Apply(rfSampleJam(id, 3000, 2, now()))
			default:
				s.Apply(rfSample(id, 0, 3000, 0, 2, now()))
			}
		}
		if dropping {
			s.Apply(navSatFrame("dropper", now(), 5))
		}
		second++
	}

	for ; second < 11*60; second++ { // every station learns its AGC baseline
		for _, id := range stations {
			s.Apply(rfSample(id, 0, 4000+second%5, 0, 2, now()))
		}
		s.Apply(navSatFrame("dropper", now(), 0))
	}
	// The mobile stations' fixes put them beside near-a (a solution every second,
	// so their position checks have an assured history). The fixed stations far
	// and unsurveyed report the same fix: far keeps its surveyed position,
	// unsurveyed gains no location.
	for _, id := range []string{"far", "unsurveyed"} {
		fixFor(id, 1, false)
	}
	var roverFixAt time.Time
	for i := 0; i < 20; i++ {
		fixFor("board-0001-aa", 1, false)
		fixFor("rover", 1, false)
		roverFixAt = now()
		jammerOn(true)
	}
	rf := s.FeedStationRF(now())
	for id, want := range map[string][]string{
		"near-a":        {"board-0001-aa", "dropper", "near-b", "rover"},
		"near-b":        {"board-0001-aa", "dropper", "near-a", "rover"},
		"board-0001-aa": {"dropper", "near-a", "near-b", "rover"},
		"lone":          {"board-0001-aa", "dropper", "near-a", "near-b", "rover"}, // corroborated by others, never a corroborator
		"far":           nil,                                                       // 100 km away, and a lone departure
		"quiet":         nil,                                                       // it has no departure of its own
		"unsurveyed":    nil,                                                       // no location without a survey
	} {
		if got := rf[id].Neighbours; !slices.Equal(got, want) {
			t.Errorf("%s neighbours = %v, want %v", id, got, want)
		}
		if got := rf[id].NeighbourCount; got != len(want) {
			t.Errorf("%s neighbour count = %d, want %d", id, got, len(want))
		}
	}
	assess := s.FeedStationIntegrity(now())
	if r := integrityResult(t, assess["near-a"], integrity.CheckAGC); r.State != integrity.Unassured ||
		!slices.Contains(r.Reasons, integrity.ReasonNeighbourInterference) || r.Metrics["neighbours"] != 4 {
		t.Fatalf("near-a agc = %s %v %v", r.State, r.Reasons, r.Metrics)
	}
	if r := integrityResult(t, assess["far"], integrity.CheckAGC); r.State != integrity.Inconsistent {
		t.Fatalf("far agc = %s %v, want a lone departure", r.State, r.Reasons)
	}

	// The board's receiver flags spoofing while its position moves 300 m: spoofing
	// indicated, so it no longer corroborates anyone. Dropper's NAV-SAT stops here.
	for i := 0; i < 10; i++ {
		fixFor("board-0001-aa", 2, true)
		jammerOn(false)
	}
	if got := s.FeedStationRF(now())["near-a"].Neighbours; !slices.Equal(got, []string{"dropper", "near-b", "rover"}) {
		t.Fatalf("near-a neighbours with the board spoofed = %v, want dropper, near-b and rover", got)
	}

	// Six minutes on, dropper's last drop evaluation is outside the neighbour
	// window although its served cn0_drop state is still held: a lone departure.
	for i := 0; i < 6*60; i++ {
		jammerOn(false)
	}
	if got := s.FeedStationRF(now())["near-a"].Neighbours; !slices.Equal(got, []string{"near-b", "rover"}) {
		t.Fatalf("near-a neighbours six minutes after dropper's drop = %v, want near-b and rover", got)
	}

	// The rover's fix ages out of the neighbour profile.
	for now().Sub(roverFixAt) <= integrity.DefaultProfile().Neighbour.LocationMaxAge {
		jammerOn(false)
	}
	if got := s.FeedStationRF(now())["near-a"].Neighbours; !slices.Equal(got, []string{"near-b"}) {
		t.Fatalf("near-a neighbours after the rover's fix aged = %v, want near-b", got)
	}
}
