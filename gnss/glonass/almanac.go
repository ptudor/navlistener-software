package glonass

// GLONASS almanac orbit propagation — the analytic long-term motion model that
// turns the coarse, long-life almanac elements into a position, for the all-SV
// acquisition-grade almanac feed (docs/OUTPUT.md §1.4) including GLONASS SVs that
// are currently out of ephemeris view. Source: GLONASS ICD Edition 5.1 (2008),
// Appendix 3, §A.3.2.2 "Algorithm of calculation of satellite motion parameters
// using almanac" — authored from that ICD text, validated against the ICD's own
// worked numerical example (§A.3.2.3) in almanac_test.go.
//
// The model iterates the semi-major axis to the draconitic period, applies the
// secular + periodic perturbations of the second zonal harmonic C20, and solves
// the perturbed Keplerian orbit. The critical frame subtlety (docs/MATH.md §3.1,
// "the F0 bug"): the almanac's node longitude λ is a *Greenwich* longitude, so the
// PZ-90 ECEF node at time t is Ω = λ − ωe·(t − tλk) + δΩ — Earth rotation folded in
// directly, no sidereal-time term. Expressing it this way makes the result ECEF
// with no astronomical-almanac input, and passes the multi-time-of-day ground-track
// check a single-instant test cannot (the exact trap §3.1 warns about).

import (
	"errors"
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/physconst"
)

// Almanac is one decoded GLONASS almanac element set (ICD §A.3.2.1), scaled to SI:
// angles in radians (broadcast half-cycles × π), times in seconds, referenced to
// the ascending-node passage tλ on calendar day NA.
type Almanac struct {
	NA        int     // calendar day number within the four-year interval
	Lambda    float64 // Greenwich longitude of ascending node at tλ, rad
	Tlambda   float64 // instant of first ascending-node passage within day NA, s
	DeltaI    float64 // correction to the mean inclination (i_avg = 63°), rad
	DeltaT    float64 // correction to the mean Draconian period (T_avg = 43200 s), s
	DeltaTdot float64 // rate of change of the Draconian period, s per period
	Ecc       float64 // eccentricity
	Omega     float64 // argument of perigee, rad
	Slot      int     // slot number n
	FreqCh    int     // FDMA channel k = freqId − 7
}

// GLONASS ICD Ed. 5.1 §A.3.2.2 constants (kilometre/second units, the ICD's).
const (
	almMu   = 398600.44    // gravitational constant μ, km³/s²
	almAe   = 6378.136     // equatorial radius a_e, km
	almC20  = -1082.63e-6  // second zonal harmonic C20
	almWe   = 7.2921150e-5 // Earth rotation rate ω_e, rad/s
	almTavg = 43200.0      // mean Draconian period T_avg, s
	// gloDaysPerCycle is the length of the GLONASS four-year interval (3×365 + 366) in
	// days; NA/NT are day numbers 1..1461 within it (regression fix cycle-boundary wrap).
	gloDaysPerCycle = 1461.0
)

// almIavg is the mean inclination, 63° (ICD §A.3.2.1), in radians.
var almIavg = 63.0 * physconst.Pi / 180.0

var (
	errAlmEcc  = errors.New("glonass: almanac eccentricity out of range [0,1)")
	errAlmNaN  = errors.New("glonass: almanac propagation produced a non-finite value")
	errAlmZero = errors.New("glonass: almanac has a zero/degenerate period")
)

// PropagateAlmanacECEF returns the PZ-90 ECEF position (metres) of the SV described
// by almanac a at time ti (seconds) on calendar day n0, using the rotating-Greenwich
// node convention so the result is ECEF directly (docs/MATH.md §3.1). It returns an
// error — never a NaN — on degenerate input.
func PropagateAlmanacECEF(a Almanac, n0 int, ti float64) (gnss.ECEF, error) {
	posKm, _, err := propagateAlmanac(a, n0, ti, ecefNode, 0)
	if err != nil {
		return gnss.ECEF{}, err
	}
	return gnss.ECEF{X: posKm.X * 1000, Y: posKm.Y * 1000, Z: posKm.Z * 1000}, nil
}

// nodeConvention selects how the base longitude of the ascending node at ti is
// formed from λk (ecefNode: rotating Greenwich frame; inertialNode: the ICD's
// absolute OXaYaZa frame, used by the §A.3.2.3 example test with s = S₀+ωe(tλk−3h)).
type nodeConvention int

const (
	ecefNode nodeConvention = iota
	inertialNode
)

// propagateAlmanac is the shared core: it iterates the semi-major axis, folds in the
// C20 secular + periodic perturbations, solves the perturbed orbit, and returns the
// position and velocity (km, km/s). node selects the frame; s0 is the Greenwich
// sidereal time at midnight (only used for inertialNode). Angles in radians.
func propagateAlmanac(a Almanac, n0 int, ti float64, node nodeConvention, s0 float64) (pos, vel gnss.ECEF, err error) {
	e := a.Ecc
	if e < 0 || e >= 1 || math.IsNaN(e) {
		return gnss.ECEF{}, gnss.ECEF{}, errAlmEcc
	}
	Tdr := almTavg + a.DeltaT // draconitic period
	if Tdr <= 0 {
		return gnss.ECEF{}, gnss.ECEF{}, errAlmZero
	}
	incl := almIavg + a.DeltaI
	sinI, cosI := math.Sincos(incl)
	s2i := sinI * sinI
	nu0 := -a.Omega // true anomaly at the ascending node
	J := -1.5 * almC20

	// Semi-major axis by successive approximation against the draconitic period
	// (ICD §A.3.2.2); three iterations suffice, guarded to a fixed tolerance.
	twoPi := 2 * physconst.Pi
	cbrtMuT := func(T float64) float64 { return math.Cbrt(T * T / (twoPi * twoPi) * almMu) }
	semiA := cbrtMuT(Tdr)
	_, cosNu0 := math.Sincos(nu0)
	for i := 0; i < 20; i++ {
		p := semiA * (1 - e*e)
		if p <= 0 {
			return gnss.ECEF{}, gnss.ECEF{}, errAlmNaN
		}
		aeP2 := (almAe / p) * (almAe / p)
		bracket := (2-2.5*s2i)*math.Pow(1-e*e, 1.5)/((1+e*cosNu0)*(1+e*cosNu0)) +
			math.Pow(1+e*cosNu0, 3)/(1-e*e)
		Tosc := Tdr / (1 + 1.5*almC20*aeP2*bracket)
		next := cbrtMuT(Tosc)
		if math.Abs(next-semiA) < 1e-3 {
			semiA = next
			break
		}
		semiA = next
	}

	n := twoPi / Tdr
	aeA2 := (almAe / semiA) * (almAe / semiA)

	// The ascending-node passage tλk of the k-th orbital period containing ti, and
	// the node's Greenwich longitude λk after k periods (secular node drift Ω' and
	// Earth rotation folded over the full periods elapsed).
	// wrap (n0 − NA) across the 1461-day four-year-interval boundary. An almanac
	// referenced to NA near the end of the cycle (e.g. 1461) but evaluated on day 1 of the
	// next cycle has a true age of ~1 day, not −1460; without the wrap the (n0−NA) term
	// injects thousands of spurious orbital periods (the ΔṪ·w² term alone diverges).
	dDay := float64(n0 - a.NA)
	if dDay > gloDaysPerCycle/2 {
		dDay -= gloDaysPerCycle
	} else if dDay < -gloDaysPerCycle/2 {
		dDay += gloDaysPerCycle
	}
	tStar := ti - a.Tlambda + 86400*dDay
	w := math.Floor(tStar / Tdr)
	dtNode := Tdr*w + a.DeltaTdot*w*w
	tLambdaK := math.Mod(a.Tlambda+dtNode, 86400)
	omegaDrift := 1.5 * almC20 * n * aeA2 * cosI / ((1 - e*e) * (1 - e*e))
	lambdaK := a.Lambda + (omegaDrift-almWe)*dtNode

	// Node mean anomaly at the passage: M(ν = −ω).
	h := e * math.Sin(a.Omega)
	l := e * math.Cos(a.Omega)
	e0 := 2 * math.Atan(math.Sqrt((1-e)/(1+e))*math.Tan(nu0/2))
	mNode := e0 - e*math.Sin(e0)

	// tau is the elapsed time since the k-th node passage, tStar − dtNode,
	// carrying the day count -- NOT ti − tLambdaK, which discards which calendar
	// day the passage fell on (tLambdaK is wrapped mod 86400). Whenever the
	// containing passage was on the previous day relative to ti, ti − tLambdaK
	// comes out ≈ 86400 s short (or, symmetrically, too large), corrupting the mean
	// anomaly / node longitude / perturbation arguments below by a full period's
	// worth of along-track motion. tLambdaK itself is kept only for the
	// inertialNode sidereal term S(tλk), which the ICD's own §A.3.2.2 worked
	// example defines against the wrapped time-of-day (the "subtract 86400,
	// increment the day" step is explicit there).
	tau := tStar - dtNode
	// Perturbations are evaluated at m=1 (τ=0, λ=M+ω) and m=2 (τ, λ=M+ω+nτ); the
	// applied correction is their difference (ICD §A.3.2.2 step 4).
	p1 := almPert(J, aeA2, incl, h, l, n, 0, mNode+a.Omega)
	p2 := almPert(J, aeA2, incl, h, l, n, tau, mNode+a.Omega+n*tau)
	dA := semiA * (p2.da - p1.da)
	dH := p2.dh - p1.dh
	dL := p2.dl - p1.dl
	dOmega := p2.dOmega - p1.dOmega
	dI := p2.di - p1.di
	dLambda := p2.dLambda - p1.dLambda

	// Perturbed osculating elements at ti.
	hi := h + dH
	li := l + dL
	epsI := math.Hypot(hi, li)
	if epsI >= 1 {
		return gnss.ECEF{}, gnss.ECEF{}, errAlmEcc
	}
	omegaI := 0.0
	if epsI != 0 {
		omegaI = math.Atan2(hi, li)
	}
	aI := semiA + dA
	iI := incl + dI
	sinII, cosII := math.Sincos(iI)

	var omegaBase float64
	switch node {
	case inertialNode:
		s := s0 + almWe*(tLambdaK-10800) // ICD S(tλk); 10800 s = the 3 h MT→GMT offset
		omegaBase = lambdaK + s
	default: // ecefNode
		omegaBase = lambdaK - almWe*tau // unwrapped tau, not ti-tLambdaK
	}
	omegaNode := omegaBase + dOmega
	lambdaStar := mNode + a.Omega + n*tau + dLambda // unwrapped tau, not ti-tLambdaK
	mI := lambdaStar - omegaI

	// Solve the perturbed Kepler orbit and rotate into the chosen frame.
	eaI := solveKeplerEcc(mI, epsI)
	nuI := 2 * math.Atan(math.Sqrt((1+epsI)/(1-epsI))*math.Tan(eaI/2))
	uI := nuI + omegaI
	rI := aI * (1 - epsI*math.Cos(eaI))
	vFactor := math.Sqrt(almMu/aI) / math.Sqrt(1-epsI*epsI)
	vr := vFactor * epsI * math.Sin(nuI)
	vu := vFactor * (1 + epsI*math.Cos(nuI))

	sinU, cosU := math.Sincos(uI)
	sinO, cosO := math.Sincos(omegaNode)
	pQ := cosU*cosO - sinU*sinO*cosII
	qQ := cosU*sinO + sinU*cosO*cosII
	pos = gnss.ECEF{X: rI * pQ, Y: rI * qQ, Z: rI * sinU * sinII}
	vel = gnss.ECEF{
		X: vr*pQ - vu*(sinU*cosO+cosU*sinO*cosII),
		Y: vr*qQ - vu*(sinU*sinO-cosU*cosO*cosII),
		Z: vr*sinU*sinII + vu*cosU*sinII,
	}
	if node == ecefNode {
		// for ecefNode, omegaBase = lambdaK − almWe·tau folds Earth
		// rotation directly into the node angle (dΩ/dt = −almWe), so pos(t) is
		// already the correct time-dependent ECEF position. But vel above is
		// only Rz(Ω)·d(r_orbital)/dt — the orbital velocity as seen in a frame
		// with Ω momentarily frozen — and misses the extra term from Ω itself
		// rotating: d/dt[Rz(Ω(t))]·r_orbital = (dΩ/dt)·(ẑ×r) = −almWe·(−Y,X,0).
		// Subtracting ωe×r (ωe = (0,0,almWe)) adds that missing (almWe·Y,
		// −almWe·X, 0) term, giving the true ECEF velocity (~1.9 km/s
		// correction at GLONASS altitude). inertialNode's Ω has no such t
		// dependence (it is the ICD's fixed OXaYaZa frame), so it is untouched.
		vel.X += almWe * pos.Y
		vel.Y -= almWe * pos.X
	}
	if !finiteVec(pos) || !finiteVec(vel) {
		return gnss.ECEF{}, gnss.ECEF{}, errAlmNaN
	}
	return pos, vel, nil
}

// pert holds the six orbital-element perturbations due to C20 at one evaluation
// point (ICD §A.3.2.2 formulae (1)). da is relative (δa/a); the rest are absolute.
type pert struct{ da, dh, dl, dOmega, di, dLambda float64 }

// almPert evaluates the C20 secular + periodic perturbation formulae at argument of
// latitude lambda and elapsed time tau (ICD §A.3.2.2). aeA2 = (a_e/a)², incl the
// perturbed inclination, h = e·sinω, l = e·cosω, n the mean motion.
func almPert(J, aeA2, incl, h, l, n, tau, lambda float64) pert {
	sinI, cosI := math.Sincos(incl)
	s2i := sinI * sinI
	c2i := cosI * cosI
	sinL, cosL := math.Sincos(lambda)
	sin2L, cos2L := math.Sincos(2 * lambda)
	sin3L, cos3L := math.Sincos(3 * lambda)
	sin4L, cos4L := math.Sincos(4 * lambda)
	nt := n * tau

	da := 2*J*aeA2*(1-1.5*s2i)*(l*cosL+h*sinL) +
		J*aeA2*s2i*(0.5*h*sinL-0.5*l*cosL+cos2L+3.5*l*cos3L+3.5*h*sin3L)

	dh := J*aeA2*(1-1.5*s2i)*(l*nt+sinL+1.5*l*sin2L-1.5*h*cos2L) -
		0.25*J*aeA2*s2i*(sinL-(7.0/3.0)*sin3L+5*l*sin2L-8.5*l*sin4L+8.5*h*cos4L+h*cos2L) +
		J*aeA2*c2i*(l*nt-0.5*l*sin2L)

	dl := J*aeA2*(1-1.5*s2i)*(-h*nt+cosL+1.5*l*cos2L+1.5*h*sin2L) -
		0.25*J*aeA2*s2i*(-cosL-(7.0/3.0)*cos3L+5*h*sin2L-8.5*l*cos4L-8.5*h*sin4L+l*cos2L) +
		J*aeA2*c2i*(-h*nt+0.5*h*sin2L)

	dOmega := -J * aeA2 * cosI * (nt + 3.5*l*sinL - 2.5*h*cosL - 0.5*sin2L - (7.0/6.0)*l*sin3L + (7.0/6.0)*h*cos3L)

	di := 0.5 * J * aeA2 * sinI * cosI * (l*cosL - h*sinL + cos2L + (7.0/3.0)*l*cos3L + (7.0/3.0)*h*sin3L)

	// The third δλ group carries cos²i (so the secular along-track factor is the
	// physically-correct 3 − 4·sin²i = 2(1−1.5·sin²i) + cos²i); this reproduces the
	// ICD §A.3.2.3 example to metres, confirming the reading.
	dLambda := 2*J*aeA2*(1-1.5*s2i)*(nt+1.75*l*sinL-1.75*h*cosL) +
		3*J*aeA2*s2i*(-(7.0/24.0)*h*cosL-(7.0/24.0)*l*sinL-(49.0/72.0)*h*cos3L+(49.0/72.0)*l*sin3L+0.25*sin2L) +
		J*aeA2*c2i*(nt+2.5*l*sinL-2.5*h*cosL-0.5*sin2L-(7.0/6.0)*l*sin3L+(7.0/6.0)*h*cos3L)

	return pert{da: da, dh: dh, dl: dl, dOmega: dOmega, di: di, dLambda: dLambda}
}

// solveKeplerEcc solves E = M + e·sin E by Newton iteration to the ICD's 1e-8 rad.
func solveKeplerEcc(m, e float64) float64 {
	ea := m
	for i := 0; i < 20; i++ {
		next := m + e*math.Sin(ea)
		if math.Abs(next-ea) < 1e-12 {
			return next
		}
		ea = next
	}
	return ea
}

func finiteVec(v gnss.ECEF) bool {
	return !math.IsNaN(v.X) && !math.IsInf(v.X, 0) &&
		!math.IsNaN(v.Y) && !math.IsInf(v.Y, 0) &&
		!math.IsNaN(v.Z) && !math.IsInf(v.Z, 0)
}
