package reception

import (
	"encoding/hex"
	"fmt"
	"math"
	"strconv"
	"strings"
)

type Config struct {
	OperatorTokenSHA256 string `toml:"operator_token_sha256"`
	Stations            []Site `toml:"station"`
}
type Site struct {
	Observer       string    `toml:"observer"`
	Position       []float64 `toml:"position"` // latitude degrees, longitude degrees, ellipsoid height metres
	Signals        []string  `toml:"signals"`  // enabled canonical gnss:signal groups
	PerSignal      bool      `toml:"per_signal"`
	Elevation      float64   `toml:"elevation_mask_degrees"`
	RadiusM        uint16    `toml:"radius_m"`
	AlarmSeconds   uint16    `toml:"alarm_seconds"`
	ClearSeconds   uint16    `toml:"clear_seconds"`
	MinExpected    uint8     `toml:"min_expected"`
	MinMissing     uint8     `toml:"min_missing"`
	MissingPercent uint8     `toml:"missing_percent"`
}

func CanonicalSignal(g, s uint8) uint8 {
	switch g {
	case 0:
		if s == 4 {
			return 3
		}
		if s == 7 {
			return 6
		}
	case 2:
		if s == 1 {
			return 0
		}
		if s == 4 {
			return 3
		}
		if s == 6 {
			return 5
		}
	case 3:
		if s == 1 {
			return 0
		}
		if s == 3 {
			return 2
		}
		if s == 6 {
			return 5
		}
		if s == 7 {
			return 8
		}
	case 5:
		if s == 5 {
			return 4
		}
		if s == 9 {
			return 8
		}
	}
	return s
}
func (s Site) Allows(g, sig uint8) bool {
	key := fmt.Sprintf("%d:%d", g, CanonicalSignal(g, sig))
	for _, v := range s.Signals {
		if v == key {
			return true
		}
	}
	return false
}
func (c *Config) Validate() error {
	if c.OperatorTokenSHA256 != "" {
		b, err := hex.DecodeString(c.OperatorTokenSHA256)
		if err != nil || len(b) != 32 {
			return fmt.Errorf("reception: operator_token_sha256 must contain 32 bytes of hex")
		}
	}
	seen := map[string]bool{}
	for i := range c.Stations {
		s := &c.Stations[i]
		if s.Observer == "" || seen[s.Observer] {
			return fmt.Errorf("reception: duplicate or empty observer")
		}
		seen[s.Observer] = true
		if len(s.Position) != 3 {
			return fmt.Errorf("reception %s: position requires latitude, longitude and height", s.Observer)
		}
		for _, v := range s.Position {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("reception %s: nonfinite position", s.Observer)
			}
		}
		if math.Abs(s.Position[0]) > 90 || math.Abs(s.Position[1]) > 180 || s.Position[2] < -1000 || s.Position[2] > 20000 {
			return fmt.Errorf("reception %s: invalid fixed site position", s.Observer)
		}
		if s.Elevation == 0 {
			s.Elevation = 20
		}
		if math.IsNaN(s.Elevation) || s.Elevation < 5 || s.Elevation > 85 {
			return fmt.Errorf("reception %s: elevation mask must be 5..85 degrees", s.Observer)
		}
		if s.RadiusM == 0 {
			s.RadiusM = 1000
		}
		if s.AlarmSeconds == 0 {
			s.AlarmSeconds = 30
		}
		if s.ClearSeconds == 0 {
			s.ClearSeconds = 30
		}
		if s.MinExpected == 0 {
			s.MinExpected = 4
		}
		if s.MinMissing == 0 {
			s.MinMissing = 3
		}
		if s.MissingPercent == 0 {
			s.MissingPercent = 50
		}
		if s.AlarmSeconds < 5 || s.AlarmSeconds > 120 || s.ClearSeconds < 5 || s.ClearSeconds > 120 || s.MissingPercent > 100 {
			return fmt.Errorf("reception %s: invalid alarm policy", s.Observer)
		}
		if len(s.Signals) == 0 {
			return fmt.Errorf("reception %s: explicitly list enabled signals", s.Observer)
		}
		keys := map[string]bool{}
		for _, k := range s.Signals {
			p := strings.Split(k, ":")
			if len(p) != 2 {
				return fmt.Errorf("reception %s: invalid signal %s", s.Observer, k)
			}
			g, e1 := strconv.Atoi(p[0])
			sig, e2 := strconv.Atoi(p[1])
			if e1 != nil || e2 != nil || g < 0 || g > 7 || g == 4 || sig < 0 || sig > 31 || CanonicalSignal(uint8(g), uint8(sig)) != uint8(sig) || fmt.Sprintf("%d:%d", g, sig) != k || keys[k] {
				return fmt.Errorf("reception %s: invalid or duplicate canonical signal %s", s.Observer, k)
			}
			keys[k] = true
		}
	}
	return nil
}
