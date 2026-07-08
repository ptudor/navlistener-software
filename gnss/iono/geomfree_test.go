package iono

import (
	"math"
	"math/rand"
	"testing"
)

// A synthetic dual-frequency observation: geometry rho, slant iono i1 at f1,
// biases, and the dispersive scaling to f2. Phase sees the iono with opposite
// sign and an arbitrary per-arc ambiguity.
type synthObs struct {
	rho, i1        float64
	ambPhi1        float64
	ambPhi2        float64
	codeBias       float64 // c·(DCB_sat + DCB_rx), metres, on the pair
	f1, f2         float64
	codeNoiseSigma float64
}

func (s synthObs) sample(rng *rand.Rand) (p1, p2, phi1, phi2 float64) {
	gamma := Gamma(s.f1, s.f2)
	n1 := rng.NormFloat64() * s.codeNoiseSigma
	n2 := rng.NormFloat64() * s.codeNoiseSigma
	p1 = s.rho + s.i1 + n1
	p2 = s.rho + gamma*s.i1 + s.codeBias + n2
	phi1 = s.rho - s.i1 + s.ambPhi1
	phi2 = s.rho - gamma*s.i1 + s.ambPhi2
	return
}

func TestSlantFromCodeRecoversExactly(t *testing.T) {
	obs := synthObs{rho: 22_345_678.9, i1: 5.25, codeBias: 3.1, f1: L1Hz, f2: L2Hz}
	p1, p2, _, _ := obs.sample(rand.New(rand.NewSource(1)))
	got := SlantFromCode(p1, p2, L1Hz, L2Hz, obs.codeBias)
	if math.Abs(got-obs.i1) > 1e-9 {
		t.Fatalf("SlantFromCode = %v, want %v", got, obs.i1)
	}
}

func TestSlantFromCodeBiasSign(t *testing.T) {
	// Omitting the bias must shift the estimate by bias/(γ−1), nothing else.
	obs := synthObs{rho: 2e7, i1: 4.0, codeBias: 2.6, f1: L1Hz, f2: L2Hz}
	p1, p2, _, _ := obs.sample(rand.New(rand.NewSource(2)))
	biased := SlantFromCode(p1, p2, L1Hz, L2Hz, 0)
	want := obs.i1 + obs.codeBias/(Gamma(L1Hz, L2Hz)-1)
	if math.Abs(biased-want) > 1e-9 {
		t.Fatalf("biased slant = %v, want %v", biased, want)
	}
}

func TestArcLevelingBeatsCodeNoise(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	obs := synthObs{
		rho: 2.1e7, i1: 3.75, ambPhi1: 17.2, ambPhi2: -4.9,
		codeBias: 1.4, f1: L1Hz, f2: L2Hz, codeNoiseSigma: 0.5,
	}
	var arc Arc
	var lastPhiGF float64
	for i := 0; i < 300; i++ {
		p1, p2, phi1, phi2 := obs.sample(rng)
		pGF := p2 - p1
		phiGF := phi1 - phi2
		arc.Add(pGF, phiGF)
		lastPhiGF = phiGF
	}
	slant, ok := arc.Slant(lastPhiGF, L1Hz, L2Hz, obs.codeBias)
	if !ok {
		t.Fatal("arc not mature after 300 epochs")
	}
	// 300 epochs of 0.5 m code noise → leveled error ~ σ·√2/√300/(γ−1) ≈ 6 cm.
	if err := math.Abs(slant - obs.i1); err > 0.20 {
		t.Fatalf("leveled slant error = %.3f m, want < 0.20 m", err)
	}
	// The single-epoch code estimate is expected to be noisier than the leveled
	// arc on average; assert the leveling at least stays within the code-noise
	// envelope rather than diverging.
	if err := math.Abs(slant - obs.i1); err > 3*obs.codeNoiseSigma {
		t.Fatalf("leveled slant diverged: %.3f m", err)
	}
}

func TestArcImmatureAndReset(t *testing.T) {
	var arc Arc
	for i := 0; i < MinArc-1; i++ {
		arc.Add(10, 4)
	}
	if _, ok := arc.Slant(4, L1Hz, L2Hz, 0); ok {
		t.Fatal("arc reported mature below MinArc")
	}
	arc.Add(10, 4)
	if _, ok := arc.Slant(4, L1Hz, L2Hz, 0); !ok {
		t.Fatal("arc not mature at MinArc")
	}
	arc.Reset()
	if arc.Count() != 0 {
		t.Fatal("Reset did not clear the arc")
	}
}

func TestObliquity(t *testing.T) {
	if m := Obliquity(math.Pi / 2); math.Abs(m-1) > 1e-12 {
		t.Fatalf("zenith obliquity = %v, want 1", m)
	}
	// 5° elevation: sinχ = 6371/6721·cos5° ≈ 0.9443 → M ≈ 3.04.
	if m := Obliquity(5 * math.Pi / 180); m < 2.8 || m > 3.3 {
		t.Fatalf("5° obliquity = %v, want ≈3.0", m)
	}
	// Monotonic: lower elevation, larger factor.
	prev := Obliquity(math.Pi / 2)
	for deg := 85.0; deg >= 5; deg -= 5 {
		m := Obliquity(deg * math.Pi / 180)
		if m <= prev {
			t.Fatalf("obliquity not monotonic at %v°", deg)
		}
		prev = m
	}
}

func TestTECUConversion(t *testing.T) {
	// 1 TECU at GPS L1 is 16.2 cm (docs/MATH.md §7.4).
	if m := TECUToMetres(L1Hz); math.Abs(m-0.1624) > 0.001 {
		t.Fatalf("TECU at L1 = %v m, want ≈0.162", m)
	}
	// Round trip through VTEC at zenith.
	slant := 5 * TECUToMetres(L1Hz)
	if v := VTEC(slant, L1Hz, math.Pi/2); math.Abs(v-5) > 1e-9 {
		t.Fatalf("VTEC round trip = %v, want 5", v)
	}
}

func TestGammaAgainstScaleDelay(t *testing.T) {
	// The dispersive relation used for measurement must be the same one the
	// broadcast-model scaling uses: ScaleDelay(d, f2)/d == Gamma(L1, f2).
	d := 1.0
	if g, s := Gamma(L1Hz, L2Hz), ScaleDelay(d, L2Hz); math.Abs(g-s) > 1e-12 {
		t.Fatalf("Gamma %v != ScaleDelay ratio %v", g, s)
	}
}
