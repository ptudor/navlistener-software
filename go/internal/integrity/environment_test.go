package integrity

import (
	"math"
	"slices"
	"testing"
	"time"
)

func number(v float64) *float64 { return &v }

func environmentAt(at time.Time, uptime uint64, temperature, humidity, pressure float64) EnvironmentSample {
	return EnvironmentSample{Received: at, UptimeMS: uptime, Readings: []EnvironmentalReading{
		{Name: "hdc", Location: SensorEnclosure, ExpectedTemperature: true, ExpectedHumidity: true, Ready: true,
			TemperatureC: number(temperature), HumidityPercent: number(humidity)},
		{Name: "bmp581", Location: SensorEnclosure, ExpectedTemperature: true, ExpectedPressure: true, Ready: true,
			TemperatureC: number(temperature + 0.2), PressurePa: number(pressure)},
	}}
}

func testEnvironmentProfile() EnvironmentProfile {
	p := DefaultProfile().Environment
	p.BaselineWindow = time.Hour
	p.BaselineMinAge = 2 * time.Minute
	p.BaselineMinSamples = 3
	p.RateMinInterval = 30 * time.Second
	p.RateMaxInterval = 10 * time.Minute
	p.FlatlineAfter = 3 * time.Minute
	p.FlatlineMinSamples = 4
	return p
}

func TestEnvironmentMagnitudeRateAndBaselineFreeze(t *testing.T) {
	var checks environmentChecks
	p := testEnvironmentProfile()
	for i := 0; i < 4; i++ {
		health, change := checks.evaluate(environmentAt(t0.Add(time.Duration(i)*time.Minute), uint64(i)*60_000, 22+float64(i)*0.05, 20+float64(i)*0.1, 101325+float64(i)), p)
		if health.State != Assured {
			t.Fatalf("healthy sample %d: %+v", i, health)
		}
		if i < 3 && change.State != Unavailable {
			t.Fatalf("warm-up sample %d: %+v", i, change)
		}
	}
	before := len(checks.history)
	health, change := checks.evaluate(environmentAt(t0.Add(4*time.Minute), 240_000, 35, 95, 85000), p)
	if health.State != Assured {
		t.Fatalf("changed but valid sensors: %+v", health)
	}
	if change.State != Unassured || !slices.Contains(change.Reasons, ReasonHumidityChange) ||
		!slices.Contains(change.Reasons, ReasonPressureChange) || !slices.Contains(change.Reasons, ReasonHumidityRate) {
		t.Fatalf("large environmental change = %+v", change)
	}
	if len(checks.history) != before {
		t.Fatalf("anomaly trained baseline: history %d -> %d", before, len(checks.history))
	}
	if got := change.Metrics["humidity_delta_percent"]; got < 74 || got > 76 {
		t.Errorf("humidity delta = %g", got)
	}
	if got := change.Metrics["pressure_delta_pa"]; got < 16000 {
		t.Errorf("pressure delta = %g", got)
	}
}

func TestEnvironmentGradualChangeAccumulatesAgainstMedian(t *testing.T) {
	var checks environmentChecks
	p := testEnvironmentProfile()
	p.HumidityRatePctPerHour = EnvironmentalBands{Inconsistent: 1000, Unassured: 2000}
	for i, humidity := range []float64{20, 20.1, 20.2, 20.3, 26, 32, 38, 45} {
		_, change := checks.evaluate(environmentAt(t0.Add(time.Duration(i)*time.Minute), uint64(i)*60_000, 22, humidity, 101325), p)
		if i == 7 && change.State != Inconsistent {
			t.Fatalf("gradual accumulated change = %+v, want inconsistent", change)
		}
	}
}

func TestEnvironmentSlowChangeUsesPersistentReference(t *testing.T) {
	var checks environmentChecks
	p := testEnvironmentProfile()
	p.BaselineWindow = 2 * time.Hour
	p.PersistenceWindow = 24 * time.Hour
	p.PersistenceMinAge = 12 * time.Hour
	p.HumidityRatePctPerHour = EnvironmentalBands{Inconsistent: 1000, Unassured: 2000}
	var change Verdict
	for hour := 0; hour <= 24; hour++ {
		humidity := 20 + 75*float64(hour)/24
		_, change = checks.evaluate(environmentAt(t0.Add(time.Duration(hour)*time.Hour), uint64(hour)*3_600_000, 22, humidity, 101325), p)
	}
	if change.State != Unassured || !slices.Contains(change.Reasons, ReasonHumiditySustained) {
		t.Fatalf("slow persistent humidity rise = %+v", change)
	}
	if got := change.Metrics["humidity_sustained_delta_percent"]; got < 40 {
		t.Fatalf("sustained humidity delta = %g", got)
	}
	t.Logf("environment_change=%s reason=%s sustained_delta=%.1f",
		change.State, ReasonHumiditySustained, change.Metrics["humidity_sustained_delta_percent"])
}

func TestEnvironmentHealthFailureModes(t *testing.T) {
	var checks environmentChecks
	p := testEnvironmentProfile()
	sample := EnvironmentSample{Received: t0, Readings: []EnvironmentalReading{
		{Name: "hdc", Location: SensorEnclosure, ExpectedTemperature: true, ExpectedHumidity: true, Ready: true,
			TemperatureC: number(20), HumidityPercent: number(100)},
		{Name: "sht3x", Location: SensorEnclosure, ExpectedTemperature: true, ExpectedHumidity: true, Ready: true,
			TemperatureC: number(31), HumidityPercent: number(70)},
		{Name: "ms5607", Location: SensorEnclosure, ExpectedTemperature: true, ExpectedPressure: true,
			Fault: true, ExtendedPressure: true},
	}}
	health, _ := checks.evaluate(sample, p)
	if health.State != Unassured {
		t.Fatalf("faulted sensors = %+v", health)
	}
	for _, reason := range []string{ReasonSensorFault, ReasonSensorSaturated, ReasonSensorDisagreement, ReasonPressureExtended} {
		if !slices.Contains(health.Reasons, reason) {
			t.Errorf("health reasons %v missing %s", health.Reasons, reason)
		}
	}
}

func TestEnvironmentFlatlineAndReset(t *testing.T) {
	var checks environmentChecks
	p := testEnvironmentProfile()
	for i := 0; i < 4; i++ {
		health, _ := checks.evaluate(environmentAt(t0.Add(time.Duration(i)*time.Minute), uint64(i)*60_000, 22, 50, 101325), p)
		if i == 3 && (health.State != Unassured || !slices.Contains(health.Reasons, ReasonSensorFlatline)) {
			t.Fatalf("flatline = %+v", health)
		}
	}
	// A lower uptime is a reboot. It clears the flatline and environmental baseline
	// instead of carrying a pre-boot fault or reference into the new session.
	health, change := checks.evaluate(environmentAt(t0.Add(5*time.Minute), 1000, 22, 50, 101325), p)
	if health.State != Assured || change.State != Unavailable || !slices.Contains(change.Reasons, ReasonEnvironmentWarmup) {
		t.Fatalf("post-restart = health %+v change %+v", health, change)
	}
	t.Logf("post_restart_health=%s change=%s reason=%s", health.State, change.State, ReasonEnvironmentWarmup)
}

func TestHumidityHeaterWithholdsHDCWithoutSensorFault(t *testing.T) {
	var checks environmentChecks
	p := testEnvironmentProfile()
	sample := EnvironmentSample{Received: t0, HeaterActive: true, Readings: []EnvironmentalReading{
		{Name: "hdc", Location: SensorEnclosure, ExpectedTemperature: true, ExpectedHumidity: true, Ready: true},
		{Name: "mcp9808", Location: SensorEnclosure, ExpectedTemperature: true, Ready: true, TemperatureC: number(23)},
	}}
	health, change := checks.evaluate(sample, p)
	if health.State != Assured || !slices.Contains(health.Reasons, ReasonHumidityHeater) {
		t.Fatalf("heater health = %+v", health)
	}
	if change.State != Unavailable || !slices.Contains(change.Reasons, ReasonEnvironmentWarmup) {
		t.Fatalf("heater change = %+v", change)
	}
}

func TestLowDewPointSpreadDoesNotWaitForBaseline(t *testing.T) {
	var checks environmentChecks
	p := testEnvironmentProfile()
	_, change := checks.evaluate(environmentAt(t0, 1000, 22, 99.9, 101325), p)
	if change.State != Unassured || !slices.Contains(change.Reasons, ReasonDewPointSpread) ||
		!slices.Contains(change.Reasons, ReasonEnvironmentWarmup) {
		t.Fatalf("near-saturation startup = %+v", change)
	}
	if len(checks.history) != 0 {
		t.Fatalf("near-saturation sample trained baseline: %d", len(checks.history))
	}
}

func TestEnvironmentAdvisoryDoesNotChangeFusion(t *testing.T) {
	checks := []Result{
		{Check: CheckStaticPosition, Domain: DomainPosition, State: Assured},
		{Check: CheckEnvironmentHealth, Domain: DomainEnvironment, State: Unassured, advisory: true},
		{Check: CheckEnvironmentChange, Domain: DomainEnvironment, State: Unassured, advisory: true},
	}
	f := Fuse(checks, nil)
	if f.State != Assured || f.SpoofingIndicated || len(f.UnassuredDomains) != 0 {
		t.Fatalf("advisory environment altered fusion: %+v", f)
	}
}

func TestEnvironmentStaleRemainsUnavailable(t *testing.T) {
	p := DefaultProfile()
	p.Filter = FilterPolicy{M: 1, N: 1, RecoveryHold: time.Minute}
	p.Environment.BaselineMinSamples = 2
	p.Environment.BaselineMinAge = time.Minute
	s, err := NewStation(p, fixedProfile())
	if err != nil {
		t.Fatal(err)
	}
	s.ApplyEnvironment(environmentAt(t0, 1000, 22, 40, 101325))
	a := s.Assess(t0.Add(p.Environment.StaleAfter + time.Second))
	health := checkResult(t, a, CheckEnvironmentHealth)
	if health.State != Unavailable || !slices.Contains(health.Reasons, ReasonStale) {
		t.Errorf("environment health stale = %+v", health)
	}
	// The change check was already unavailable during baseline warm-up; it must not
	// become assured merely because input stopped. The health check carries the
	// explicit stale transition.
	if change := checkResult(t, a, CheckEnvironmentChange); change.State != Unavailable {
		t.Errorf("environment change after input stopped = %+v", change)
	}
}

func TestDewPoint(t *testing.T) {
	if got := dewPointC(30, 70); math.Abs(got-23.93) > 0.1 {
		t.Fatalf("dew point = %.3f C", got)
	}
}
