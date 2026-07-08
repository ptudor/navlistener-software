package glonass

import (
	"math"
	"testing"
)

// nearCircular is a plausible GLONASS state: r ≈ 25100 km with velocity
// perpendicular to position at the circular speed √(μ/r) ≈ 3.985 km/s.
var nearCircular = Ephemeris{
	X: 10000, Y: 13000, Z: 19000,
	Vx: 3.159, Vy: -2.430, Vz: 0,
	Ax: 1e-6, Ay: -1e-6, Az: 2e-6,
	Tb: 0, TodKnown: true, FreqCh: 1, Slot: 7,
}

func TestPropagateZeroReturnsState(t *testing.T) {
	// tk = 0 returns the broadcast position, in metres.
	p, err := Propagate(nearCircular, 0)
	if err != nil {
		t.Fatal(err)
	}
	if p.X != 10000*1000 || p.Y != 13000*1000 || p.Z != 19000*1000 {
		t.Errorf("tk=0 → %+v, want the broadcast state ×1000", p)
	}
}

func TestRadiusSaneOverSpan(t *testing.T) {
	for _, tk := range []float64{-600, -60, 60, 300, 600} {
		p, err := Propagate(nearCircular, tk)
		if err != nil {
			t.Fatal(err)
		}
		if r := p.Norm(); r < 24.0e6 || r > 26.5e6 {
			t.Errorf("tk=%v radius = %.0f m, want ~25100 km", tk, r)
		}
	}
}

// TestReversibility is the strong integrator check: stepping forward then back by
// the same interval must return (nearly) to the start.
func TestReversibility(t *testing.T) {
	s0 := state{nearCircular.X, nearCircular.Y, nearCircular.Z,
		nearCircular.Vx, nearCircular.Vy, nearCircular.Vz}
	j := accel{nearCircular.Ax, nearCircular.Ay, nearCircular.Az}
	fwd := integrate(s0, j, 300)
	back := integrate(fwd, j, -300)
	var maxErr float64
	for i := 0; i < 6; i++ {
		if d := math.Abs(back[i] - s0[i]); d > maxErr {
			maxErr = d
		}
	}
	if maxErr > 1e-3 { // < 1 metre / < 1 mm/s round-trip residual
		t.Errorf("forward/back round-trip residual = %.3e (km, km/s), want < 1e-3", maxErr)
	}
}

func TestGuardTimeless(t *testing.T) {
	e := nearCircular
	e.TodKnown = false
	if _, err := Propagate(e, 100); err == nil {
		t.Error("expected errTimeless when todKnown is false")
	}
}

func TestGuardZeroState(t *testing.T) {
	e := Ephemeris{TodKnown: true} // all-zero position
	if _, err := Propagate(e, 100); err == nil {
		t.Error("expected errZero for a zero state vector")
	}
}

func TestDeterministic(t *testing.T) {
	a, _ := Propagate(nearCircular, 321)
	b, _ := Propagate(nearCircular, 321)
	if a != b {
		t.Errorf("non-deterministic: %+v vs %+v", a, b)
	}
}

func TestVelocityMagnitude(t *testing.T) {
	// Sanity on the constructed state: near the circular speed.
	v := math.Sqrt(nearCircular.Vx*nearCircular.Vx + nearCircular.Vy*nearCircular.Vy + nearCircular.Vz*nearCircular.Vz)
	if v < 3.5 || v > 4.3 {
		t.Errorf("GLONASS speed = %.3f km/s, want ~3.95", v)
	}
}
