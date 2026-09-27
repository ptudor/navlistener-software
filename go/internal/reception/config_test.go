package reception

import (
	"math"
	"testing"
)

func TestReceptionProfiles(t *testing.T) {
	base := func() Config {
		return Config{Stations: []Site{{Observer: "edge", Position: []float64{35, 140, 50}, Signals: []string{"0:0", "2:0", "3:0", "6:0"}}}}
	}
	c := base()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if s := c.Stations[0]; s.AlarmSeconds != 30 || s.ClearSeconds != 30 || s.Elevation != 20 || !s.Allows(2, 1) || s.Allows(0, 6) {
		t.Fatalf("defaults/capabilities: %+v", s)
	}
	for _, mutate := range []func(*Config){
		func(c *Config) { c.Stations[0].Position = nil }, func(c *Config) { c.Stations[0].Position[0] = 91 },
		func(c *Config) { c.Stations[0].Position[2] = math.NaN() }, func(c *Config) { c.Stations[0].Elevation = 90 },
		func(c *Config) { c.Stations[0].Signals = nil }, func(c *Config) { c.Stations[0].Signals = []string{"2:1"} },
		func(c *Config) { c.Stations[0].Signals = []string{"0:0", "0:0"} }, func(c *Config) { c.Stations[0].AlarmSeconds = 1 },
		func(c *Config) { c.Stations[0].MissingPercent = 101 }, func(c *Config) { c.Stations = append(c.Stations, c.Stations[0]) },
		func(c *Config) { c.OperatorTokenSHA256 = "plaintext" },
	} {
		c := base()
		mutate(&c)
		if c.Validate() == nil {
			t.Fatalf("invalid profile accepted: %+v", c)
		}
	}
}
