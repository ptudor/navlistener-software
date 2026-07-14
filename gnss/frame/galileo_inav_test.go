package frame

import "testing"

// setContentBits packs v's low n bits (MSB-first) into content starting at bit
// offset off — the same layout DecodeGalileoINAV's internal bit reader expects
// (content is the reconstructed 128-bit nav word, even part then odd part).
func setContentBits(content []byte, off int, v uint64, n int) {
	for i := 0; i < n; i++ {
		if v&(1<<uint(n-1-i)) != 0 {
			p := off + i
			content[p>>3] |= 1 << uint(7-(p&7))
		}
	}
}

// copyBits copies n bits from src[srcOff:] into dst starting at dstOff, both
// MSB-first bit-addressed.
func copyBits(dst []byte, dstOff int, src []byte, srcOff, n int) {
	for i := 0; i < n; i++ {
		sp := srcOff + i
		bit := (src[sp>>3] >> uint(7-(sp&7))) & 1
		if bit != 0 {
			dp := dstOff + i
			dst[dp>>3] |= 1 << uint(7-(dp&7))
		}
	}
}

// buildGalileoINAVWords is the inverse of DecodeGalileoINAV's page reconstruction:
// given a 128-bit content blob (the nav word DecodeGalileoINAV would produce),
// place it back into an eight-word page at the same offsets the decoder reads
// (page[2:114) = content[0:112), page[130:146) = content[112:128)). Every other
// page bit is left zero — DecodeGalileoINAV never reads them.
func buildGalileoINAVWords(content []byte) []uint32 {
	page := make([]byte, 32)
	// the odd part's Even/Odd flag bit (page bit 128) must be 1 for a
	// nominal page -- every other flag bit (even-part bit 0, both Page Type bits)
	// is correctly 0 by the zeroed page, but this one isn't, so it must be set
	// explicitly or DecodeGalileoINAV now rejects every test-built page.
	page[16] = 0x80 // bit 128 is the MSB of byte 16 (128/8 = 16)
	copyBits(page, 2, content, 0, 112)
	copyBits(page, 130, content, 112, 16)
	words := make([]uint32, 8)
	for i := 0; i < 8; i++ {
		words[i] = uint32(page[i*4])<<24 | uint32(page[i*4+1])<<16 | uint32(page[i*4+2])<<8 | uint32(page[i*4+3])
	}
	StampGalileoINAVCRC(words)
	return words
}

// TestDecodeGalileoINAVCRC guards corruption in either protected data or
// the transmitted checksum must fail, while the ICD-excluded SSP field must not
// be accidentally included in the check.
func TestDecodeGalileoINAVCRC(t *testing.T) {
	content := make([]byte, 16)
	setContentBits(content, 0, 5, 6)
	words := buildGalileoINAVWords(content)
	if _, err := DecodeGalileoINAV(words); err != nil {
		t.Fatalf("valid stamped page: %v", err)
	}

	badData := append([]uint32(nil), words...)
	badData[0] ^= 1 << 21 // page bit 10: protected even-part data
	if _, err := DecodeGalileoINAV(badData); err != ErrBadCRC {
		t.Errorf("protected-data flip: err = %v, want ErrBadCRC", err)
	}

	badCRC := append([]uint32(nil), words...)
	badCRC[6] ^= 1 << 13 // page bit 210: first transmitted CRC bit
	if _, err := DecodeGalileoINAV(badCRC); err != ErrBadCRC {
		t.Errorf("CRC flip: err = %v, want ErrBadCRC", err)
	}

	sspFlip := append([]uint32(nil), words...)
	sspFlip[7] ^= 1 << 21 // page bit 234: SSP/reserved2, outside CRC coverage
	if _, err := DecodeGalileoINAV(sspFlip); err != nil {
		t.Errorf("unprotected SSP flip: err = %v, want success", err)
	}
}

// buildGalileoWord5 builds a Word Type 5 page with distinct BGD, E5b_HS, and
// E1B_HS values so a bit-offset regression  is caught by a Health/TGD
// mismatch, not accidentally matched by symmetric test data.
func buildGalileoWord5(bgdARaw, bgdBRaw int64, e5bHS, e1bHS uint64) []uint32 {
	content := make([]byte, 16)
	setContentBits(content, 0, 5, 6)                       // word type = 5
	setContentBits(content, 47, uint64(bgdARaw)&0x3FF, 10) // BGD(E1,E5a), bits 47-56
	setContentBits(content, 57, uint64(bgdBRaw)&0x3FF, 10) // BGD(E1,E5b), bits 57-66 
	setContentBits(content, 67, e5bHS, 2)                  // E5b_HS, bits 67-68
	setContentBits(content, 69, e1bHS, 2)                  // E1B_HS, bits 69-70
	return buildGalileoINAVWords(content)
}

// TestDecodeGalileoINAVWord5Health guards bit 67 is E5b_HS, bit 69 is
// E1B_HS — the decoder must report E1B_HS (Health), not E5b_HS, and the two
// must be distinguishable (a prior bug read bit 67 and stored it as E1B health).
func TestDecodeGalileoINAVWord5Health(t *testing.T) {
	words := buildGalileoWord5(100, 200, 0b01, 0b10) // BGD E5a=100, E5b=200; E5b_HS=1, E1B_HS=2
	w, err := DecodeGalileoINAV(words)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w.Type != 5 {
		t.Fatalf("word type = %d, want 5", w.Type)
	}
	if w.Health != 2 {
		t.Errorf("Health = %d, want 2 (E1B_HS at bit 69, not E5b_HS=1 at bit 67)", w.Health)
	}
}

// buildGalileoINAVWord1234 builds a minimal, internally consistent (matching
// IODnav) word 1-4 set. Field values beyond IODnav are physically meaningless
// (zeroed) — this test only exercises the assembler's clock/TGD wiring, not
// orbit propagation.
func buildGalileoINAVWord1234(t *testing.T, iod uint64) (w1, w2, w3, w4 *GalileoINAV) {
	t.Helper()
	mk := func(wordType uint64) []uint32 {
		content := make([]byte, 16)
		setContentBits(content, 0, wordType, 6)
		setContentBits(content, 6, iod, 10)
		return buildGalileoINAVWords(content)
	}
	var err error
	w1, err = DecodeGalileoINAV(mk(1))
	if err != nil {
		t.Fatalf("decode word1: %v", err)
	}
	w2, err = DecodeGalileoINAV(mk(2))
	if err != nil {
		t.Fatalf("decode word2: %v", err)
	}
	w3, err = DecodeGalileoINAV(mk(3))
	if err != nil {
		t.Fatalf("decode word3: %v", err)
	}
	w4, err = DecodeGalileoINAV(mk(4))
	if err != nil {
		t.Fatalf("decode word4: %v", err)
	}
	return w1, w2, w3, w4
}

// TestDecodeGalileoINAVRejectsAlertPage guards a page whose Even/Odd or
// Page Type flag bits don't match a nominal even+odd pair must error, not decode
// as an arbitrary nav word -- an alert page's data fields aren't a nav word at
// all, and word 5 writes health directly into live state.
func TestDecodeGalileoINAVRejectsAlertPage(t *testing.T) {
	content := make([]byte, 16)
	setContentBits(content, 0, 1, 6) // word type = 1 (would otherwise decode fine)

	base := buildGalileoINAVWords(content)
	page := make([]byte, 32)
	for i := 0; i < 8; i++ {
		page[i*4] = byte(base[i] >> 24)
		page[i*4+1] = byte(base[i] >> 16)
		page[i*4+2] = byte(base[i] >> 8)
		page[i*4+3] = byte(base[i])
	}
	toWords := func(p []byte) []uint32 {
		words := make([]uint32, 8)
		for i := 0; i < 8; i++ {
			words[i] = uint32(p[i*4])<<24 | uint32(p[i*4+1])<<16 | uint32(p[i*4+2])<<8 | uint32(p[i*4+3])
		}
		return words
	}

	for _, tt := range []struct {
		name string
		bit  int // absolute page bit to flip to 1
	}{
		{"even-part alert (bit 1, the finding's exact PoC)", 1},
		{"even-part Even/Odd flag set (bit 0)", 0},
		{"odd-part alert (bit 129)", 129},
	} {
		t.Run(tt.name, func(t *testing.T) {
			p := make([]byte, 32)
			copy(p, page)
			p[tt.bit>>3] |= 1 << uint(7-(tt.bit&7))
			if _, err := DecodeGalileoINAV(toWords(p)); err != errGalileoAlertPage {
				t.Errorf("DecodeGalileoINAV with bit %d set = %v, want errGalileoAlertPage", tt.bit, err)
			}
		})
	}

	// The odd part's Even/Odd flag bit (128) missing (i.e. 0, a misaligned pair)
	// must also be rejected.
	p := make([]byte, 32)
	copy(p, page)
	p[16] = 0 // clear bit 128
	if _, err := DecodeGalileoINAV(toWords(p)); err != errGalileoAlertPage {
		t.Errorf("DecodeGalileoINAV with bit 128 cleared = %v, want errGalileoAlertPage", err)
	}

	// The unmodified page (nominal, bit 128 set, all others 0) must still decode.
	if _, err := DecodeGalileoINAV(toWords(page)); err != nil {
		t.Errorf("DecodeGalileoINAV on nominal page: %v, want success", err)
	}
}

// TestDecodeGalileoINAVWord5TimeAndDVS guards word 5 must decode GST
// WN/TOW and both DVS bits, at the exact OS-SIS-ICD Issue 2.1 Table 44 offsets
// (WN@73, 12 bits; TOW@85, 20 bits; E5bDVS@71; E1BDVS@72) — confirmed against
// the published ICD text, not assumed. WN must be able to hold a full 12-bit
// value (up to 4095) without truncation, distinguishing it from GPS's 10-bit WN.
func TestDecodeGalileoINAVWord5TimeAndDVS(t *testing.T) {
	content := make([]byte, 16)
	setContentBits(content, 0, 5, 6)        // word type = 5
	setContentBits(content, 71, 1, 1)       // E5bDVS = 1 (working without guarantee)
	setContentBits(content, 72, 0, 1)       // E1BDVS = 0 (valid)
	setContentBits(content, 73, 3500, 12)   // WN = 3500 (> GPS's 10-bit max of 1023)
	setContentBits(content, 85, 483000, 20) // TOW = 483000 s (< 604800, within-week)
	w, err := DecodeGalileoINAV(buildGalileoINAVWords(content))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w.WN != 3500 {
		t.Errorf("WN = %d, want 3500", w.WN)
	}
	if w.TOW != 483000 {
		t.Errorf("TOW = %v, want 483000", w.TOW)
	}
	if w.E5bDVS != 1 {
		t.Errorf("E5bDVS = %d, want 1", w.E5bDVS)
	}
	if w.E1BDVS != 0 {
		t.Errorf("E1BDVS = %d, want 0", w.E1BDVS)
	}
}

// setFNAVBufBits packs v's low n bits (MSB-first) into a raw 256-bit F/NAV page
// buffer at bit offset off — F/NAV pages have no even/odd split (unlike I/NAV),
// so this writes directly into the 32-byte buffer DecodeGalileoFNAV reads.
func setFNAVBufBits(buf []byte, off int, v uint64, n int) {
	for i := 0; i < n; i++ {
		if v&(1<<uint(n-1-i)) != 0 {
			p := off + i
			buf[p>>3] |= 1 << uint(7-(p&7))
		}
	}
}

func fnavBufToWords(buf []byte) []uint32 {
	words := make([]uint32, 8)
	for i := 0; i < 8; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestDecodeGalileoFNAVPage1SISAHealth guards F/NAV page 1 must decode
// SISA(E1,E5a) and E5a Signal Health Status, at the exact OS-SIS-ICD Issue 2.1
// Table 28 offsets (SISA@94, 8 bits; E5aHS@153, 2 bits) — confirmed against the
// published ICD text, not assumed.
func TestDecodeGalileoFNAVPage1SISAHealth(t *testing.T) {
	buf := make([]byte, 32)
	setFNAVBufBits(buf, 0, 1, 6)    // page type = 1
	setFNAVBufBits(buf, 94, 200, 8) // SISA = 200
	setFNAVBufBits(buf, 153, 2, 2)  // E5aHS = 2
	w, err := DecodeGalileoFNAV(fnavBufToWords(buf))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w.PageType != 1 {
		t.Fatalf("PageType = %d, want 1", w.PageType)
	}
	if w.SISA != 200 {
		t.Errorf("SISA = %d, want 200", w.SISA)
	}
	if w.E5aHS != 2 {
		t.Errorf("E5aHS = %d, want 2", w.E5aHS)
	}
}

// buildFNAVPage constructs one decoded F/NAV page with the given page type and
// IODnav, at the correct bit offset for that page type (page 1's IODnav sits at
// bit 12 after its extra SVID field; pages 2-4 share bit offset 6).
func buildFNAVPage(t *testing.T, pageType, iod int) *GalileoFNAV {
	t.Helper()
	buf := make([]byte, 32)
	setFNAVBufBits(buf, 0, uint64(pageType), 6)
	if pageType == 1 {
		setFNAVBufBits(buf, 12, uint64(iod), 10)
	} else {
		setFNAVBufBits(buf, 6, uint64(iod), 10)
	}
	w, err := DecodeGalileoFNAV(fnavBufToWords(buf))
	if err != nil {
		t.Fatalf("decode page %d: %v", pageType, err)
	}
	return w
}

// TestAssembleGalileoFNAVChecksPage1IODnav guards page 1 (the clock)
// carries its own IODnav but was excluded from the assembler's mismatch check —
// a stale page 1 riding along with a fresh, mutually-matching pages 2/3/4 must
// still be rejected, not silently combine a clock from a different data set than
// the ephemeris.
func TestAssembleGalileoFNAVChecksPage1IODnav(t *testing.T) {
	const iod = 7
	p1 := buildFNAVPage(t, 1, iod)
	p2 := buildFNAVPage(t, 2, iod)
	p3 := buildFNAVPage(t, 3, iod)
	p4 := buildFNAVPage(t, 4, iod)

	if _, _, err := AssembleGalileoFNAV(14, p1, p2, p3, p4); err != nil {
		t.Fatalf("matching IODnav across all four pages must assemble cleanly: %v", err)
	}

	p1Stale := buildFNAVPage(t, 1, iod+1) // page 1's own IODnav now differs
	if _, _, err := AssembleGalileoFNAV(14, p1Stale, p2, p3, p4); err != errIODMismatch {
		t.Fatalf("AssembleGalileoFNAV with mismatched page-1 IODnav = %v, want errIODMismatch", err)
	}
}

// TestAssembleGalileoBGD guards Galileo BGD (decoded in word 5) must
// reach the assembled clock model's TGD, which word 4 alone never sets.
func TestAssembleGalileoBGD(t *testing.T) {
	const iod = 42
	w1, w2, w3, w4 := buildGalileoINAVWord1234(t, iod)

	// Without word 5, TGD stays 0 (nil-tolerant — word 5 may not have arrived yet).
	_, clkNoBGD, err := AssembleGalileo(7, w1, w2, w3, w4, nil)
	if err != nil {
		t.Fatalf("assemble without word5: %v", err)
	}
	if clkNoBGD.TGD != 0 {
		t.Errorf("TGD = %v without word5, want 0", clkNoBGD.TGD)
	}

	// distinct BGD(E1,E5a)=100 (bit 47) and BGD(E1,E5b)=200 (bit 57); the I/NAV clock
	// must use BGD(E1,E5b) — the bit-57 value — not BGD(E1,E5a).
	w5, err := DecodeGalileoINAV(buildGalileoWord5(100, 200, 0, 0))
	if err != nil {
		t.Fatalf("decode word5: %v", err)
	}
	bgdScale := 1.0 / float64(uint64(1)<<32)
	if w5.BGDE1E5a != 100*bgdScale || w5.BGDE1E5b != 200*bgdScale {
		t.Errorf("decoded BGD E5a/E5b = %v/%v, want %v/%v", w5.BGDE1E5a, w5.BGDE1E5b, 100*bgdScale, 200*bgdScale)
	}
	_, clk, err := AssembleGalileo(7, w1, w2, w3, w4, w5)
	if err != nil {
		t.Fatalf("assemble with word5: %v", err)
	}
	wantTGD := float64(200) * bgdScale // BGD(E1,E5b), not BGD(E1,E5a)
	if clk.TGD != wantTGD {
		t.Errorf("TGD = %v, want %v (BGD(E1,E5b) from word5 bit 57)", clk.TGD, wantTGD)
	}
}
