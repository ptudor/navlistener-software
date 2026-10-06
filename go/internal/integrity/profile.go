package integrity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
}

// DefaultProfile returns the standard operating points. They are conservative
// starting values, to be calibrated against stored station data.
func DefaultProfile() Profile {
	return Profile{
		// Three of the last four, as in the CISA Epsilon monitors.
		Filter: FilterPolicy{M: 3, N: 4, RecoveryHold: 5 * time.Minute},
		// Matches the station RF staleness bound (state.rfStaleAfter).
		StaleAfter: 5 * time.Minute,
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
		if !finite(w) || w < 0 {
			return fmt.Errorf("integrity: weight for %s must be a finite non-negative number, got %g", name, w)
		}
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
			p.HeightM < -1000 || p.HeightM > 10000 {
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
