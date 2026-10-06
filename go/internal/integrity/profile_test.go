package integrity

import (
	"math"
	"strings"
	"testing"
	"time"
)

func TestDefaultProfileValid(t *testing.T) {
	if err := DefaultProfile().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestProfileValidateRejects(t *testing.T) {
	cases := map[string]func(*Profile){
		"m above n":           func(p *Profile) { p.Filter.M = 5 },
		"zero n":              func(p *Profile) { p.Filter.N, p.Filter.M = 0, 0 },
		"negative hold":       func(p *Profile) { p.Filter.RecoveryHold = -time.Second },
		"zero stale bound":    func(p *Profile) { p.StaleAfter = 0 },
		"negative weight":     func(p *Profile) { p.Weights = map[string]float64{"a": -1} },
		"not-a-number weight": func(p *Profile) { p.Weights = map[string]float64{"a": math.NaN()} },
	}
	for name, mutate := range cases {
		p := DefaultProfile()
		mutate(&p)
		if p.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestStationProfileValidate(t *testing.T) {
	good := []StationProfile{
		{},
		{Mode: ModeFixed},
		{Mode: ModeFixed, Position: &Surveyed{LatDeg: 37.4, LonDeg: -122.1, HeightM: 12}},
		{Mode: ModeMobile, MaxSpeedMPS: 30},
		{Mode: ModeMobile},
	}
	for _, s := range good {
		if err := s.Validate(); err != nil {
			t.Errorf("%+v rejected: %v", s, err)
		}
	}
	bad := []StationProfile{
		{Mode: "airborne"},
		{Mode: ModeFixed, Position: &Surveyed{LatDeg: 91}},
		{Mode: ModeFixed, Position: &Surveyed{LonDeg: math.Inf(1)}},
		{Mode: ModeFixed, Position: &Surveyed{HeightM: math.NaN()}},
		{Mode: ModeMobile, Position: &Surveyed{}},
		{Mode: ModeFixed, MaxSpeedMPS: 5},
		{Mode: ModeMobile, MaxSpeedMPS: -1},
	}
	for _, s := range bad {
		if s.Validate() == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

func TestConfigHash(t *testing.T) {
	p := DefaultProfile()
	station := StationProfile{Mode: ModeFixed, Position: &Surveyed{LatDeg: 37.4, LonDeg: -122.1, HeightM: 12}}
	checks := map[string]int{"a": 1, "b": 2}
	base := ConfigHash(p, station, checks)
	if !strings.HasPrefix(base, "sha256:") || len(base) != len("sha256:")+64 {
		t.Fatalf("hash format %q", base)
	}
	if again := ConfigHash(p, station, map[string]int{"b": 2, "a": 1}); again != base {
		t.Fatalf("hash depends on map construction order: %s vs %s", again, base)
	}
	changed := map[string]string{}
	p2 := DefaultProfile()
	p2.Filter.M = 2
	changed["profile threshold"] = ConfigHash(p2, station, checks)
	moved := *station.Position
	moved.HeightM += 0.01
	changed["surveyed position"] = ConfigHash(p, StationProfile{Mode: ModeFixed, Position: &moved}, checks)
	changed["check version"] = ConfigHash(p, station, map[string]int{"a": 1, "b": 3})
	changed["station mode"] = ConfigHash(p, StationProfile{Mode: ModeMobile}, checks)
	for what, h := range changed {
		if h == base {
			t.Errorf("hash unchanged by a different %s", what)
		}
	}
}
