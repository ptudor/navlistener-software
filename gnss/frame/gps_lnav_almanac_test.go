package frame

import (
	"errors"
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// Broadcast ephemerides from gnss/testdata/BRDC00WRD_R_20240100000_truth.rnx,
// whose propagation the root package checks against ESA SP3 orbits: G05 at
// toe 302400 and J02 (QZSS SV ID 2, a QZO satellite) at toe 291600.
var (
	g05Broadcast = kepler.Ephemeris{
		ID: gnss.GPS, SVID: 5, SqrtA: 5.153794351578e+03, Ecc: 5.774090415798e-03,
		M0: 3.070734965626e+00, DeltaN: 4.180888436499e-09, I0: 9.694621079767e-01,
		IDot: -1.714357124141e-10, Omega0: -7.568817821586e-01, OmegaDot: -7.893185925733e-09,
		Omega: 1.235315548011e+00, Cuc: -9.324401617050e-06, Cus: 5.723908543587e-06,
		Crc: 2.741250000000e+02, Crs: -1.787500000000e+02, Cic: 5.587935447693e-08,
		Cis: -5.029141902924e-08, Toe: 302400,
	}
	j02Broadcast = kepler.Ephemeris{
		ID: gnss.QZSS, SVID: 2, SqrtA: 6.493249412537e+03, Ecc: 7.515605562367e-02,
		M0: -2.440045118725e+00, DeltaN: 2.760829285336e-09, I0: 7.127244484284e-01,
		IDot: -1.990797210409e-09, Omega0: 2.619689100635e+00, OmegaDot: -2.436530062686e-09,
		Omega: -1.577446905990e+00, Cuc: 8.817762136459e-06, Cus: 2.485513687134e-05,
		Crc: -6.311875000000e+02, Crs: 3.414375000000e+02, Cic: 3.233551979065e-06,
		Cis: 6.072223186493e-07, Toe: 291600,
	}
)

// almanacRaw holds the integer field values of one almanac page.
type almanacRaw struct {
	dataID, svID, ecc, toa, deltaI, omegaDot, health, sqrtA, omega0, omega, m0, af0, af1 int64
}

// almanacPage writes raw into an LNAV subframe sf page (IS-GPS-200N Figure 20-1
// sheet 4) and returns the delivered words.
func almanacPage(t *testing.T, sf int, raw almanacRaw) []uint32 {
	t.Helper()
	buf := make([]byte, 30)
	setField(buf, 2, 1, 17, 1000)
	setField(buf, 2, 20, 3, int64(sf))
	setField(buf, 3, 1, 2, raw.dataID)
	setField(buf, 3, 3, 6, raw.svID)
	setField(buf, 3, 9, 16, raw.ecc)
	setField(buf, 4, 1, 8, raw.toa)
	setField(buf, 4, 9, 16, raw.deltaI)
	setField(buf, 5, 1, 16, raw.omegaDot)
	setField(buf, 5, 17, 8, raw.health)
	setField(buf, 6, 1, 24, raw.sqrtA)
	setField(buf, 7, 1, 24, raw.omega0)
	setField(buf, 8, 1, 24, raw.omega)
	setField(buf, 9, 1, 24, raw.m0)
	af0 := uint64(raw.af0) & 0x7FF
	setField(buf, 10, 1, 8, int64(af0>>3))
	setField(buf, 10, 9, 11, raw.af1)
	setField(buf, 10, 20, 3, int64(af0&7))
	return packLNAV(t, buf)
}

// almanacFromBroadcast reduces a broadcast ephemeris to almanac precision at
// toa (seconds of week, a multiple of 2^12), moving the epoch-dependent
// elements from toe to toa first. eRef and iRef (semicircles) are the
// constellation's almanac references.
func almanacFromBroadcast(t *testing.T, eph kepler.Ephemeris, dataID, svID int64, toa, eRef, iRef float64) almanacRaw {
	t.Helper()
	p, ok := physconst.For(eph.ID)
	if !ok {
		t.Fatal("no constants")
	}
	semi := physconst.Pi
	a := eph.SqrtA * eph.SqrtA
	dt := toa - eph.Toe
	m0 := math.Remainder(eph.M0+(math.Sqrt(p.Mu/(a*a*a))+eph.DeltaN)*dt, 2*math.Pi)
	omega0 := math.Remainder(eph.Omega0+eph.OmegaDot*dt, 2*math.Pi)
	i := eph.I0 + eph.IDot*dt
	round := func(v, lsb float64) int64 { return int64(math.Round(v / lsb)) }
	return almanacRaw{
		dataID: dataID, svID: svID,
		ecc:      round(eph.Ecc-eRef, 1.0/(1<<21)),
		toa:      round(toa, 1<<12),
		deltaI:   round(i/semi-iRef, 1.0/(1<<19)),
		omegaDot: round(eph.OmegaDot/semi, 1.0/float64(uint64(1)<<38)),
		sqrtA:    round(eph.SqrtA, 1.0/(1<<11)),
		omega0:   round(omega0/semi, 1.0/(1<<23)),
		omega:    round(eph.Omega/semi, 1.0/(1<<23)),
		m0:       round(m0/semi, 1.0/(1<<23)),
	}
}

// almanacMiss reports the largest distance between the almanac and broadcast
// positions over toa ± span.
func almanacMiss(t *testing.T, alm, eph kepler.Ephemeris, toa, span float64) float64 {
	t.Helper()
	worst := 0.0
	for dt := -span; dt <= span; dt += 600 {
		got, err := kepler.Propagate(alm, toa+dt)
		if err != nil {
			t.Fatal(err)
		}
		want, err := kepler.Propagate(eph, toa+dt)
		if err != nil {
			t.Fatal(err)
		}
		worst = math.Max(worst, got.Sub(want).Norm())
	}
	return worst
}

// TestLNAVAlmanacTracksBroadcastOrbit: a GPS almanac page built from a real
// broadcast orbit decodes to the Table 20-VI elements, and propagating them
// with i0 = 0.30 semicircles + δi and every omitted term zero stays within 3 km
// of the full broadcast orbit for two hours either side of toa. That is the
// size of the harmonic and Δn terms the almanac omits.
func TestLNAVAlmanacTracksBroadcastOrbit(t *testing.T) {
	const toa = 303104 // 74 × 2^12, 704 s after toe
	raw := almanacFromBroadcast(t, g05Broadcast, 1, 5, toa, 0, gpsAlmanacI0)
	raw.health, raw.af0, raw.af1 = 0x25, -700, 300
	sf, err := DecodeGPSLNAV(almanacPage(t, 5, raw))
	if err != nil {
		t.Fatal(err)
	}
	a := sf.Almanac
	if a == nil || a.DataID != 1 || a.SVID != 5 || a.Toa != toa || a.Health != 0x25 {
		t.Fatalf("almanac = %+v", a)
	}
	if a.Af0 != -700.0/(1<<20) || a.Af1 != 300.0/float64(uint64(1)<<38) {
		t.Fatalf("af0 = %v, af1 = %v: word 10 split mis-read", a.Af0, a.Af1)
	}
	alm, err := a.Ephemeris(gnss.GPS)
	if err != nil {
		t.Fatal(err)
	}
	if alm.Toe != toa || alm.DeltaN != 0 || alm.Cus != 0 || alm.IDot != 0 {
		t.Fatalf("almanac ephemeris carries terms the almanac lacks: %+v", alm)
	}
	if miss := almanacMiss(t, alm, g05Broadcast, toa, 7200); miss > 3000 {
		t.Fatalf("almanac %.0f m from broadcast orbit", miss)
	}
}

// TestQZSAlmanacUsesOrbitTypeReference: QZS almanacs carry e and δi relative
// to eREF 0.06 and iREF 0.25 semicircles for a QZO satellite (QZSS-PNT-006
// Table 5.7.1-3). Applying them places J02 within 5 km of its broadcast orbit;
// GPS references would put it on a different orbit entirely.
func TestQZSAlmanacUsesOrbitTypeReference(t *testing.T) {
	const toa = 290816 // 71 × 2^12
	raw := almanacFromBroadcast(t, j02Broadcast, 3, 2, toa, 0.06, 0.25)
	sf, err := DecodeGPSLNAV(almanacPage(t, 4, raw))
	if err != nil {
		t.Fatal(err)
	}
	if sf.Almanac == nil || sf.Almanac.DataID != 3 || sf.Almanac.SVID != 2 {
		t.Fatalf("almanac = %+v", sf.Almanac)
	}
	alm, err := sf.Almanac.Ephemeris(gnss.QZSS)
	if err != nil {
		t.Fatal(err)
	}
	if miss := almanacMiss(t, alm, j02Broadcast, toa, 3600); miss > 5000 {
		t.Fatalf("QZS almanac %.0f m from broadcast orbit", miss)
	}
	if _, err := sf.Almanac.Ephemeris(gnss.GPS); !errors.Is(err, errBadAlmanac) {
		t.Fatal("a QZS almanac (data ID 3) was accepted as GPS")
	}
	// A GEO SV ID uses eREF = iREF = 0.
	geo := *sf.Almanac
	geo.SVID = 7
	if eph, err := geo.Ephemeris(gnss.QZSS); err != nil || eph.Ecc != sf.Almanac.Ecc || eph.I0 != sf.Almanac.DeltaI {
		t.Fatalf("GEO reference not zero: %+v, %v", eph, err)
	}
	// SV IDs without an assigned orbit type cannot be placed.
	for _, sv := range []int{1, 6, 10} {
		unassigned := *sf.Almanac
		unassigned.SVID = sv
		if _, err := unassigned.Ephemeris(gnss.QZSS); !errors.Is(err, errBadAlmanac) {
			t.Fatalf("SV ID %d placed without an orbit type", sv)
		}
	}
}

// TestLNAVPagesWithoutAlmanac: dummy-SV pages (SV ID 0), QZSS test mode, and the
// page IDs 51..63 (health, iono/UTC, NMCT, special messages) decode without an
// almanac, as do data IDs that belong to neither constellation.
func TestLNAVPagesWithoutAlmanac(t *testing.T) {
	base := almanacFromBroadcast(t, g05Broadcast, 1, 5, 303104, 0, gpsAlmanacI0)
	for _, c := range []struct{ dataID, svID int64 }{
		{1, 0}, {3, 0}, {1, 51}, {1, 56}, {1, 63}, {3, 51}, {3, 61}, {1, 40}, {3, 11}, {0, 5}, {2, 5},
	} {
		raw := base
		raw.dataID, raw.svID = c.dataID, c.svID
		sf, err := DecodeGPSLNAV(almanacPage(t, 4, raw))
		if err != nil || sf.Almanac != nil {
			t.Fatalf("data ID %d SV ID %d: almanac %+v, err %v", c.dataID, c.svID, sf.Almanac, err)
		}
	}
}

// TestLNAVAlmanacRejectsOutOfRange: toa codes up to 1 044 480 s but the ICD
// bounds it to 602 112 (Table 20-VI), and the GPS e, √A and Ω̇ ranges reject a
// page no satellite could transmit.
func TestLNAVAlmanacRejectsOutOfRange(t *testing.T) {
	base := almanacFromBroadcast(t, g05Broadcast, 1, 5, 303104, 0, gpsAlmanacI0)
	raw := base
	raw.toa = 148
	if _, err := DecodeGPSLNAV(almanacPage(t, 5, raw)); !errors.Is(err, errBadEpoch) {
		t.Fatalf("toa past 602112 s: err %v", err)
	}
	semi := physconst.Pi
	for name, mutate := range map[string]func(*LNAVAlmanac){
		"eccentricity above 0.03": func(a *LNAVAlmanac) { a.Ecc = 0.031 },
		"√A below 2530":           func(a *LNAVAlmanac) { a.SqrtA = 2529 },
		"√A above 8192":           func(a *LNAVAlmanac) { a.SqrtA = 8193 },
		"positive Ω̇":             func(a *LNAVAlmanac) { a.OmegaDot = 1e-9 },
		"Ω̇ below range":          func(a *LNAVAlmanac) { a.OmegaDot = -1.2e-7 * semi },
		"QZS data ID":             func(a *LNAVAlmanac) { a.DataID = 3 },
	} {
		sf, err := DecodeGPSLNAV(almanacPage(t, 5, base))
		if err != nil {
			t.Fatal(err)
		}
		mutate(sf.Almanac)
		if _, err := sf.Almanac.Ephemeris(gnss.GPS); !errors.Is(err, errBadAlmanac) {
			t.Fatalf("%s accepted", name)
		}
	}
	sf, _ := DecodeGPSLNAV(almanacPage(t, 5, base))
	if _, err := sf.Almanac.Ephemeris(gnss.Galileo); !errors.Is(err, errBadAlmanac) {
		t.Fatal("LNAV almanac accepted for Galileo")
	}
}
