package config

import (
	"fmt"
	"math"

	"github.com/ptudor/navlistener/internal/integrity"
)

// Integrity holds the installation profiles the station integrity checks use
// (docs/proposals/STATION-ASSURANCE.md). Every station is checked; an installation
// profile adds the checks that depend on how the antenna is mounted.
type Integrity struct {
	Stations  []IntegrityStation  `toml:"station"`
	Baselines []IntegrityBaseline `toml:"baseline"`
}

// IntegrityBaseline pairs two co-located stations whose antennas are a known distance
// apart, for the baseline check. A station belongs to at most one pair.
type IntegrityBaseline struct {
	Stations []string `toml:"stations"`
	// DistanceM is the antenna separation in metres. It defaults to the distance
	// between the two surveyed positions when both stations have one, and must
	// agree with it within baselineSurveyTolerance when both are given.
	DistanceM float64 `toml:"distance_m"`
}

// baselineSurveyTolerance is how far a configured baseline may differ from the
// distance between the two surveyed positions.
const baselineSurveyTolerance = 1.0

// IntegrityStation is one observer's installation.
type IntegrityStation struct {
	Observer string `toml:"observer"`
	// Mode is "fixed" or "mobile".
	Mode string `toml:"mode"`
	// Position is a fixed antenna's surveyed latitude and longitude in degrees and
	// ellipsoid height in metres. It defaults to the observer's
	// [[reception.station]] position; set it in one place only.
	Position []float64 `toml:"position"`
	// MaxSpeedMPS is a mobile station's maximum plausible speed, required.
	MaxSpeedMPS float64 `toml:"max_speed_mps"`
}

// finalizeIntegrity validates the section and resolves every installation. A
// [[reception.station]] is a fixed site by definition, so one without an
// [[integrity.station]] entry is a fixed installation at its surveyed position.
func (c *Config) finalizeIntegrity() error {
	reception := make(map[string][]float64, len(c.Reception.Stations))
	for _, site := range c.Reception.Stations {
		reception[site.Observer] = site.Position
	}
	out := make(map[string]integrity.StationProfile)
	for _, s := range c.Integrity.Stations {
		if !ValidObserverID(s.Observer) {
			return fmt.Errorf("integrity: invalid observer %q", s.Observer)
		}
		if _, dup := out[s.Observer]; dup {
			return fmt.Errorf("integrity: duplicate observer %q", s.Observer)
		}
		sp := integrity.StationProfile{Mode: integrity.Mode(s.Mode)}
		site, hasSite := reception[s.Observer]
		switch sp.Mode {
		case integrity.ModeFixed:
			if s.MaxSpeedMPS != 0 {
				return fmt.Errorf("integrity %s: max_speed_mps applies only to a mobile station", s.Observer)
			}
			pos := s.Position
			switch {
			case len(pos) == 0 && hasSite:
				pos = site
			case len(pos) != 0 && hasSite && !slicesEqualFloat(pos, site):
				return fmt.Errorf("integrity %s: position differs from the [[reception.station]] position; set the surveyed position in one place", s.Observer)
			}
			if len(pos) != 0 {
				surveyed, err := surveyedPosition(s.Observer, pos)
				if err != nil {
					return err
				}
				sp.Position = surveyed
			}
		case integrity.ModeMobile:
			if hasSite {
				return fmt.Errorf("integrity %s: a mobile station cannot have a [[reception.station]], which is a fixed site", s.Observer)
			}
			if len(s.Position) != 0 {
				return fmt.Errorf("integrity %s: a mobile station has no surveyed position", s.Observer)
			}
			if math.IsNaN(s.MaxSpeedMPS) || math.IsInf(s.MaxSpeedMPS, 0) || s.MaxSpeedMPS <= 0 || s.MaxSpeedMPS > 1000 {
				return fmt.Errorf("integrity %s: a mobile station needs max_speed_mps in (0, 1000]", s.Observer)
			}
			sp.MaxSpeedMPS = s.MaxSpeedMPS
		default:
			return fmt.Errorf("integrity %s: mode must be \"fixed\" or \"mobile\", got %q", s.Observer, s.Mode)
		}
		if err := sp.Validate(); err != nil {
			return fmt.Errorf("integrity %s: %w", s.Observer, err)
		}
		out[s.Observer] = sp
	}
	for observer, pos := range reception {
		if _, configured := out[observer]; configured {
			continue
		}
		surveyed, err := surveyedPosition(observer, pos)
		if err != nil {
			return err
		}
		out[observer] = integrity.StationProfile{Mode: integrity.ModeFixed, Position: surveyed}
	}
	if err := resolveBaselines(c.Integrity.Baselines, out); err != nil {
		return err
	}
	c.integrityStations = out
	return nil
}

// resolveBaselines adds each pair to both stations' installations.
func resolveBaselines(pairs []IntegrityBaseline, out map[string]integrity.StationProfile) error {
	for _, b := range pairs {
		if len(b.Stations) != 2 || b.Stations[0] == b.Stations[1] {
			return fmt.Errorf("integrity baseline: stations must name two different observers, got %q", b.Stations)
		}
		one, other := b.Stations[0], b.Stations[1]
		for _, id := range b.Stations {
			if !ValidObserverID(id) {
				return fmt.Errorf("integrity baseline: invalid observer %q", id)
			}
			if out[id].Baseline != nil {
				return fmt.Errorf("integrity baseline: %s is already in a pair", id)
			}
		}
		if b.DistanceM < 0 || math.IsNaN(b.DistanceM) || math.IsInf(b.DistanceM, 0) {
			return fmt.Errorf("integrity baseline %s/%s: distance_m must be a finite positive number", one, other)
		}
		distance := b.DistanceM
		a, c := out[one].Position, out[other].Position
		switch {
		case a != nil && c != nil:
			surveyed := integrity.SurveyedDistanceM(*a, *c)
			if distance == 0 {
				distance = surveyed
			} else if math.Abs(distance-surveyed) > baselineSurveyTolerance {
				return fmt.Errorf("integrity baseline %s/%s: distance_m %g differs from the surveyed positions' %.2f m", one, other, distance, surveyed)
			}
		case distance == 0:
			return fmt.Errorf("integrity baseline %s/%s: distance_m is required unless both stations have surveyed positions", one, other)
		}
		for _, side := range [][2]string{{one, other}, {other, one}} {
			sp := out[side[0]]
			sp.Baseline = &integrity.Baseline{Partner: side[1], DistanceM: distance}
			if err := sp.Validate(); err != nil {
				return fmt.Errorf("integrity baseline %s/%s: %w", one, other, err)
			}
			out[side[0]] = sp
		}
	}
	return nil
}

func surveyedPosition(observer string, pos []float64) (*integrity.Surveyed, error) {
	if len(pos) != 3 {
		return nil, fmt.Errorf("integrity %s: position requires latitude, longitude and height", observer)
	}
	s := &integrity.Surveyed{LatDeg: pos[0], LonDeg: pos[1], HeightM: pos[2]}
	if err := (integrity.StationProfile{Mode: integrity.ModeFixed, Position: s}).Validate(); err != nil {
		return nil, fmt.Errorf("integrity %s: %w", observer, err)
	}
	return s, nil
}

func slicesEqualFloat(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// IntegrityStations returns the resolved installation profiles by observer id. The
// map is a copy; Load must have succeeded.
func (c *Config) IntegrityStations() map[string]integrity.StationProfile {
	out := make(map[string]integrity.StationProfile, len(c.integrityStations))
	for id, sp := range c.integrityStations {
		if sp.Position != nil {
			pos := *sp.Position
			sp.Position = &pos
		}
		if sp.Baseline != nil {
			pair := *sp.Baseline
			sp.Baseline = &pair
		}
		out[id] = sp
	}
	return out
}
