package integrity

import (
	"slices"
	"testing"
	"time"
)

// skyAt is a quiet eight-satellite NAV-SAT epoch: each signal's C/N₀ wanders by one
// step from second to second, as a real receiver's whole-dB-Hz values do, lowered by
// dropDB.
func skyAt(at time.Time, second, dropDB int) Cn0Snapshot {
	snap := Cn0Snapshot{Received: at}
	for sv := 1; sv <= 8; sv++ {
		snap.Signals = append(snap.Signals, Cn0Signal{GnssID: 0, SvID: sv, Cn0: 36 + sv + (second+sv)%2 - dropDB, Used: true})
	}
	return snap
}

func cn0DropStation(t *testing.T) *Station {
	t.Helper()
	return mustStation(t, fixedProfile())
}

func TestCn0DropNeedsAReferenceInsideTheWindow(t *testing.T) {
	s := cn0DropStation(t)
	for i := 0; i < 3; i++ {
		s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, 0))
	}
	r := checkResult(t, s.Assess(t0.Add(2*time.Second)), CheckCn0Drop)
	if r.State != Unavailable || !slices.Contains(r.Reasons, ReasonNoWindow) {
		t.Fatalf("two seconds of history = %s %v, want unavailable/%s", r.State, r.Reasons, ReasonNoWindow)
	}
	// A gap longer than the window leaves no reference either.
	s.ApplyCn0Snapshot(skyAt(t0.Add(30*time.Second), 30, 0))
	if r := checkResult(t, s.Assess(t0.Add(30*time.Second)), CheckCn0Drop); !slices.Contains(r.Reasons, ReasonNoWindow) {
		t.Fatalf("after a gap = %s %v", r.State, r.Reasons)
	}
}

func TestCn0DropQuietSkyIsAssured(t *testing.T) {
	s := cn0DropStation(t)
	for i := 0; i < 20; i++ {
		s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, 0))
	}
	r := checkResult(t, s.Assess(t0.Add(19*time.Second)), CheckCn0Drop)
	if r.State != Assured || r.Metrics["signals"] != 8 || r.Metrics["span_s"] < 3 || r.Metrics["span_s"] > 5 {
		t.Fatalf("quiet sky = %+v", r)
	}
}

// TestCn0DropGradesTheMedian drops every signal at once and holds it: the drop is
// seen against references before the step, then the comparison is quiet again, and
// the served state holds through the recovery.
func TestCn0DropGradesTheMedian(t *testing.T) {
	for _, tc := range []struct {
		drop int
		want State
	}{{2, Assured}, {4, Inconsistent}, {8, Unassured}} {
		s := cn0DropStation(t)
		for i := 0; i < 20; i++ {
			s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, 0))
		}
		// Three epochs after the step, each still compared against a reference
		// from before it.
		for i := 20; i < 23; i++ {
			s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, tc.drop))
		}
		r := checkResult(t, s.Assess(t0.Add(22*time.Second)), CheckCn0Drop)
		if r.State != tc.want {
			t.Fatalf("drop %d dB = %s (%v %v), want %s", tc.drop, r.State, r.Metrics, r.Reasons, tc.want)
		}
		if tc.want == Assured {
			continue
		}
		if !slices.Contains(r.Reasons, ReasonSimultaneousDrop) || r.Metrics["min_drop_db"] < 1 {
			t.Fatalf("drop %d dB evidence = %v %v", tc.drop, r.Metrics, r.Reasons)
		}
		// Quiet at the lower level: the comparison recovers, the served state holds.
		for i := 23; i < 60; i++ {
			s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, tc.drop))
		}
		r = checkResult(t, s.Assess(t0.Add(59*time.Second)), CheckCn0Drop)
		if r.State != tc.want || r.Candidate != Assured || r.RecoveringSince == nil {
			t.Fatalf("drop %d dB after the step = %s candidate %s", tc.drop, r.State, r.Candidate)
		}
		at := 60 + int(DefaultProfile().Filter.RecoveryHold/time.Second)
		for i := 60; i <= at; i++ {
			s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, tc.drop))
		}
		if r = checkResult(t, s.Assess(t0.Add(time.Duration(at)*time.Second)), CheckCn0Drop); r.State != Assured {
			t.Fatalf("drop %d dB after the hold = %s", tc.drop, r.State)
		}
	}
}

// TestCn0DropNeedsEverySignal: one satellite holding its level is geometry or a
// local obstruction, not a raised noise floor.
func TestCn0DropNeedsEverySignal(t *testing.T) {
	s := cn0DropStation(t)
	for i := 0; i < 20; i++ {
		s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, 0))
	}
	for i := 20; i < 26; i++ {
		snap := skyAt(t0.Add(time.Duration(i)*time.Second), i, 10)
		snap.Signals[3].Cn0 += 10
		s.ApplyCn0Snapshot(snap)
	}
	if r := checkResult(t, s.Assess(t0.Add(25*time.Second)), CheckCn0Drop); r.State != Assured || r.Metrics["min_drop_db"] > 0 {
		t.Fatalf("one steady signal = %s %v", r.State, r.Metrics)
	}
}

// TestCn0DropComparesOnlyUsedTrackedSignals: signals lost outright, signals the
// receiver did not use, and a sky too thin for "every signal" to mean anything.
func TestCn0DropComparesOnlyUsedTrackedSignals(t *testing.T) {
	s := cn0DropStation(t)
	for i := 0; i < 20; i++ {
		snap := skyAt(t0.Add(time.Duration(i)*time.Second), i, 0)
		snap.Signals[0].Used = false
		s.ApplyCn0Snapshot(snap)
	}
	for i := 20; i < 26; i++ {
		snap := skyAt(t0.Add(time.Duration(i)*time.Second), i, 0)
		snap.Signals[0].Used = false
		snap.Signals[1].Cn0 = 0                           // lost
		snap.Signals = snap.Signals[:len(snap.Signals)-1] // gone from the list
		s.ApplyCn0Snapshot(snap)
	}
	r := checkResult(t, s.Assess(t0.Add(25*time.Second)), CheckCn0Drop)
	if r.Metrics["signals"] != 5 || r.State != Unavailable || !slices.Contains(r.Reasons, ReasonTooFewSatellites) {
		t.Fatalf("five comparable signals = %s %v %v", r.State, r.Metrics, r.Reasons)
	}
}

func TestCn0DropIgnoresDuplicateAndOutOfOrderEpochs(t *testing.T) {
	var c cn0DropCheck
	prof := DefaultProfile().Cn0Drop
	for i := 0; i < 6; i++ {
		if _, ok := c.evaluate(skyAt(t0.Add(time.Duration(i)*time.Second), i, 0), prof); !ok {
			t.Fatalf("epoch %d rejected", i)
		}
	}
	if _, ok := c.evaluate(skyAt(t0.Add(5*time.Second), 5, 20), prof); ok {
		t.Fatal("duplicate epoch evaluated")
	}
	if _, ok := c.evaluate(skyAt(t0.Add(2*time.Second), 2, 20), prof); ok {
		t.Fatal("out-of-order epoch evaluated")
	}
	if len(c.recent) > 6 {
		t.Fatalf("history kept %d snapshots", len(c.recent))
	}
}

func TestCn0DropHistoryIsBounded(t *testing.T) {
	var c cn0DropCheck
	prof := DefaultProfile().Cn0Drop
	for i := 0; i < 10*maxCn0Snapshots; i++ {
		c.evaluate(skyAt(t0.Add(time.Duration(i)*time.Millisecond), i, 0), prof)
	}
	if len(c.recent) != maxCn0Snapshots {
		t.Fatalf("history = %d snapshots, want %d", len(c.recent), maxCn0Snapshots)
	}
}

func TestCn0DropProfileValidation(t *testing.T) {
	for name, mutate := range map[string]func(*Cn0DropProfile){
		"span beyond window": func(p *Cn0DropProfile) { p.MinSpan = p.Window + time.Second },
		"zero span":          func(p *Cn0DropProfile) { p.MinSpan = 0 },
		"no step":            func(p *Cn0DropProfile) { p.EveryDB = 0 },
		"inverted bands":     func(p *Cn0DropProfile) { p.UnassuredDB = p.InconsistentDB - 1 },
		"too few signals":    func(p *Cn0DropProfile) { p.MinSignals = 2 },
		"quality above 7":    func(p *Cn0DropProfile) { p.MinQuality = 8 },
		"no quality gate":    func(p *Cn0DropProfile) { p.MinQuality = 0 },
	} {
		p := DefaultProfile()
		mutate(&p.Cn0Drop)
		if err := p.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestStationServedReadsOneCheck(t *testing.T) {
	s := cn0DropStation(t)
	if _, ok := s.Served("no_such_check", t0); ok {
		t.Fatal("unknown check served")
	}
	for i := 0; i < 20; i++ {
		s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, 0))
	}
	for i := 20; i < 26; i++ {
		s.ApplyCn0Snapshot(skyAt(t0.Add(time.Duration(i)*time.Second), i, 8))
	}
	if st, ok := s.Served(CheckCn0Drop, t0.Add(26*time.Second)); !ok || st != Unassured {
		t.Fatalf("served = %s %v", st, ok)
	}
	// Input stopping is not a recovery: the unassured state is held for the
	// recovery period before the check reports that it merely lacks input.
	stopped := t0.Add(26*time.Second + DefaultProfile().StaleAfter + time.Second)
	if st, _ := s.Served(CheckCn0Drop, stopped); st != Unassured {
		t.Fatalf("served when input stopped = %s", st)
	}
	if st, _ := s.Served(CheckCn0Drop, stopped.Add(DefaultProfile().Filter.RecoveryHold)); st != Unavailable {
		t.Fatalf("served after the hold = %s", st)
	}
}

// TestCn0DropGatesOnTheQualityIndicator: with the indicator reported, a signal locked
// but not used in the solution is compared (Epsilon's gate), and a used signal that is
// not locked is not.
func TestCn0DropGatesOnTheQualityIndicator(t *testing.T) {
	sky := func(at time.Time, second, dropDB int) Cn0Snapshot {
		snap := skyAt(at, second, dropDB)
		for i := range snap.Signals {
			snap.Signals[i].HaveQuality, snap.Signals[i].Quality = true, 7
		}
		snap.Signals[0].Used = false  // locked, not used: compared
		snap.Signals[1].Quality = 3   // used, not locked: not compared
		snap.Signals[1].Cn0 += dropDB // and it does not drop
		return snap
	}
	s := cn0DropStation(t)
	for i := 0; i < 20; i++ {
		s.ApplyCn0Snapshot(sky(t0.Add(time.Duration(i)*time.Second), i, 0))
	}
	for i := 20; i < 23; i++ {
		s.ApplyCn0Snapshot(sky(t0.Add(time.Duration(i)*time.Second), i, 4))
	}
	r := checkResult(t, s.Assess(t0.Add(22*time.Second)), CheckCn0Drop)
	if r.State != Inconsistent || r.Metrics["signals"] != 7 {
		t.Fatalf("quality-gated drop = %s %v %v", r.State, r.Metrics, r.Reasons)
	}
}
