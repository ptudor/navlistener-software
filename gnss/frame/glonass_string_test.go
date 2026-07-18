package frame

import (
	"encoding/binary"
	"math"
	"testing"
)

// gloSetSignMag writes a sign-magnitude field (MSB sign, per GLONASS ICD Ed. 5.1
// Table 4.5 Note 2) at absolute block-bit offset start, width n — the encoding
// BitReader.SignMag reads back.
func gloSetSignMag(buf []byte, start, n int, value int64) {
	var sign, mag uint64
	if value < 0 {
		sign, mag = 1, uint64(-value)
	} else {
		mag = uint64(value)
	}
	setBits(buf, start, n, (sign<<uint(n-1))|(mag&((1<<uint(n-1))-1)))
}

// gloStringWords packs one synthetic GLONASS string: the string number at block
// offset 1, then fill writes any fields at their block offsets (85 − ICD hi-bit).
func gloStringWords(number int, fill func(buf []byte)) []uint32 {
	buf := make([]byte, 16) // 128 bits
	setBits(buf, 1, 4, uint64(number))
	if fill != nil {
		fill(buf)
	}
	words := make([]uint32, 4)
	for i := 0; i < 4; i++ {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	StampGLONASSHamming(words) // synthetic strings need valid §4.7 check bits
	return words
}

// TestDecodeGLONASSStringNonPositionalZeroed guards for a string 4 (which carries
// τn/Δτn where strings 1–3 carry position), the Coord/Vel/Accel fields must decode to 0, not
// to garbage read off the τn bit span.
func TestDecodeGLONASSStringNonPositionalZeroed(t *testing.T) {
	s4 := gloStringWords(4, func(buf []byte) { gloSetSignMag(buf, 5, 22, -123456) }) // τn set
	s, err := DecodeGLONASSString(s4)
	if err != nil {
		t.Fatal(err)
	}
	if s.Coord != 0 || s.Vel != 0 || s.Accel != 0 {
		t.Errorf("string 4 Coord/Vel/Accel = %v/%v/%v, want all 0", s.Coord, s.Vel, s.Accel)
	}
}

// TestDecodeGLONASSStringHammingReject guards a valid string decodes, but flipping
// any single data bit fails the ICD §4.7 Hamming check and is rejected (a corrupt push-path
// frame must not reach live state).
func TestDecodeGLONASSStringHammingReject(t *testing.T) {
	good := gloStringWords(2, func(buf []byte) {
		gloSetSignMag(buf, 50, 27, 12345)
		setBits(buf, 9, 7, 40) // tb
	})
	if _, err := DecodeGLONASSString(good); err != nil {
		t.Fatalf("valid GLONASS string rejected: %v", err)
	}
	// Flip a single data bit (block offset 30 → an interior data bit).
	bad := append([]uint32(nil), good...)
	bad[0] ^= 1 << 1 // flip a bit inside word 0 (a data bit, not a check bit)
	if _, err := DecodeGLONASSString(bad); err != ErrGLONASSHamming {
		t.Errorf("single-bit-flipped string: err = %v, want ErrGLONASSHamming", err)
	}
}

// TestDecodeGLONASSStringRejectsZeroNumber guards a length-valid GLONASS block with
// string number 0 (out of the 1..15 range) is mis-tagged/corrupt and must return an error;
// a valid number decodes.
func TestDecodeGLONASSStringRejectsZeroNumber(t *testing.T) {
	if _, err := DecodeGLONASSString(gloStringWords(0, nil)); err != errBadStringNum {
		t.Errorf("string number 0: err = %v, want errBadStringNum", err)
	}
	if _, err := DecodeGLONASSString(gloStringWords(1, nil)); err != nil {
		t.Errorf("string number 1: err = %v, want nil", err)
	}
}

func TestExportedGLONASSDecodersValidateInputs(t *testing.T) {
	s1 := gloStringWords(1, nil)
	s5 := gloStringWords(5, nil)
	s6 := gloStringWords(6, nil)
	s7 := gloStringWords(7, nil)
	if _, err := DecodeGLONASSAlmanac(s1, s7, 1); err != errBadStringNum {
		t.Errorf("string-1 first error = %v, want errBadStringNum", err)
	}
	if _, err := DecodeGLONASSAlmanac(s7, s6, 1); err != errBadStringNum {
		t.Errorf("swapped pair error = %v, want errBadStringNum", err)
	}
	if _, err := DecodeGLONASSFrameNA(s6); err != errBadStringNum {
		t.Errorf("string-6 NA error = %v, want errBadStringNum", err)
	}
	if _, err := DecodeGLONASSFrameNA(s5); err != nil {
		t.Errorf("valid string-5 NA rejected: %v", err)
	}
	if _, err := DecodeGLONASSAlmanac(s6, s7, 1); err != nil {
		t.Errorf("valid 6/7 pair rejected: %v", err)
	}
	bad := append([]uint32(nil), s6...)
	bad[0] ^= 1
	if _, err := DecodeGLONASSAlmanac(bad, s7, 1); err != ErrGLONASSHamming {
		t.Errorf("corrupt pair error = %v, want ErrGLONASSHamming", err)
	}
}

// TestGLONASSFreqChValidation guards k = freqID − 7 must be a real FDMA
// channel −7..+6 (GLO-ICD-5.1 §3.3.1.1) before AssembleGLONASS stores it as
// ephemeris identity; the almanac word HnA maps 0..6 verbatim and 25..31 → −7..−1
// (Table 4.10), and the dead codespace 7..24 must reject the pair rather than
// store an impossible channel.
func TestGLONASSFreqChValidation(t *testing.T) {
	mk := func(number int, fill func([]byte)) *GLONASSString {
		s, err := DecodeGLONASSString(gloStringWords(number, fill))
		if err != nil {
			t.Fatalf("string %d: %v", number, err)
		}
		return s
	}
	s1 := mk(1, nil)
	s2 := mk(2, func(buf []byte) { setBits(buf, 9, 7, 30) })
	s3 := mk(3, nil)

	for _, freqID := range []int{-1, 14, 200} {
		if _, err := AssembleGLONASS(7, freqID, s1, s2, s3, nil); err != errBadFreqCh {
			t.Errorf("freqID %d: err = %v, want errBadFreqCh", freqID, err)
		}
	}
	for freqID, wantK := range map[int]int{0: -7, 7: 0, 13: 6} {
		eph, err := AssembleGLONASS(7, freqID, s1, s2, s3, nil)
		if err != nil {
			t.Fatalf("freqID %d rejected: %v", freqID, err)
		}
		if eph.FreqCh != wantK {
			t.Errorf("freqID %d: FreqCh = %d, want %d", freqID, eph.FreqCh, wantK)
		}
	}

	almPair := func(hn int) ([]uint32, []uint32) {
		first := gloStringWords(6, func(buf []byte) { setBits(buf, 8, 5, 7) }) // slot 7
		second := gloStringWords(7, func(buf []byte) { setBits(buf, 71, 5, uint64(hn)) })
		return first, second
	}
	for _, hn := range []int{7, 20, 24} {
		f, s := almPair(hn)
		if _, err := DecodeGLONASSAlmanac(f, s, 1); err != errBadFreqCh {
			t.Errorf("HnA %d: err = %v, want errBadFreqCh", hn, err)
		}
	}
	for hn, wantK := range map[int]int{0: 0, 6: 6, 25: -7, 31: -1} {
		f, s := almPair(hn)
		a, err := DecodeGLONASSAlmanac(f, s, 1)
		if err != nil {
			t.Fatalf("HnA %d rejected: %v", hn, err)
		}
		if a.Alm.FreqCh != wantK {
			t.Errorf("HnA %d: FreqCh = %d, want %d", hn, a.Alm.FreqCh, wantK)
		}
	}
}

// TestDecodeGLONASSStringP1En guards P1 (string 1 bits 77–78 → block
// offset 7, the tb-update-interval flag of Table 4.3) and En (string 4 bits
// 49–53 → offset 32, the SV-declared age of the immediate data in days) decode
// at their Table 4.6 positions and stay isolated from their neighbors.
func TestDecodeGLONASSStringP1En(t *testing.T) {
	s1, err := DecodeGLONASSString(gloStringWords(1, func(buf []byte) {
		setBits(buf, 7, 2, 2) // P1 = 10 → 45 min interval
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s1.P1 != 2 {
		t.Errorf("P1 = %d, want 2", s1.P1)
	}
	s4, err := DecodeGLONASSString(gloStringWords(4, func(buf []byte) {
		setBits(buf, 32, 5, 21)            // En = 21 days
		gloSetSignMag(buf, 5, 22, -123456) // τn alongside — no bleed
		gloSetSignMag(buf, 27, 5, 5)       // Δτn (offsets 27–31 abut En's 32–36)
	}))
	if err != nil {
		t.Fatal(err)
	}
	if s4.En != 21 {
		t.Errorf("En = %d, want 21", s4.En)
	}
	if want := 5.0 / (1 << 30); s4.DeltaTauN != want {
		t.Errorf("Δτn = %v, want %v (En decode must not bleed into the adjacent span)", s4.DeltaTauN, want)
	}
}

// TestGLONASSStringUnhealthy guards the exported per-string health
// accessor must flag only the Bn MSB (mask 0x4, GLO-ICD-5.1 §4.4 — the natural
// Health != 0 test over-flags on the benign low bits) or a set ℓn.
func TestGLONASSStringUnhealthy(t *testing.T) {
	for raw, want := range map[int]bool{0: false, 1: false, 3: false, 4: true, 7: true} {
		s := &GLONASSString{Health: raw}
		if s.Unhealthy() != want {
			t.Errorf("Health=%d: Unhealthy = %v, want %v", raw, s.Unhealthy(), want)
		}
	}
	if s := (&GLONASSString{Ln: 1, LnKnown: true}); !s.Unhealthy() {
		t.Error("ℓn=1 must report Unhealthy")
	}
	if s := (&GLONASSString{Ln: 1}); s.Unhealthy() {
		t.Error("Ln without LnKnown must not report Unhealthy (fabricated flag)")
	}
}

// TestDecodeGLONASSStringLn guards the GLONASS-M ℓn fast malfunction
// flag (0 healthy, 1 malfunction; GLO-ICD-5.1 §4.4) must decode from string 3
// at ICD bit 65 (block offset 20) and from the odd strings 5,7,9,11,13,15 at
// ICD bit 9 (block offset 76) — Table 4.6 — and LnKnown must be false for every
// string that does not carry ℓn, so callers never read a fabricated flag.
func TestDecodeGLONASSStringLn(t *testing.T) {
	setLn := func(number int) func([]byte) {
		off := 20
		if number != 3 {
			off = 76
		}
		return func(buf []byte) {
			setBits(buf, off, 1, 1)
			if number == 2 {
				setBits(buf, 9, 7, 40) // string 2 needs a valid tb 
			}
		}
	}
	for _, num := range []int{3, 5, 7, 13, 15} {
		s, err := DecodeGLONASSString(gloStringWords(num, setLn(num)))
		if err != nil {
			t.Fatalf("string %d: %v", num, err)
		}
		if !s.LnKnown || s.Ln != 1 {
			t.Errorf("string %d: Ln=%d LnKnown=%v, want 1/true", num, s.Ln, s.LnKnown)
		}
		// The same string with the flag clear: known, healthy.
		clear, err := DecodeGLONASSString(gloStringWords(num, nil))
		if err != nil {
			t.Fatalf("string %d (clear): %v", num, err)
		}
		if !clear.LnKnown || clear.Ln != 0 {
			t.Errorf("string %d (clear): Ln=%d LnKnown=%v, want 0/true", num, clear.Ln, clear.LnKnown)
		}
	}
	// Strings without ℓn (1, 2, 4, and the even almanac strings) must not claim it —
	// even with the would-be flag bit positions set.
	for _, num := range []int{1, 4, 6, 14} {
		s, err := DecodeGLONASSString(gloStringWords(num, func(buf []byte) {
			setBits(buf, 20, 1, 1)
			setBits(buf, 76, 1, 1)
		}))
		if err != nil {
			t.Fatalf("string %d: %v", num, err)
		}
		if s.LnKnown {
			t.Errorf("string %d: LnKnown = true, want false (ℓn not carried)", num)
		}
	}
	s2, err := DecodeGLONASSString(gloStringWords(2, setLn(2)))
	if err != nil {
		t.Fatalf("string 2: %v", err)
	}
	if s2.LnKnown {
		t.Error("string 2: LnKnown = true, want false (ℓn not carried)")
	}
	// γn must be unaffected by the string-3 ℓn read (adjacent field isolation).
	s3, err := DecodeGLONASSString(gloStringWords(3, func(buf []byte) {
		gloSetSignMag(buf, 6, 11, -42)
		setBits(buf, 20, 1, 1)
	}))
	if err != nil {
		t.Fatalf("string 3 (γn+ℓn): %v", err)
	}
	if want := -42.0 / (1 << 40); s3.GammaN != want || s3.Ln != 1 {
		t.Errorf("string 3: GammaN=%v Ln=%d, want %v/1", s3.GammaN, s3.Ln, want)
	}
}

// TestDecodeGLONASSStringTbRange guards tb is a 7-bit index of a 15-min
// interval within the current day, effective range 15…1425 min = index 1..95
// (GLO-ICD-5.1 §4.4, Table 4.5). The codespace 96..127 exceeds a day and index 0
// is outside the effective range; both decode without complaint upstream (the §4.7
// Hamming check is detect-only, not a strong CRC) and a garbage tb aliases through
// EphAgeDay's single ±43 200 s wrap into a finite, in-domain, silently wrong RK4
// propagation interval — so the decoder must reject out-of-range indices outright.
func TestDecodeGLONASSStringTbRange(t *testing.T) {
	mk := func(tb int) []uint32 {
		return gloStringWords(2, func(buf []byte) { setBits(buf, 9, 7, uint64(tb)) })
	}
	for _, tb := range []int{0, 96, 100, 127} {
		if _, err := DecodeGLONASSString(mk(tb)); err != errBadTb {
			t.Errorf("tb index %d: err = %v, want errBadTb", tb, err)
		}
	}
	for _, tb := range []int{1, 45, 95} {
		s, err := DecodeGLONASSString(mk(tb))
		if err != nil {
			t.Fatalf("tb index %d rejected: %v", tb, err)
		}
		if want := float64(tb) * 900; s.Tb != want {
			t.Errorf("tb index %d: Tb = %v s, want %v", tb, s.Tb, want)
		}
	}
}

// TestDecodeGLONASSStringClockTerms guards γn(tb) from string 3 and
// τn(tb)/Δτn from string 4, at the GLONASS ICD Ed. 5.1 Table 4.6 positions
// (γn bits 69–79 → block offset 6, width 11; τn bits 59–80 → offset 5, width 22;
// Δτn bits 54–58 → offset 27, width 5), sign-magnitude, scales 2⁻⁴⁰/2⁻³⁰/2⁻³⁰
// (Table 4.5). Negative values exercise the MSB-sign convention.
func TestDecodeGLONASSStringClockTerms(t *testing.T) {
	const (
		gammaRaw = -42     // → -42 × 2⁻⁴⁰ ≈ -3.8e-11 (realistic magnitude)
		tauRaw   = -123456 // → -123456 × 2⁻³⁰ ≈ -1.15e-4 s
		dtauRaw  = 5       // → 5 × 2⁻³⁰ ≈ 4.7e-9 s
	)

	s3, err := DecodeGLONASSString(gloStringWords(3, func(buf []byte) {
		gloSetSignMag(buf, 6, 11, gammaRaw)
	}))
	if err != nil {
		t.Fatalf("string 3: %v", err)
	}
	if want := float64(gammaRaw) / (1 << 40); s3.GammaN != want {
		t.Errorf("string 3 GammaN = %v, want %v", s3.GammaN, want)
	}
	if s3.TauN != 0 || s3.DeltaTauN != 0 {
		t.Errorf("string 3 must not populate τn/Δτn: got %v/%v", s3.TauN, s3.DeltaTauN)
	}

	s4, err := DecodeGLONASSString(gloStringWords(4, func(buf []byte) {
		gloSetSignMag(buf, 5, 22, tauRaw)
		gloSetSignMag(buf, 27, 5, dtauRaw)
	}))
	if err != nil {
		t.Fatalf("string 4: %v", err)
	}
	if want := float64(tauRaw) / (1 << 30); s4.TauN != want {
		t.Errorf("string 4 TauN = %v, want %v", s4.TauN, want)
	}
	if want := float64(dtauRaw) / (1 << 30); s4.DeltaTauN != want {
		t.Errorf("string 4 DeltaTauN = %v, want %v", s4.DeltaTauN, want)
	}
	if s4.GammaN != 0 {
		t.Errorf("string 4 must not populate γn: got %v", s4.GammaN)
	}

	// Field isolation: τn's 22-bit span (block offsets 5–26) must not bleed into
	// Δτn's (27–31) — set τn to all-ones magnitude and Δτn to zero.
	s4b, err := DecodeGLONASSString(gloStringWords(4, func(buf []byte) {
		gloSetSignMag(buf, 5, 22, -(1<<21 - 1))
	}))
	if err != nil {
		t.Fatalf("string 4 (isolation): %v", err)
	}
	if s4b.DeltaTauN != 0 {
		t.Errorf("Δτn bled from an all-ones τn: got %v, want 0", s4b.DeltaTauN)
	}
	if want := -float64(1<<21-1) / (1 << 30); math.Abs(s4b.TauN-want) > 0 {
		t.Errorf("all-ones τn = %v, want %v", s4b.TauN, want)
	}
}

// TestAssembleGLONASSClock guards assembly half: a same-frame string 4
// contributes τn/Δτn and sets ClockKnown; without one the ephemeris assembles
// clockless (γn, riding string 3, is present either way); a wrong-numbered s4
// is rejected like any other mis-wired argument (regression fix defense-in-depth).
func TestAssembleGLONASSClock(t *testing.T) {
	mk := func(number int, fill func([]byte)) *GLONASSString {
		s, err := DecodeGLONASSString(gloStringWords(number, fill))
		if err != nil {
			t.Fatalf("string %d: %v", number, err)
		}
		return s
	}
	s1 := mk(1, func(buf []byte) { gloSetSignMag(buf, 50, 27, 1000) })
	s2 := mk(2, func(buf []byte) { setBits(buf, 9, 7, 30) }) // tb
	s3 := mk(3, func(buf []byte) {
		gloSetSignMag(buf, 50, 27, 3000)
		gloSetSignMag(buf, 6, 11, -42) // γn
	})
	s4 := mk(4, func(buf []byte) {
		gloSetSignMag(buf, 5, 22, -123456) // τn
		gloSetSignMag(buf, 27, 5, 5)       // Δτn
	})

	eph, err := AssembleGLONASS(7, 7, s1, s2, s3, s4)
	if err != nil {
		t.Fatalf("assemble with s4: %v", err)
	}
	if !eph.ClockKnown {
		t.Error("ClockKnown = false with a string 4 present")
	}
	if eph.TauN != s4.TauN || eph.DeltaTauN != s4.DeltaTauN || eph.GammaN != s3.GammaN {
		t.Errorf("clock terms not carried: τn=%v γn=%v Δτn=%v, want %v/%v/%v",
			eph.TauN, eph.GammaN, eph.DeltaTauN, s4.TauN, s3.GammaN, s4.DeltaTauN)
	}

	clockless, err := AssembleGLONASS(7, 7, s1, s2, s3, nil)
	if err != nil {
		t.Fatalf("assemble without s4: %v", err)
	}
	if clockless.ClockKnown || clockless.TauN != 0 || clockless.DeltaTauN != 0 {
		t.Errorf("nil s4 must assemble clockless: ClockKnown=%v τn=%v Δτn=%v",
			clockless.ClockKnown, clockless.TauN, clockless.DeltaTauN)
	}
	if clockless.GammaN != s3.GammaN {
		t.Errorf("γn must ride string 3 even without s4: got %v, want %v", clockless.GammaN, s3.GammaN)
	}

	if _, err := AssembleGLONASS(7, 7, s1, s2, s3, s3); err != errGLONASSStringOrder {
		t.Errorf("a non-string-4 passed as s4 must be rejected: err = %v, want errGLONASSStringOrder", err)
	}
}
