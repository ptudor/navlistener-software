package state

import (
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
