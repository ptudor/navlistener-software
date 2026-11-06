package state

import (
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
