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
	if _, err := DecodeGLONASSString(bad); err != errGLONASSHamming {
		t.Errorf("single-bit-flipped string: err = %v, want errGLONASSHamming", err)
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
