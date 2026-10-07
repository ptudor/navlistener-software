package frame

import (
	"errors"
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

// clearField zeroes an n-bit field so a test can override a value a builder set
// (setField only ORs bits in).
func clearField(buf []byte, w, a, n int) {
	start := field(w, a)
	for i := 0; i < n; i++ {
		pos := start + i
		buf[pos>>3] &^= 1 << uint(7-(pos&7))
	}
}

// packLNAV turns a 240-bit data buffer (the intended data) into ten 30-bit words
// the way u-blox delivers them: the true 24 data bits sit in bits 29..6,
// receiver-validated and already un-inverted, with the six parity bits below in
// the receiver-normalised form DecodeGPSLNAV verifies (StampGPSLNAVParity). The
// GPSParity primitive is exercised separately by TestGPSParity*.
func packLNAV(t *testing.T, buf []byte) []uint32 {
	t.Helper()
	setField(buf, 1, 1, 8, 0x8B)
	r := NewBitReaderN(buf, 240)
	words := make([]uint32, 10)
	for i := 0; i < 10; i++ {
		want, _ := r.Bits(i*24, 24)
		words[i] = uint32(want) << 6
	}
	StampGPSLNAVParity(words)
	return words
}

func TestGPSLNAVRejectsBadTLMPreamble(t *testing.T) {
	words := packLNAV(t, buildSf1(t))
	words[0] &^= uint32(0xFF) << 22
	StampGPSLNAVParity(words) // parity-valid, so the preamble check is what fires
	if _, err := DecodeGPSLNAV(words); err != ErrBadTLMPreamble {
		t.Errorf("err = %v, want ErrBadTLMPreamble", err)
	}
}

// TestGPSLNAVParityRejectsEveryBitFlip: a delivered subframe carries 60 parity bits
// the decoder now verifies through the receiver's normalisation chain, so flipping
// any one of the 300 bits — data or parity, in any word — is ErrParity, where the
// preamble/TOW/id checks alone caught only three fields.
func TestGPSLNAVParityRejectsEveryBitFlip(t *testing.T) {
	good := packLNAV(t, buildSf2(t))
	if _, err := DecodeGPSLNAV(good); err != nil {
		t.Fatalf("stamped frame: %v", err)
	}
	for w := 0; w < 10; w++ {
		for b := 0; b < 30; b++ {
			words := append([]uint32(nil), good...)
			words[w] ^= 1 << uint(b)
			if _, err := DecodeGPSLNAV(words); err != ErrParity {
				t.Fatalf("word %d bit %d flipped: err = %v, want ErrParity", w+1, b, err)
			}
		}
	}
}

// lnavRawForm builds a subframe as transmitted (IS-GPS-200N §20.3.5): each word's
// parity is computed over its source data bits and the previous word's
// transmitted D29*/D30*, and the data bits are complemented when D30* = 1. It
// returns the words and how many of them were complemented.
func lnavRawForm(data [10]uint32) (words []uint32, complemented int) {
	var d29, d30 uint32
	words = make([]uint32, 10)
	for i, d := range data {
		p := parityBits(d, d29, d30)
		tx := d
		if d30 == 1 {
			tx = ^d & 0xFFFFFF
			complemented++
		}
		words[i] = tx<<6 | p
		d29, d30 = (p>>1)&1, p&1
	}
	return words, complemented
}

// TestGPSLNAVAcceptsRawSignalForm: a source that delivers words as transmitted (a
// raw-signal path, not the fleet's receivers) verifies under GPSParity's chain, so
// the same subframe decodes in both delivery forms, and the normalised form of the
// raw frame is exactly what StampGPSLNAVParity produces. An almanac page (subframe
// 5) is used so that no decoded field depends on which words were complemented.
func TestGPSLNAVAcceptsRawSignalForm(t *testing.T) {
	var data [10]uint32
	data[0] = 0x8B << 16 // TLM preamble in data bits 1..8
	// A TLM message whose transmitted D30 is 0 keeps the HOW un-complemented; the
	// HOW's own t bits are not solved here, so later words may be complemented.
	for msg := uint32(0); msg < 1<<14; msg++ {
		data[0] = 0x8B<<16 | msg<<2
		if parityBits(data[0], 0, 0)&1 == 0 {
			break
		}
	}
	data[1] = 5 << 2 // HOW: TOW count 0, subframe id 5 in data bits 20..22
	for i := 2; i < 10; i++ {
		data[i] = 0x5A5A5A ^ uint32(i)*0x010101
	}
	raw, complemented := lnavRawForm(data)
	if complemented == 0 {
		t.Fatal("fixture: no word was complemented, so the raw form equals the normalised one")
	}
	sf, err := DecodeGPSLNAV(raw)
	if err != nil || sf.SubframeID != 5 {
		t.Fatalf("raw-form subframe: sf=%+v err=%v, want subframe 5 accepted via GPSParity's chain", sf, err)
	}
	if !lnavRawParityOK(raw) || lnavNormalizedParityOK(raw) {
		t.Fatal("raw-form frame should verify under the raw chain only")
	}

	normalised := make([]uint32, 10)
	for i, d := range data {
		normalised[i] = d << 6
	}
	StampGPSLNAVParity(normalised)
	if !lnavNormalizedParityOK(normalised) || lnavRawParityOK(normalised) {
		t.Fatal("normalised frame should verify under the normalised chain only")
	}
	if sf, err := DecodeGPSLNAV(normalised); err != nil || sf.SubframeID != 5 {
		t.Fatalf("normalised subframe: sf=%+v err=%v", sf, err)
	}
	// The receiver's normalisation of the transmitted words is the stamped form.
	for i := range raw {
		want := raw[i]
		if i > 0 && raw[i-1]&1 != 0 {
			want ^= 0x3FFFFFFF
		}
		if normalised[i] != want {
			t.Fatalf("word %d: normalised %#x, want tx XOR D30*·all-ones = %#x", i+1, normalised[i], want)
		}
	}
	// A frame verifying under neither chain is rejected.
	neither := append([]uint32(nil), normalised...)
	neither[4] ^= 0x3F
	if _, err := DecodeGPSLNAV(neither); err != ErrParity {
		t.Fatalf("frame verifying under neither chain: err = %v, want ErrParity", err)
	}
}

// TestGPSLNAVRejectsEpochOutsideWeek: toc (subframe 1) and toe (subframe 2) are
// 16-bit counts × 16 s that code up to 1 048 560 s, while IS-GPS-200N bounds both to
// 604 784; a value at or past the week is a corrupt or crafted field that EphAge
// would alias into a plausible age, so it is rejected at decode.
func TestGPSLNAVRejectsEpochOutsideWeek(t *testing.T) {
	sf2WithToe := func(raw int64) []uint32 {
		buf := buildSf2(t)
		clearField(buf, 10, 1, 16)
		setField(buf, 10, 1, 16, raw)
		return packLNAV(t, buf)
	}
	sf1WithToc := func(raw int64) []uint32 {
		buf := buildSf1(t)
		clearField(buf, 8, 9, 16)
		setField(buf, 8, 9, 16, raw)
		return packLNAV(t, buf)
	}
	if _, err := DecodeGPSLNAV(sf2WithToe(65535)); err != errBadEpoch { // 1 048 560 s
		t.Errorf("toe raw 65535: err = %v, want errBadEpoch", err)
	}
	if _, err := DecodeGPSLNAV(sf2WithToe(37800)); err != errBadEpoch { // exactly the week
		t.Errorf("toe raw 37800 (604 800 s): err = %v, want errBadEpoch", err)
	}
	if sf, err := DecodeGPSLNAV(sf2WithToe(37799)); err != nil || sf.Toe != 604784 { // the ICD maximum
		t.Errorf("toe raw 37799: sf=%v err=%v, want toe 604784 accepted", sf, err)
	}
	if _, err := DecodeGPSLNAV(sf1WithToc(65535)); err != errBadEpoch {
		t.Errorf("toc raw 65535: err = %v, want errBadEpoch", err)
	}
	if sf, err := DecodeGPSLNAV(sf1WithToc(37799)); err != nil || sf.Toc != 604784 {
		t.Errorf("toc raw 37799: sf=%v err=%v, want toc 604784 accepted", sf, err)
	}
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
	if _, _, err := AssembleGPS(gnss.GPS, 5, sf1, sf2, sf3); !errors.Is(err, errIODMismatch) {
		t.Errorf("IODE/IODC mismatch: got %v, want errIODMismatch", err)
	}
}

func TestAssembleGPSRejectsWrongSlots(t *testing.T) {
	sf1 := decodeBuf(t, buildSf1(t))
	sf2 := decodeBuf(t, buildSf2(t))
	sf3 := decodeBuf(t, buildSf3(t))
	if _, _, err := AssembleGPS(gnss.GPS, 5, sf2, sf1, sf3); err != ErrWrongMsgType {
		t.Errorf("transposed args error = %v, want ErrWrongMsgType", err)
	}
}

func TestGPSLNAVShortFrame(t *testing.T) {
	if _, err := DecodeGPSLNAV([]uint32{1, 2, 3}); err != ErrShortFrame {
		t.Errorf("err = %v, want ErrShortFrame", err)
	}
}

// TestGPSLNAVRejectsOutOfRangeSubframe guards a length-valid LNAV frame whose HOW
// carries an out-of-range subframe id (0/6/7) is a mis-tagged or corrupt frame and must
// return an error (so the state layer counts a decode error and records no capability off
// garbage), while a valid but non-ephemeris id (4/5) still decodes cleanly.
func TestGPSLNAVRejectsOutOfRangeSubframe(t *testing.T) {
	for _, id := range []int64{0, 6, 7} {
		buf := make([]byte, 30)
		setField(buf, 2, 20, 3, id)
		if _, err := DecodeGPSLNAV(packLNAV(t, buf)); err != errBadSubframe {
			t.Errorf("subframe id %d: err = %v, want errBadSubframe", id, err)
		}
	}
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 4) // almanac page: valid id, no ephemeris fields
	if sf, err := DecodeGPSLNAV(packLNAV(t, buf)); err != nil || sf.SubframeID != 4 {
		t.Errorf("subframe 4: sf=%v err=%v, want a clean decode", sf, err)
	}
}

// TestGPSLNAVHOWFlagsAndFitInterval guards regression fix/the HOW alert flag
// (bit 18) and anti-spoof flag (bit 19) sit exactly between the TOW count
// (bits 1–17) and the subframe id (bits 20–22) and were never extracted; the
// subframe-2 word-10 fit-interval flag (bit 17) and AODO (bits 18–22, ×900 s)
// sat past toe undecoded (IS-GPS-200N §20.3.3.2, §20.3.3.4.3.1, §20.3.3.4.1).
func TestGPSLNAVHOWFlagsAndFitInterval(t *testing.T) {
	// All four bits clear: flags false, AODO 0.
	sf := decodeBuf(t, buildSf2(t))
	if sf.Alert || sf.AntiSpoof {
		t.Errorf("clear HOW: Alert=%v AntiSpoof=%v, want false/false", sf.Alert, sf.AntiSpoof)
	}
	if sf.FitIntervalFlag || sf.AODO != 0 {
		t.Errorf("clear word 10: fit=%v AODO=%d, want false/0", sf.FitIntervalFlag, sf.AODO)
	}

	// Alert+AS raised, fit flag set, AODO raw 31 (the 27900 s NMCT-unavailable
	// sentinel, §20.3.3.4.4) — and toe/IODE must be unaffected by the new reads.
	buf := buildSf2(t)
	setField(buf, 2, 18, 1, 1)  // HOW alert
	setField(buf, 2, 19, 1, 1)  // HOW anti-spoof
	setField(buf, 10, 17, 1, 1) // fit interval flag
	setField(buf, 10, 18, 5, 31)
	sf = decodeBuf(t, buf)
	if !sf.Alert || !sf.AntiSpoof {
		t.Errorf("raised HOW: Alert=%v AntiSpoof=%v, want true/true", sf.Alert, sf.AntiSpoof)
	}
	if !sf.FitIntervalFlag {
		t.Error("fit interval flag not decoded")
	}
	if sf.AODO != 27900 {
		t.Errorf("AODO = %d, want 27900 (raw 31 × 900 s)", sf.AODO)
	}
	if sf.IODE != 85 || sf.Toe != 432000 {
		t.Errorf("IODE/toe = %d/%v disturbed by new word-10 reads, want 85/432000", sf.IODE, sf.Toe)
	}

	// The flags decode on subframe 1 too (the HOW rides every subframe).
	buf = buildSf1(t)
	setField(buf, 2, 18, 1, 1)
	if sf = decodeBuf(t, buf); !sf.Alert || sf.AntiSpoof {
		t.Errorf("sf1 HOW: Alert=%v AntiSpoof=%v, want true/false", sf.Alert, sf.AntiSpoof)
	}
}

// TestGPSLNAVRejectsTOWOverflow guards the HOW TOW count maxes at
// 100,799 before rolling over (IS-GPS-200N §20.3.3.2); a larger count scales
// past the week and is a corrupt HOW, rejected like bad subframe id.
func TestGPSLNAVRejectsTOWOverflow(t *testing.T) {
	buf := buildSf1(t)
	setField(buf, 2, 1, 17, 100800)
	if _, err := DecodeGPSLNAV(packLNAV(t, buf)); err != errBadTOWCount {
		t.Errorf("towCount 100800: err = %v, want errBadTOWCount", err)
	}
	buf = buildSf1(t)
	setField(buf, 2, 1, 17, 100799)
	sf, err := DecodeGPSLNAV(packLNAV(t, buf))
	if err != nil || sf.TOW != 100799*6 {
		t.Errorf("towCount 100799: sf=%v err=%v, want a clean decode at TOW %d", sf, err, 100799*6)
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
