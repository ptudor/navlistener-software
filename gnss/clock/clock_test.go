package clock

import (
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/gnsstime"
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
	if math.Abs(disco-1e-7) > 1e-12 { // the 100 ns af0 step
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

// TestUTCOffsetQuadraticTerm pins the A2 (drift-rate) term of UTCOffset —
// TestUTCOffset above exercises A2 = 0 only, and no navlistener code
// calls UTCOffset (feed.go inlines its own Eq. 7-25 evaluation on the absolute
// BDT axis), so deleting `+ u.A2*dt*dt` used to leave both modules' suites
// green. gnss/ is the vendorable Apache-2.0 module: an external consumer is the
// only caller of this exported three-term form, so the module must guard it.
//
// The vector is a legal broadcast set, not an arbitrary one. BDS-SIS-B2a-1.0
// v1.0 Table 7-20 gives the BDT-UTC field widths and scales — A0UTC 16 bits
// two's complement × 2⁻³⁵ s, A1UTC 13 bits × 2⁻⁵¹ s/s, A2UTC 7 bits × 2⁻⁶⁸
// s/s², tot 16 bits × 2⁴ s over 0~604784 — so each coefficient below is
// (raw integer within the signed field) × (Table 7-20 scale), and A2's raw 61
// sits inside the 7-bit two's-complement range −64…63. Eq. 7-25 is the
// governing polynomial (§7.12.2 case 1).
func TestUTCOffsetQuadraticTerm(t *testing.T) {
	const (
		p2m35 = 1.0 / (1 << 35)
		p2m51 = p2m35 / (1 << 16)
		p2m68 = p2m51 / (1 << 17)
	)
	u := UTCParams{
		A0:   -131 * p2m35, // raw −131 of 16 signed bits
		A1:   -13 * p2m51,  // raw −13 of 13 signed bits
		A2:   61 * p2m68,   // raw +61 of 7 signed bits — the term under test
		Tot:  518400,       // 32400 × 2⁴, inside Table 7-20's 0~604784
		DtLS: 18,
	}
	const tow = 259200 // dt = −259200 s: inside the ±half-week EphAge window, no wrap
	dt := gnsstime.EphAge(tow, u.Tot)
	if dt != -259200 {
		t.Fatalf("test setup: EphAge(%v, %v) = %v, want -259200 (no half-week wrap)", tow, u.Tot, dt)
	}

	want := u.A0 + u.A1*dt + u.A2*dt*dt + u.DtLS
	if got := UTCOffset(u, tow); math.Abs(got-want) > 1e-14 {
		t.Errorf("UTCOffset = %.17g, want %.17g (three-term Eq. 7-25)", got, want)
	}

	// Isolate the quadratic contribution: the same set with A2 zeroed must
	// differ by exactly A2·dt².
	flat := u
	flat.A2 = 0
	quad := UTCOffset(u, tow) - UTCOffset(flat, tow)
	wantQuad := u.A2 * dt * dt
	if math.Abs(quad-wantQuad) > 1e-13 {
		t.Errorf("A2 contribution = %.17g s, want %.17g s", quad, wantQuad)
	}
	// Sanity on the vector itself: a legal A2 at this dt is worth ~14 ns —
	// several times the 2.5 ns time-disco threshold, so dropping the term is a
	// real error, not a rounding artefact. (Guards against a future edit that
	// keeps the assertions but neuters the vector.)
	if math.Abs(wantQuad) < 1e-9 {
		t.Fatalf("test vector is degenerate: A2 contributes only %.3e s", wantQuad)
	}
}

func TestL2GroupDelayFactor(t *testing.T) {
	// γ = (1575.42/1227.60)² ≈ 1.6469.
	if math.Abs(L2GroupDelayFactor-1.6469) > 1e-3 {
		t.Errorf("γ = %v, want ≈1.6469", L2GroupDelayFactor)
	}
}

func TestE5aGroupDelayFactor(t *testing.T) {
	// (f_E1/f_E5a)² = (1575.420/1176.450)² ≈ 1.7933 (GAL-OS-SIS-ICD-2.2 Eq. 19,
	// carriers per Table 2) — and it must exceed 1 (E5a is the lower frequency).
	if math.Abs(E5aGroupDelayFactor-1.7933) > 1e-3 {
		t.Errorf("(f_E1/f_E5a)² = %v, want ≈1.7933", E5aGroupDelayFactor)
	}
}
