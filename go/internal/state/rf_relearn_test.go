package state

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/integrity"
)

// The self-healing hold (docs/DEFENSE-PNT.md §2): a band or front end that has sat
// away from its baseline for the profile's BaselineRelearnAfter starts learning again
// at the level it holds, and a profile with no hold never learns a gain cut.

// relearnStore is a store whose profile holds a departure for hold before learning it;
// zero never does.
func relearnStore(t *testing.T, hold time.Duration) *Store {
	t.Helper()
	profile := integrity.DefaultProfile()
	profile.AGC.BaselineRelearnAfter = hold
	cfg, err := NewIntegrityConfig(profile, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := New(1)
	s.SetIntegrity(cfg)
	return s
}

// holdBand reports one u-blox band at a level every ten seconds for d and returns the
// instant after the last report.
func holdBand(s *Store, source string, agc int, from time.Time, d time.Duration) time.Time {
	at := from
	for ; at.Before(from.Add(d)); at = at.Add(10 * time.Second) {
		s.Apply(rfSample(source, 0, agc, 0, 2, at))
	}
	return at
}

// holdGains reports a Septentrio station's L1 and L5 gains every ten seconds for d.
func holdGains(s *Store, source string, l1, l5 int, from time.Time, d time.Duration) time.Time {
	at := from
	for ; at.Before(from.Add(d)); at = at.Add(10 * time.Second) {
		s.Apply(statusFrame(source, at, gainOf(feL1, l1), gainOf(feL5, l5)))
	}
	return at
}

// relearnedAt is the station's relearn mark as served at at: the zero time when it
// carries none.
func relearnedAt(s *Store, source string, at time.Time) time.Time {
	if mark := s.FeedStationRF(at)[source].BaselineRelearnedAt; mark != nil {
		return time.Unix(*mark, 0)
	}
	return time.Time{}
}

// TestBandCutSelfHeals: a u-blox band cut and held is reported for the whole hold, with
// the station's votes down-weighted, and then learned as the station's normal.
func TestBandCutSelfHeals(t *testing.T) {
	s := New(1) // the default profile holds for a day
	t0 := time.Unix(1_700_000_000, 0)
	onset := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	at := holdBand(s, "stn", 2500, onset, 23*time.Hour)
	if dep := departure(t, s, "stn", at); !near(dep, 1500, 5) {
		t.Fatalf("23 hours into the cut: departure %v, want still 1500", dep)
	}
	if st := s.FeedStationRF(at)["stn"]; st.RFTrust != 0.3 || st.BaselineRelearnedAt != nil {
		t.Fatalf("23 hours into the cut: rf_trust %.1f, relearned %v", st.RFTrust, st.BaselineRelearnedAt)
	}
	at = holdBand(s, "stn", 2500, at, time.Hour+15*time.Minute)
	if dep := departure(t, s, "stn", at); !near(dep, 0, 5) {
		t.Fatalf("after the hold: departure %v, want about 0 against the relearned baseline", dep)
	}
	st := s.FeedStationRF(at)["stn"]
	if st.RFTrust != 1 {
		t.Fatalf("after the hold: rf_trust %.1f, want 1", st.RFTrust)
	}
	// Learning restarted when the cut had lasted exactly the hold.
	if got := relearnedAt(s, "stn", at); got.Before(onset.Add(24*time.Hour)) || got.After(onset.Add(24*time.Hour+time.Minute)) {
		t.Fatalf("relearned at %v, want a day after the onset at %v", got, onset)
	}
	// What it holds now is its normal: going back up is the departure.
	at = holdBand(s, "stn", 4000, at, 5*time.Minute)
	if dep := departure(t, s, "stn", at); !near(dep, -1500, 5) {
		t.Fatalf("back at the old level: departure %v, want -1500", dep)
	}
}

// TestBandCutLatchedNeverHeals: with no hold a gain cut is never learned, as the
// baseline always behaved, while a band above its baseline still starts again after
// fifteen minutes.
func TestBandCutLatchedNeverHeals(t *testing.T) {
	s := relearnStore(t, 0)
	t0 := time.Unix(1_700_000_000, 0)
	at := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	at = holdBand(s, "stn", 2500, at, 72*time.Hour)
	if dep := departure(t, s, "stn", at); !near(dep, 1500, 5) {
		t.Fatalf("three days into a latched cut: departure %v, want 1500", dep)
	}
	if got := relearnedAt(s, "stn", at); !got.IsZero() {
		t.Fatalf("a latched cut relearned at %v", got)
	}
	// The cut ends, and then the band sits above its baseline: learned again in under
	// half an hour, whatever the profile.
	at = holdBand(s, "stn", 4000, at, 5*time.Minute)
	at = holdBand(s, "stn", 5500, at, 27*time.Minute)
	if dep := departure(t, s, "stn", at); !near(dep, 0, 5) {
		t.Fatalf("a band above a latched baseline: departure %v, want relearned to about 0", dep)
	}
}

// TestBandRiseRuleUnchangedByHold: the fifteen-minute rule for a band above its
// baseline does not wait for the profile's hold.
func TestBandRiseRuleUnchangedByHold(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	at := holdBand(s, "stn", 2500, t0, 15*time.Minute)
	at = holdBand(s, "stn", 4000, at, 27*time.Minute)
	if dep := departure(t, s, "stn", at); !near(dep, 0, 5) {
		t.Fatalf("after a jammed start: departure %v, want about 0 within half an hour", dep)
	}
}

// TestFrontEndUnpluggedForAnHour: an antenna unplugged for an hour and plugged back in.
// The rise is reported for the hour, nothing is learned from it, and the gains come
// back to the baseline they left: no cut, and no relearning to explain.
func TestFrontEndUnpluggedForAnHour(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	at := holdGains(s, "p3", 40, 36, t0, 15*time.Minute)
	at = holdGains(s, "p3", 52, 48, at, time.Hour)
	if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, -12, 0) {
		t.Fatalf("an hour unplugged: L1 %+v, want a 12 dB rise still measured", e)
	}
	at = holdGains(s, "p3", 40, 36, at, 5*time.Minute)
	for _, id := range []int{feL1, feL5} {
		if e := frontEnd(t, s, "p3", id, at); !near(e.GainDeparture, 0, 0) {
			t.Fatalf("plugged back in: front end %d = %+v, want its old baseline", id, e)
		}
	}
	st := s.FeedStationRF(at)["p3"]
	if st.RFTrust != 1 || st.BaselineRelearnedAt != nil {
		t.Fatalf("plugged back in: rf_trust %.1f, relearned %v", st.RFTrust, st.BaselineRelearnedAt)
	}
}

// TestFrontEndSelfHeals: a front end's gain held away from its baseline for a day,
// either way, becomes its baseline.
func TestFrontEndSelfHeals(t *testing.T) {
	for name, level := range map[string]int{"a rise": 52, "a cut": 30} {
		s := New(1)
		t0 := time.Unix(1_700_000_000, 0)
		onset := holdGains(s, "p3", 40, 36, t0, 15*time.Minute)
		at := holdGains(s, "p3", level, 36, onset, 23*time.Hour)
		if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, float64(40-level), 0) {
			t.Fatalf("%s, 23 hours in: %+v, want it still measured", name, e)
		}
		at = holdGains(s, "p3", level, 36, at, time.Hour+15*time.Minute)
		if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, 0, 0) {
			t.Fatalf("%s, after the hold: %+v, want about 0", name, e)
		}
		if got := relearnedAt(s, "p3", at); got.Before(onset.Add(24*time.Hour)) || got.After(onset.Add(24*time.Hour+time.Minute)) {
			t.Fatalf("%s: relearned at %v, want a day after %v", name, got, onset)
		}
		// The front end beside it never moved and keeps the baseline it had.
		if e := frontEnd(t, s, "p3", feL5, at); !near(e.GainDeparture, 0, 0) {
			t.Fatalf("%s: L5 = %+v", name, e)
		}
	}
}

// TestFrontEndLatchedNeverRelearns: with no hold a front end keeps its baseline whichever
// way its gain has gone, until the gain comes back.
func TestFrontEndLatchedNeverRelearns(t *testing.T) {
	for name, level := range map[string]int{"a rise": 52, "a cut": 30} {
		s := relearnStore(t, 0)
		t0 := time.Unix(1_700_000_000, 0)
		at := holdGains(s, "p3", 40, 36, t0, 15*time.Minute)
		at = holdGains(s, "p3", level, 36, at, 72*time.Hour)
		if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, float64(40-level), 0) {
			t.Fatalf("%s, three days in: %+v, want it still measured", name, e)
		}
		if got := relearnedAt(s, "p3", at); !got.IsZero() {
			t.Fatalf("%s relearned at %v under a latched profile", name, got)
		}
		at = holdGains(s, "p3", 40, 36, at, time.Minute)
		if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, 0, 0) {
			t.Fatalf("%s, gain back: %+v", name, e)
		}
	}
}

// TestRelearnHoldNeedsAnUnbrokenRun: a departure that comes back inside the learn band,
// even for a minute, has not held; the hold starts again when it leaves again.
func TestRelearnHoldNeedsAnUnbrokenRun(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	at := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	at = holdBand(s, "stn", 2500, at, 20*time.Hour)
	at = holdBand(s, "stn", 4000, at, time.Minute)
	second := at
	at = holdBand(s, "stn", 2500, at, 20*time.Hour)
	if dep := departure(t, s, "stn", at); !near(dep, 1500, 10) {
		t.Fatalf("40 hours of cut in two runs: departure %v, want still 1500", dep)
	}
	at = holdBand(s, "stn", 2500, at, 4*time.Hour+15*time.Minute)
	if dep := departure(t, s, "stn", at); !near(dep, 0, 10) {
		t.Fatalf("a day into the second run: departure %v, want about 0", dep)
	}
	if got := relearnedAt(s, "stn", at); got.Before(second.Add(24*time.Hour)) || got.After(second.Add(24*time.Hour+time.Minute)) {
		t.Fatalf("relearned at %v, want a day after the second run began at %v", got, second)
	}
}

// TestRelearnHoldConfigured: the hold is the profile's.
func TestRelearnHoldConfigured(t *testing.T) {
	s := relearnStore(t, time.Hour)
	t0 := time.Unix(1_700_000_000, 0)
	at := holdGains(s, "p3", 40, 36, t0, 15*time.Minute)
	at = holdGains(s, "p3", 30, 36, at, 50*time.Minute)
	if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, 10, 0) {
		t.Fatalf("50 minutes into a one-hour hold: %+v", e)
	}
	at = holdGains(s, "p3", 30, 36, at, 25*time.Minute)
	if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, 0, 0) {
		t.Fatalf("after a one-hour hold: %+v", e)
	}
}

// TestRelearnHoldSurvivesRestart: a checkpoint carries how long the departure has been
// watched, so a collector restarted during the hold heals when the rest of it has been
// watched too: the restart costs the time the collector was down and nothing more. A
// checkpoint is dated by when it was saved: a baseline that has not moved for most of a
// day, because its band is held away from it, is still restored.
func TestRelearnHoldSurvivesRestart(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	onset := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	gainOnset := holdGains(s, "p3", 40, 36, t0, 15*time.Minute)
	at := holdBand(s, "stn", 2500, onset, 20*time.Hour)
	holdGains(s, "p3", 52, 36, gainOnset, 20*time.Hour)

	// Twenty hours of reports ten seconds apart, from the first to the last.
	watched := 20*time.Hour - 10*time.Second
	saved := s.AGCBaselines()
	band := saved["stn"][0]
	if band.DepartedFor != watched || !band.DepartedBelow || band.RelearnedAt != nil {
		t.Fatalf("band checkpoint = %+v, want held below for %v", band, watched)
	}
	l1 := saved["p3"][0]
	if !l1.FrontEnd || l1.DepartedFor != watched || l1.DepartedBelow {
		t.Fatalf("L1 checkpoint = %+v, want held above for %v", l1, watched)
	}
	if l5 := saved["p3"][1]; l5.DepartedFor != 0 || l5.DepartedBelow {
		t.Fatalf("L5 checkpoint = %+v, want no hold", l5)
	}
	// The wire shape of a checkpoint entry.
	raw, err := json.Marshal(band)
	if err != nil || !strings.Contains(string(raw), `"departed_for_ns":71990000000000`) || !strings.Contains(string(raw), `"departed_below":true`) {
		t.Fatalf("checkpoint JSON %s (%v)", raw, err)
	}
	if quiet, _ := json.Marshal(saved["p3"][1]); strings.Contains(string(quiet), "departed") || strings.Contains(string(quiet), "relearned") {
		t.Fatalf("a quiet front end's checkpoint carries a hold: %s", quiet)
	}

	// The collector restarts two minutes later.
	restart := at.Add(2 * time.Minute)
	restarted := New(1)
	for id, bands := range saved {
		if n := restarted.RestoreAGCBaselines(id, bands, at, restart); n != len(bands) {
			t.Fatalf("%s: restored %d of %d baselines", id, n, len(bands))
		}
	}
	at = holdBand(restarted, "stn", 2500, restart, 3*time.Hour)
	if dep := departure(t, restarted, "stn", at); !near(dep, 1500, 5) {
		t.Fatalf("23 hours into the cut, after a restart: departure %v, want 1500", dep)
	}
	at = holdBand(restarted, "stn", 2500, at, time.Hour+15*time.Minute)
	if dep := departure(t, restarted, "stn", at); !near(dep, 0, 5) {
		t.Fatalf("a watched day into the cut, after a restart: departure %v, want about 0", dep)
	}
	// A day of watching: the two minutes the collector was down are left out, and the
	// twenty hours before them are not watched again.
	want := onset.Add(24*time.Hour + 2*time.Minute)
	if got := relearnedAt(restarted, "stn", at); got.Before(want) || got.After(want.Add(time.Minute)) {
		t.Fatalf("relearned at %v, want %v: a day after the onset %v and the restart's two minutes", got, want, onset)
	}
	// The relearn date is checkpointed with the baseline that came of it.
	again := restarted.AGCBaselines()["stn"][0]
	if again.RelearnedAt == nil || again.DepartedFor != 0 || math.Abs(again.Baseline-2500) > 5 {
		t.Fatalf("checkpoint after the relearn = %+v", again)
	}

	// A hold longer than the time since its baseline last moved is not believed, since
	// a baseline does not move while its band is held away from it: the baseline is
	// restored and the hold starts with the next report. One that fits is restored.
	for name, c := range map[string]struct {
		baselineAt time.Time
		want       time.Duration
	}{
		"a hold longer than its baseline has stood": {at.Add(-time.Hour), 10 * time.Second},
		"a hold that fits":                          {at.Add(-24 * time.Hour), 23*time.Hour + 10*time.Second},
	} {
		odd := New(1)
		odd.RestoreAGCBaselines("stn", []AGCBaseline{{Block: 0, Baseline: 4000, BaselineAt: c.baselineAt, DepartedFor: 23 * time.Hour, DepartedBelow: true}}, at, at)
		odd.Apply(rfSample("stn", 0, 2500, 0, 2, at))
		odd.Apply(rfSample("stn", 0, 2500, 0, 2, at.Add(10*time.Second)))
		if got := odd.AGCBaselines()["stn"][0]; got.DepartedFor != c.want || !got.DepartedBelow {
			t.Fatalf("%s: checkpoint after two reports = %+v, want held below for %v", name, got, c.want)
		}
	}
}

// TestRelearnMarkLastsTwoDays: a station whose baseline healed itself carries a mark for
// two days, on the station and on the band or front end it happened to, with the
// departure that was accepted as its normal. An operator who missed the event still
// sees that this station's normal has moved.
func TestRelearnMarkLastsTwoDays(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	onset := holdGains(s, "p3", 40, 36, t0, 15*time.Minute)
	at := holdGains(s, "p3", 30, 36, onset, 24*time.Hour+5*time.Minute)
	relearn := onset.Add(24 * time.Hour)

	// Five minutes after learning restarted: marked, and the new baseline not yet
	// established, so there is no accepted departure to state.
	st := s.FeedStationRF(at)["p3"]
	if st.BaselineRelearnedAt == nil || *st.BaselineRelearnedAt != relearn.Unix() {
		t.Fatalf("station mark = %v, want %d", st.BaselineRelearnedAt, relearn.Unix())
	}
	if l1 := st.FrontEnds[0]; l1.BaselineRelearnedAt == nil || *l1.BaselineRelearnedAt != relearn.Unix() || l1.GainDepartureAccepted != nil || l1.GainDeparture != nil {
		t.Fatalf("L1 while relearning = %+v", l1)
	}
	if l5 := st.FrontEnds[1]; l5.BaselineRelearnedAt != nil || l5.GainDepartureAccepted != nil {
		t.Fatalf("L5, which never moved, is marked: %+v", l5)
	}

	// Once the new baseline stands, the mark says what was accepted: a 10 dB cut.
	at = holdGains(s, "p3", 30, 36, at, 10*time.Minute)
	l1 := frontEnd(t, s, "p3", feL1, at)
	if !near(l1.GainDepartureAccepted, 10, 0) || !near(l1.GainDeparture, 0, 0) || l1.BaselineRelearnedAt == nil {
		t.Fatalf("L1 with its new baseline = %+v, want a 10 dB cut accepted", l1)
	}
	raw, err := json.Marshal(s.FeedStationRF(at)["p3"])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"baseline_relearned_at":` + strconv.FormatInt(relearn.Unix(), 10) + `}`, // the station's, last in the object
		`"gain_departure_accepted_db":10`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("served RF %s lacks %s", raw, want)
		}
	}
	if n := strings.Count(string(raw), `"baseline_relearned_at"`); n != 2 {
		t.Fatalf("served RF carries %d marks, want the station's and L1's: %s", n, raw)
	}

	// The mark lasts two days from the relearn and then goes, with nothing else changed.
	at = holdGains(s, "p3", 30, 36, at, relearn.Add(48*time.Hour-time.Minute).Sub(at))
	if got := relearnedAt(s, "p3", at); !got.Equal(relearn) {
		t.Fatalf("a minute before two days: mark %v, want %v", got, relearn)
	}
	at = holdGains(s, "p3", 30, 36, at, 2*time.Minute)
	st = s.FeedStationRF(at)["p3"]
	if st.BaselineRelearnedAt != nil || st.FrontEnds[0].BaselineRelearnedAt != nil || st.FrontEnds[0].GainDepartureAccepted != nil {
		t.Fatalf("after two days the station is still marked: %+v", st)
	}
	if !near(st.FrontEnds[0].GainDeparture, 0, 0) || st.RFTrust != 1 {
		t.Fatalf("after two days: %+v", st)
	}
	if raw, _ := json.Marshal(st); strings.Contains(string(raw), "relearned") || strings.Contains(string(raw), "accepted") {
		t.Fatalf("an unmarked station serves %s", raw)
	}
}

// TestRelearnMarkOnASilentRelearn: a u-blox band above its baseline starts again after
// fifteen minutes and raises no event at all, so the mark is the only trace that the
// station's normal moved, and it says which way: here a level 1500 counts higher.
func TestRelearnMarkOnASilentRelearn(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	onset := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	at := holdBand(s, "stn", 5500, onset, 30*time.Minute)
	st := s.FeedStationRF(at)["stn"]
	relearn := onset.Add(15 * time.Minute)
	if st.BaselineRelearnedAt == nil || *st.BaselineRelearnedAt != relearn.Unix() {
		t.Fatalf("station mark = %v, want %d", st.BaselineRelearnedAt, relearn.Unix())
	}
	band := st.Bands[0]
	if band.BaselineRelearnedAt == nil || !near(band.AGCDepartureAccepted, -1500, 5) || !near(band.AGCDeparture, 0, 5) {
		t.Fatalf("band = %+v, want a rise of 1500 accepted", band)
	}
	raw, _ := json.Marshal(st)
	if !strings.Contains(string(raw), `"agc_departure_accepted":-1500`) {
		t.Fatalf("served RF %s", raw)
	}
}

// TestRelearnMarkSurvivesRestartAndIsReplaced: the checkpoint carries the mark and the
// baseline it replaced, and a later relearn takes the mark's place.
func TestRelearnMarkSurvivesRestartAndIsReplaced(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	onset := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	at := holdBand(s, "stn", 2500, onset, 24*time.Hour+15*time.Minute)
	first := onset.Add(24 * time.Hour)

	saved := s.AGCBaselines()["stn"]
	if saved[0].RelearnedAt == nil || !saved[0].RelearnedAt.Equal(first) || saved[0].RelearnedFrom == nil || math.Abs(*saved[0].RelearnedFrom-4000) > 5 {
		t.Fatalf("checkpoint = %+v", saved[0])
	}
	restarted := New(1)
	if n := restarted.RestoreAGCBaselines("stn", saved, at, at.Add(time.Minute)); n != 1 {
		t.Fatalf("restored %d", n)
	}
	at = holdBand(restarted, "stn", 2500, at.Add(time.Minute), time.Minute)
	band := restarted.FeedStationRF(at)["stn"].Bands[0]
	if band.BaselineRelearnedAt == nil || *band.BaselineRelearnedAt != first.Unix() || !near(band.AGCDepartureAccepted, 1500, 5) {
		t.Fatalf("band after a restart = %+v, want the mark and a 1500-count cut accepted", band)
	}

	// The band goes back up and stays: fifteen minutes later that is learned, and the
	// mark is this relearn's.
	second := at
	at = holdBand(restarted, "stn", 4000, at, 30*time.Minute)
	band = restarted.FeedStationRF(at)["stn"].Bands[0]
	if band.BaselineRelearnedAt == nil || *band.BaselineRelearnedAt != second.Add(15*time.Minute).Unix() || !near(band.AGCDepartureAccepted, -1500, 5) {
		t.Fatalf("band after a second relearn = %+v", band)
	}

	// A checkpoint whose relearn has no previous baseline, or an impossible one, keeps
	// its baseline and loses the mark.
	bad := 70000.0
	odd := New(1)
	odd.RestoreAGCBaselines("stn", []AGCBaseline{
		{Block: 0, Baseline: 2500, BaselineAt: t0, RelearnedAt: &first},
		{Block: 1, Baseline: 2500, BaselineAt: t0, RelearnedAt: &first, RelearnedFrom: &bad},
	}, at, at)
	odd.Apply(rfSample("stn", 0, 2500, 0, 2, at))
	odd.Apply(rfSample("stn", 1, 2500, 0, 2, at))
	for _, band := range odd.FeedStationRF(at)["stn"].Bands {
		if band.BaselineRelearnedAt != nil || band.AGCDeparture == nil {
			t.Fatalf("band %d from an implausible checkpoint = %+v", band.Block, band)
		}
	}
}

// TestRelearnHoldCountsWatchedTimeOnly: the hold is a day of the level being seen away
// from the baseline, and not a day on the calendar. A station that falls silent while it
// is away is owed nothing for the silence, since nobody saw what its level was: it heals
// when it has been watched away for a day, however long that takes.
func TestRelearnHoldCountsWatchedTimeOnly(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	onset := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	holdGains(s, "p3", 40, 36, t0, 15*time.Minute)
	// An hour away from the baseline, then thirty hours unheard.
	holdBand(s, "stn", 2500, onset, time.Hour)
	holdGains(s, "p3", 52, 36, onset, time.Hour)
	back := onset.Add(31 * time.Hour)
	at := holdBand(s, "stn", 2500, back, 22*time.Hour)
	holdGains(s, "p3", 52, 36, back, 22*time.Hour)
	if dep := departure(t, s, "stn", at); !near(dep, 1500, 5) {
		t.Fatalf("band watched away for 23 hours across a silence: departure %v, want still 1500", dep)
	}
	if e := frontEnd(t, s, "p3", feL1, at); !near(e.GainDeparture, -12, 0) {
		t.Fatalf("front end watched away for 23 hours across a silence: %+v, want its rise still measured", e)
	}
	if got := relearnedAt(s, "stn", at); !got.IsZero() {
		t.Fatalf("band relearned at %v after 23 watched hours", got)
	}
	if got := relearnedAt(s, "p3", at); !got.IsZero() {
		t.Fatalf("front end relearned at %v after 23 watched hours", got)
	}
	// The twenty-fourth watched hour completes the hold.
	next := holdBand(s, "stn", 2500, at, time.Hour+15*time.Minute)
	holdGains(s, "p3", 52, 36, at, time.Hour+15*time.Minute)
	want := back.Add(23 * time.Hour)
	for _, id := range []string{"stn", "p3"} {
		if got := relearnedAt(s, id, next); got.Before(want) || got.After(want.Add(time.Minute)) {
			t.Fatalf("%s relearned at %v, want %v: a day of reports, the silence left out", id, got, want)
		}
	}
	if dep := departure(t, s, "stn", next); !near(dep, 0, 5) {
		t.Fatalf("after a watched day: departure %v, want about 0", dep)
	}
}

// TestRelearnHoldCreditsShortGaps: a station that reports only every few minutes is
// still being watched, so its hold runs at the calendar's pace.
func TestRelearnHoldCreditsShortGaps(t *testing.T) {
	s := New(1)
	t0 := time.Unix(1_700_000_000, 0)
	onset := holdBand(s, "stn", 4000, t0, 15*time.Minute)
	at := onset
	for ; at.Before(onset.Add(25 * time.Hour)); at = at.Add(4 * time.Minute) {
		s.Apply(rfSample("stn", 0, 2500, 0, 2, at))
	}
	want := onset.Add(24 * time.Hour)
	if got := relearnedAt(s, "stn", at); got.Before(want) || got.After(want.Add(4*time.Minute)) {
		t.Fatalf("a band reporting every four minutes relearned at %v, want %v", got, want)
	}
}
