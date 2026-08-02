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
	// night-time floor. At zenith the obliquity factor F ≈ 1. Note tow=43200
	// lands *inside* the cosine window (x ≈ −0.63) — with amp = 0 both branches
	// agree, so the |x| ≥ 1.57 branch itself is pinned separately below.
	el := 90 * math.Pi / 180
	got := Klobuchar([4]float64{}, [4]float64{}, 0, 0, 0, el, 43200)
	if math.Abs(got-5e-9) > 1e-11 {
		t.Errorf("night floor = %.4e s, want ≈5e-9", got)
	}
}

// TestKlobucharOutsideWindowFloor exercises the |x| >= 1.57 branch, which
// TestKlobucharNightFloor cannot distinguish (its zero coefficients make both
// branches equal). With a nonzero amplitude, deep-night local time must return
// exactly F·5 ns — the cosine term contributing here would mean the window
// comparison leaked.
func TestKlobucharOutsideWindowFloor(t *testing.T) {
	alpha := [4]float64{1e-7, 0, 0, 0} // constant amp = 1e-7 s, 20× the floor
	var beta [4]float64                // per floors to 72000 s
	el := 90 * math.Pi / 180
	// lat = lon = 0 and az = 0 make the IPP local time ≈ tow itself; at
	// tow = 7200 (02:00), x = 2π(7200−50400)/72000 ≈ −3.77, well outside 1.57.
	got := Klobuchar(alpha, beta, 0, 0, 0, el, 7200)
	f := 1 + 16*math.Pow(0.53-0.5, 3) // obliquity at zenith (el = 0.5 semicircles)
	want := f * 5e-9
	if math.Abs(got-want) > 1e-13 {
		t.Errorf("outside-window delay = %.6e s, want F·5e-9 = %.6e", got, want)
	}
	// The same inputs at 14:00 local (x = 0) must exceed the floor — proving the
	// two branches genuinely differ under this alpha, so the assertion above is
	// checking the branch, not a coincidence.
	if day := Klobuchar(alpha, beta, 0, 0, 0, el, 50400); day <= want {
		t.Errorf("in-window delay %.6e s should exceed the floor %.6e s", day, want)
	}
}

// TestKlobucharNegativeElevationGuarded guards IS-GPS-200N defines the
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

// TestKlobucharPublishedVector pins the model to the classic worked example from
// Klobuchar's own chapter (Parkinson & Spilker, "GPS: Theory and Applications"
// Vol. I ch. 12): user at 40°N 100°W, satellite at azimuth 210° elevation 20°,
// 2000-01-01 20:45:00 UTC, broadcast α=(.3820e-7,.1490e-7,-.1790e-6,0),
// β=(.1430e6,0,-.3280e6,.1130e6) → 23.784 m slant L1 delay. The expected value is
// as pinned by Orekit's KlobucharModelTest.compareExpectedValue (Apache-2.0,
// asserted there to ±1 mm) — an independent implementation, so this is an external
// oracle in the docs/MATH.md §12 sense, not a self-referential regression.
func TestKlobucharPublishedVector(t *testing.T) {
	a := [4]float64{0.3820e-7, 0.1490e-7, -0.1790e-6, 0}
	b := [4]float64{0.1430e6, 0, -0.3280e6, 0.1130e6}
	lat := 40 * math.Pi / 180
	lon := -100 * math.Pi / 180
	az := 210 * math.Pi / 180
	el := 20 * math.Pi / 180
	// 2000-01-01 20:45:00 UTC: Saturday (GPS day-of-week 6), GPS−UTC = 13 s then,
	// so GPS TOW = 6·86400 + 20·3600 + 45·60 + 13. The 13 s only shifts the
	// diurnal phase by 13/86400 of a cycle (sub-mm here).
	tow := 6*86400.0 + 20*3600 + 45*60 + 13
	const c = 299792458.0 // m/s, physconst.SpeedOfLight (unimported to keep iono dep-free)

	gotM := Klobuchar(a, b, lat, lon, az, el, tow) * c
	const wantM = 23.784
	// 20 mm gate: Orekit pins ±1 mm; the leap-second phase shift and float
	// noise stay far below this, while any structural error is metres.
	if math.Abs(gotM-wantM) > 0.02 {
		t.Errorf("Klobuchar published vector = %.4f m, want %.3f ± 0.02 m", gotM, wantM)
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
