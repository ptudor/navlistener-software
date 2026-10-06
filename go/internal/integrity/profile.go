package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

// FilterPolicy is the per-check noise filter and recovery hysteresis.
type FilterPolicy struct {
	// M of the last N available evaluations must support a degraded state before
	// it becomes the candidate.
	M int `json:"m"`
	N int `json:"n"`
	// RecoveryHold is how long every candidate must stay better than the served
	// degraded state before the better state is served.
	RecoveryHold time.Duration `json:"recovery_hold_ns"`
}

// Profile is the complete, versioned set of operating points. Every served result
// and event names the hash of the profile it was evaluated with, so a stored judgment
// can be reproduced and a threshold change is visible.
type Profile struct {
	Filter FilterPolicy `json:"filter"`
	// StaleAfter is how long a check may go without input before it is evaluated
	// as unavailable.
	StaleAfter time.Duration `json:"stale_after_ns"`
	// Weights are fusion weights by check name; an absent check weighs 1.
	Weights map[string]float64 `json:"weights,omitempty"`

	StaticPosition     StaticPositionProfile   `json:"static_position"`
	StationaryVelocity ScaledBands             `json:"stationary_velocity"`
	MotionBound        MotionBoundProfile      `json:"motion_bound"`
	PositionVelocity   PositionVelocityProfile `json:"position_velocity"`
	ClockBiasDrift     ClockBiasDriftProfile   `json:"clock_bias_drift"`
	ClockDriftRate     ClockDriftRateProfile   `json:"clock_drift_rate"`
	UTCOffset          UTCOffsetProfile        `json:"utc_offset"`
	PPSRTCPhase        PPSRTCPhaseProfile      `json:"pps_rtc_phase"`
	Cn0Uniformity      Cn0UniformityProfile    `json:"cn0_uniformity"`
	Cn0Drop            Cn0DropProfile          `json:"cn0_drop"`
	AGC                AGCProfile              `json:"agc"`
}

// DefaultProfile returns the standard operating points. They are conservative
// starting values, to be calibrated against stored station data; where a value
// follows an upstream monitor, the comment names it.
func DefaultProfile() Profile {
	return Profile{
		// Three of the last four, as in the CISA Epsilon monitors.
		Filter: FilterPolicy{M: 3, N: 4, RecoveryHold: 5 * time.Minute},
		// Matches the station RF staleness bound (state.rfStaleAfter).
		StaleAfter: 5 * time.Minute,

		// Epsilon's stationary position monitor alarms at 21.1 m from its running
		// average; these bands sit either side of it and scale with the reported
		// accuracy. The ten-minute mean catches a steady offset below them.
		StaticPosition: StaticPositionProfile{
			Horizontal: ScaledBands{
				Inconsistent: ScaledBand{Sigmas: 3, Min: 15, Max: 100},
				Unassured:    ScaledBand{Sigmas: 6, Min: 50, Max: 300},
			},
			Vertical: ScaledBands{
				Inconsistent: ScaledBand{Sigmas: 3, Min: 25, Max: 150},
				Unassured:    ScaledBand{Sigmas: 6, Min: 80, Max: 450},
			},
			DriftWindow: 10 * time.Minute, DriftMinEpochs: 300,
			DriftHorizontalM: 8, DriftVerticalM: 15,
			DriftUnassuredHorizontalM: 25, DriftUnassuredVerticalM: 40,
		},
		// Epsilon's stationary velocity monitor alarms at 0.71 m/s (0.5 m²/s²).
		StationaryVelocity: ScaledBands{
			Inconsistent: ScaledBand{Sigmas: 3, Min: 0.5, Max: 5},
			Unassured:    ScaledBand{Sigmas: 6, Min: 2, Max: 20},
		},
		MotionBound: MotionBoundProfile{Sigmas: 3, MarginM: 10, MaxReferenceAge: 10 * time.Minute},
		PositionVelocity: PositionVelocityProfile{
			Window: 5 * time.Second, MaxSlack: 2 * time.Second,
			Bands: ScaledBands{
				Inconsistent: ScaledBand{Sigmas: 3, Min: 1, Max: 10},
				Unassured:    ScaledBand{Sigmas: 6, Min: 3, Max: 30},
			},
		},
		// Epsilon's clock bias/drift divergence monitor (ccd_monitor): 30–40 s
		// window, alarm at 43.6 m, which is 145 ns.
		ClockBiasDrift: ClockBiasDriftProfile{
			MinWindow: 30 * time.Second, MaxWindow: 40 * time.Second,
			InconsistentNs: 75, UnassuredNs: 145, AdjustmentToleranceNs: 1000,
		},
		// Epsilon's clock rate monitor uses a 60–120 s window and 0.0015 m/s²
		// (0.005 ns/s²). u-blox reports drift in whole ns/s, so one quantization
		// step over 60 s is already 0.017 ns/s²; these bands sit well above both
		// that step and a cheap oscillator's thermal drift.
		ClockDriftRate: ClockDriftRateProfile{
			MinWindow: 60 * time.Second, MaxWindow: 120 * time.Second,
			InconsistentNsPerS2: 0.05, UnassuredNsPerS2: 0.2,
		},
		UTCOffset: UTCOffsetProfile{Inconsistent: 2 * time.Second, Unassured: 5 * time.Second},
		PPSRTCPhase: PPSRTCPhaseProfile{
			FitMinAge: 10 * time.Second, FitMaxAge: 180 * time.Second, FitMinSamples: 60,
			MaxSampleGap: 5 * time.Second, InconsistentNs: 20_000, UnassuredNs: 100_000,
		},
		Cn0Uniformity: Cn0UniformityProfile{ResidVar: DefaultCn0ResidVar, Mean: DefaultCn0Mean, MinSats: DefaultCn0MinSats},
		// Epsilon's C/N₀ drop monitor alarms when every signal falls by 1 dB within
		// 5 s. u-blox reports whole dB-Hz, so 1 dB is a single step; six signals
		// and a graded median keep chance coincidences of that step out.
		Cn0Drop: Cn0DropProfile{
			Window: 5 * time.Second, MinSpan: 3 * time.Second, MinSignals: 6, MinQuality: 4,
			EveryDB: 1, InconsistentDB: 3, UnassuredDB: 6,
		},
		AGC: AGCProfile{Departure: DefaultAGCDeparture, DepartureSevere: DefaultAGCDepartureSevere, CWSuppress: DefaultCWSuppress},
	}
}

// Validate rejects a profile the trackers cannot apply.
func (p Profile) Validate() error {
	if p.Filter.N < 1 || p.Filter.M < 1 || p.Filter.M > p.Filter.N {
		return fmt.Errorf("integrity: filter needs 1 <= m <= n, got m=%d n=%d", p.Filter.M, p.Filter.N)
	}
	if p.Filter.RecoveryHold < 0 {
		return fmt.Errorf("integrity: negative recovery hold %s", p.Filter.RecoveryHold)
	}
	if p.StaleAfter <= 0 {
		return fmt.Errorf("integrity: stale bound must be positive, got %s", p.StaleAfter)
	}
	for name, w := range p.Weights {
		if _, ok := checkInfos[name]; !ok {
			return fmt.Errorf("integrity: weight for unknown check %q", name)
		}
		if !finite(w) || w < 0 {
			return fmt.Errorf("integrity: weight for %s must be a finite non-negative number, got %g", name, w)
		}
	}
	sp := p.StaticPosition
	errs := []error{
		sp.Horizontal.validate("static_position.horizontal"),
		sp.Vertical.validate("static_position.vertical"),
		p.StationaryVelocity.validate("stationary_velocity"),
		p.PositionVelocity.Bands.validate("position_velocity.bands"),
		positive("static_position.drift_window", sp.DriftWindow.Seconds()),
		bands("static_position.drift_horizontal", sp.DriftHorizontalM, sp.DriftUnassuredHorizontalM),
		bands("static_position.drift_vertical", sp.DriftVerticalM, sp.DriftUnassuredVerticalM),
		nonNegative("motion_bound.sigmas", p.MotionBound.Sigmas),
		nonNegative("motion_bound.margin_m", p.MotionBound.MarginM),
		positive("motion_bound.max_reference_age", p.MotionBound.MaxReferenceAge.Seconds()),
		positive("position_velocity.window", p.PositionVelocity.Window.Seconds()),
		nonNegative("position_velocity.max_slack", p.PositionVelocity.MaxSlack.Seconds()),
		window("clock_bias_drift", p.ClockBiasDrift.MinWindow, p.ClockBiasDrift.MaxWindow),
		bands("clock_bias_drift", p.ClockBiasDrift.InconsistentNs, p.ClockBiasDrift.UnassuredNs),
		positive("clock_bias_drift.adjustment_tolerance_ns", p.ClockBiasDrift.AdjustmentToleranceNs),
		window("clock_drift_rate", p.ClockDriftRate.MinWindow, p.ClockDriftRate.MaxWindow),
		bands("clock_drift_rate", p.ClockDriftRate.InconsistentNsPerS2, p.ClockDriftRate.UnassuredNsPerS2),
		bands("utc_offset", p.UTCOffset.Inconsistent.Seconds(), p.UTCOffset.Unassured.Seconds()),
		window("pps_rtc_phase.fit", p.PPSRTCPhase.FitMinAge, p.PPSRTCPhase.FitMaxAge),
		bands("pps_rtc_phase", p.PPSRTCPhase.InconsistentNs, p.PPSRTCPhase.UnassuredNs),
		positive("pps_rtc_phase.max_sample_gap", p.PPSRTCPhase.MaxSampleGap.Seconds()),
		positive("cn0_uniformity.resid_var_db2", p.Cn0Uniformity.ResidVar),
		positive("cn0_uniformity.mean_db_hz", p.Cn0Uniformity.Mean),
		window("cn0_drop", p.Cn0Drop.MinSpan, p.Cn0Drop.Window),
		positive("cn0_drop.every_drop_db", p.Cn0Drop.EveryDB),
		bands("cn0_drop.median", p.Cn0Drop.InconsistentDB, p.Cn0Drop.UnassuredDB),
		bands("agc.departure", p.AGC.Departure, p.AGC.DepartureSevere),
	}
	if sp.DriftMinEpochs < 1 {
		errs = append(errs, fmt.Errorf("static_position.drift_min_epochs must be at least 1"))
	}
	if p.PPSRTCPhase.FitMinSamples < 2 {
		errs = append(errs, fmt.Errorf("pps_rtc_phase.fit_min_samples must be at least 2"))
	}
	if p.Cn0Uniformity.MinSats < 3 {
		errs = append(errs, fmt.Errorf("cn0_uniformity.min_sats must be at least 3"))
	}
	if p.Cn0Drop.MinSignals < 3 {
		errs = append(errs, fmt.Errorf("cn0_drop.min_signals must be at least 3"))
	}
	if p.Cn0Drop.MinQuality < 1 || p.Cn0Drop.MinQuality > 7 {
		errs = append(errs, fmt.Errorf("cn0_drop.min_quality must be within 1..7"))
	}
	if p.AGC.CWSuppress < 0 || p.AGC.CWSuppress > 255 {
		errs = append(errs, fmt.Errorf("agc.cw_suppress must be within 0..255"))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("integrity: %w", err)
	}
	return nil
}

func positive(name string, v float64) error {
	if !finite(v) || v <= 0 {
		return fmt.Errorf("%s must be a finite positive number, got %g", name, v)
	}
	return nil
}

func nonNegative(name string, v float64) error {
	if !finite(v) || v < 0 {
		return fmt.Errorf("%s must be a finite non-negative number, got %g", name, v)
	}
	return nil
}

func window(name string, minW, maxW time.Duration) error {
	if minW <= 0 || maxW < minW {
		return fmt.Errorf("%s window needs 0 < min <= max, got %s..%s", name, minW, maxW)
	}
	return nil
}

func bands(name string, inconsistent, unassured float64) error {
	if !finite(inconsistent) || !finite(unassured) || inconsistent <= 0 || unassured < inconsistent {
		return fmt.Errorf("%s needs 0 < inconsistent <= unassured, got %g and %g", name, inconsistent, unassured)
	}
	return nil
}

func (b ScaledBands) validate(name string) error {
	for _, x := range []struct {
		which string
		band  ScaledBand
	}{{"inconsistent", b.Inconsistent}, {"unassured", b.Unassured}} {
		if !finite(x.band.Sigmas) || !finite(x.band.Min) || !finite(x.band.Max) ||
			x.band.Sigmas < 0 || x.band.Min <= 0 || x.band.Max < x.band.Min {
			return fmt.Errorf("%s.%s needs sigmas >= 0 and 0 < min <= max, got %+v", name, x.which, x.band)
		}
	}
	if b.Unassured.Min < b.Inconsistent.Min || b.Unassured.Max < b.Inconsistent.Max || b.Unassured.Sigmas < b.Inconsistent.Sigmas {
		return fmt.Errorf("%s unassured band must not be tighter than the inconsistent band", name)
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// Mode is how a station is installed.
type Mode string

const (
	// ModeUnknown: no installation profile is configured.
	ModeUnknown Mode = ""
	// ModeFixed: the antenna does not move; a surveyed position may be configured.
	ModeFixed Mode = "fixed"
	// ModeMobile: the antenna moves no faster than a configured speed.
	ModeMobile Mode = "mobile"
)

// Surveyed is a surveyed antenna position: WGS-84 latitude and longitude in degrees,
// height above the ellipsoid in metres.
type Surveyed struct {
	LatDeg  float64 `json:"lat_deg"`
	LonDeg  float64 `json:"lon_deg"`
	HeightM float64 `json:"height_m"`
}

// StationProfile is one station's installation profile.
type StationProfile struct {
	Mode        Mode      `json:"mode"`
	Position    *Surveyed `json:"position,omitempty"`
	MaxSpeedMPS float64   `json:"max_speed_mps,omitempty"`
}

// Validate rejects an installation profile the checks cannot apply.
func (s StationProfile) Validate() error {
	switch s.Mode {
	case ModeUnknown, ModeFixed, ModeMobile:
	default:
		return fmt.Errorf("integrity: unknown station mode %q", s.Mode)
	}
	if s.Position != nil {
		p := s.Position
		if !finite(p.LatDeg) || !finite(p.LonDeg) || !finite(p.HeightM) ||
			p.LatDeg < -90 || p.LatDeg > 90 || p.LonDeg < -180 || p.LonDeg > 180 ||
			p.HeightM < -1000 || p.HeightM > 20000 {
			return fmt.Errorf("integrity: surveyed position out of range: %+v", *p)
		}
		if s.Mode == ModeMobile {
			return fmt.Errorf("integrity: a mobile station has no surveyed position")
		}
	}
	if !finite(s.MaxSpeedMPS) || s.MaxSpeedMPS < 0 {
		return fmt.Errorf("integrity: maximum speed must be a finite non-negative number, got %g", s.MaxSpeedMPS)
	}
	if s.MaxSpeedMPS > 0 && s.Mode != ModeMobile {
		return fmt.Errorf("integrity: a maximum speed applies only to a mobile station")
	}
	return nil
}

// ConfigHash identifies everything that determines a station's evaluation besides
// its inputs: the engine version, each check's version, the profile and the station
// profile. It is "sha256:" and the hex digest of their canonical JSON (struct field
// order is fixed and encoding/json sorts map keys). The surveyed position is part
// of the hash but cannot be read back from it in practice. Both profiles must have
// passed Validate.
func ConfigHash(p Profile, station StationProfile, checks map[string]int) string {
	b, err := json.Marshal(struct {
		Engine  int            `json:"engine"`
		Checks  map[string]int `json:"checks"`
		Profile Profile        `json:"profile"`
		Station StationProfile `json:"station"`
	}{EngineVersion, checks, p, station})
	if err != nil {
		// Validated profiles hold only finite numbers, strings and maps of them.
		panic(fmt.Sprintf("integrity: config hash: %v", err))
	}
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
