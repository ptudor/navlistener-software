package integrity

import (
	"math"
	"slices"
	"testing"
)

func TestOffsetHelperRoundTrip(t *testing.T) {
	a, b := solutionAt(0, 0, 0, 0, 0, 0, 0), solutionAt(0, 30, 40, 0, 0, 0, 0)
	if d := ecefOf(b).Sub(ecefOf(a)).Norm(); math.Abs(d-50) > 1e-6 {
		t.Fatalf("30 m east and 40 m north are %g m apart, want 50", d)
	}
}

func TestStaticPositionBands(t *testing.T) {
	prof := DefaultProfile().StaticPosition
	cases := []struct {
		name           string
		east, up, hAcc float64
		want           State
		reason         string
	}{
		{"on the survey", 2, 1, 1.5, Assured, ""},
		{"beyond the horizontal floor", 30, 0, 1.5, Inconsistent, ReasonHorizontalError},
		{"beyond the unassured floor", 60, 0, 1.5, Unassured, ReasonHorizontalError},
		{"band scales with reported accuracy", 60, 0, 30, Assured, ""},
		{"band is capped", 120, 0, 100, Inconsistent, ReasonHorizontalError},
		{"vertical error", 0, -40, 1.5, Inconsistent, ReasonVerticalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPositionChecks(fixedProfile())
			s := solutionAt(0, tc.east, 0, tc.up, 0, 0, 0)
			s.HAccM = tc.hAcc
			p.accept(s)
			v := p.evaluate(s, DefaultProfile())[CheckStaticPosition]
			if v.State != tc.want {
				t.Fatalf("state = %s, want %s (%+v)", v.State, tc.want, v)
			}
			if tc.reason != "" && !slices.Contains(v.Reasons, tc.reason) {
				t.Fatalf("reasons = %v, want %s", v.Reasons, tc.reason)
			}
			if math.Abs(v.Metrics["horizontal_m"]-math.Abs(tc.east)) > 1e-6 {
				t.Fatalf("horizontal_m = %g, want %g", v.Metrics["horizontal_m"], math.Abs(tc.east))
			}
			if v.Thresholds["horizontal_unassured_m"] < prof.Horizontal.Unassured.Min {
				t.Fatalf("threshold below the floor: %+v", v.Thresholds)
			}
		})
	}
}

// TestStaticPositionMeanOffset is the slow-drag case: an offset inside the
// instantaneous bands becomes inconsistent once the ten-minute mean exceeds the
// drift threshold, and not before enough epochs exist.
func TestStaticPositionMeanOffset(t *testing.T) {
	p := newPositionChecks(fixedProfile())
	prof := DefaultProfile()
	var last Verdict
	for i := 0; i < prof.StaticPosition.DriftMinEpochs; i++ {
		s := solutionAt(i, 9, 0, 0, 0, 0, 0)
		p.accept(s)
		last = p.evaluate(s, prof)[CheckStaticPosition]
		if i < prof.StaticPosition.DriftMinEpochs-1 && last.State != Assured {
			t.Fatalf("epoch %d: %+v, want assured before the mean is available", i, last)
		}
	}
	if last.State != Inconsistent || !slices.Contains(last.Reasons, ReasonMeanOffset) {
		t.Fatalf("after %d epochs at 9 m: %+v, want inconsistent mean offset", prof.StaticPosition.DriftMinEpochs, last)
	}

	// A sustained 30 m offset is inconsistent epoch by epoch but unassured as a mean.
	p = newPositionChecks(fixedProfile())
	for i := 0; i < prof.StaticPosition.DriftMinEpochs; i++ {
		s := solutionAt(i, 30, 0, 0, 0, 0, 0)
		p.accept(s)
		last = p.evaluate(s, prof)[CheckStaticPosition]
	}
	if last.State != Unassured || !slices.Contains(last.Reasons, ReasonMeanOffset) {
		t.Fatalf("after %d epochs at 30 m: %+v, want an unassured mean offset", prof.StaticPosition.DriftMinEpochs, last)
	}
}

func TestStaticPositionUnavailable(t *testing.T) {
	p := newPositionChecks(StationProfile{Mode: ModeFixed})
	s := solutionAt(0, 0, 0, 0, 0, 0, 0)
	p.accept(s)
	if v := p.evaluate(s, DefaultProfile())[CheckStaticPosition]; v.State != Unavailable || v.Reasons[0] != ReasonNoSurveyedPosition {
		t.Fatalf("no survey: %+v", v)
	}
	p = newPositionChecks(fixedProfile())
	s.FixType = Fix2D
	p.accept(s)
	if v := p.evaluate(s, DefaultProfile())[CheckStaticPosition]; v.State != Unavailable || v.Reasons[0] != ReasonNo3DFix {
		t.Fatalf("2D fix: %+v", v)
	}
}

func TestStationaryVelocity(t *testing.T) {
	bands := DefaultProfile().StationaryVelocity
	for _, tc := range []struct {
		speed float64
		want  State
	}{{0.1, Assured}, {1, Inconsistent}, {3, Unassured}} {
		s := solutionAt(0, 0, 0, 0, tc.speed, 0, 0)
		if v := stationaryVelocity(s, true, bands); v.State != tc.want {
			t.Errorf("speed %g: %+v, want %s", tc.speed, v, tc.want)
		}
	}
}

// TestPositionVelocityConsistentMotion moves the antenna at the velocity it reports.
func TestPositionVelocityConsistentMotion(t *testing.T) {
	p := newPositionChecks(StationProfile{})
	prof := DefaultProfile()
	for i := 0; i <= 10; i++ {
		s := solutionAt(i, 0, 4*float64(i), 0, 4, 0, 0) // 4 m/s north
		p.accept(s)
		v := p.evaluate(s, prof)[CheckPositionVelocity]
		switch {
		case i < 5 && v.State != Unavailable:
			t.Fatalf("epoch %d: %+v, want unavailable before the window fills", i, v)
		case i >= 5 && v.State != Assured:
			t.Fatalf("epoch %d: %+v, want assured", i, v)
		}
	}
}

// TestPositionVelocityDraggedPosition moves the position while the reported velocity
// stays zero: the two solution paths disagree.
func TestPositionVelocityDraggedPosition(t *testing.T) {
	p := newPositionChecks(StationProfile{})
	prof := DefaultProfile()
	var v Verdict
	for i := 0; i <= 6; i++ {
		s := solutionAt(i, 5*float64(i), 0, 0, 0, 0, 0) // 5 m/s east, velocity reported zero
		p.accept(s)
		v = p.evaluate(s, prof)[CheckPositionVelocity]
	}
	if v.State != Unassured || math.Abs(v.Metrics["residual_mps"]-5) > 1e-3 {
		t.Fatalf("dragged position: %+v, want unassured with a 5 m/s residual", v)
	}
}

func TestPositionVelocityGapResets(t *testing.T) {
	p := newPositionChecks(StationProfile{})
	prof := DefaultProfile()
	for i := 0; i <= 6; i++ {
		s := solutionAt(i, 0, 0, 0, 0, 0, 0)
		p.accept(s)
		p.evaluate(s, prof)
	}
	s := solutionAt(30, 0, 0, 0, 0, 0, 0) // 24 s gap
	p.accept(s)
	if v := p.evaluate(s, prof)[CheckPositionVelocity]; v.State != Unavailable {
		t.Fatalf("after a gap: %+v, want unavailable", v)
	}
}

func TestPositionDuplicateAndRollover(t *testing.T) {
	p := newPositionChecks(StationProfile{})
	a := solutionAt(0, 0, 0, 0, 0, 0, 0)
	a.TOW = weekMS - 500
	if !p.accept(a) {
		t.Fatal("first epoch rejected")
	}
	if p.accept(a) {
		t.Fatal("duplicate epoch accepted")
	}
	b := a
	b.TOW = 500 // across the week rollover
	if !p.accept(b) || p.clock != 1000 {
		t.Fatalf("rollover: accepted clock %d, want 1000", p.clock)
	}
	c := a
	c.TOW = weekMS - 1500 // older than a
	if p.accept(c) {
		t.Fatal("out-of-order epoch accepted")
	}
}

// TestPositionBackwardTimeStepReanchors: a reported time-of-week that steps back by
// more than an epoch gap is the receiver's time being reset, not a reordered
// epoch. Refusing it left lastTOW ahead of true time and rejected every genuine
// epoch for up to half a week; the step must re-anchor receiver time, start the
// kinematic history afresh, mark that one epoch, and accept what follows.
func TestPositionBackwardTimeStepReanchors(t *testing.T) {
	p := newPositionChecks(StationProfile{Mode: ModeMobile, MaxSpeedMPS: 30})
	prof := DefaultProfile()
	s := solutionAt(0, 0, 0, 0, 0, 0, 0)
	s.TOW = 500_000_000
	if !p.accept(s) {
		t.Fatal("first epoch rejected")
	}
	p.evaluate(s, prof)
	back := solutionAt(1, 0, 0, 0, 0, 0, 0)
	back.TOW = 400_000_000 // a 28-hour backward step
	if !p.accept(back) {
		t.Fatal("backward time step rejected as out of order")
	}
	if len(p.history) != 0 || p.reference != nil {
		t.Fatalf("history kept across a time reset: %d epochs, reference %v", len(p.history), p.reference)
	}
	for name, v := range p.evaluate(back, prof) {
		if !slices.Contains(v.Reasons, ReasonReceiverTimeReset) {
			t.Errorf("%s at the reset epoch = %+v, want %s among its reasons", name, v, ReasonReceiverTimeReset)
		}
	}
	next := solutionAt(2, 0, 0, 0, 0, 0, 0)
	next.TOW = 400_001_000
	if !p.accept(next) || p.lastTOW != 400_001_000 {
		t.Fatalf("epoch after the reset rejected (lastTOW %d)", p.lastTOW)
	}
	for name, v := range p.evaluate(next, prof) {
		if slices.Contains(v.Reasons, ReasonReceiverTimeReset) {
			t.Errorf("%s one epoch after the reset still carries %s: %+v", name, ReasonReceiverTimeReset, v)
		}
	}
	// A small backward step is still a reordered epoch.
	reordered := next
	reordered.TOW = 400_000_500
	if p.accept(reordered) {
		t.Fatal("epoch reordered within an epoch gap accepted")
	}
}

func TestMotionBound(t *testing.T) {
	sp := StationProfile{Mode: ModeMobile, MaxSpeedMPS: 30}
	p := newPositionChecks(sp)
	prof := DefaultProfile()
	eval := func(i int, north float64) Verdict {
		s := solutionAt(i, 0, north, 0, 20, 0, 0)
		p.accept(s)
		return p.evaluate(s, prof)[CheckMotionBound]
	}
	if v := eval(0, 0); v.State != Unavailable || v.Reasons[0] != ReasonReferenceReset {
		t.Fatalf("first epoch: %+v, want the reference set", v)
	}
	for i := 1; i <= 5; i++ {
		if v := eval(i, 20*float64(i)); v.State != Assured {
			t.Fatalf("20 m/s under a 30 m/s bound at %d: %+v", i, v)
		}
	}
	if v := eval(6, 2100); v.State != Unassured {
		t.Fatalf("2 km jump: %+v, want unassured", v)
	}
	// The reference stays at the last in-bounds epoch, so the jump remains
	// implausible until the bound has grown to cover it.
	if v := eval(10, 2180); v.State != Unassured {
		t.Fatalf("after the jump: %+v, want unassured against the old reference", v)
	}
	if v := eval(80, 2200); v.State != Assured {
		t.Fatalf("75 s later at 30 m/s the bound covers 2 km: %+v", v)
	}
}

func TestMotionBoundNeedsMaxSpeed(t *testing.T) {
	p := newPositionChecks(StationProfile{Mode: ModeMobile})
	s := solutionAt(0, 0, 0, 0, 0, 0, 0)
	p.accept(s)
	if v := p.evaluate(s, DefaultProfile())[CheckMotionBound]; v.State != Unavailable || v.Reasons[0] != ReasonNoMaxSpeed {
		t.Fatalf("no max speed: %+v", v)
	}
}

func TestPositionChecksByMode(t *testing.T) {
	names := func(infos []Info) []string {
		var out []string
		for _, i := range infos {
			out = append(out, i.Name)
		}
		return out
	}
	for mode, want := range map[Mode][]string{
		ModeFixed:   {CheckStaticPosition, CheckStationaryVelocity, CheckPositionVelocity},
		ModeMobile:  {CheckMotionBound, CheckPositionVelocity},
		ModeUnknown: {CheckPositionVelocity},
	} {
		if got := names(newPositionChecks(StationProfile{Mode: mode}).infos()); !slices.Equal(got, want) {
			t.Errorf("mode %q checks = %v, want %v", mode, got, want)
		}
	}
}
