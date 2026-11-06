package state

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
)

func envFloat(v float64) *float64  { return &v }
func envPressure(v uint32) *uint32 { return &v }

func environmentFrame(seq uint64, at time.Time, humidity float64, pressure uint32) *ingest.RawFrame {
	return &ingest.RawFrame{Source: "board-0001-aa", Session: "boot-a", HasSeq: true, Seq: seq,
		Recv: at, RecvLocal: at, BoardSampleStamped: true,
		Details: &ingest.ObserverDetails{
			UptimeMS: uint64(at.Sub(integrityT0) / time.Millisecond), HumiditySensor: "hdc2080", PressureSensor: "bmp581",
			Environment: &ingest.BoardEnvironment{ReadyMask: 7, MCP9808C: envFloat(22), HDC2080C: envFloat(22.1),
				BMP3C: envFloat(22.2), HumidityPercent: envFloat(humidity), PressurePa: envPressure(pressure)},
		}}
}

func qualifiedBarometricSite(lat float64) integrity.StationProfile {
	return integrity.StationProfile{
		Mode: integrity.ModeFixed, Position: &integrity.Surveyed{LatDeg: lat, LonDeg: -122.0841, HeightM: 12.5},
		Barometer: &integrity.BarometerPolicy{Sensor: "bmp581", Vented: true, RelativeUncertaintyPa: 10},
	}
}

func applyBarometricEnvironment(s *Store, source string, seq uint64, at time.Time, pressure float64) {
	f := environmentFrame(seq, at, 40, uint32(math.Round(pressure)))
	f.Source = source
	s.Apply(f)
}

func applyBarometricSolution(s *Store, source string, seconds int, heightM float64) {
	f := solutionFrame(seconds, true, 1)
	f.Source = source
	f.Solution.PVT.HeightMM = int32(math.Round(heightM * 1000))
	s.Apply(f)
}

func TestBoardEnvironmentReachesIntegrityAdvisories(t *testing.T) {
	p := integrity.DefaultProfile()
	p.Filter = integrity.FilterPolicy{M: 1, N: 1, RecoveryHold: time.Minute}
	p.Environment.BaselineMinSamples = 3
	p.Environment.BaselineMinAge = 2 * time.Minute
	cfg, err := NewIntegrityConfig(p, map[string]integrity.StationProfile{"board-0001-aa": fixedSite()})
	if err != nil {
		t.Fatal(err)
	}
	s := New(1)
	s.SetIntegrity(cfg)
	for i := 0; i < 4; i++ {
		s.Apply(environmentFrame(uint64(i+1), integrityT0.Add(time.Duration(i)*time.Minute), 20+float64(i)/10, 101325+uint32(i)))
	}
	before := s.FeedStationIntegrity(integrityT0.Add(3 * time.Minute))["board-0001-aa"]
	if health := integrityResult(t, before, integrity.CheckEnvironmentHealth); health.State != integrity.Assured {
		t.Fatalf("environment health = %+v", health)
	}
	if change := integrityResult(t, before, integrity.CheckEnvironmentChange); change.State != integrity.Assured {
		t.Fatalf("stable environment = %+v", change)
	}

	s.Apply(environmentFrame(5, integrityT0.Add(4*time.Minute), 95, 85000))
	a := s.FeedStationIntegrity(integrityT0.Add(4 * time.Minute))["board-0001-aa"]
	change := integrityResult(t, a, integrity.CheckEnvironmentChange)
	if change.State != integrity.Unassured || !slices.Contains(change.Reasons, integrity.ReasonHumidityChange) ||
		!slices.Contains(change.Reasons, integrity.ReasonPressureChange) {
		t.Fatalf("environmental anomaly = %+v", change)
	}
	if a.State != integrity.Unavailable || a.SpoofingIndicated {
		t.Fatalf("advisory environment altered GNSS fusion without GNSS inputs: %+v", a.Fusion)
	}
	t.Logf("environment_change=%s reasons=%v humidity_delta=%.1f pressure_delta=%.0f station=%s spoofing=%t",
		change.State, change.Reasons, change.Metrics["humidity_delta_percent"], change.Metrics["pressure_delta_pa"], a.State, a.SpoofingIndicated)
}

func TestBoardEnvironmentFaultAndHeaterMapping(t *testing.T) {
	p := integrity.DefaultProfile()
	p.Filter = integrity.FilterPolicy{M: 1, N: 1, RecoveryHold: time.Minute}
	cfg, err := NewIntegrityConfig(p, map[string]integrity.StationProfile{"board-0001-aa": fixedSite()})
	if err != nil {
		t.Fatal(err)
	}
	s := New(1)
	s.SetIntegrity(cfg)
	at := integrityT0
	s.Apply(&ingest.RawFrame{Source: "board-0001-aa", Session: "boot-a", HasSeq: true, Seq: 1, Recv: at, RecvLocal: at,
		Details: &ingest.ObserverDetails{UptimeMS: 1000, Barometer: &ingest.BoardBarometer{State: "calibration_rejected"}}})
	a := s.FeedStationIntegrity(at)["board-0001-aa"]
	if health := integrityResult(t, a, integrity.CheckEnvironmentHealth); health.State != integrity.Unassured ||
		!slices.Contains(health.Reasons, integrity.ReasonSensorFault) {
		t.Fatalf("calibration failure = %+v", health)
	}

	heater := &ingest.BoardHumidityHeater{State: "heating"}
	s.Apply(&ingest.RawFrame{Source: "board-0001-aa", Session: "boot-a", HasSeq: true, Seq: 2, Recv: at.Add(time.Minute), RecvLocal: at.Add(time.Minute),
		Details: &ingest.ObserverDetails{UptimeMS: 61_000, HumiditySensor: "hdc2080", Heater: heater,
			Environment: &ingest.BoardEnvironment{ReadyMask: 3, MCP9808C: envFloat(23)}}})
	a = s.FeedStationIntegrity(at.Add(time.Minute))["board-0001-aa"]
	health := integrityResult(t, a, integrity.CheckEnvironmentHealth)
	if health.State != integrity.Unassured {
		// Recovery hold retains the preceding fault, but the latest evidence must not
		// call the deliberately withheld HDC values invalid.
		t.Fatalf("held health state = %+v", health)
	}
	if slices.Contains(health.Reasons, integrity.ReasonSensorInvalid) {
		t.Fatalf("heater-created null treated as sensor failure: %+v", health)
	}
}

func TestRegionalPressureCorroboratesGNSSHeightChange(t *testing.T) {
	p := integrity.DefaultProfile()
	p.Filter = integrity.FilterPolicy{M: 1, N: 1, RecoveryHold: time.Minute}
	stations := map[string]integrity.StationProfile{
		"target":      qualifiedBarometricSite(37.4219),
		"neighbour-a": qualifiedBarometricSite(37.47),
		"neighbour-b": qualifiedBarometricSite(37.52),
	}
	cfg, err := NewIntegrityConfig(p, stations)
	if err != nil {
		t.Fatal(err)
	}
	s := New(1)
	s.SetIntegrity(cfg)
	bases := map[string]float64{"target": 101325, "neighbour-a": 92000, "neighbour-b": 84000}
	for id, pressure := range bases {
		if id == "target" {
			applyBarometricSolution(s, id, 0, 12.5)
		}
		applyBarometricEnvironment(s, id, 1, integrityT0, pressure)
	}
	end := integrityT0.Add(p.Barometric.Window)
	seconds := int(p.Barometric.Window / time.Second)
	applyBarometricSolution(s, "target", seconds, 112.5)
	for _, id := range []string{"neighbour-a", "neighbour-b", "target"} {
		applyBarometricEnvironment(s, id, 2, end, bases[id])
	}
	a := s.FeedStationIntegrity(end)["target"]
	if regional := integrityResult(t, a, integrity.CheckRegionalPressure); regional.State != integrity.Assured {
		t.Fatalf("regional pressure = %+v", regional)
	}
	barometric := integrityResult(t, a, integrity.CheckBarometricHeight)
	if barometric.State != integrity.Unassured || !slices.Contains(barometric.Reasons, integrity.ReasonBarometricHeightDivergence) {
		t.Fatalf("barometric height = %+v", barometric)
	}
	if !a.SpoofingIndicated || !slices.Contains(a.UnassuredDomains, integrity.DomainPosition) ||
		!slices.Contains(a.UnassuredDomains, integrity.DomainBarometricPosition) {
		t.Fatalf("pressure did not independently corroborate position: %+v", a.Fusion)
	}
	t.Logf("regional=assured barometric=%s divergence=%.1fm domains=%v spoofing=%t",
		barometric.State, barometric.Metrics["barometric_divergence_m"], a.UnassuredDomains, a.SpoofingIndicated)
}

func TestRegionalWeatherAndLocalPressureAreNotSpoofing(t *testing.T) {
	tests := []struct {
		name            string
		targetFactor    float64
		neighbourFactor float64
		wantRegional    integrity.State
	}{
		{name: "coherent weather across elevations", targetFactor: 0.98, neighbourFactor: 0.98, wantRegional: integrity.Assured},
		{name: "target-only pressure change", targetFactor: 0.98, neighbourFactor: 1, wantRegional: integrity.Unassured},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := integrity.DefaultProfile()
			p.Filter = integrity.FilterPolicy{M: 1, N: 1, RecoveryHold: time.Minute}
			stations := map[string]integrity.StationProfile{
				"target": qualifiedBarometricSite(37.4219), "a": qualifiedBarometricSite(37.47), "b": qualifiedBarometricSite(37.52),
			}
			cfg, err := NewIntegrityConfig(p, stations)
			if err != nil {
				t.Fatal(err)
			}
			s := New(1)
			s.SetIntegrity(cfg)
			bases := map[string]float64{"target": 101325, "a": 92000, "b": 84000}
			applyBarometricSolution(s, "target", 0, 12.5)
			for id, pressure := range bases {
				applyBarometricEnvironment(s, id, 1, integrityT0, pressure)
			}
			end := integrityT0.Add(p.Barometric.Window)
			seconds := int(p.Barometric.Window / time.Second)
			applyBarometricSolution(s, "target", seconds, 12.5)
			applyBarometricEnvironment(s, "a", 2, end, bases["a"]*tc.neighbourFactor)
			applyBarometricEnvironment(s, "b", 2, end, bases["b"]*tc.neighbourFactor)
			applyBarometricEnvironment(s, "target", 2, end, bases["target"]*tc.targetFactor)
			a := s.FeedStationIntegrity(end)["target"]
			if regional := integrityResult(t, a, integrity.CheckRegionalPressure); regional.State != tc.wantRegional {
				t.Fatalf("regional pressure = %+v, want %s", regional, tc.wantRegional)
			}
			if barometric := integrityResult(t, a, integrity.CheckBarometricHeight); barometric.State != integrity.Assured {
				t.Fatalf("stable GNSS height treated as pressure evidence: %+v", barometric)
			}
			if a.SpoofingIndicated || slices.Contains(a.UnassuredDomains, integrity.DomainBarometricPosition) {
				t.Fatalf("weather/local pressure raised spoofing: %+v", a.Fusion)
			}
			t.Logf("regional=%s barometric=assured residual=%.2fm spoofing=%t",
				tc.wantRegional, integrityResult(t, a, integrity.CheckRegionalPressure).Metrics["regional_pressure_residual_m"], a.SpoofingIndicated)
		})
	}
}

func TestBarometricContextMissingNeighboursRemainsUnavailable(t *testing.T) {
	p := integrity.DefaultProfile()
	p.Filter = integrity.FilterPolicy{M: 1, N: 1, RecoveryHold: time.Minute}
	cfg, err := NewIntegrityConfig(p, map[string]integrity.StationProfile{"target": qualifiedBarometricSite(37.4219)})
	if err != nil {
		t.Fatal(err)
	}
	s := New(1)
	s.SetIntegrity(cfg)
	applyBarometricSolution(s, "target", 0, 12.5)
	applyBarometricEnvironment(s, "target", 1, integrityT0, 101325)
	end := integrityT0.Add(p.Barometric.Window)
	applyBarometricSolution(s, "target", int(p.Barometric.Window/time.Second), 112.5)
	applyBarometricEnvironment(s, "target", 2, end, 101325)
	a := s.FeedStationIntegrity(end)["target"]
	for _, name := range []string{integrity.CheckRegionalPressure, integrity.CheckBarometricHeight} {
		result := integrityResult(t, a, name)
		if result.State != integrity.Unavailable || !slices.Contains(result.Reasons, integrity.ReasonInsufficientPressureNeighbours) {
			t.Fatalf("%s without neighbours = %+v", name, result)
		}
	}
	if a.SpoofingIndicated {
		t.Fatalf("missing weather context raised spoofing: %+v", a.Fusion)
	}
	t.Logf("regional=unavailable barometric=unavailable reason=%s spoofing=%t",
		integrity.ReasonInsufficientPressureNeighbours, a.SpoofingIndicated)
}
