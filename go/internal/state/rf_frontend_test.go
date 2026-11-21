package state

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
)

// The front ends of a Septentrio receiver in these tests: the FrontEndID of GPS L1 and
// of L5, as a mosaic-G5 reports them.
const (
	feL1 = 0
	feL5 = 5
)

func gainOf(id uint8, gain int) ingest.SBFFrontEnd {
	variance := 100
	return ingest.SBFFrontEnd{ID: id, Code: id & 0x1f, Antenna: id >> 5, Locked: true, GainDB: &gain, SampleVariance: &variance}
}

func unlocked(id uint8) ingest.SBFFrontEnd {
	return ingest.SBFFrontEnd{ID: id, Code: id & 0x1f, Antenna: id >> 5}
}

// statusFrame is one ReceiverStatus report from a station.
func statusFrame(source string, recv time.Time, ends ...ingest.SBFFrontEnd) *ingest.RawFrame {
	return &ingest.RawFrame{Source: source, Recv: recv, MsgType: ingest.SBFReceiverStatusBlock,
		RF: &ingest.RawRF{Status: &ingest.SBFReceiverStatus{FrontEnds: ends}}}
}

// quietGains feeds two front ends at 1 Hz for the given duration: L1 near l1, moving a
// decibel either way as a real receiver's gain does, and L5 steady at l5. It returns
// the instant after the last sample.
func quietGains(s *Store, source string, l1, l5 int, from time.Time, d time.Duration) time.Time {
	at := from
	for ; at.Before(from.Add(d)); at = at.Add(time.Second) {
		s.Apply(statusFrame(source, at, gainOf(feL1, l1+int(at.Unix()%3)-1), gainOf(feL5, l5)))
	}
	return at
}

func frontEnd(t *testing.T, s *Store, source string, id int, at time.Time) StationFrontEnd {
	t.Helper()
	st, ok := s.FeedStationRF(at)[source]
	if !ok {
		t.Fatalf("station %s missing from the RF feed", source)
	}
	for _, e := range st.FrontEnds {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("front end %d missing from %+v", id, st.FrontEnds)
	return StationFrontEnd{}
}

func near(v *float64, want, within float64) bool {
	return v != nil && math.Abs(*v-want) <= within
}

// TestFrontEndBaselineAndGainCut: the baseline learns each front end's quiet gain, and a
// cut is measured against it without being learned, on that front end alone. A real
// cut down-weights the station's votes, as an AGC departure does.
func TestFrontEndBaselineAndGainCut(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	at := quietGains(s, "p3", 40, 36, now, 11*time.Minute)
	quiet := s.FeedStationRF(at)["p3"]
	if len(quiet.Bands) != 0 || len(quiet.FrontEnds) != 2 || quiet.RFTrust != 1 {
		t.Fatalf("quiet station = %+v", quiet)
	}
	l1 := quiet.FrontEnds[0]
	if l1.ID != feL1 || l1.Name != "GPSL1/E1" || l1.Antenna != 0 || !l1.Locked || l1.GainDB == nil ||
		l1.SampleVariance == nil || *l1.SampleVariance != 100 || !near(l1.GainDeparture, 0, 1) {
		t.Fatalf("quiet L1 = %+v", l1)
	}
	if l5 := quiet.FrontEnds[1]; l5.ID != feL5 || l5.Name != "L5/E5a/B2a" || !near(l5.GainDeparture, 0, 0) {
		t.Fatalf("quiet L5 = %+v", l5)
	}

	// Noise arrives on L1: the receiver turns that gain down 9 dB and holds it there.
	for i := 0; i < 120; i++ {
		s.Apply(statusFrame("p3", at.Add(time.Duration(i)*time.Second), gainOf(feL1, 31), gainOf(feL5, 36)))
	}
	cut := at.Add(2 * time.Minute)
	if e := frontEnd(t, s, "p3", feL1, cut); !near(e.GainDeparture, 9, 0.5) || *e.GainDB != 31 {
		t.Fatalf("L1 after a 9 dB cut = %+v, want a departure of about 9", e)
	}
	if e := frontEnd(t, s, "p3", feL5, cut); !near(e.GainDeparture, 0, 0) {
		t.Fatalf("L5 beside a cut L1 = %+v, want no departure", e)
	}
	if st := s.FeedStationRF(cut)["p3"]; st.RFTrust != 0.3 {
		t.Fatalf("rf_trust = %.2f under a gain cut, want 0.3", st.RFTrust)
	}
	if weights := s.sourceVoteWeights(cut); weights["p3"] != 0.3 {
		t.Fatalf("vote weight = %v, want 0.3", weights)
	}
}

// TestFrontEndBaselineWarmUp: no departure is served until ten quiet minutes exist.
func TestFrontEndBaselineWarmUp(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	at := quietGains(s, "p3", 40, 36, now, 9*time.Minute)
	if e := frontEnd(t, s, "p3", feL1, at); e.GainDeparture != nil || e.GainDB == nil {
		t.Fatalf("during warm-up: %+v", e)
	}
	at = quietGains(s, "p3", 40, 36, at, 2*time.Minute)
	if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, 0, 1) {
		t.Fatalf("after warm-up: %+v", e)
	}
}

// TestFrontEndGainRise: a gain that rises is measured as a negative departure. It is
// less power arriving, not interference, so it neither lowers the station's vote nor
// counts as a departure for neighbours. Half an hour of it is not learned: a front
// end's rise is held as long as a cut is (rf_relearn_test.go).
func TestFrontEndGainRise(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	at := quietGains(s, "p3", 40, 36, now, 11*time.Minute)
	// The antenna's amplifier stops: both gains go up 12 dB.
	at = quietGains(s, "p3", 52, 48, at, 5*time.Minute)
	if e := frontEnd(t, s, "p3", feL5, at); !near(e.GainDeparture, -12, 0) {
		t.Fatalf("L5 after a 12 dB rise = %+v", e)
	}
	st := s.FeedStationRF(at)["p3"]
	if st.RFTrust != 1 {
		t.Fatalf("rf_trust = %.2f for a gain rise, want 1", st.RFTrust)
	}
	s.rfMu.Lock()
	departed := s.rf["p3"].departed(at)
	signs, severe := s.interferenceEvidence(s.rf["p3"], at, time.Minute)
	s.rfMu.Unlock()
	if departed || signs != 0 || severe {
		t.Fatalf("a gain rise read as interference: departed=%v signs=%d severe=%v", departed, signs, severe)
	}
	at = quietGains(s, "p3", 52, 48, at, 27*time.Minute)
	if e := frontEnd(t, s, "p3", feL5, at); !near(e.GainDeparture, -12, 0) {
		t.Fatalf("half an hour into the rise: %+v, want it still measured against the old baseline", e)
	}
}

// TestFrontEndBaselineResistsSlowRamp: noise that lowers a gain by 3 dB an hour never
// departs by more than the learn band from one sample to the next, but the baseline
// may follow at most 0.75 dB an hour, so the ramp shows as a departure.
func TestFrontEndBaselineResistsSlowRamp(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	at := now
	for ; at.Before(now.Add(30 * time.Minute)); at = at.Add(time.Second) {
		s.Apply(statusFrame("p3", at, gainOf(feL1, 40)))
	}
	ramp := 2 * time.Hour
	for i := 0; i < int(ramp/time.Second); i++ {
		s.Apply(statusFrame("p3", at.Add(time.Duration(i)*time.Second), gainOf(feL1, 40-int(3*float64(i)/3600))))
	}
	e := frontEnd(t, s, "p3", feL1, at.Add(ramp))
	if e.GainDeparture == nil || *e.GainDeparture < integrity.DefaultGainDepartureDB {
		t.Fatalf("after a 6 dB ramp the departure is %v, want at least the %g dB gain departure", e.GainDeparture, integrity.DefaultGainDepartureDB)
	}
}

// TestFrontEndUnlocked: a front end whose PLL is not locked has no gain, so it is
// neither measured nor learned; its baseline waits, and the gain is measured against
// it again when it returns.
func TestFrontEndUnlocked(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	at := quietGains(s, "p3", 40, 36, now, 11*time.Minute)
	for i := 0; i < 180; i++ {
		s.Apply(statusFrame("p3", at.Add(time.Duration(i)*time.Second), unlocked(feL1), gainOf(feL5, 36)))
	}
	at = at.Add(3 * time.Minute)
	e := frontEnd(t, s, "p3", feL1, at)
	if e.Locked || e.GainDB != nil || e.GainDeparture != nil || e.SampleVariance != nil {
		t.Fatalf("unlocked front end = %+v", e)
	}
	if st := s.FeedStationRF(at)["p3"]; st.RFTrust != 1 {
		t.Fatalf("rf_trust = %.2f with an unlocked front end", st.RFTrust)
	}
	s.Apply(statusFrame("p3", at, gainOf(feL1, 33), gainOf(feL5, 36)))
	if e := frontEnd(t, s, "p3", feL1, at); !e.Locked || !near(e.GainDeparture, 7, 0.5) {
		t.Fatalf("gain back 7 dB below the kept baseline = %+v", e)
	}
}

// TestFrontEndStale: a front end that stops reporting is withheld, while the ones still
// reporting keep the station and their own measurements.
func TestFrontEndStale(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(statusFrame("p3", now, gainOf(feL1, 40), gainOf(feL5, 36)))
	later := now.Add(rfStaleAfter + time.Minute)
	s.Apply(statusFrame("p3", later, gainOf(feL5, 36)))
	st := s.FeedStationRF(later)["p3"]
	if len(st.FrontEnds) != 1 || st.FrontEnds[0].ID != feL5 {
		t.Fatalf("front ends = %+v, want only L5", st.FrontEnds)
	}
	if got := s.FeedStationRF(later.Add(rfStaleAfter + time.Minute)); len(got) != 0 {
		t.Fatalf("a silent station is still served: %+v", got)
	}
}

// TestFrontEndNamesAndOrder: front ends are served in FrontEndID order whatever order
// the receiver sends them in, an auxiliary antenna's front end is its own entry, and a
// code the reference guide does not define keeps a stable name.
func TestFrontEndNamesAndOrder(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(statusFrame("p3", now, gainOf(12, 50), gainOf(1<<5|feL1, 41), gainOf(feL5, 36), gainOf(21, 30), gainOf(feL1, 40)))
	var ids []int
	var names []string
	for _, e := range s.FeedStationRF(now)["p3"].FrontEnds {
		ids, names = append(ids, e.ID), append(names, e.Name)
	}
	if !slices.Equal(ids, []int{0, 5, 12, 21, 32}) ||
		!slices.Equal(names, []string{"GPSL1/E1", "L5/E5a/B2a", "B3", "fe21", "aux1 GPSL1/E1"}) {
		t.Fatalf("ids %v names %v", ids, names)
	}
	if e := frontEnd(t, s, "p3", 32, now); e.Antenna != 1 || *e.GainDB != 41 {
		t.Fatalf("aux1 L1 = %+v", e)
	}
}

// TestFrontEndBaselineCheckpointRestore: gain baselines are checkpointed with the AGC
// ones, marked as front ends, and a restored one serves departures at once.
func TestFrontEndBaselineCheckpointRestore(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := New(1)
	at := quietGains(s, "p3", 40, 36, now, 11*time.Minute)
	clearSky(s, "ublox", 4000, now, 11*time.Minute)
	all := s.AGCBaselines()
	saved := all["p3"]
	if len(saved) != 2 || !saved[0].FrontEnd || saved[0].Block != feL1 || math.Abs(saved[0].Baseline-40) > 1 ||
		!saved[1].FrontEnd || saved[1].Block != feL5 || saved[1].Baseline != 36 || len(saved[1].Minutes) < 10 {
		t.Fatalf("checkpoint = %+v", saved)
	}
	if band := all["ublox"]; len(band) != 1 || band[0].FrontEnd {
		t.Fatalf("a u-blox band checkpointed as %+v", band)
	}

	restarted := New(1)
	if n := restarted.RestoreAGCBaselines("p3", saved, at, at); n != 2 {
		t.Fatalf("restored %d baselines", n)
	}
	if got := restarted.FeedStationRF(at); len(got) != 0 {
		t.Fatalf("restored station live before reporting: %+v", got)
	}
	restarted.Apply(statusFrame("p3", at, gainOf(feL1, 30), gainOf(feL5, 36)))
	if e := frontEnd(t, restarted, "p3", feL1, at); !near(e.GainDeparture, 10, 1) {
		t.Fatalf("departure after restore = %+v, want about 10 at once", e)
	}
	if again := restarted.RestoreAGCBaselines("p3", saved, at, at); again != 0 {
		t.Fatal("a front end that already reported was overwritten by a restore")
	}

	// A front end's baseline is never installed on a band of the same number, nor a
	// band's on a front end.
	crossed := New(1)
	crossed.RestoreAGCBaselines("x", []AGCBaseline{{Block: 0, FrontEnd: true, Baseline: 40, BaselineAt: at}, {Block: 5, Baseline: 4000, BaselineAt: at}}, at, at)
	crossed.Apply(rfSample("x", 0, 4000, 0, 2, at))
	crossed.Apply(statusFrame("x", at, gainOf(feL5, 36)))
	x := crossed.FeedStationRF(at)["x"]
	if x.Bands[0].AGCDeparture != nil || x.FrontEnds[0].GainDeparture != nil {
		t.Fatalf("a restored baseline crossed between a band and a front end: %+v", x)
	}
}

func TestFrontEndBaselineRestoreRejects(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := New(1)
	bad := []AGCBaseline{
		{Block: 1, FrontEnd: true, Baseline: math.NaN(), BaselineAt: now},
		{Block: 2, FrontEnd: true, Baseline: -128, BaselineAt: now}, // the value that means "not locked"
		{Block: 3, FrontEnd: true, Baseline: 128, BaselineAt: now},
		{Block: 256, FrontEnd: true, Baseline: 40, BaselineAt: now},
		{Block: -1, FrontEnd: true, Baseline: 40, BaselineAt: now},
		{Block: 4, FrontEnd: true, Baseline: 40, BaselineAt: now.Add(time.Hour)},
	}
	if n := s.RestoreAGCBaselines("p3", bad, now, now); n != 0 {
		t.Fatalf("restored %d invalid baselines", n)
	}
	// A gain may be negative: only the range of the byte bounds it.
	if n := s.RestoreAGCBaselines("p3", []AGCBaseline{{Block: 0, FrontEnd: true, Baseline: -5.5, BaselineAt: now}}, now, now); n != 1 {
		t.Fatal("a negative gain baseline was refused")
	}
	s.Reset()
	s.Apply(statusFrame("p3", now, gainOf(feL1, 40)))
	if e := frontEnd(t, s, "p3", feL1, now); e.GainDeparture != nil {
		t.Fatal("a pending restore survived Reset")
	}
}

// TestFrontEndIntegrityCheck: the agc check judges a Septentrio station by its front
// ends: quiet is assured, a lone cut is inconsistent, a collapse is unassured, and a
// rise is inconsistent with its own reason.
func TestFrontEndIntegrityCheck(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	run := func(l1, l5 int) (integrity.Result, integrity.Assessment) {
		s := New(1)
		at := quietGains(s, "p3", 40, 36, now, 11*time.Minute)
		for i := 0; i < 30; i++ {
			s.Apply(statusFrame("p3", at.Add(time.Duration(i)*time.Second), gainOf(feL1, l1), gainOf(feL5, l5)))
		}
		a := s.FeedStationIntegrity(at.Add(30 * time.Second))["p3"]
		return integrityResult(t, a, integrity.CheckAGC), a
	}
	quiet, assessment := run(40, 36)
	if quiet.State != integrity.Assured || quiet.Metrics["front_ends"] != 2 || quiet.Metrics["bands"] != 0 ||
		quiet.Thresholds["gain_departure_db"] != 4 || quiet.Thresholds["gain_departure_severe_db"] != 10 {
		t.Fatalf("quiet front ends: %+v", quiet)
	}
	// Lower-only checks alone cannot assure a station.
	if assessment.State != integrity.Unavailable {
		t.Fatalf("station with only the agc check = %s, want unavailable", assessment.State)
	}
	if cut, _ := run(35, 36); cut.State != integrity.Inconsistent || !slices.Contains(cut.Reasons, integrity.ReasonAGCDeparture) ||
		math.Abs(cut.Metrics["max_gain_cut_db"]-5) > 1 {
		t.Fatalf("a 5 dB cut: %+v", cut)
	}
	if collapse, _ := run(40, 25); collapse.State != integrity.Unassured || !slices.Contains(collapse.Reasons, integrity.ReasonAGCCollapse) {
		t.Fatalf("an 11 dB cut: %+v", collapse)
	}
	if rise, _ := run(52, 48); rise.State != integrity.Inconsistent || !slices.Equal(rise.Reasons, []string{integrity.ReasonGainRise}) ||
		rise.Metrics["max_gain_rise_db"] != 12 {
		t.Fatalf("a 12 dB rise: %+v", rise)
	}
}

// TestFrontEndStatusWithoutFrontEnds: a ReceiverStatus with no AGC sub-blocks keeps the
// station heard from and gives the RF checks nothing to judge.
func TestFrontEndStatusWithoutFrontEnds(t *testing.T) {
	s := New(1)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(statusFrame("p3", now))
	st, ok := s.FeedStationRF(now)["p3"]
	if !ok || len(st.FrontEnds) != 0 || len(st.Bands) != 0 || st.RFTrust != 1 {
		t.Fatalf("station = %+v (present %v)", st, ok)
	}
	if a, ok := s.FeedStationIntegrity(now)["p3"]; ok {
		for _, r := range a.Checks {
			if r.Check == integrity.CheckAGC && r.State != integrity.Unavailable {
				t.Fatalf("agc check with no front ends: %+v", r)
			}
		}
	}
}
