package iono

import (
	"math"
	"testing"
)

// typical mid-latitude broadcast Klobuchar coefficients.
var (
	alpha = [4]float64{1.1e-8, 2.2e-8, -1.2e-7, -1.2e-7}
	beta  = [4]float64{1.0e5, 3.3e4, -3.3e5, -1.3e5}
)

func TestKlobucharZenithPlausible(t *testing.T) {
	// Satellite at zenith over a mid-latitude user around local noon.
	lat := 40 * math.Pi / 180
	lon := -75 * math.Pi / 180
	el := 90 * math.Pi / 180
	delay := Klobuchar(alpha, beta, lat, lon, 0, el, 50400)
	ns := delay * 1e9
	if ns <= 0 || ns > 60 {
		t.Errorf("zenith delay = %.2f ns, want a small positive value", ns)
	}
}

func TestKlobucharLowElevationLarger(t *testing.T) {
	// Obliquity makes a low-elevation ray's delay larger than the zenith ray's.
	lat := 40 * math.Pi / 180
	lon := -75 * math.Pi / 180
	zenith := Klobuchar(alpha, beta, lat, lon, 0, 90*math.Pi/180, 50400)
	low := Klobuchar(alpha, beta, lat, lon, 0, 10*math.Pi/180, 50400)
	if low <= zenith {
		t.Errorf("low-elevation delay %.3e should exceed zenith %.3e", low, zenith)
	}
}

func TestKlobucharNightFloor(t *testing.T) {
	// With zero coefficients the amplitude term vanishes, leaving the F·5 ns
	// night-time floor. At zenith the obliquity factor F ≈ 1.
	el := 90 * math.Pi / 180
	got := Klobuchar([4]float64{}, [4]float64{}, 0, 0, 0, el, 43200)
	if math.Abs(got-5e-9) > 1e-11 {
		t.Errorf("night floor = %.4e s, want ≈5e-9", got)
	}
}

// TestKlobucharNegativeElevationGuarded guards IS-GPS-200 defines the
// model for el >= 0. At el = -19.8 deg (-0.11*pi rad) the earth-centred-angle
// term 0.0137/(e+0.11) - 0.022 divides by zero (e+0.11 == 0), yielding ±Inf ->
// NaN/Inf delay before the fix; any negative elevation must now clamp to el=0
// and return the same finite, ICD-plausible delay as an actual el=0 call.
func TestKlobucharNegativeElevationGuarded(t *testing.T) {
	lat := 40 * math.Pi / 180
	lon := -75 * math.Pi / 180

	zero := Klobuchar(alpha, beta, lat, lon, 0, 0, 50400)

	for _, elDeg := range []float64{-19.8, -5, 0} {
		el := elDeg * math.Pi / 180
		got := Klobuchar(alpha, beta, lat, lon, 0, el, 50400)
		if math.IsNaN(got) || math.IsInf(got, 0) {
			t.Errorf("el=%.1fdeg: delay = %v, want finite", elDeg, got)
		}
		if got != zero {
			t.Errorf("el=%.1fdeg: delay = %v, want the clamped-to-zero delay %v", elDeg, got, zero)
		}
	}
}

func TestScaleDelay(t *testing.T) {
	// L5 delay exceeds L1 by (f_L1/f_L5)² ≈ 1.793.
	l1 := 10e-9
	l5 := ScaleDelay(l1, L5Hz)
	ratio := l5 / l1
	if math.Abs(ratio-(L1Hz/L5Hz)*(L1Hz/L5Hz)) > 1e-9 {
		t.Errorf("L5 scale ratio = %v", ratio)
	}
	if ratio < 1.7 || ratio > 1.9 {
		t.Errorf("L5/L1 ratio = %v, want ≈1.79", ratio)
	}
	// Scaling to L1 itself is a no-op.
	if math.Abs(ScaleDelay(l1, L1Hz)-l1) > 1e-18 {
		t.Error("scaling to L1 should be identity")
	}
}

func TestNeQuickEffectiveIonisation(t *testing.T) {
	n := NeQuickG{A0: 100, A1: 2, A2: 0.5}
	if got := n.EffectiveIonisation(10); got != 100+20+50 {
		t.Errorf("Az = %v, want 170", got)
	}
}
