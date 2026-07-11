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
	return words
}

// buildGalileoWord5 builds a Word Type 5 page with distinct BGD, E5b_HS, and
// E1B_HS values so a bit-offset regression  is caught by a Health/TGD
// mismatch, not accidentally matched by symmetric test data.
func buildGalileoWord5(bgdRaw int64, e5bHS, e1bHS uint64) []uint32 {
	content := make([]byte, 16)
	setContentBits(content, 0, 5, 6)                      // word type = 5
	setContentBits(content, 47, uint64(bgdRaw)&0x3FF, 10) // BGD(E1,E5a), bits 47-56
	setContentBits(content, 67, e5bHS, 2)                 // E5b_HS, bits 67-68
	setContentBits(content, 69, e1bHS, 2)                 // E1B_HS, bits 69-70
	return buildGalileoINAVWords(content)
}

// TestDecodeGalileoINAVWord5Health guards bit 67 is E5b_HS, bit 69 is
// E1B_HS — the decoder must report E1B_HS (Health), not E5b_HS, and the two
// must be distinguishable (a prior bug read bit 67 and stored it as E1B health).
func TestDecodeGalileoINAVWord5Health(t *testing.T) {
	words := buildGalileoWord5(100, 0b01, 0b10) // E5b_HS=1, E1B_HS=2
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

	w5, err := DecodeGalileoINAV(buildGalileoWord5(100, 0, 0))
	if err != nil {
		t.Fatalf("decode word5: %v", err)
	}
	_, clk, err := AssembleGalileo(7, w1, w2, w3, w4, w5)
	if err != nil {
		t.Fatalf("assemble with word5: %v", err)
	}
	wantTGD := float64(100) * (1.0 / float64(uint64(1)<<32))
	if clk.TGD != wantTGD {
		t.Errorf("TGD = %v, want %v (BGD from word5)", clk.TGD, wantTGD)
	}
}
