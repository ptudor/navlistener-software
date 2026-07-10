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
