package integrity

import (
	"math"
	"slices"
	"testing"
	"time"
)

func pairProfile(partner string, distance float64) StationProfile {
	sp := fixedProfile()
	sp.Baseline = &Baseline{Partner: partner, DistanceM: distance}
	return sp
}

func TestBaselineAgreesWithTheKnownDistance(t *testing.T) {
	prof := DefaultProfile().Baseline
	own, partner := solutionAt(0, 0, 0, 0, 0, 0, 0), solutionAt(0, 50, 0, 0, 0, 0, 0)
	v := baseline(own, partner, 50, prof)
	if v.State != Assured || math.Abs(v.Metrics["measured_m"]-50) > 0.01 || v.Metrics["error_m"] > 0.01 {
		t.Fatalf("matching baseline = %+v", v)
	}
	// σ is the pair's combined 3D accuracy: hypot(hypot(1.5, 2.5), hypot(1.5, 2.5)).
	if want := math.Sqrt(2 * (1.5*1.5 + 2.5*2.5)); math.Abs(v.Metrics["sigma_m"]-want) > 1e-9 {
		t.Fatalf("sigma = %v, want %v", v.Metrics["sigma_m"], want)
	}
}

// TestBaselineCollapse: a single transmitter feeding both receivers puts them at one
// position; with the antennas 50 m apart that is unassured, and named.
func TestBaselineCollapse(t *testing.T) {
	prof := DefaultProfile().Baseline
	own, partner := solutionAt(0, 3, 0, 0, 0, 0, 0), solutionAt(0, 3.5, 0, 0, 0, 0, 0)
	v := baseline(own, partner, 50, prof)
	if v.State != Unassured || !slices.Equal(v.Reasons, []string{ReasonBaselineCollapse}) {
		t.Fatalf("collapsed pair = %s %v %v", v.State, v.Reasons, v.Metrics)
	}
	// A short baseline cannot be told from noise: a collapse of 10 m stays assured.
	if v := baseline(own, partner, 10, prof); v.State != Assured {
		t.Fatalf("10 m collapse = %s %v", v.State, v.Metrics)
	}
}

func TestBaselineError(t *testing.T) {
	prof := DefaultProfile().Baseline
	own, partner := solutionAt(0, 0, 0, 0, 0, 0, 0), solutionAt(0, 0, 130, 0, 0, 0, 0)
	if v := baseline(own, partner, 50, prof); v.State != Unassured || !slices.Equal(v.Reasons, []string{ReasonBaselineError}) {
		t.Fatalf("80 m error = %s %v %v", v.State, v.Reasons, v.Metrics)
	}
	partner = solutionAt(0, 0, 66, 0, 0, 0, 0)
	if v := baseline(own, partner, 50, prof); v.State != Inconsistent || v.Thresholds["inconsistent_m"] != 3*v.Metrics["sigma_m"] {
		t.Fatalf("16 m error = %s %v %v", v.State, v.Metrics, v.Thresholds)
	}
}

func TestBaselineNeedsTwoFixesAtOneEpoch(t *testing.T) {
	prof := DefaultProfile().Baseline
	own, partner := solutionAt(0, 0, 0, 0, 0, 0, 0), solutionAt(0, 50, 0, 0, 0, 0, 0)
	lost := partner
	lost.FixOK = false
	if v := baseline(own, lost, 50, prof); v.State != Unavailable || !slices.Equal(v.Reasons, []string{ReasonNo3DFix}) {
		t.Fatalf("no partner fix = %+v", v)
	}
	late := partner
	late.TOW += 200
	if v := baseline(own, late, 50, prof); v.State != Unavailable || !slices.Equal(v.Reasons, []string{ReasonNoCommonEpoch}) {
		t.Fatalf("200 ms apart = %+v", v)
	}
	// Within the skew, a moving pair's motion widens the accuracy.
	moving, movingPartner := solutionAt(0, 0, 0, 0, 20, 0, 0), solutionAt(0, 50, 0, 0, 20, 0, 0)
	movingPartner.TOW += 50
	still := baseline(own, partner, 50, prof)
	if v := baseline(moving, movingPartner, 50, prof); math.Abs(v.Metrics["sigma_m"]-still.Metrics["sigma_m"]-1) > 1e-9 {
		t.Fatalf("20 m/s over 50 ms adds %v m, want 1", v.Metrics["sigma_m"]-still.Metrics["sigma_m"])
	}
}

func TestStationBaselineCheck(t *testing.T) {
	if r := mustStation(t, fixedProfile()).Assess(t0).Checks; slices.ContainsFunc(r, func(r Result) bool { return r.Check == CheckBaseline }) {
		t.Fatal("baseline check without a partner")
	}
	s := mustStation(t, pairProfile("roof-west", 50))
	for i := 0; i < 5; i++ {
		own, partner := solutionAt(i, 0, 0, 0, 0, 0, 0), solutionAt(i, 0.4, 0, 0, 0, 0, 0)
		s.ApplyBaseline(own, partner, own.Received)
	}
	r := checkResult(t, s.Assess(t0.Add(5*time.Second)), CheckBaseline)
	if r.State != Unassured || r.Domain != DomainPosition || r.Version != 1 {
		t.Fatalf("collapsed pair = %+v", r)
	}
	// The partner and distance are part of the configuration hash.
	if mustStation(t, pairProfile("roof-west", 51)).ConfigHash() == s.ConfigHash() ||
		mustStation(t, pairProfile("roof-east", 50)).ConfigHash() == s.ConfigHash() {
		t.Fatal("baseline not in the configuration hash")
	}
	// A station without a partner ignores pair evaluations.
	alone := mustStation(t, fixedProfile())
	alone.ApplyBaseline(solutionAt(0, 0, 0, 0, 0, 0, 0), solutionAt(0, 0, 0, 0, 0, 0, 0), t0)
}

func TestBaselineValidation(t *testing.T) {
	for name, b := range map[string]Baseline{
		"no partner":    {DistanceM: 10},
		"zero":          {Partner: "x"},
		"negative":      {Partner: "x", DistanceM: -1},
		"too far":       {Partner: "x", DistanceM: maxBaselineM + 1},
		"not a number":  {Partner: "x", DistanceM: math.NaN()},
		"infinite away": {Partner: "x", DistanceM: math.Inf(1)},
	} {
		sp := fixedProfile()
		sp.Baseline = &b
		if err := sp.Validate(); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	p := DefaultProfile()
	p.Baseline.MaxEpochSkew = -time.Millisecond
	if p.Validate() == nil {
		t.Error("negative epoch skew accepted")
	}
}
