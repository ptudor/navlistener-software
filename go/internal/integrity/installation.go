package integrity

import (
	"fmt"
	"time"
)

// Installation describes the antenna's use. It never configures receiver dynamics.
type Installation string

const (
	FixedStructure Installation = "fixed_structure"
	Pedestrian     Installation = "pedestrian"
	Bicycle        Installation = "bicycle"
	RoadVehicle    Installation = "road_vehicle"
	Aircraft       Installation = "aircraft"
	PortableTest   Installation = "portable_test"
)

type MotionPolicy struct {
	ReportedSpeed              bool    `toml:"reported_speed" json:"reported_speed"`
	MaxAccelerationMPS2        float64 `toml:"max_acceleration_mps2" json:"max_acceleration_mps2,omitempty"`
	MaxLateralAccelerationMPS2 float64 `toml:"max_lateral_acceleration_mps2" json:"max_lateral_acceleration_mps2,omitempty"`
	MaxVerticalSpeedMPS        float64 `toml:"max_vertical_speed_mps" json:"max_vertical_speed_mps,omitempty"`
	// Zero keeps the legacy continuity rule. Only portable_test may reset across gaps.
	Gap                  time.Duration `toml:"gap" json:"gap_ns,omitempty"`
	ResetAfterGap        bool          `toml:"reset_after_gap" json:"reset_after_gap"`
	MaxSpeedAccuracyMPS  float64       `toml:"max_speed_accuracy_mps" json:"max_speed_accuracy_mps,omitempty"`
	MaxPositionAccuracyM float64       `toml:"max_position_accuracy_m" json:"max_position_accuracy_m,omitempty"`
}

// FixedPolicy is opt-in: a tight displacement test needs both receiver and
// reference uncertainty to resolve the requested movement. Values are metres.
type FixedPolicy struct {
	DisplacementM          float64 `toml:"displacement_m" json:"displacement_m,omitempty"`
	VerticalDisplacementM  float64 `toml:"vertical_displacement_m" json:"vertical_displacement_m,omitempty"`
	MaxHorizontalAccuracyM float64 `toml:"max_horizontal_accuracy_m" json:"max_horizontal_accuracy_m,omitempty"`
	MaxVerticalAccuracyM   float64 `toml:"max_vertical_accuracy_m" json:"max_vertical_accuracy_m,omitempty"`
	MinSV                  int     `toml:"min_sv" json:"min_sv,omitempty"`
	RequireGNSSOnly        bool    `toml:"require_gnss_only" json:"require_gnss_only"`
}

// ReferenceEvidence describes the explicitly pinned position. Receiver-derived
// positions remain dependent evidence, even after operator acceptance.
type ReferenceEvidence struct {
	Source                 string           `toml:"source" json:"source"`
	AcceptedBy             string           `toml:"accepted_by" json:"accepted_by"`
	Reason                 string           `toml:"reason" json:"reason"`
	SiteID                 string           `toml:"site_id" json:"site_id"`
	HorizontalUncertaintyM float64          `toml:"horizontal_uncertainty_m" json:"horizontal_uncertainty_m"`
	VerticalUncertaintyM   float64          `toml:"vertical_uncertainty_m" json:"vertical_uncertainty_m"`
	Candidate              *SurveyCandidate `toml:"candidate" json:"candidate,omitempty"`
}

// InstallationDefaults supplies analysis starting points, not certified physical
// envelopes. Aircraft deliberately has its own vertical and acceleration limits.
func InstallationDefaults(i Installation) (StationProfile, error) {
	s := StationProfile{Installation: i, Mode: ModeMobile,
		Motion: MotionPolicy{ReportedSpeed: true, MaxSpeedAccuracyMPS: 2, MaxPositionAccuracyM: 25, Gap: 10 * time.Second}}
	switch i {
	case FixedStructure:
		s.Mode, s.Motion = ModeFixed, MotionPolicy{}
	case Pedestrian:
		s.MaxSpeedMPS, s.Motion.MaxAccelerationMPS2 = 3, 4
	case Bicycle:
		s.MaxSpeedMPS, s.Motion.MaxAccelerationMPS2, s.Motion.MaxLateralAccelerationMPS2 = 20, 6, 6
	case RoadVehicle:
		s.MaxSpeedMPS, s.Motion.MaxAccelerationMPS2, s.Motion.MaxLateralAccelerationMPS2 = 70, 12, 10
	case Aircraft:
		s.MaxSpeedMPS, s.Motion.MaxVerticalSpeedMPS = 350, 80
		s.Motion.MaxAccelerationMPS2, s.Motion.MaxLateralAccelerationMPS2 = 30, 30
	case PortableTest:
		s.MaxSpeedMPS, s.Motion.MaxAccelerationMPS2 = 50, 15
		s.Motion.Gap, s.Motion.ResetAfterGap = 30*time.Second, true
	default:
		return StationProfile{}, fmt.Errorf("integrity: unknown installation %q", i)
	}
	return s, nil
}

func (s StationProfile) validateInstallation() error {
	if s.Installation != "" {
		d, err := InstallationDefaults(s.Installation)
		if err != nil {
			return err
		}
		if s.Mode != d.Mode {
			return fmt.Errorf("integrity: installation %s requires mode %s", s.Installation, d.Mode)
		}
	}
	m, f := s.Motion, s.Fixed
	for name, v := range map[string]float64{"max_acceleration_mps2": m.MaxAccelerationMPS2, "max_lateral_acceleration_mps2": m.MaxLateralAccelerationMPS2, "max_vertical_speed_mps": m.MaxVerticalSpeedMPS, "max_speed_accuracy_mps": m.MaxSpeedAccuracyMPS, "max_position_accuracy_m": m.MaxPositionAccuracyM, "displacement_m": f.DisplacementM, "vertical_displacement_m": f.VerticalDisplacementM, "max_horizontal_accuracy_m": f.MaxHorizontalAccuracyM, "max_vertical_accuracy_m": f.MaxVerticalAccuracyM} {
		if err := nonNegative(name, v); err != nil {
			return err
		}
	}
	if m.Gap < 0 || m.Gap > time.Hour {
		return fmt.Errorf("integrity: motion gap must be within 0..1h")
	}
	if m.ResetAfterGap && (s.Installation != PortableTest || m.Gap <= 0) {
		return fmt.Errorf("integrity: reset_after_gap requires portable_test and a positive gap")
	}
	if (m != MotionPolicy{}) && s.Mode != ModeMobile {
		return fmt.Errorf("integrity: motion policy requires mobile mode")
	}
	if (f != FixedPolicy{} || s.Reference != nil || s.Survey != nil) && s.Mode != ModeFixed {
		return fmt.Errorf("integrity: fixed/reference/survey policy requires fixed mode")
	}
	if f.MinSV < 0 || f.MinSV > 255 {
		return fmt.Errorf("integrity: min_sv must be within 0..255")
	}
	if f.DisplacementM > 0 || f.VerticalDisplacementM > 0 {
		if s.Position == nil || s.Reference == nil {
			return fmt.Errorf("integrity: displacement policy requires a pinned position and reference evidence")
		}
		if f.DisplacementM > 0 && (f.MaxHorizontalAccuracyM <= 0 || f.MaxHorizontalAccuracyM > f.DisplacementM/3 || s.Reference.HorizontalUncertaintyM > f.DisplacementM/3) {
			return fmt.Errorf("integrity: horizontal uncertainty must resolve displacement_m at three sigma")
		}
		if f.VerticalDisplacementM > 0 && (f.MaxVerticalAccuracyM <= 0 || f.MaxVerticalAccuracyM > f.VerticalDisplacementM/3 || s.Reference.VerticalUncertaintyM > f.VerticalDisplacementM/3) {
			return fmt.Errorf("integrity: vertical uncertainty must resolve vertical_displacement_m at three sigma")
		}
	}
	if r := s.Reference; r != nil {
		if s.Position == nil || r.Source == "" || r.AcceptedBy == "" || r.Reason == "" || r.SiteID == "" {
			return fmt.Errorf("integrity: reference requires position, source, accepted_by, reason and site_id")
		}
		if err := nonNegative("reference horizontal uncertainty", r.HorizontalUncertaintyM); err != nil {
			return err
		}
		if err := nonNegative("reference vertical uncertainty", r.VerticalUncertaintyM); err != nil {
			return err
		}
		if r.HorizontalUncertaintyM == 0 || r.VerticalUncertaintyM == 0 {
			return fmt.Errorf("integrity: reference uncertainties must be positive")
		}
		if r.Candidate != nil {
			if err := r.Candidate.Validate(); err != nil {
				return err
			}
			if r.Source != "receiver_survey" || *s.Position != r.Candidate.Position {
				return fmt.Errorf("integrity: survey reference must retain its candidate position and receiver_survey source")
			}
			u := max(r.Candidate.MaxReportedAccuracyM, r.Candidate.DispersionM)
			if r.HorizontalUncertaintyM < u || r.VerticalUncertaintyM < u {
				return fmt.Errorf("integrity: accepted survey uncertainty cannot be smaller than its reported accuracy or dispersion")
			}
		}
		if r.Source == "receiver_survey" && r.Candidate == nil {
			return fmt.Errorf("integrity: receiver_survey reference requires candidate evidence")
		}
	}
	if s.Survey != nil {
		return s.Survey.Validate()
	}
	return nil
}

// Clone prevents callers from mutating a validated configuration through pointers.
func (s StationProfile) Clone() StationProfile {
	if s.Position != nil {
		v := *s.Position
		s.Position = &v
	}
	if s.Baseline != nil {
		v := *s.Baseline
		s.Baseline = &v
	}
	if s.Survey != nil {
		v := *s.Survey
		s.Survey = &v
	}
	if s.Reference != nil {
		v := *s.Reference
		s.Reference = &v
		if v.Candidate != nil {
			c := *v.Candidate
			s.Reference.Candidate = &c
		}
	}
	return s
}
