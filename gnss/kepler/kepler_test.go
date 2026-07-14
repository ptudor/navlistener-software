package kepler

import (
	"errors"
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

// TestVelocityRejectsHalfWeekStraddle guards Velocity's central
// difference calls Propagate independently at tow+0.5 and tow-0.5, each of
// which re-derives its own half-week-wrapped tk via gnsstime.EphAge. When
// tow-toe sits within 0.5s of ±HalfWeek, only one of the two shifted instants
// crosses the wrap threshold, so the "velocity" silently becomes the position
// delta across a ~604800s jump (thousands of km) rather than 1s — finite, so
// it was never caught. Must now return an error instead, exactly at toe +
// HalfWeek and toe - HalfWeek, and still work normally comfortably away from
// the wrap.
func TestVelocityRejectsHalfWeekStraddle(t *testing.T) {
	toe := realisticGPS.Toe
	for _, tow := range []float64{toe + gnsstime.HalfWeek, toe - gnsstime.HalfWeek} {
		if _, err := Velocity(realisticGPS, tow); err != errHalfWeekStraddle {
			t.Errorf("tow=toe%+v: err = %v, want errHalfWeekStraddle", tow-toe, err)
		}
	}

	// Comfortably clear of the wrap (a full day short of it): must still work
	// and return a sane orbital speed, never the multi-thousand-km/s garbage
	// the bug produced.
	for _, tow := range []float64{toe + gnsstime.HalfWeek - 86400, toe - gnsstime.HalfWeek + 86400, toe} {
		vel, err := Velocity(realisticGPS, tow)
		if err != nil {
			t.Fatalf("tow=toe%+v: unexpected error: %v", tow-toe, err)
		}
		if s := vel.Norm(); s <= 0 || s > 10000 {
			t.Errorf("tow=toe%+v: speed = %.0f m/s, want a sane orbital speed (<10 km/s)", tow-toe, s)
		}
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

// TestBeiDouGEOStationarity guards a BeiDou GEO SV is nearly Earth-fixed in ECEF, so
// over an hour it drifts far less than a MEO SV over the same hour. A sign error in the GEO
// frame's Rz(ωe·tk)/Rx(−5°) rotation would make the GEO sweep at roughly Earth rate (tens of
// thousands of km/hour), so this stationarity property catches a rotation-sign regression the
// ≥1-km branch-switch assertion in TestBeiDouGEOBranch cannot.
func TestBeiDouGEOStationarity(t *testing.T) {
	const a = 42164000.0
	geo := Ephemeris{
		ID: gnss.BeiDou, SVID: 2,
		SqrtA: math.Sqrt(a), Ecc: 0.0003, M0: 0.1,
		I0: 0.02, Omega0: 0.5, Omega: 0.2, Toe: 0,
	}
	p0, err := Propagate(geo, 0)
	if err != nil {
		t.Fatal(err)
	}
	p1, err := Propagate(geo, 3600)
	if err != nil {
		t.Fatal(err)
	}
	// A correct BeiDou GEO traces a small analemma (~hundreds of km/hour from its residual
	// inclination and the −5° frame tilt). If the Rz(ωe·tk) rotation sign were flipped, the
	// SV would sweep at ~2× the sidereal rate — a ~30°/hour arc at 42 164 km ≈ 22 000 km of
	// ECEF drift — which this bound catches while passing the correct ~hundreds of km.
	geoDrift := p1.Sub(p0).Norm()
	if geoDrift > 3_000_000 {
		t.Errorf("GEO drifted %.0f km over 1h, want a small analemma (Rz/Rx sign error?)", geoDrift/1000)
	}
}

// TestBeiDouGEOTiltDirection analytically pins the Rx(-5°) branch. At
// tk=0, Omega=0, inclination=0, and x'=0, a positive in-plane y' maps to the
// sign and magnitude below. Changing the hard-coded tilt to +5° flips Z.
func TestBeiDouGEOTiltDirection(t *testing.T) {
	p, ok := physconst.For(gnss.BeiDou)
	if !ok {
		t.Fatal("missing BeiDou parameters")
	}
	const y = 42_164_000.0
	pos := beidouGEO(0, y, 0, Ephemeris{}, p, 0)
	wantY := math.Cos(5*math.Pi/180) * y
	wantZ := math.Sin(5*math.Pi/180) * y
	if math.Abs(pos.Y-wantY) > 1e-6 || math.Abs(pos.Z-wantZ) > 1e-6 {
		t.Fatalf("Rx(-5°) fixed point = (Y=%g,Z=%g), want (%g,%g)", pos.Y, pos.Z, wantY, wantZ)
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
	// a spoofed/malformed high-eccentricity ephemeris (far outside any real
	// broadcast, e<~0.03) must be rejected outright rather than risk a
	// non-converged Newton-Raphson solution silently passing as a finite position.
	bad = base
	bad.Ecc = 0.9
	bad.M0 = math.Pi
	if _, err := Propagate(bad, 432000); err == nil {
		t.Error("expected error for e=0.9 (spoofed/malformed high-eccentricity ephemeris)")
	}
	// A realistic eccentricity just under the new gate must still propagate fine —
	// the tightened bound must not reject legitimate slightly-eccentric orbits.
	ok := base
	ok.Ecc = 0.03
	if _, err := Propagate(ok, 432000); err != nil {
		t.Errorf("e=0.03 (realistic) rejected: %v", err)
	}
	// A constellation with no Keplerian parameter set (SBAS).
	bad = base
	bad.ID = gnss.SBAS
	if _, err := Propagate(bad, 432000); err == nil {
		t.Error("expected error for SBAS (no Kepler params)")
	}
	bad.ID = gnss.GLONASS
	if _, err := Propagate(bad, 432000); !errors.Is(err, errNoParams) {
		t.Errorf("GLONASS error = %v, want errNoParams (Cartesian broadcast)", err)
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

// TestDopplerRejectsInvalidInputs guards PredictedDoppler is a public
// API bound by the package's errors-never-NaN contract (gnss.go), but carrier
// frequency and receiver ECEF bypass propagation's validation — every invalid
// combination must return an error with a finite zero value, and the valid
// vector must be unchanged.
func TestDopplerRejectsInvalidInputs(t *testing.T) {
	recv := gnss.ECEF{X: 6378137}
	const l1 = 1575.42e6
	nan, inf := math.NaN(), math.Inf(1)
	cases := []struct {
		name string
		freq float64
		recv gnss.ECEF
	}{
		{"NaN frequency", nan, recv},
		{"+Inf frequency", inf, recv},
		{"-Inf frequency", -inf, recv},
		{"zero frequency", 0, recv},
		{"negative frequency", -l1, recv},
		{"NaN receiver X", l1, gnss.ECEF{X: nan}},
		{"Inf receiver Y", l1, gnss.ECEF{Y: inf}},
		{"NaN receiver Z", l1, gnss.ECEF{X: 6378137, Z: nan}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fd, err := PredictedDoppler(realisticGPS, 432000, tc.freq, tc.recv)
			if err == nil {
				t.Fatal("invalid input accepted")
			}
			if fd != 0 || math.IsNaN(fd) {
				t.Errorf("value = %v, want finite zero alongside the error", fd)
			}
		})
	}

	// A receiver coincident with the SV has no line of sight: error, not NaN.
	pos, err := Propagate(realisticGPS, 432000)
	if err != nil {
		t.Fatal(err)
	}
	if fd, err := PredictedDoppler(realisticGPS, 432000, l1, pos); err == nil || fd != 0 {
		t.Errorf("coincident receiver = (%v, %v), want (0, error)", fd, err)
	}

	// The realistic vector still returns the same finite Doppler as before.
	fd, err := PredictedDoppler(realisticGPS, 432000, l1, recv)
	if err != nil || math.Abs(fd) > 6000 {
		t.Errorf("valid vector = (%v, %v), want small finite Doppler with nil error", fd, err)
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
