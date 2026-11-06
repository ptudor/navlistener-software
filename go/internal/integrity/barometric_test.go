package integrity

import (
	"math"
	"slices"
	"testing"
	"time"
)

func testBarometricProfile() BarometricProfile {
	return DefaultProfile().Barometric
}

func pressureObservation(at time.Time, pressure float64) PressureObservation {
	return PressureObservation{
		At: at, PressurePa: pressure, TemperatureC: 20, HumidityPercent: 50,
		HasHumidity: true, RelativeUncertaintyPa: 10,
	}
}

func pressureContext(start, end PressureObservation, regionalLogChange, spreadM float64) PressureContext {
	dt := end.At.Sub(start.At).Seconds()
	// Near room temperature, 8.5 km of hypsometric scale is sufficient to
	// construct a requested spread for a test fixture.
	return PressureContext{
		Start: start, End: end, RegionalLogRatePerSecond: regionalLogChange / dt,
		RegionalSpreadLogRatePerSecond:      spreadM / 8500 / dt,
		RegionalUncertaintyLogRatePerSecond: math.Sqrt2 * 10 / start.PressurePa / dt,
		NeighbourCount:                      3, MaxNeighbourAge: time.Minute, MaxNeighbourDistanceM: 40_000,
	}
}

func barometricSolutionAt(at time.Time, height float64) Solution {
	return Solution{Received: at, FixType: Fix3D, FixOK: true, LatDeg: 37, LonDeg: -122,
		HeightM: height, VAccM: 2}
}

func seedBarometricAnchor(b *barometricChecks, at time.Time, pressure float64, height float64, prof BarometricProfile) PressureObservation {
	observation := pressureObservation(at, pressure)
	b.observeSolution(barometricSolutionAt(at, height), prof)
	b.evaluate(PressureSample{Received: at, Observation: &observation, UnavailableReason: ReasonPressureHistoryWarmup}, prof)
	return observation
}

func TestBarometricStablePressureCorroboratesGNSSHeightDivergence(t *testing.T) {
	prof := testBarometricProfile()
	var checks barometricChecks
	start := seedBarometricAnchor(&checks, t0, 101325, 10, prof)
	endAt := t0.Add(prof.Window)
	end := pressureObservation(endAt, 101325)
	checks.observeSolution(barometricSolutionAt(endAt, 110), prof)
	regional, barometric := checks.evaluate(PressureSample{Received: endAt, Observation: &end,
		Context: ptrPressureContext(pressureContext(start, end, 0, 0.2))}, prof)
	if regional.State != Assured {
		t.Fatalf("regional pressure = %+v", regional)
	}
	if barometric.State != Unassured || !slices.Contains(barometric.Reasons, ReasonBarometricHeightDivergence) {
		t.Fatalf("barometric height = %+v", barometric)
	}
	if got := barometric.Metrics["barometric_divergence_m"]; got < 99 || got > 101 {
		t.Fatalf("barometric divergence = %g", got)
	}
}

func TestBarometricCoherentRegionalWeatherDoesNotEscalate(t *testing.T) {
	prof := testBarometricProfile()
	var checks barometricChecks
	start := seedBarometricAnchor(&checks, t0, 101325, 10, prof)
	endAt := t0.Add(prof.Window)
	end := pressureObservation(endAt, 100825)
	regionalLog := math.Log(end.PressurePa / start.PressurePa)
	checks.observeSolution(barometricSolutionAt(endAt, 10), prof)
	regional, barometric := checks.evaluate(PressureSample{Received: endAt, Observation: &end,
		Context: ptrPressureContext(pressureContext(start, end, regionalLog, 0.5))}, prof)
	if regional.State != Assured || barometric.State != Assured {
		t.Fatalf("coherent weather = regional %+v barometric %+v", regional, barometric)
	}
	if residual := regional.Metrics["regional_pressure_residual_m"]; math.Abs(residual) > 0.01 {
		t.Fatalf("weather residual = %g m", residual)
	}
}

func TestBarometricLocalPressureStepIsEnvironmentalNotGNSSEvidence(t *testing.T) {
	prof := testBarometricProfile()
	var checks barometricChecks
	start := seedBarometricAnchor(&checks, t0, 101325, 10, prof)
	endAt := t0.Add(prof.Window)
	end := pressureObservation(endAt, 98000)
	checks.observeSolution(barometricSolutionAt(endAt, 10), prof)
	regional, barometric := checks.evaluate(PressureSample{Received: endAt, Observation: &end,
		Context: ptrPressureContext(pressureContext(start, end, 0, 0.5))}, prof)
	if regional.State != Unassured || !slices.Contains(regional.Reasons, ReasonRegionalPressureDeparture) {
		t.Fatalf("local pressure step = %+v", regional)
	}
	if barometric.State != Assured || !slices.Contains(barometric.Reasons, ReasonNoSignificantHeightChange) {
		t.Fatalf("pressure-only step became GNSS evidence: %+v", barometric)
	}
}

func TestBarometricLocalPressureDepartureMakesMismatchAmbiguous(t *testing.T) {
	prof := testBarometricProfile()
	var checks barometricChecks
	start := seedBarometricAnchor(&checks, t0, 101325, 10, prof)
	endAt := t0.Add(prof.Window)
	end := pressureObservation(endAt, 98000)
	checks.observeSolution(barometricSolutionAt(endAt, 110), prof)
	regional, barometric := checks.evaluate(PressureSample{Received: endAt, Observation: &end,
		Context: ptrPressureContext(pressureContext(start, end, 0, 0.5))}, prof)
	if regional.State != Unassured {
		t.Fatalf("regional departure = %+v", regional)
	}
	if barometric.State != Unavailable || !slices.Contains(barometric.Reasons, ReasonLocalPressureAmbiguous) {
		t.Fatalf("ambiguous local pressure became spoof evidence: %+v", barometric)
	}
}

func TestBarometricPhysicalHeightChangeIsConsistent(t *testing.T) {
	prof := testBarometricProfile()
	var checks barometricChecks
	start := seedBarometricAnchor(&checks, t0, 101325, 10, prof)
	endAt := t0.Add(prof.Window)
	// Invert the same hypsometric scale used by the implementation closely
	// enough to represent a real 100 m rise.
	end := pressureObservation(endAt, start.PressurePa*math.Exp(-100.0/8500))
	checks.observeSolution(barometricSolutionAt(endAt, 110), prof)
	regional, barometric := checks.evaluate(PressureSample{Received: endAt, Observation: &end,
		Context: ptrPressureContext(pressureContext(start, end, 0, 0.5))}, prof)
	if regional.State != Unassured {
		t.Fatalf("physical pressure departure = %+v", regional)
	}
	if barometric.State != Assured {
		t.Fatalf("matching physical height change = %+v", barometric)
	}
}

func TestBarometricRejectsIncoherentOrUnsynchronizedContext(t *testing.T) {
	prof := testBarometricProfile()
	var checks barometricChecks
	start := seedBarometricAnchor(&checks, t0, 101325, 10, prof)
	endAt := t0.Add(prof.Window)
	end := pressureObservation(endAt, 101325)
	checks.observeSolution(barometricSolutionAt(endAt, 10), prof)
	context := pressureContext(start, end, 0, prof.MaxRegionalSpreadM+1)
	regional, barometric := checks.evaluate(PressureSample{Received: endAt, Observation: &end, Context: &context}, prof)
	if regional.State != Unavailable || barometric.State != Unavailable ||
		!slices.Contains(regional.Reasons, ReasonRegionalPressureIncoherent) ||
		!slices.Contains(barometric.Reasons, ReasonRegionalPressureIncoherent) {
		t.Fatalf("incoherent regional context = regional %+v barometric %+v", regional, barometric)
	}
}

func ptrPressureContext(context PressureContext) *PressureContext { return &context }
