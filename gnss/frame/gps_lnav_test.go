package frame

import (
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

func propagateAt(e kepler.Ephemeris, tow float64) (gnss.ECEF, error) {
	return kepler.Propagate(e, tow)
}

// setField writes an n-bit value (masked as two's complement for negatives) at
// ICD word w / data bit a into a 30-byte (240-bit) subframe data buffer, MSB-first.
func setField(buf []byte, w, a, n int, val int64) {
	start := field(w, a)
	u := uint64(val) & ((1 << uint(n)) - 1)
	for i := 0; i < n; i++ {
		if u&(1<<uint(n-1-i)) != 0 {
			pos := start + i
			buf[pos>>3] |= 1 << uint(7-(pos&7))
		}
	}
}

// setSplit32 writes a 32-bit signed value split as 8 MSB (word wHi, bit aHi) then
// 24 LSB (word wLo, bit 1) — the LNAV encoding for M0, Ω0, i0, ω, e, √A.
func setSplit32(buf []byte, wHi, aHi, wLo int, val int64) {
	u := uint64(uint32(val)) // low 32 bits, two's complement
	setField(buf, wHi, aHi, 8, int64(u>>24))
	setField(buf, wLo, 1, 24, int64(u&0xFFFFFF))
}

// packLNAV turns a 240-bit data buffer (the intended, normalized data) into ten
// parity-bearing 30-bit words as they are actually transmitted: when the previous
// word's D30* is 1 the data bits are sent complemented (the SV inverts them so the
// receiver's D30* un-complement recovers the intent), and the six parity bits are
// computed by validParity (the GPSParity oracle) over the transmitted bits.
func packLNAV(t *testing.T, buf []byte) []uint32 {
	t.Helper()
	r := NewBitReaderN(buf, 240)
	words := make([]uint32, 10)
	var d29, d30 uint32
	for i := 0; i < 10; i++ {
		want, _ := r.Bits(i*24, 24)
		stored := uint32(want)
		if d30 == 1 {
			stored = ^stored & 0xFFFFFF // transmitted complemented; decode recovers want
		}
		p := validParity(t, stored, d29, d30)
		word := (stored << 6) | p
		words[i] = word
		d29 = (word >> 1) & 1
		d30 = word & 1
	}
	return words
}

func TestGPSLNAVSubframe2RoundTrip(t *testing.T) {
	const m0raw, eccRaw, sqrtARaw = 205075516, 42949673, 2702199603
	buf := buildSf2(t)

	sf, err := DecodeGPSLNAV(packLNAV(t, buf))
	if err != nil {
		t.Fatal(err)
	}
	if sf.SubframeID != 2 {
		t.Fatalf("subframe ID = %d, want 2", sf.SubframeID)
	}
	if sf.IODE != 85 {
		t.Errorf("IODE = %d, want 85", sf.IODE)
	}
	if math.Abs(sf.Toe-432000) > 1e-9 {
		t.Errorf("toe = %v, want 432000", sf.Toe)
	}
	if math.Abs(sf.eph.SqrtA-float64(sqrtARaw)*p2m19) > 1e-9 {
		t.Errorf("√A = %v, want %v", sf.eph.SqrtA, float64(sqrtARaw)*p2m19)
	}
	if math.Abs(sf.eph.Ecc-float64(eccRaw)*p2m33) > 1e-12 {
		t.Errorf("e = %v", sf.eph.Ecc)
	}
	wantM0 := float64(m0raw) * p2m31 * physconst.Pi
	if math.Abs(sf.eph.M0-wantM0) > 1e-9 {
		t.Errorf("M0 = %v, want %v", sf.eph.M0, wantM0)
	}
	if sf.eph.Crs > 0 {
		t.Errorf("Crs = %v, want negative", sf.eph.Crs)
	}
}

func TestGPSLNAVFullAssembleAndPropagate(t *testing.T) {
	sf1 := decodeBuf(t, buildSf1(t))
	sf2 := decodeBuf(t, buildSf2(t))
	sf3 := decodeBuf(t, buildSf3(t))

	eph, clk, err := AssembleGPS(gnss.GPS, 5, sf1, sf2, sf3)
	if err != nil {
		t.Fatal(err)
	}
	if clk.Af0 == 0 {
		t.Error("clock af0 not populated")
	}
	pos, err := propagateAt(eph, 432000)
	if err != nil {
		t.Fatal(err)
	}
	if r := pos.Norm(); r < 26.0e6 || r > 27.2e6 {
		t.Errorf("assembled GPS ephemeris radius = %.0f m, want ~26560 km", r)
	}
}

func TestGPSLNAVIODEMismatch(t *testing.T) {
	sf1 := decodeBuf(t, buildSf1(t))
	sf2 := decodeBuf(t, buildSf2(t))
	sf3 := decodeBuf(t, buildSf3(t))
	sf3.IODE = 99 // break consistency
	if _, _, err := AssembleGPS(gnss.GPS, 5, sf1, sf2, sf3); err == nil {
		t.Error("expected IODE/IODC mismatch error")
	}
}

func TestGPSLNAVShortFrame(t *testing.T) {
	if _, err := DecodeGPSLNAV([]uint32{1, 2, 3}); err != ErrShortFrame {
		t.Errorf("err = %v, want ErrShortFrame", err)
	}
}

// --- builders for the full-ephemeris test ---

func buildSf1(t *testing.T) []byte {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 1) // subframe 1
	setField(buf, 3, 1, 10, 2200)
	setField(buf, 3, 13, 4, 4)       // URA
	setField(buf, 3, 23, 2, 0)       // IODC hi
	setField(buf, 8, 1, 8, 85)       // IODC lo = 85
	setField(buf, 8, 9, 16, 27000)   // toc
	setField(buf, 7, 17, 8, 11)      // TGD
	setField(buf, 9, 1, 8, 0)        // af2
	setField(buf, 9, 9, 16, 88)      // af1
	setField(buf, 10, 1, 22, 214748) // af0
	return buf
}

func buildSf2(t *testing.T) []byte {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 2)
	setField(buf, 3, 1, 8, 85)            // IODE
	setField(buf, 3, 9, 16, -640)         // Crs
	setField(buf, 4, 1, 16, 12596)        // Δn
	setSplit32(buf, 4, 17, 5, 205075516)  // M0
	setField(buf, 6, 1, 16, 537)          // Cuc
	setSplit32(buf, 6, 17, 7, 42949673)   // e
	setField(buf, 8, 1, 16, 4295)         // Cus
	setSplit32(buf, 8, 17, 9, 2702199603) // √A
	setField(buf, 10, 1, 16, 27000)       // toe
	return buf
}

func buildSf3(t *testing.T) []byte {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 3)
	setField(buf, 3, 1, 16, 54)           // Cic
	setSplit32(buf, 3, 17, 4, -341830285) // Ω0 ≈ −0.5 rad
	setField(buf, 5, 1, 16, -107)         // Cis
	setSplit32(buf, 5, 17, 6, 656335797)  // i0 ≈ 0.96 rad
	setField(buf, 7, 1, 16, 8000)         // Crc
	setSplit32(buf, 7, 17, 8, 478536844)  // ω ≈ 0.7 rad
	setField(buf, 9, 1, 24, -22679)       // Ωdot
	setField(buf, 10, 1, 8, 85)           // IODE
	setField(buf, 10, 9, 14, 279)         // IDOT
	return buf
}

func decodeBuf(t *testing.T, buf []byte) *GPSSubframe {
	t.Helper()
	sf, err := DecodeGPSLNAV(packLNAV(t, buf))
	if err != nil {
		t.Fatal(err)
	}
	return sf
}
