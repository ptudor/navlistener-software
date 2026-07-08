package kepler

import (
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/physconst"
)

// TestCircularEquatorialAnalytic exercises the whole propagation chain against a
// closed-form answer: a perfectly circular (e=0), equatorial (i₀=0) orbit with no
// perturbation corrections. Then r = A exactly, Z = 0, and the in-plane angle is
// u + Ω = (M₀ + n·tk + ω) + Ω, with Ω = Ω₀ − ωe·(tk + toe).
func TestCircularEquatorialAnalytic(t *testing.T) {
	const a = 26560000.0 // ~GPS semi-major axis, m
	e := Ephemeris{
		ID: gnss.GPS, SVID: 5,
		SqrtA:  math.Sqrt(a),
		Ecc:    0,
		M0:     0.5,
		Omega0: 0.4,
		Omega:  0.3,
		I0:     0,
		Toe:    3600,
	}
	tow := 5400.0
	sol, err := Solve(e, tow)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(sol.Pos.Z) > 1e-6 {
		t.Errorf("equatorial orbit Z = %v, want 0", sol.Pos.Z)
	}
	if rel := math.Abs(sol.Pos.Norm()-a) / a; rel > 1e-9 {
		t.Errorf("circular radius = %v, want %v (rel %e)", sol.Pos.Norm(), a, rel)
	}
	// Expected planar angle.
	p := physconst.MustFor(gnss.GPS)
	n := math.Sqrt(p.Mu/(a*a*a)) + e.DeltaN
	tk := gnsstime.EphAge(tow, e.Toe)
	om := e.Omega0 - p.OmegaE*(tk+e.Toe)
	wantAng := e.M0 + n*tk + e.Omega + om
	gotAng := math.Atan2(sol.Pos.Y, sol.Pos.X)
	if d := angleDiff(gotAng, wantAng); math.Abs(d) > 1e-9 {
		t.Errorf("planar angle off by %v rad", d)
	}
	// tk was 1800 s; E should equal M for a circular orbit.
	if math.Abs(sol.E-(e.M0+n*tk)) > 1e-9 {
		t.Errorf("circular E = %v, want M = %v", sol.E, e.M0+n*tk)
	}
}

// realisticGPS is a plausible, SI-scaled GPS broadcast ephemeris used for
// order-of-magnitude sanity (a scaling bug would move the radius by orders).
var realisticGPS = Ephemeris{
	ID: gnss.GPS, SVID: 5,
	SqrtA: 5153.65, Ecc: 0.005, M0: 0.3, DeltaN: 4.5e-9,
	I0: 0.96, IDot: 1e-10, Omega0: -0.5, OmegaDot: -8.1e-9, Omega: 0.7,
	Cuc: 1e-6, Cus: 8e-6, Crc: 250, Crs: -20, Cic: 1e-7, Cis: -2e-7,
	Toe: 432000,
}

func TestRealisticGPSRadiusAndSpeed(t *testing.T) {
	pos, err := Propagate(realisticGPS, 432000)
	if err != nil {
		t.Fatal(err)
	}
	if r := pos.Norm(); r < 26.0e6 || r > 27.2e6 {
		t.Errorf("GPS orbit radius = %.0f m, want ~26560 km", r)
	}
	vel, err := Velocity(realisticGPS, 432000)
	if err != nil {
		t.Fatal(err)
	}
	if s := vel.Norm(); s < 2500 || s > 4200 {
		t.Errorf("GPS ECEF speed = %.0f m/s, want ~3–4 km/s", s)
	}
}

func TestBeiDouGEOBranch(t *testing.T) {
	// A near-stationary BeiDou GEO SV (~42164 km, small inclination). The GEO
	// branch must be taken for SVID ≤ 5 and yield a GEO-radius position.
	const a = 42164000.0
	geo := Ephemeris{
		ID: gnss.BeiDou, SVID: 2,
		SqrtA: math.Sqrt(a), Ecc: 0.0003, M0: 0.1,
		I0: 0.02, Omega0: 0.5, Omega: 0.2, Toe: 0,
	}
	pos, err := Propagate(geo, 100)
	if err != nil {
		t.Fatal(err)
	}
	if r := pos.Norm(); r < 41.5e6 || r > 42.8e6 {
		t.Errorf("BeiDou GEO radius = %.0f m, want ~42164 km", r)
	}
	// The same elements treated as a MEO SV (SVID 20) take the ordinary branch and
	// must land in a different ECEF spot — proof the branch actually switches.
	meo := geo
	meo.SVID = 20
	posMEO, err := Propagate(meo, 100)
	if err != nil {
		t.Fatal(err)
	}
	if pos.Sub(posMEO).Norm() < 1000 {
		t.Error("GEO and MEO branches produced ~identical positions; branch not taken")
	}
}

func TestErrorGuards(t *testing.T) {
	base := realisticGPS
	// Non-positive semi-major axis.
	bad := base
	bad.SqrtA = 0
	if _, err := Propagate(bad, 432000); err == nil {
		t.Error("expected error for zero √A")
	}
	// Out-of-range eccentricity.
	bad = base
	bad.Ecc = 1.5
	if _, err := Propagate(bad, 432000); err == nil {
		t.Error("expected error for e >= 1")
	}
	// A constellation with no Keplerian parameter set (SBAS).
	bad = base
	bad.ID = gnss.SBAS
	if _, err := Propagate(bad, 432000); err == nil {
		t.Error("expected error for SBAS (no Kepler params)")
	}
}

func TestDeterministic(t *testing.T) {
	a, err1 := Propagate(realisticGPS, 432123)
	b, err2 := Propagate(realisticGPS, 432123)
	if err1 != nil || err2 != nil || a != b {
		t.Errorf("propagation not deterministic: %v %v (%v %v)", a, b, err1, err2)
	}
}

func TestDopplerPlausible(t *testing.T) {
	// Receiver on the ground under the constellation; L1 Doppler for a GPS SV is
	// within a few kHz.
	recv := gnss.ECEF{X: 6378137} // equator, prime meridian
	const l1 = 1575.42e6
	fd, err := PredictedDoppler(realisticGPS, 432000, l1, recv)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(fd) > 6000 {
		t.Errorf("L1 Doppler = %.0f Hz, implausibly large (want < ~5 kHz)", fd)
	}
}

// angleDiff returns the smallest signed difference a − b wrapped to (−π, π].
func angleDiff(a, b float64) float64 {
	d := math.Mod(a-b, 2*math.Pi)
	if d > math.Pi {
		d -= 2 * math.Pi
	} else if d <= -math.Pi {
		d += 2 * math.Pi
	}
	return d
}
