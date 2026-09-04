package state

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// setAbsBits sets n bits of val (right-aligned) into buf at absolute bit offset
// start, MSB-first — matching gnss/frame.BitReader's convention.
func setAbsBits(buf []byte, start, n int, val uint64) {
	for i := 0; i < n; i++ {
		bit := start + i
		b := (val >> uint(n-1-i)) & 1
		if b != 0 {
			buf[bit>>3] |= 1 << (7 - uint(bit&7))
		}
	}
}

// bdsD1Words builds the 10-word raw form DecodeBeiDouD1 expects from a 224-bit
// information-stream buffer — the inverse of beidou_d1.go's beidouInfo (top 26
// bits of word 0, top 22 bits of words 1-9).
func bdsD1Words(infoBits []byte) []uint32 {
	words := make([]uint32, 10)
	get := func(start, n int) uint32 {
		var v uint32
		for i := 0; i < n; i++ {
			bit := start + i
			b := (infoBits[bit>>3] >> uint(7-(bit&7))) & 1
			v = (v << 1) | uint32(b)
		}
		return v
	}
	words[0] = get(0, 26) << 4
	for i := 1; i < 10; i++ {
		words[i] = get(26+22*(i-1), 22) << 8
	}
	frame.StampBeiDouD1BCH(words)
	return words
}

// bdsD1Frame builds a minimal synthetic BeiDou D1 subframe (FraID + SOW, plus the
// toe split — toeMSB in subframe 2, toeLSB in subframe 3 — so a changeover
// between two coherent triples actually changes AssembleBeiDou's IOD key; other
// fields are zero) as a RawFrame ready for Store.Apply.
func bdsD1Frame(svID, fraID, sow, toe int, recv time.Time) *ingest.RawFrame {
	return bdsD1FrameHealth(svID, fraID, sow, toe, 0, recv)
}

// bdsD1FrameHealth is bdsD1Frame with an explicit SatH1 health bit (subframe 1, bit 38),
// so a test can flip health under an unchanged toe changeover key.
func bdsD1FrameHealth(svID, fraID, sow, toe, health int, recv time.Time) *ingest.RawFrame {
	buf := make([]byte, 28) // 224 bits
	setAbsBits(buf, 15, 3, uint64(fraID))
	setAbsBits(buf, 18, 8, uint64(sow>>12))
	setAbsBits(buf, 26, 12, uint64(sow&0xFFF))
	switch fraID {
	case 1:
		setAbsBits(buf, 38, 1, uint64(health&1)) // SatH1
	case 2:
		setAbsBits(buf, 222, 2, uint64(toe>>15)) // toeMSB
	case 3:
		setAbsBits(buf, 38, 15, uint64(toe&0x7FFF)) // toeLSB
	}
	return &ingest.RawFrame{
		Recv: recv, Source: "test", GnssID: gnss.BeiDou, SvID: svID, SigID: 0,
		Words: bdsD1Words(buf),
	}
}

func TestBeiDouD1BadBCHDoesNotMutateHealth(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	good := bdsD1FrameHealth(6, 1, 100, 0, 0, now)
	s.Apply(good)
	key := Key{G: gnss.BeiDou, Sv: 6, Sig: 0}
	st := s.shardFor(key).m[key]
	if st == nil || st.health != 0 {
		t.Fatalf("setup health state = %+v", st)
	}
	bad := bdsD1FrameHealth(6, 1, 100, 0, 1, now.Add(time.Second))
	bad.Words[1] ^= 1 << 29
	s.Apply(bad)
	if st.health != 0 {
		t.Errorf("BCH-failing subframe mutated health to %d", st.health)
	}
}

// TestFeedBeiDouD1KlobucharServed guards the subframe-1 broadcast iono
// coefficient set (B1I §5.2.4.7, Table 5-5 — α@98 at 2⁻³⁰/2⁻²⁷/2⁻²⁴/2⁻²⁴,
// β@130 at 2¹¹/2¹⁴/2¹⁶/2¹⁶, all 8-bit two's complement) must be folded at
// subframe-1 arrival and served raw as klob_alpha/klob_beta instead of dying
// in the frame struct. Distinct per-index values catch scale/offset swaps.
func TestFeedBeiDouD1KlobucharServed(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	sf1 := bdsD1Frame(6, 1, 100, 0, now)
	buf := make([]byte, 28)
	setAbsBits(buf, 15, 3, 1)          // FraID 1
	setAbsBits(buf, 18, 8, 100>>12)    // SOW
	setAbsBits(buf, 26, 12, 100&0xFFF) //
	setAbsBits(buf, 98, 8, (1<<8)-3)   // α0 raw −3 (×2⁻³⁰)
	setAbsBits(buf, 122, 8, 7)         // α3 raw 7 (×2⁻²⁴)
	setAbsBits(buf, 130, 8, 5)         // β0 raw 5 (×2¹¹)
	setAbsBits(buf, 154, 8, (1<<8)-2)  // β3 raw −2 (×2¹⁶)
	sf1.Words = bdsD1Words(buf)
	s.Apply(sf1)
	s.Apply(bdsD1Frame(6, 2, 106, 800, now))
	s.Apply(bdsD1Frame(6, 3, 112, 800, now))

	sv, ok := s.FeedSVs(now)["C06@0"]
	if !ok || sv.KlobAlpha == nil || sv.KlobBeta == nil {
		t.Fatalf("klob_alpha/klob_beta not served: %+v", sv)
	}
	if got, want := sv.KlobAlpha[0], -3.0/(1<<30); got != want {
		t.Errorf("α0 = %g, want %g (−3 × 2⁻³⁰)", got, want)
	}
	if got, want := sv.KlobAlpha[3], 7.0/(1<<24); got != want {
		t.Errorf("α3 = %g, want %g (7 × 2⁻²⁴)", got, want)
	}
	if got, want := sv.KlobBeta[0], 5.0*(1<<11); got != want {
		t.Errorf("β0 = %g, want %g (5 × 2¹¹)", got, want)
	}
	if got, want := sv.KlobBeta[3], -2.0*(1<<16); got != want {
		t.Errorf("β3 = %g, want %g (−2 × 2¹⁶)", got, want)
	}
	if sv.Bdgim != nil {
		t.Errorf("bdgim served on a D1 (B1I) entry: %v", sv.Bdgim)
	}
}

// TestBeiDouD1ChangeoverWithDroppedSubframe guards applyBeiDouD1 must not
// splice a stale subframe-2 with a fresh subframe-1/3 across an hourly changeover
// (e.g. a dropped subframe 2) into a garbage ephemeris. AssembleBeiDou's SOW-
// adjacency guard should make the incoherent triple a no-op — no disco computed,
// st.eph unchanged — until a fully coherent triple arrives.
func TestBeiDouD1ChangeoverWithDroppedSubframe(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)

	// First, fully coherent data set (SOW 100/106/112, toe=800).
	st.Apply(bdsD1Frame(6, 1, 100, 0, now))
	st.Apply(bdsD1Frame(6, 2, 106, 800, now))
	st.Apply(bdsD1Frame(6, 3, 112, 800, now))

	key := Key{G: gnss.BeiDou, Sv: 6, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	firstEph := sh.m[key].eph
	firstIOD := sh.m[key].iod
	haveEph := sh.m[key].haveEph
	sh.mu.Unlock()
	if !haveEph {
		t.Fatal("first coherent triple did not assemble")
	}

	// Changeover: a fresh subframe 1 (new frame) arrives, but subframe 2 is lost —
	// the cached subframe 2 is now stale (SOW 106, 3600s+ old in a real changeover;
	// here just far outside the +6 window from the new subframe 1's SOW). The new
	// subframe 3 carries a different toeLSB (a new data set), which is exactly what
	// would splice into a garbage toe if paired with the stale subframe 2's toeMSB.
	st.Apply(bdsD1Frame(6, 1, 3700, 0, now))
	// No subframe 2 arrives (dropped). A fresh subframe 3 arrives next.
	st.Apply(bdsD1Frame(6, 3, 3712, 1600, now))

	sh.mu.Lock()
	afterEph := sh.m[key].eph
	afterIOD := sh.m[key].iod
	sh.mu.Unlock()
	if afterEph != firstEph || afterIOD != firstIOD {
		t.Errorf("st.eph/iod changed from an incoherent triple (stale sf2 spliced with fresh sf1/sf3): eph %+v -> %+v, iod %d -> %d",
			firstEph, afterEph, firstIOD, afterIOD)
	}

	// The coherent triple completes once the matching subframe 2 (SOW 3706, same
	// toe=1600 data set as the fresh subframe 3) finally arrives — now the
	// changeover must take effect.
	st.Apply(bdsD1Frame(6, 2, 3706, 1600, now))
	sh.mu.Lock()
	finalIOD := sh.m[key].iod
	sh.mu.Unlock()
	if finalIOD == firstIOD {
		t.Error("coherent changeover triple did not update st.iod")
	}
}

func d1ClockFrame(sow, toc, af0, aodc int, at time.Time) *ingest.RawFrame {
	b := make([]byte, 28)
	setAbsBits(b, 15, 3, 1)
	setAbsBits(b, 18, 8, uint64(sow>>12))
	setAbsBits(b, 26, 12, uint64(sow&0xfff))
	setAbsBits(b, 39, 5, uint64(aodc))
	setAbsBits(b, 61, 17, uint64(toc))
	setAbsBits(b, 78, 10, uint64(aodc+10))
	setAbsBits(b, 162, 11, 3)
	setAbsBits(b, 173, 24, uint64(af0))
	setAbsBits(b, 197, 22, 200)
	setAbsBits(b, 219, 5, 2)
	return &ingest.RawFrame{Recv: at, RecvLocal: at, Source: "test", GnssID: gnss.BeiDou, SvID: 6, SigID: 0, Words: bdsD1Words(b)}
}

func TestBeiDouD1ClockRefreshKeepsOrbitalAgeAndAdjacency(t *testing.T) {
	for _, sow := range []int{130, 604794} {
		t.Run(fmt.Sprint(sow), func(t *testing.T) {
			s := New(1)
			t0 := time.Unix(1700000000, 0)
			key := Key{G: gnss.BeiDou, Sv: 6, Sig: 0}
			first := sow - 30
			s.Apply(d1ClockFrame(first, 100, 1000, 1, t0))
			s.Apply(bdsD1Frame(6, 2, first+6, 800, t0))
			s.Apply(bdsD1Frame(6, 3, first+12, 800, t0))
			st := s.shardFor(key).m[key]
			oldClock, oldOrbit := st.clk, st.eph
			ephAt, ephRecvAt := st.ephAt, st.ephRecvAt
			later := t0.Add(time.Minute)
			s.Apply(d1ClockFrame(sow, 101, 2000, 3, later))
			s.Apply(bdsD1Frame(6, 2, (sow+7)%604800, 800, later)) // one second outside adjacency
			s.Apply(bdsD1Frame(6, 3, (sow+12)%604800, 800, later))
			if st.clk != oldClock || st.eph != oldOrbit || st.ephAt != ephAt || st.ephRecvAt != ephRecvAt {
				t.Fatal("nonadjacent triplet refreshed a product")
			}
			s.Apply(bdsD1Frame(6, 2, (sow+6)%604800, 800, later))
			sv := s.FeedSVs(later)[key.Name()]
			if sv.Af0 == nil || *sv.Af0 != math.Ldexp(2000, -33) || st.clk.Toc != 808 || st.clk.TGD != 13e-10 {
				t.Fatalf("clock fields did not refresh coherently: %+v", st.clk)
			}
			if sv.AODC == nil || *sv.AODC != 3 || sv.AODE == nil || *sv.AODE != 2 {
				t.Fatal("clock metadata disagrees with refreshed sf1")
			}
			if st.eph != oldOrbit || st.ephAt != ephAt || st.ephRecvAt != ephRecvAt || st.orbitDiscoValid || st.timeDiscoValid {
				t.Fatal("clock-only refresh changed orbital product/age/discontinuity")
			}
		})
	}
}
