package integrity

import (
	"math"
	"testing"
	"time"
)

func TestNamedInstallationReplay(t *testing.T) {
	for _, i := range []Installation{Pedestrian, Bicycle, RoadVehicle, Aircraft, PortableTest} {
		t.Run(string(i), func(t *testing.T) {
			sp, err := InstallationDefaults(i)
			if err != nil {
				t.Fatal(err)
			}
			s := mustStation(t, sp)
			for k := 0; k < 20; k++ {
				s.ApplySolution(solutionAt(k, float64(k), 0, 0, 0, 1, 0))
			}
			v := checkResult(t, s.Assess(t0.Add(19*time.Second)), CheckMotionKinematics)
			if v.State != Assured {
				t.Fatalf("normal motion: %+v", v)
			}
			for k := 20; k < 30; k++ {
				s.ApplySolution(solutionAt(k, float64(k), 0, 0, 0, sp.MaxSpeedMPS+10, 0))
			}
			v = checkResult(t, s.Assess(t0.Add(29*time.Second)), CheckMotionKinematics)
			if v.State != Unassured {
				t.Fatalf("reported overspeed: %+v", v)
			}
		})
	}
}

func TestRoadTurnReplayAndStops(t *testing.T) {
	sp, _ := InstallationDefaults(RoadVehicle)
	p := newPositionChecks(sp)
	// 20 m/s in a 5 m radius: 80 m/s² continuously, while displacement alone
	// is well inside the 70 m/s envelope. Each sample has accurate velocity.
	for k := 0; k < 30; k++ {
		a := .4 * float64(k)
		s := solutionAt(k, 5*math.Cos(a), 5*math.Sin(a), 0, 20*math.Cos(a), -20*math.Sin(a), 0)
		s.TOW = uint32(tow0 + k*100)
		s.SAccMPS = .01
		p.accept(s)
		v := p.evaluate(s, DefaultProfile())[CheckMotionKinematics]
		if k > 0 && v.State != Unassured {
			t.Fatalf("continuous turn %d: %+v", k, v)
		}
	}
	p = newPositionChecks(sp)
	for k, vn := range []float64{2, 1, 0, 0, -1, -2, -1, 0} {
		s := solutionAt(k, 0, 0, 0, vn, 0, 0)
		p.accept(s)
		v := p.evaluate(s, DefaultProfile())[CheckMotionKinematics]
		if v.State == Inconsistent || v.State == Unassured {
			t.Fatalf("normal stop/reversal: %+v", v)
		}
	}
}

func TestAircraftVerticalAndAccuracyGates(t *testing.T) {
	sp, _ := InstallationDefaults(Aircraft)
	p := newPositionChecks(sp)
	for k := 0; k < 3; k++ {
		s := solutionAt(k, 0, 0, float64(k)*100, 0, 0, -100)
		p.accept(s)
		v := p.evaluate(s, DefaultProfile())[CheckMotionKinematics]
		if v.State != Unassured {
			t.Fatalf("aircraft vertical envelope: %+v", v)
		}
	}
	s := solutionAt(3, 0, 0, 0, 0, 0, 0)
	s.SAccMPS = 100
	p.accept(s)
	if v := p.evaluate(s, DefaultProfile())[CheckMotionKinematics]; v.State != Unavailable {
		t.Fatalf("poor velocity accuracy: %+v", v)
	}
}

func TestMetreDisplacementNeedsResolvingEvidence(t *testing.T) {
	sp := fixedProfile()
	sp.Installation = FixedStructure
	sp.Fixed = FixedPolicy{DisplacementM: 1, MaxHorizontalAccuracyM: .2, MinSV: 8, RequireGNSSOnly: true}
	sp.Reference = &ReferenceEvidence{Source: "external_survey", AcceptedBy: "operator", Reason: "antenna survey", SiteID: "site-v2", HorizontalUncertaintyM: .1, VerticalUncertaintyM: .2}
	if err := sp.Validate(); err != nil {
		t.Fatal(err)
	}
	p := newPositionChecks(sp)
	for k := 0; k < 3; k++ {
		s := solutionAt(k, 5, 0, 0, 0, 0, 0)
		p.accept(s)
		v := p.evaluate(s, DefaultProfile())[CheckStaticPosition]
		if v.State != Unavailable {
			t.Fatalf("standalone noise called movement: %+v", v)
		}
	}
	s := solutionAt(3, 2, 0, 0, 0, 0, 0)
	s.HAccM = .1
	p.accept(s)
	if v := p.evaluate(s, DefaultProfile())[CheckStaticPosition]; v.State != Unassured {
		t.Fatalf("resolved movement: %+v", v)
	}
	s = solutionAt(4, 2, 0, 0, 0, 0, 0)
	s.HAccM = .1
	s.FixType = FixTimeOnlyMode
	p.accept(s)
	if v := p.evaluate(s, DefaultProfile())[CheckStaticPosition]; v.State != Unavailable {
		t.Fatalf("held timing solution: %+v", v)
	}
	sp.Reference.HorizontalUncertaintyM = 2
	if sp.Validate() == nil {
		t.Fatal("poor reference accepted for metre rule")
	}
}

func TestPortableSegmentsKeepAlarmEvidence(t *testing.T) {
	sp, _ := InstallationDefaults(PortableTest)
	s := mustStation(t, sp)
	for k := 0; k < 12; k++ {
		s.ApplySolution(solutionAt(k, float64(k)*1000, 0, 0, 0, 1000, 0))
		s.ApplyClock(ClockSample{Received: t0.Add(time.Duration(k) * time.Second), TOW: uint32(tow0 + k*1000)})
	}
	if s.segment != 0 {
		t.Fatal("continuous implausible motion reset the segment")
	}
	s.ApplySolution(solutionAt(15, 0, 0, 0, 0, 0, 0))
	if s.segment != 0 {
		t.Fatal("short gap reset the segment")
	}
	s.ApplySolution(solutionAt(50, 100000, 0, 0, 0, 0, 0))
	a := s.Assess(t0.Add(50 * time.Second))
	if a.Segment != 1 || len(a.Segments) != 1 {
		t.Fatalf("long gap: %+v", a)
	}
	if v := checkResult(t, a, CheckMotionBound); v.Candidate != Unavailable {
		t.Fatalf("reference did not refill: %+v", v)
	}
	if len(s.clock.samples) != 0 {
		t.Fatal("clock comparison crossed relocation")
	}
	found := false
	for _, r := range a.Segments[0].Checks {
		if r.State == Unassured {
			found = true
		}
	}
	if !found {
		t.Fatal("previous alarm evidence lost")
	}
	if err := s.Relocate(t0.Add(51*time.Second), "moved to next test site"); err != nil {
		t.Fatal(err)
	}
	if s.segment != 2 {
		t.Fatal("declared relocation ignored")
	}
	legacy := mustStation(t, StationProfile{Mode: ModeMobile, MaxSpeedMPS: 5})
	if legacy.Relocate(t0, "move") == nil {
		t.Fatal("legacy mobile silently relocates")
	}
}

func TestPortableFixLossAndPolicyHash(t *testing.T) {
	sp, _ := InstallationDefaults(PortableTest)
	s := mustStation(t, sp)
	s.ApplySolution(solutionAt(0, 0, 0, 0, 0, 0, 0))
	for k := 1; k < 35; k++ {
		sol := solutionAt(k, 0, 0, 0, 0, 0, 0)
		sol.FixOK = false
		s.ApplySolution(sol)
	}
	s.ApplySolution(solutionAt(35, 10000, 0, 0, 0, 0, 0))
	if s.segment != 1 {
		t.Fatalf("fix loss segment=%d", s.segment)
	}
	other := sp
	other.Motion.MaxAccelerationMPS2++
	if s.ConfigHash() == mustStation(t, other).ConfigHash() {
		t.Fatal("effective motion policy missing from hash")
	}
	other = sp
	other.SegmentID = "deployment-2"
	if s.ConfigHash() == mustStation(t, other).ConfigHash() {
		t.Fatal("segment identity missing from hash")
	}
}

func TestSurveyCandidateAndExplicitPin(t *testing.T) {
	p := SurveyPolicy{Duration: 10 * time.Second, MinSamples: 10, MaxDispersionM: 1, MaxAccuracyM: .2, MinSV: 8}
	survey, err := NewSurvey(p)
	if err != nil {
		t.Fatal(err)
	}
	for k := 0; k < 20; k++ {
		s := solutionAt(k, 0, 0, 0, 0, 0, 0)
		s.HAccM = .1
		s.VAccM = .15
		survey.Apply(s)
	}
	c := survey.Candidate()
	if c == nil || c.Independent || c.Source != "receiver_survey" || c.Samples != 11 {
		t.Fatalf("candidate: %+v", c)
	}
	r, err := c.Pin("operator", "accepted review", "new-site-2")
	if err != nil {
		t.Fatal(err)
	}
	sp := StationProfile{Mode: ModeFixed, Position: &c.Position, Reference: r}
	if err := sp.Validate(); err != nil {
		t.Fatal(err)
	}
	before := *survey.Candidate()
	survey.Apply(solutionAt(20, 10000, 0, 0, 0, 0, 0))
	if *survey.Candidate() != before {
		t.Fatal("candidate followed suspect movement")
	}
	if _, err := c.Pin("", "", "site"); err == nil {
		t.Fatal("pin without operator acceptance")
	}
	survey, _ = NewSurvey(p)
	for k := 0; k < 15; k++ {
		s := solutionAt(k, float64(k), 0, 0, 0, 0, 0)
		s.HAccM = .1
		s.VAccM = .1
		survey.Apply(s)
	}
	if survey.Candidate() != nil {
		t.Fatal("moving survey produced a reference")
	}
	for k := 15; k < 30; k++ {
		s := solutionAt(k, 0, 0, 0, 0, 0, 0)
		s.FixType = FixGNSSAndDR
		survey.Apply(s)
	}
	if survey.Candidate() != nil {
		t.Fatal("dead reckoning survey accepted")
	}
}
