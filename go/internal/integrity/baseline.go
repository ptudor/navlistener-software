package integrity

import (
	"fmt"
	"math"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/geo"
	"github.com/ptudor/gnss/physconst"
)

// CheckBaseline compares the distance between two co-located stations' solutions with
// the known distance between their antennas.
const CheckBaseline = "baseline"

// baseline reads the station's position, so it shares the position domain: a station
// whose own fix is wrong fails it together with its position checks, and one bad fix
// must not count twice toward the spoofing quorum.
var baselineInfo = Info{Name: CheckBaseline, Version: 1, Domain: DomainPosition}

// Reason codes for the baseline check.
const (
	// ReasonBaselineCollapse: both stations report the same position, as when a
	// single transmitter feeds every receiver it reaches.
	ReasonBaselineCollapse = "baseline_collapse"
	ReasonBaselineError    = "baseline_error"
	ReasonNoCommonEpoch    = "no_common_epoch"
)

// maxBaselineM bounds a configured baseline: the check is for co-located antennas.
const maxBaselineM = 100_000

// Baseline is a station's partner in a co-located pair and the distance between their
// antennas, metres.
type Baseline struct {
	Partner   string  `json:"partner"`
	DistanceM float64 `json:"distance_m"`
}

// SurveyedDistanceM is the straight-line distance between two surveyed positions,
// metres.
func SurveyedDistanceM(a, b Surveyed) float64 {
	return surveyedECEF(a).Sub(surveyedECEF(b)).Norm()
}

func surveyedECEF(s Surveyed) gnss.ECEF {
	return geo.GeodeticToECEF(geo.Geodetic{Lat: geo.Rad(s.LatDeg), Lon: geo.Rad(s.LonDeg), Height: s.HeightM}, physconst.WGS84)
}

// EpochSkew is the GPS-time difference between two times of week, across the week
// rollover.
func EpochSkew(a, b uint32) time.Duration {
	d := towDelta(a, b)
	if d < 0 {
		d = -d
	}
	return time.Duration(d) * time.Millisecond
}

func (b Baseline) validate() error {
	if b.Partner == "" {
		return fmt.Errorf("integrity: a baseline needs a partner station")
	}
	if !finite(b.DistanceM) || b.DistanceM <= 0 || b.DistanceM > maxBaselineM {
		return fmt.Errorf("integrity: baseline distance must be within (0, %d] m, got %g", maxBaselineM, b.DistanceM)
	}
	return nil
}

// BaselineProfile holds the baseline operating points. The bands apply to the
// difference between the measured and known distances, scaled by the pair's
// combined 3D accuracy.
type BaselineProfile struct {
	Bands ScaledBands `json:"bands"`
	// MaxEpochSkew is the largest difference in GPS time between the two epochs
	// compared; a moving pair's motion over it widens the accuracy.
	MaxEpochSkew time.Duration `json:"max_epoch_skew_ns"`
}

// baseline evaluates one pair of epochs. A collapse, both stations reporting one
// position while their antennas are apart, is the single-transmitter signature; it
// can be told from noise only when the known distance exceeds the bands.
func baseline(own, partner Solution, knownM float64, prof BaselineProfile) Verdict {
	thresholds := map[string]float64{"max_epoch_skew_ms": float64(prof.MaxEpochSkew.Milliseconds())}
	if !own.usable3D() || !partner.usable3D() {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNo3DFix}, Thresholds: thresholds}
	}
	skew := math.Abs(float64(towDelta(own.TOW, partner.TOW)))
	if skew > float64(prof.MaxEpochSkew.Milliseconds()) {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoCommonEpoch}, Metrics: map[string]float64{"skew_ms": skew}, Thresholds: thresholds}
	}
	measured := own.ecef().Sub(partner.ecef()).Norm()
	speed := math.Max(math.Hypot(own.VelN, own.VelE), math.Hypot(partner.VelN, partner.VelE))
	sigma := math.Hypot(math.Hypot(own.HAccM, own.VAccM), math.Hypot(partner.HAccM, partner.VAccM)) + speed*skew/1000
	errM := math.Abs(measured - knownM)
	state, inc, un := prof.Bands.classify(errM, sigma)
	thresholds["inconsistent_m"], thresholds["unassured_m"] = inc, un
	v := Verdict{
		State: state,
		Metrics: map[string]float64{
			"measured_m": measured, "known_m": knownM, "error_m": errM, "sigma_m": sigma, "skew_ms": skew,
		},
		Thresholds: thresholds,
	}
	switch {
	case state == Assured:
	case measured <= inc && knownM > un:
		v.Reasons = []string{ReasonBaselineCollapse}
	default:
		v.Reasons = []string{ReasonBaselineError}
	}
	return v
}
