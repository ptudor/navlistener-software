package frame

import (
	"errors"
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/physconst"
)

// galAlmanacSat is one satellite's raw almanac fields (GAL-OS-SIS-ICD-2.2 Table 86).
type galAlmanacSat struct {
	svid, dSqrtA, ecc, omega, di, omega0, omegaDot, m0, af0, af1, e5b, e1b int64
}

// galAlmanacWords encodes three satellites' almanacs into word types 7–10 at
// the Table 48–51 offsets, for batch iod with reference wna/t0aRaw.
func galAlmanacWords(t *testing.T, iod, wna, t0aRaw int64, s [3]galAlmanacSat) map[int]*GalileoINAV {
	t.Helper()
	set := func(c []byte, off int, v int64, n int) { setContentBits(c, off, uint64(v), n) }
	orbit := func(c []byte, off int, a galAlmanacSat) {
		set(c, off, a.svid, 6)
		set(c, off+6, a.dSqrtA, 13)
		set(c, off+19, a.ecc, 11)
		set(c, off+30, a.omega, 16)
		set(c, off+46, a.di, 11)
	}
	clk := func(c []byte, off int, a galAlmanacSat) {
		set(c, off, a.af0, 16)
		set(c, off+16, a.af1, 13)
		set(c, off+29, a.e5b, 2)
		set(c, off+31, a.e1b, 2)
	}
	words := map[int]*GalileoINAV{}
	for wt := 7; wt <= 10; wt++ {
		c := make([]byte, 16)
		set(c, 0, int64(wt), 6)
		set(c, 6, iod, 4)
		switch wt {
		case 7:
			set(c, 10, wna, 2)
			set(c, 12, t0aRaw, 10)
			orbit(c, 22, s[0])
			set(c, 79, s[0].omega0, 16)
			set(c, 95, s[0].omegaDot, 11)
			set(c, 106, s[0].m0, 16)
		case 8:
			clk(c, 10, s[0])
			orbit(c, 43, s[1])
			set(c, 100, s[1].omega0, 16)
			set(c, 116, s[1].omegaDot, 11)
		case 9:
			set(c, 10, wna, 2)
			set(c, 12, t0aRaw, 10)
			set(c, 22, s[1].m0, 16)
			clk(c, 38, s[1])
			orbit(c, 71, s[2])
		case 10:
			set(c, 10, s[2].omega0, 16)
			set(c, 26, s[2].omegaDot, 11)
			set(c, 37, s[2].m0, 16)
			clk(c, 53, s[2])
		}
		w, err := DecodeGalileoINAV(buildGalileoINAVWords(c))
		if err != nil {
			t.Fatalf("word type %d: %v", wt, err)
		}
		words[wt] = w
	}
	return words
}

var galTestSats = [3]galAlmanacSat{
	{svid: 11, dSqrtA: -120, ecc: 9, omega: 12000, di: -40, omega0: -21000, omegaDot: -300, m0: 17000, af0: -900, af1: 50, e5b: 0, e1b: 1},
	{svid: 12, dSqrtA: 75, ecc: 17, omega: -8000, di: 33, omega0: 9000, omegaDot: -290, m0: -26000, af0: 700, af1: -20, e5b: 2, e1b: 0},
	{svid: 13, dSqrtA: 4, ecc: 3, omega: 30000, di: 0, omega0: 31000, omegaDot: -310, m0: 5, af0: 1, af1: 4000, e5b: 3, e1b: 3},
}

// checkGalAlmanac compares a completed almanac with the raw fields it encodes.
func checkGalAlmanac(t *testing.T, got GalileoAlmanac, want galAlmanacSat, t0a float64) {
	t.Helper()
	semi := physconst.Pi
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-12*math.Max(1, math.Abs(b)) }
	if got.SVID != int(want.svid) || got.IODa != 5 || got.WNa != 2 || got.T0a != t0a ||
		!near(got.SqrtA, math.Sqrt(29600000)+float64(want.dSqrtA)/512) ||
		!near(got.Ecc, float64(want.ecc)/65536) ||
		!near(got.I0, (56.0/180+float64(want.di)/16384)*semi) ||
		!near(got.Omega, float64(want.omega)/32768*semi) ||
		!near(got.Omega0, float64(want.omega0)/32768*semi) ||
		!near(got.OmegaDot, float64(want.omegaDot)/float64(uint64(1)<<33)*semi) ||
		!near(got.M0, float64(want.m0)/32768*semi) ||
		!near(got.Af0, float64(want.af0)/(1<<19)) ||
		!near(got.Af1, float64(want.af1)/float64(uint64(1)<<38)) ||
		got.E5bSHS != int(want.e5b) || got.E1BSHS != int(want.e1b) {
		t.Fatalf("almanac E%02d = %+v", want.svid, got)
	}
}

// TestGalileoAlmanacJoinsConsecutiveWords: each satellite's almanac is split
// differently across word types 7–10 (Tables 48–51), so every field must come
// from the right half: SVID2's M0 rides word 9, SVID3's Ω0, Ω̇ and M0 ride
// word 10, and t0a comes from word 7 or 9.
func TestGalileoAlmanacJoinsConsecutiveWords(t *testing.T) {
	const t0aRaw = 481 // 288 600 s
	w := galAlmanacWords(t, 5, 2, t0aRaw, galTestSats)
	if w[7].Almanac.HeadSVID != 11 || w[8].Almanac.HeadSVID != 12 || w[9].Almanac.HeadSVID != 13 || w[10].Almanac.HeadSVID != 0 {
		t.Fatal("head SVIDs mis-read")
	}
	for i, pair := range [][2]int{{7, 8}, {8, 9}, {9, 10}} {
		a, err := CompleteGalileoAlmanac(w[pair[0]], w[pair[1]])
		if err != nil {
			t.Fatal(err)
		}
		checkGalAlmanac(t, a, galTestSats[i], t0aRaw*600)
		eph := a.Ephemeris()
		if eph.ID != gnss.Galileo || eph.Toe != a.T0a || eph.SVID != a.SVID || eph.DeltaN != 0 || eph.Cuc != 0 {
			t.Fatalf("ephemeris %+v", eph)
		}
	}
}

// TestGalileoAlmanacRefusesMismatchedWords: only consecutive word types of one
// batch join; unused entries (SVID 0) and reserved SVIDs are refused; t0a past
// the week is rejected at decode.
func TestGalileoAlmanacRefusesMismatchedWords(t *testing.T) {
	w := galAlmanacWords(t, 5, 2, 481, galTestSats)
	if _, err := CompleteGalileoAlmanac(w[7], w[9]); !errors.Is(err, ErrWrongMsgType) {
		t.Fatalf("7+9: %v", err)
	}
	if _, err := CompleteGalileoAlmanac(w[8], w[7]); !errors.Is(err, ErrWrongMsgType) {
		t.Fatalf("8+7: %v", err)
	}
	other := galAlmanacWords(t, 6, 2, 481, galTestSats)
	if _, err := CompleteGalileoAlmanac(w[7], other[8]); !errors.Is(err, errIODMismatch) {
		t.Fatalf("IODa 5+6: %v", err)
	}
	unused, reserved := galTestSats, galTestSats
	unused[0].svid, reserved[0].svid = 0, 40
	if _, err := CompleteGalileoAlmanac(galAlmanacWords(t, 5, 2, 481, unused)[7], w[8]); !errors.Is(err, errUnusedAlmanac) {
		t.Fatalf("SVID 0: %v", err)
	}
	if _, err := CompleteGalileoAlmanac(galAlmanacWords(t, 5, 2, 481, reserved)[7], w[8]); !errors.Is(err, errBadAlmanac) {
		t.Fatalf("SVID 40: %v", err)
	}
	if _, err := CompleteGalileoAlmanac(w[5], w[8]); !errors.Is(err, ErrShortFrame) {
		t.Fatalf("missing word: %v", err)
	}
	c := make([]byte, 16)
	setContentBits(c, 0, 7, 6)
	setContentBits(c, 12, 1008, 10) // 604 800 s
	if _, err := DecodeGalileoINAV(buildGalileoINAVWords(c)); !errors.Is(err, errBadEpoch) {
		t.Fatalf("t0a at the week end: %v", err)
	}
}

// TestGalileoWord10KeepsGGTOWithAlmanac: word 10 now also carries SVID3's
// almanac tail, and its GST-GPS parameters still decode alongside it.
func TestGalileoWord10KeepsGGTOWithAlmanac(t *testing.T) {
	w := galAlmanacWords(t, 5, 2, 481, galTestSats)[10]
	if !w.HasGGTO || w.Almanac == nil || w.Almanac.HasRef {
		t.Fatalf("word 10 = %+v", w)
	}
}
