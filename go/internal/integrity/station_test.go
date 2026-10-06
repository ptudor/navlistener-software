package integrity

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNewStationValidates(t *testing.T) {
	bad := DefaultProfile()
	bad.Filter.M = 9
	if _, err := NewStation(bad, StationProfile{}); err == nil {
		t.Error("invalid profile accepted")
	}
	if _, err := NewStation(DefaultProfile(), StationProfile{Mode: "orbital"}); err == nil {
		t.Error("invalid station profile accepted")
	}
	w := DefaultProfile()
	w.Weights = map[string]float64{"static_positoin": 2}
	if _, err := NewStation(w, StationProfile{}); err == nil || !strings.Contains(err.Error(), "unknown check") {
		t.Errorf("misspelled weight accepted: %v", err)
	}
}

func TestStationCheckOrder(t *testing.T) {
	a := mustStation(t, fixedProfile()).Assess(t0)
	var names []string
	for _, r := range a.Checks {
		names = append(names, r.Check)
	}
	want := []string{CheckStaticPosition, CheckStationaryVelocity, CheckPositionVelocity, CheckClockBiasDrift, CheckClockDriftRate,
		CheckUTCOffset, CheckPPSRTCPhase, CheckCn0Uniformity, CheckAGC, CheckReceiverSpoofing}
	if !slices.Equal(names, want) {
		t.Fatalf("checks = %v, want %v", names, want)
	}
	if a.State != Unavailable || a.Engine != EngineVersion || !strings.HasPrefix(a.ConfigHash, "sha256:") || !a.Surveyed || a.Mode != ModeFixed {
		t.Fatalf("fresh assessment = %+v", a)
	}
}

// feedHealthy applies a quiet fixed station for n seconds starting at second from.
func feedHealthy(s *Station, from, n int) {
	for i := from; i < from+n; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		s.ApplySolution(solutionAt(i, 0.5, -0.3, 0.8, 0, 0, 0))
		s.ApplyClock(clockAt(i))
		s.ApplyStatus(ReceiverStatus{Received: at, SpoofState: SpoofNone, SinceStartMS: uint32(10_000_000 + i*1000), HaveStart: true})
		s.ApplyTiming(timingAt(i, 0))
		s.ApplyCn0(Cn0Fit{Received: at, Mean: 41, ResidVar: 16, NumSats: 12})
		dep := 20.0
		s.ApplyRF(RFSample{Received: at, Bands: []RFBand{{Departure: &dep, AntStatus: 2}}})
	}
}

func TestStationHealthyIsAssured(t *testing.T) {
	s := mustStation(t, fixedProfile())
	feedHealthy(s, 0, 200)
	a := s.Assess(t0.Add(200 * time.Second))
	if a.State != Assured || a.SpoofingIndicated || len(a.UnassuredDomains) != 0 {
		t.Fatalf("healthy station = %+v", a)
	}
	for _, r := range a.Checks {
		if r.State != Assured {
			t.Errorf("%s = %s (%v)", r.Check, r.State, r.Reasons)
		}
	}
}

// TestStationTimeAndPositionSpoof moves the antenna 200 m and steps the receiver clock
// and the PPS together, as a takeover would: position, receiver clock and time
// reference all become unassured, which indicates spoofing.
func TestStationTimeAndPositionSpoof(t *testing.T) {
	s := mustStation(t, fixedProfile())
	feedHealthy(s, 0, 200)
	for i := 200; i < 210; i++ {
		at := t0.Add(time.Duration(i) * time.Second)
		s.ApplySolution(solutionAt(i, 200, 0, 0, 0, 0, 0))
		c := clockAt(i)
		c.BiasNs += 3000
		s.ApplyClock(c)
		s.ApplyStatus(ReceiverStatus{Received: at, SpoofState: SpoofNone, SinceStartMS: uint32(10_000_000 + i*1000), HaveStart: true})
		s.ApplyTiming(timingAt(i, 3000_000))
	}
	a := s.Assess(t0.Add(210 * time.Second))
	want := []Domain{DomainPosition, DomainReceiverClock, DomainTimeReference}
	if a.State != Unassured || !a.SpoofingIndicated || !slices.Equal(a.UnassuredDomains, want) {
		t.Fatalf("takeover = state %s spoofing %v domains %v, want unassured/true/%v", a.State, a.SpoofingIndicated, a.UnassuredDomains, want)
	}
	if r := checkResult(t, a, CheckReceiverSpoofing); r.State != Assured {
		t.Fatalf("the receiver's own flag stayed clean, as a cold-start spoof can: %+v", r)
	}
}

func TestStationSingleDomainIsInconsistent(t *testing.T) {
	s := mustStation(t, fixedProfile())
	feedHealthy(s, 0, 200)
	for i := 200; i < 206; i++ {
		s.ApplySolution(solutionAt(i, 200, 0, 0, 0, 0, 0))
	}
	a := s.Assess(t0.Add(206 * time.Second))
	if a.State != Inconsistent || a.SpoofingIndicated {
		t.Fatalf("position alone = %+v, want inconsistent without a spoofing indication", a.Fusion)
	}
}

func TestStationStaleInputs(t *testing.T) {
	s := mustStation(t, fixedProfile())
	feedHealthy(s, 0, 200)
	a := s.Assess(t0.Add(200*time.Second + DefaultProfile().StaleAfter + time.Second))
	if a.State != Unavailable {
		t.Fatalf("after input stopped = %+v", a.Fusion)
	}
	for _, r := range a.Checks {
		if r.State != Unavailable || !slices.Contains(r.Reasons, ReasonStale) {
			t.Errorf("%s = %s %v, want unavailable/stale", r.Check, r.State, r.Reasons)
		}
	}
}

// TestAssessmentCarriesNoCoordinates guards the privacy rule: the served assessment
// names offsets from the survey but never the surveyed or reported coordinates.
func TestAssessmentCarriesNoCoordinates(t *testing.T) {
	s := mustStation(t, fixedProfile())
	feedHealthy(s, 0, 30)
	b, err := json.Marshal(s.Assess(t0.Add(30 * time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range []float64{site.LatDeg, site.LonDeg} {
		for _, form := range []string{fmt.Sprint(v), fmt.Sprintf("%.4f", v), fmt.Sprintf("%.3f", v)} {
			if strings.Contains(string(b), form) {
				t.Fatalf("assessment JSON contains coordinate %s: %s", form, b)
			}
		}
	}
	for _, key := range []string{"lat", "lon", "latitude", "longitude"} {
		if strings.Contains(string(b), `"`+key) {
			t.Fatalf("assessment JSON has a %q key: %s", key, b)
		}
	}
}

// TestStationDeterministic: the same inputs give byte-identical assessments, the
// property replay depends on.
func TestStationDeterministic(t *testing.T) {
	run := func() []byte {
		s := mustStation(t, fixedProfile())
		feedHealthy(s, 0, 150)
		for i := 150; i < 160; i++ {
			s.ApplySolution(solutionAt(i, 70, 0, 0, 0, 0, 0))
		}
		b, err := json.Marshal(s.Assess(t0.Add(160 * time.Second)))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if a, b := run(), run(); string(a) != string(b) {
		t.Fatalf("assessments differ:\n%s\n%s", a, b)
	}
}
