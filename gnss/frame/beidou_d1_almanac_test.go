package frame

import (
	"errors"
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/physconst"
)

// d1AlmanacWords builds a BCH-valid D1 almanac page (BDS-SIS-B1I-3.0 Figure
// 5-11-1, information-stream offsets) for subframe fraID page pnum, with
// last2 in the trailing AmEpID/AmID field.
func d1AlmanacWords(fraID, pnum int, toaRaw, last2 uint64, sqrtARaw uint64) []uint32 {
	info := make([]byte, 28)
	setBits(info, 15, 3, uint64(fraID))
	setBits(info, 39, 7, uint64(pnum))
	setBits(info, 46, 24, sqrtARaw)
	setBits(info, 70, 11, 5)        // a1
	setBits(info, 81, 11, 0x7F9)    // a0 = −7
	setBits(info, 92, 24, 0x123456) // Ω0
	setBits(info, 116, 17, 4000)    // e
	setBits(info, 133, 16, 0xFFF0)  // δi = −16
	setBits(info, 149, 8, toaRaw)
	setBits(info, 157, 17, 0x1FFF0) // Ω̇ = −16
	setBits(info, 174, 24, 0x400000)
	setBits(info, 198, 24, 0x0ABCDE)
	setBits(info, 222, 2, last2)
	r := NewBitReaderN(info, 224)
	words := make([]uint32, 10)
	v, _ := r.Bits(0, 26)
	words[0] = uint32(v) << 4
	for i := 1; i < 10; i++ {
		v, _ = r.Bits(26+(i-1)*22, 22)
		words[i] = uint32(v) << 8
	}
	StampBeiDouD1BCH(words)
	return words
}

// d1AlmanacRefWords builds subframe 5 page 8, which anchors the almanac set's
// toa to an eight-bit BDT week number (Figure 5-11-3, §5.2.4.16).
func d1AlmanacRefWords(wn, toaRaw uint64) []uint32 {
	info := make([]byte, 28)
	setBits(info, 15, 3, 5)
	setBits(info, 39, 7, 8)
	setBits(info, 145, 8, wn)
	setBits(info, 153, 8, toaRaw)
	r := NewBitReaderN(info, 224)
	words := make([]uint32, 10)
	v, _ := r.Bits(0, 26)
	words[0] = uint32(v) << 4
	for i := 1; i < 10; i++ {
		v, _ = r.Bits(26+(i-1)*22, 22)
		words[i] = uint32(v) << 8
	}
	StampBeiDouD1BCH(words)
	return words
}

// TestBeiDouD1AlmanacPages: subframe 4 page n describes SV n and subframe 5
// pages 1–6 SVs 25–30 (§5.2.4.13), at the Table 5-14 scalings, each carrying
// AmEpID; a GEO SV ID references i0 = 0 and every almanac skips the GEO
// ephemeris rotation (Table 5-15).
func TestBeiDouD1AlmanacPages(t *testing.T) {
	semi := physconst.Pi
	sf, err := DecodeBeiDouD1(d1AlmanacWords(4, 12, 100, 3, 10818560))
	if err != nil {
		t.Fatal(err)
	}
	a := sf.Almanac
	if sf.Pnum != 12 || !sf.HasAmEpID || sf.AmEpID != 3 || a == nil || a.SVID != 12 || a.Expanded {
		t.Fatalf("page = %+v, almanac %+v", sf, a)
	}
	if a.Toa != 100*4096 || a.SqrtA != 10818560.0/2048 || a.Ecc != 4000.0/(1<<21) ||
		a.DeltaI != -16.0/(1<<19)*semi || a.OmegaDot != -16.0/float64(uint64(1)<<38)*semi ||
		a.Omega0 != float64(0x123456)/(1<<23)*semi || a.Omega != float64(0x400000)/(1<<23)*semi ||
		a.M0 != float64(0x0ABCDE)/(1<<23)*semi || a.A0 != -7.0/(1<<20) || a.A1 != 5.0/float64(uint64(1)<<38) {
		t.Fatalf("almanac fields = %+v", a)
	}
	eph, err := a.Ephemeris()
	if err != nil || !eph.Almanac || eph.SVID != 12 || math.Abs(eph.I0-(0.30*semi+a.DeltaI)) > 1e-15 {
		t.Fatalf("ephemeris %+v, %v", eph, err)
	}
	sf, _ = DecodeBeiDouD1(d1AlmanacWords(5, 3, 100, 3, 10818560))
	if sf.Almanac == nil || sf.Almanac.SVID != 27 {
		t.Fatalf("subframe 5 page 3 = %+v", sf.Almanac)
	}
	sf, _ = DecodeBeiDouD1(d1AlmanacWords(4, 5, 100, 3, 13298000))
	if geo, err := sf.Almanac.Ephemeris(); err != nil || geo.I0 != sf.Almanac.DeltaI || geo.ID != gnss.BeiDou {
		t.Fatalf("GEO C05 i0 not 0: %+v, %v", geo, err)
	}
	if sf, err := DecodeBeiDouD1(d1AlmanacWords(4, 9, 0, 3, 0)); err != nil || sf.Almanac != nil || !sf.HasAmEpID {
		t.Fatalf("unused all-zero entry: %+v, %v", sf, err)
	}
	if _, err := DecodeBeiDouD1(d1AlmanacWords(4, 12, 148, 3, 10818560)); !errors.Is(err, errBadEpoch) {
		t.Fatalf("toa past 602112 s: %v", err)
	}
}

// TestBeiDouD1ExpandedPages: subframe 5 pages 11–23 are almanacs only when
// the satellite's AmEpID is "11", and AmID selects SV IDs 31–43, 44–56 or
// 57–63 (Table 5-13). Reserved pages decode without error even with an
// out-of-range toa, and never resolve.
func TestBeiDouD1ExpandedPages(t *testing.T) {
	for _, c := range []struct {
		pnum   int
		amID   uint64
		amEpID int
		svid   int
	}{
		{11, 1, 3, 31}, {23, 1, 3, 43}, {11, 2, 3, 44}, {23, 2, 3, 56}, {11, 3, 3, 57}, {17, 3, 3, 63},
		{18, 3, 3, 0}, {12, 0, 3, 0}, {12, 1, 0, 0}, {12, 1, 2, 0},
	} {
		sf, err := DecodeBeiDouD1(d1AlmanacWords(5, c.pnum, 100, c.amID, 10818560))
		if err != nil {
			t.Fatal(err)
		}
		if sf.HasAmEpID || sf.Almanac == nil || !sf.Almanac.Expanded {
			t.Fatalf("expanded page %d = %+v", c.pnum, sf)
		}
		a := *sf.Almanac
		if ok := a.ResolveExpanded(c.amEpID); ok != (c.svid != 0) || a.SVID != c.svid {
			t.Fatalf("page %d AmID %d AmEpID %d → SV %d (ok %v), want %d", c.pnum, c.amID, c.amEpID, a.SVID, ok, c.svid)
		}
	}
	reserved, err := DecodeBeiDouD1(d1AlmanacWords(5, 15, 200, 1, 10818560))
	if err != nil {
		t.Fatalf("reserved expanded page failed the frame: %v", err)
	}
	if a := *reserved.Almanac; a.ResolveExpanded(3) {
		t.Fatal("expanded page with toa past 602112 s resolved")
	}
	if _, err := (&BeiDouD1Almanac{Expanded: true}).Ephemeris(); !errors.Is(err, errBadAlmanac) {
		t.Fatal("unresolved expanded almanac converted")
	}
}

func TestBeiDouD1AlmanacReferencePage(t *testing.T) {
	sf, err := DecodeBeiDouD1(d1AlmanacRefWords(0xA5, 100))
	if err != nil {
		t.Fatal(err)
	}
	if sf.FraID != 5 || sf.Pnum != 8 || !sf.HasAlmanacRef || sf.AlmanacWN != 0xA5 || sf.AlmanacToa != 100*4096 || sf.Almanac != nil {
		t.Fatalf("almanac reference page = %+v", sf)
	}
	if _, err := DecodeBeiDouD1(d1AlmanacRefWords(0xA5, 148)); !errors.Is(err, errBadEpoch) {
		t.Fatalf("toa past 602112 s: %v", err)
	}
}
