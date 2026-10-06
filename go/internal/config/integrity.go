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
	Stations []IntegrityStation `toml:"station"`
}

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
	c.integrityStations = out
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
		out[id] = sp
	}
	return out
}
