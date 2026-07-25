package glonass

import (
	"errors"
	"math"
	"testing"

	"github.com/ptudor/gnss/physconst"
)

// newtonKepler is an independent high-accuracy reference for E − e·sin E = M, used to check
// that solveKeplerEcc returns the RIGHT root inside the broadcast domain rather than merely
// a self-consistent one. Newton converges quadratically over the whole e ∈ [0,1) domain, so
// it is a genuine oracle for the fixed-point iterator under test, not a re-implementation of
// it. Starting guess per the standard e-weighted form; 60 iterations is far past convergence
// for anything this test feeds it.
func newtonKepler(m, e float64) float64 {
	ea := m
	if e > 0.8 {
		ea = physconst.Pi
	}
	for i := 0; i < 60; i++ {
		f := ea - e*math.Sin(ea) - m
		fp := 1 - e*math.Cos(ea)
		if fp == 0 {
			break
		}
		step := f / fp
		ea -= step
		if math.Abs(step) < 1e-15 {
			break
		}
	}
	return ea
}

func keplerResidual(ea, m, e float64) float64 { return math.Abs(ea - e*math.Sin(ea) - m) }

// TestSolveKeplerBroadcastDomainConverges covers the eccentricity range a decoded GLONASS
// almanac can actually produce. εnA is a 15-bit field scaled 2⁻²⁰ (gnss/frame's almanac
// string decode), so the largest representable broadcast value is 32767·2⁻²⁰ ≈ 0.03125.
// Across that whole domain and a dense sweep of mean anomalies the solve must converge,
// report no error, and land on the same root Newton finds (regression fix asked for exactly this:
// "test the full accepted eccentricity domain and record the worst residual").
func TestSolveKeplerBroadcastDomainConverges(t *testing.T) {
	// 32767·2⁻²⁰ — the largest e the 15-bit εnA field can carry.
	const maxBroadcastEcc = 32767.0 / 1048576.0

	eccs := []float64{0, 1e-9, 1e-4, 0.001482010 /* the ICD §A.3.2.3 example */, 0.01,
		maxBroadcastEcc / 2, maxBroadcastEcc}

	worst := 0.0
	worstAt := [2]float64{}
	for _, e := range eccs {
		// Sweep M over a full revolution plus values outside [0, 2π): the caller passes
		// λ* − ω, which is not range-reduced, so negative and multi-revolution M reach here.
		for k := -720; k <= 720; k += 3 {
			m := float64(k) * physconst.Pi / 180
			ea, err := solveKeplerEcc(m, e)
			if err != nil {
				t.Fatalf("e=%.9f M=%.4f rad: unexpected error %v (broadcast domain must converge)", e, m, err)
			}
			r := keplerResidual(ea, m, e)
			if r > worst {
				worst, worstAt = r, [2]float64{e, m}
			}
			if r > keplerResidualTol {
				t.Errorf("e=%.9f M=%.4f rad: residual %.3e exceeds tol %.0e", e, m, r, keplerResidualTol)
			}
			if ref := newtonKepler(m, e); math.Abs(ea-ref) > 1e-11 {
				t.Errorf("e=%.9f M=%.4f rad: E=%.15f, Newton reference %.15f (Δ=%.3e)", e, m, ea, ref, ea-ref)
			}
		}
	}
	// Recorded, not asserted at a tight bound: the point is that the worst case across the
	// entire broadcast domain sits orders of magnitude under the tolerance, so the guard
	// added in regression fix can never fire on real data.
	t.Logf("worst residual over the broadcast domain: %.3e at e=%.9f M=%.4f rad (tol %.0e)",
		worst, worstAt[0], worstAt[1], keplerResidualTol)
}

// TestSolveKeplerRejectsUnconverged is the other half of outside the broadcast
// domain, twenty fixed-point iterations cannot reach the stopping delta, and the function
// used to return that last iterate as if it were a solution. Each case below is a finite,
// entirely plausible-looking number — which is precisely why silently returning it was the
// dangerous behaviour.
//
// The cases are measured, not assumed. Sweeping e in 0.05 steps against M over a full
// revolution puts the practical boundary at e ≈ 0.45: at e ≤ 0.4 every mean anomaly
// converges inside 20 iterations, and from e ≈ 0.45 up the hard region near M → 0 (where the
// contraction factor |e·cos E| is largest) stops converging — 32/72 sampled anomalies at
// e = 0.5, rising to 54/72 at e = 0.999. Small M is therefore where each case sits; large M
// at the same e can converge fine, which is exactly why the guard checks the residual of the
// answer rather than trying to bound the input domain.
func TestSolveKeplerRejectsUnconverged(t *testing.T) {
	for _, tc := range []struct{ e, m float64 }{
		{0.45, 0.1}, {0.5, 0.05}, {0.5, 0.1},
		{0.75, 0.1}, {0.9, 0.1}, {0.9, 2.0},
		{0.999, 0.1}, {0.999, 3.0},
	} {
		ea, err := solveKeplerEcc(tc.m, tc.e)
		if !errors.Is(err, errAlmKepler) {
			t.Errorf("e=%.3f M=%.3f: got (%.9f, %v), want errAlmKepler — the residual is %.3e",
				tc.e, tc.m, ea, err, keplerResidual(ea, tc.m, tc.e))
			continue
		}
		if ea != 0 {
			t.Errorf("e=%.3f M=%.3f: error path returned E=%v, want the zero value", tc.e, tc.m, ea)
		}
	}
}

// TestSolveKeplerInvariant sweeps the entire accepted eccentricity domain the exported API
// permits (propagateAlmanac only rejects e < 0 or e ≥ 1) and asserts the one property that
// must hold everywhere: a nil error means the returned anomaly actually solves Kepler's
// equation to the stated tolerance. This is the guarantee callers get; which specific
// (e, M) pairs converge is an implementation detail of the iterator.
func TestSolveKeplerInvariant(t *testing.T) {
	converged, rejected := 0, 0
	for ei := 0; ei < 100; ei++ {
		e := float64(ei) / 100 // 0.00 .. 0.99
		for k := 0; k < 360; k += 7 {
			m := float64(k) * physconst.Pi / 180
			ea, err := solveKeplerEcc(m, e)
			if err != nil {
				if !errors.Is(err, errAlmKepler) {
					t.Fatalf("e=%.2f M=%.4f: unexpected error kind %v", e, m, err)
				}
				rejected++
				continue
			}
			converged++
			if r := keplerResidual(ea, m, e); r > keplerResidualTol {
				t.Fatalf("e=%.2f M=%.4f: nil error but residual %.3e > tol %.0e", e, m, r, keplerResidualTol)
			}
			if math.IsNaN(ea) || math.IsInf(ea, 0) {
				t.Fatalf("e=%.2f M=%.4f: nil error but non-finite E %v", e, m, ea)
			}
		}
	}
	t.Logf("invariant held: %d converged, %d rejected as unconverged", converged, rejected)
}

// TestPropagateAlmanacRejectsUnconvergedKepler pins the guard end-to-end through the
// exported API. PropagateAlmanacECEF documents "an error — never a NaN — on degenerate
// input"; before regression fix an out-of-domain eccentricity produced a finite ECEF triple built
// on an unconverged anomaly, which satisfies the letter of that contract while violating its
// intent. Whether a given epoch lands in the non-converging region depends on the mean
// anomaly the propagation happens to reach, so this sweeps a full day: the guard must be
// reachable through the public API (at least one epoch rejected), and every rejection must
// return the zero position rather than a partial one.
func TestPropagateAlmanacRejectsUnconvergedKepler(t *testing.T) {
	a := r135Alm(615)
	a.Ecc = 0.9 // far outside anything εnA can encode; accepted by the [0,1) domain check
	kepler, other := 0, 0
	for ti := 0.0; ti < 86400; ti += 97 {
		pos, err := PropagateAlmanacECEF(a, 615, ti)
		if err == nil {
			continue
		}
		if errors.Is(err, errAlmKepler) {
			kepler++
		} else {
			other++
		}
		if pos.X != 0 || pos.Y != 0 || pos.Z != 0 {
			t.Fatalf("ti=%.0f: error %v returned a non-zero position %+v", ti, err, pos)
		}
	}
	if kepler == 0 {
		t.Fatalf("no epoch hit the Kepler convergence guard (%d rejected by other guards); "+
			"the guard is unreachable through the exported API", other)
	}
	t.Logf("e=0.9 over one day: %d epochs rejected as unconverged, %d by other guards", kepler, other)
}
