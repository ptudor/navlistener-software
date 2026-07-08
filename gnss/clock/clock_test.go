package clock

import (
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

func TestRelativisticZeroForCircular(t *testing.T) {
	// e = 0 ⇒ Δtr = 0 exactly.
	if got := Relativistic(-4.442807633e-10, 0, 5153, 1.2); got != 0 {
		t.Errorf("Δtr for circular orbit = %v, want 0", got)
	}
}

func TestRelativisticMagnitude(t *testing.T) {
	// A typical GPS relativistic term is tens of nanoseconds.
	f := physconst.MustFor(gnss.GPS).RelF
	dtr := Relativistic(f, 0.01, 5153.65, 1.0) // seconds
	ns := math.Abs(dtr) * 1e9
	if ns < 1 || ns > 100 {
		t.Errorf("Δtr = %.2f ns, want tens of ns", ns)
	}
	if dtr >= 0 {
		t.Errorf("Δtr should be negative for positive sin E (F<0), got %v", dtr)
	}
}

func TestOffsetPolynomial(t *testing.T) {
	// At tow = toc the linear/quadratic terms vanish, leaving af0 + Δtr − TGD.
	c := Model{ID: gnss.GPS, Af0: 1e-4, Af1: 1e-11, Af2: 0, Toc: 432000, TGD: 5e-9}
	const ecc, sqrtA, E = 0.01, 5153.65, 1.0
	got := Offset(c, 432000, ecc, sqrtA, E)
	want := c.Af0 + Relativistic(physconst.MustFor(gnss.GPS).RelF, ecc, sqrtA, E) - c.TGD
	if math.Abs(got-want) > 1e-18 {
		t.Errorf("Offset = %.12e, want %.12e", got, want)
	}
}

func TestOffsetLinearGrowth(t *testing.T) {
	// With only af1, the offset grows linearly in Δt.
	c := Model{ID: gnss.GPS, Af1: 1e-11, Toc: 0}
	o1 := Offset(c, 100, 0, 5153, 0)
	o2 := Offset(c, 200, 0, 5153, 0)
	if math.Abs((o2-o1)-1e-11*100) > 1e-20 {
		t.Errorf("linear drift wrong: Δ = %.3e over 100 s", o2-o1)
	}
}

func TestOffsetForReusesSolveE(t *testing.T) {
	e := kepler.Ephemeris{
		ID: gnss.GPS, SVID: 5, SqrtA: 5153.65, Ecc: 0.01, M0: 0.3,
		I0: 0.96, Omega0: -0.5, Omega: 0.7, Toe: 432000,
	}
	c := Model{ID: gnss.GPS, Af0: 1e-4, Toc: 432000, TGD: 5e-9}
	got, err := OffsetFor(c, e, 432500)
	if err != nil {
		t.Fatal(err)
	}
	// Manually solve and apply Offset — must match exactly.
	sol, _ := kepler.Solve(e, 432500)
	want := Offset(c, 432500, e.Ecc, e.SqrtA, sol.E)
	if got != want {
		t.Errorf("OffsetFor = %v, want %v (E not reused consistently)", got, want)
	}
}

func TestOffsetForPropagatesError(t *testing.T) {
	bad := kepler.Ephemeris{ID: gnss.GPS, SqrtA: 0} // zero axis
	if _, err := OffsetFor(Model{ID: gnss.GPS}, bad, 0); err == nil {
		t.Error("OffsetFor should surface the kepler solve error")
	}
}

func TestTimeDiscoShape(t *testing.T) {
	// A clock changeover: two models evaluated at the same epoch give the
	// time-disco (docs/INTEGRITY.md §3). A tiny af0 step shows up directly.
	old := Model{ID: gnss.Galileo, Af0: 1.000e-4, Toc: 100000}
	neu := Model{ID: gnss.Galileo, Af0: 1.001e-4, Toc: 100000}
	const ecc, sqrtA, E = 0.0002, 5440.0, 0.5
	disco := math.Abs(Offset(neu, 100000, ecc, sqrtA, E) - Offset(old, 100000, ecc, sqrtA, E))
	if math.Abs(disco-1e-7) > 1e-12 { // 0.1 ns step
		t.Errorf("time-disco = %.3e s, want 1e-7", disco)
	}
}

func TestUTCOffset(t *testing.T) {
	u := UTCParams{A0: -3.8e-9, A1: -1e-15, Tot: 0, DtLS: 18}
	got := UTCOffset(u, 3600)
	want := -3.8e-9 + -1e-15*3600 + 18
	if math.Abs(got-want) > 1e-18 {
		t.Errorf("UTCOffset = %v, want %v", got, want)
	}
}

func TestL2GroupDelayFactor(t *testing.T) {
	// γ = (1575.42/1227.60)² ≈ 1.6469.
	if math.Abs(L2GroupDelayFactor-1.6469) > 1e-3 {
		t.Errorf("γ = %v, want ≈1.6469", L2GroupDelayFactor)
	}
}
