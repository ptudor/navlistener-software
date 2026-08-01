package frame

import (
	"testing"

	"github.com/ptudor/gnss/clock"
)

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
// E5b_HS must also be decoded into its OWN field (E5bSHS), never
// folded into Health — E5b health is broadcast on E1-B (Table 46/83), so its
// visibility needs no E5b-I dispatch.
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
	if w.E5bSHS != 1 {
		t.Errorf("E5bSHS = %d, want 1 (bits 67-68, distinct from E1B's 2)", w.E5bSHS)
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
			if _, err := DecodeGalileoINAV(toWords(p)); err != ErrGalileoAlertPage {
				t.Errorf("DecodeGalileoINAV with bit %d set = %v, want ErrGalileoAlertPage", tt.bit, err)
			}
		})
	}

	// The odd part's Even/Odd flag bit (128) missing (i.e. 0, a misaligned pair)
	// must also be rejected.
	p := make([]byte, 32)
	copy(p, page)
	p[16] = 0 // clear bit 128
	if _, err := DecodeGalileoINAV(toWords(p)); err != ErrGalileoAlertPage {
		t.Errorf("DecodeGalileoINAV with bit 128 cleared = %v, want ErrGalileoAlertPage", err)
	}

	// The unmodified page (nominal, bit 128 set, all others 0) must still decode.
	if _, err := DecodeGalileoINAV(toWords(page)); err != nil {
		t.Errorf("DecodeGalileoINAV on nominal page: %v, want success", err)
	}
}

// TestDecodeGalileoINAVWord5TimeAndDVS guards word 5 must decode GST
// WN/TOW and both DVS bits, at the exact OS-SIS-ICD Issue 2.2 Table 46 offsets
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

// TestDecodeGalileoFNAVPage1SISAHealth guards regression fix and F/NAV page 1
// must decode SISA(E1,E5a), the E5a Signal Health Status, AND the E5a Data
// Validity Status, at the exact OS-SIS-ICD Issue 2.2 Table 30 offsets (SISA@94,
// 8 bits; E5aHS@153, 2 bits; E5aDVS@187, 1 bit) — confirmed against the
// published ICD text, not assumed. SHS and DVS are the signal's TWO integrity
// flags; the maintenance-window signature is exactly E5aHS=0 with E5aDVS=1, so
// the test sets DVS while leaving the surrounding WN/TOW/spare bits zero to pin
// the offset (a ±1-bit regression would land in TOW's LSB or the spare field).
func TestDecodeGalileoFNAVPage1SISAHealth(t *testing.T) {
	buf := make([]byte, 32)
	setFNAVBufBits(buf, 0, 1, 6)         // page type = 1
	setFNAVBufBits(buf, 94, 200, 8)      // SISA = 200
	setFNAVBufBits(buf, 153, 2, 2)       // E5aHS = 2
	setFNAVBufBits(buf, 155, 3500, 12)   // GST WN = 3500 (> a 10-bit field's max, regression fix)
	setFNAVBufBits(buf, 167, 483000, 20) // GST TOW = 483000 s
	setFNAVBufBits(buf, 187, 1, 1)       // E5aDVS = 1 (working without guarantee, Table 81)
	words := fnavBufToWords(buf)
	StampGalileoFNAVCRC(words)
	w, err := DecodeGalileoFNAV(words)
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
	if w.WN != 3500 || w.TOW != 483000 {
		t.Errorf("WN/TOW = %d/%v, want 3500/483000 (Table 30 offsets 155/167)", w.WN, w.TOW)
	}
	if w.E5aDVS != 1 {
		t.Errorf("E5aDVS = %d, want 1 (bit 187, Table 30)", w.E5aDVS)
	}

	// The DVS=0 (valid) case must not be an artifact of the zeroed buffer: flip
	// only the neighboring spare bit and confirm DVS stays 0.
	buf2 := make([]byte, 32)
	setFNAVBufBits(buf2, 0, 1, 6)
	setFNAVBufBits(buf2, 188, 1, 1) // first spare bit, adjacent to DVS
	words2 := fnavBufToWords(buf2)
	StampGalileoFNAVCRC(words2)
	w2, err := DecodeGalileoFNAV(words2)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if w2.E5aDVS != 0 {
		t.Errorf("E5aDVS = %d with only spare bit 188 set, want 0", w2.E5aDVS)
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
	words := fnavBufToWords(buf)
	StampGalileoFNAVCRC(words)
	w, err := DecodeGalileoFNAV(words)
	if err != nil {
		t.Fatalf("decode page %d: %v", pageType, err)
	}
	return w
}

func TestDecodeGalileoFNAVCRC(t *testing.T) {
	buf := make([]byte, 32)
	setFNAVBufBits(buf, 0, 1, 6)
	words := fnavBufToWords(buf)
	StampGalileoFNAVCRC(words)
	if _, err := DecodeGalileoFNAV(words); err != nil {
		t.Fatalf("stamped page rejected: %v", err)
	}

	dataFlip := append([]uint32(nil), words...)
	dataFlip[1] ^= 1 << 20
	if _, err := DecodeGalileoFNAV(dataFlip); err != ErrBadCRC {
		t.Errorf("protected-bit flip error = %v, want ErrBadCRC", err)
	}
	crcFlip := append([]uint32(nil), words...)
	crcFlip[7] ^= 1 << 28 // bit 227 lies inside the transmitted CRC
	if _, err := DecodeGalileoFNAV(crcFlip); err != ErrBadCRC {
		t.Errorf("CRC-bit flip error = %v, want ErrBadCRC", err)
	}
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

func TestGalileoAssemblersRejectWrongSlots(t *testing.T) {
	const iod = 7
	w1, w2, w3, w4 := buildGalileoINAVWord1234(t, iod)
	if _, _, err := AssembleGalileo(14, w1, w2, w4, w3, nil); err != errWrongMsgType {
		t.Errorf("I/NAV w3/w4 swap error = %v, want errWrongMsgType", err)
	}
	p1 := buildFNAVPage(t, 1, iod)
	p2 := buildFNAVPage(t, 2, iod)
	p3 := buildFNAVPage(t, 3, iod)
	p4 := buildFNAVPage(t, 4, iod)
	if _, _, err := AssembleGalileoFNAV(14, p2, p1, p3, p4); err != errWrongMsgType {
		t.Errorf("F/NAV p1/p2 swap error = %v, want errWrongMsgType", err)
	}
}

// setINAVPageBits pokes raw page bits into an already-built 8-word I/NAV page
// (fields OUTSIDE the 128-bit nav-word content, e.g. the odd part's OSNMA
// field) and re-stamps the CRC, which protects them.
func setINAVPageBits(words []uint32, off int, v uint64, n int) {
	page := make([]byte, 32)
	for i := 0; i < 8; i++ {
		page[i*4] = byte(words[i] >> 24)
		page[i*4+1] = byte(words[i] >> 16)
		page[i*4+2] = byte(words[i] >> 8)
		page[i*4+3] = byte(words[i])
	}
	for i := 0; i < n; i++ {
		p := off + i
		if v&(1<<uint(n-1-i)) != 0 {
			page[p>>3] |= 1 << uint(7-(p&7))
		} else {
			page[p>>3] &^= 1 << uint(7-(p&7))
		}
	}
	for i := 0; i < 8; i++ {
		words[i] = uint32(page[i*4])<<24 | uint32(page[i*4+1])<<16 | uint32(page[i*4+2])<<8 | uint32(page[i*4+3])
	}
	StampGalileoINAVCRC(words)
}

// TestDecodeGalileoINAVOSNMA guards the 40-bit OSNMA protocol-data
// field of the E1-B odd page part (page bits 146..185, Table 38 — odd part
// starts at page bit 128, then Even/odd(1) PageType(1) Data2/2(16) → OSNMA@146)
// must be extracted on every nominal page regardless of word type, must
// round-trip a known pattern, and must be discarded for dummy messages (word
// type 63) per the OSNMA ICD.
//
// what pins offset 146 here is the PATTERN ROUND-TRIP, not the CRC.
// setINAVPageBits re-stamps the CRC after poking, so the frame is self-
// consistent at whatever offset the poke used — the CRC would pass for a
// wrong offset too. The oracle is that the poke offset (146, from Table 38)
// and the decoder's read offset must agree bit-for-bit on a pattern that is
// nonzero in every byte: move the decoder's offset unilaterally and the
// recovered 40 bits differ. (Moving BOTH in step would still pass — that is
// what the Table 38 citation above, not the test, guards against.)
func TestDecodeGalileoINAVOSNMA(t *testing.T) {
	const pattern = uint64(0xA1B2C3D4E5) // 40 bits, nonzero in every byte
	content := make([]byte, 16)
	setContentBits(content, 0, 1, 6) // word type 1
	words := buildGalileoINAVWords(content)
	setINAVPageBits(words, 146, pattern, 40)
	w, err := DecodeGalileoINAV(words)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !w.HasOSNMA || w.OSNMA != pattern {
		t.Errorf("HasOSNMA=%v OSNMA=%#x, want true/%#x", w.HasOSNMA, w.OSNMA, pattern)
	}

	// All-zeros field (SV not distributing): still observed, value 0.
	words = buildGalileoINAVWords(content)
	w, err = DecodeGalileoINAV(words)
	if err != nil {
		t.Fatalf("decode zero-field: %v", err)
	}
	if !w.HasOSNMA || w.OSNMA != 0 {
		t.Errorf("zero field: HasOSNMA=%v OSNMA=%#x, want true/0", w.HasOSNMA, w.OSNMA)
	}

	// Dummy message (word type 63): the OSNMA field must be discarded.
	dummy := make([]byte, 16)
	setContentBits(dummy, 0, 63, 6)
	words = buildGalileoINAVWords(dummy)
	setINAVPageBits(words, 146, pattern, 40)
	w, err = DecodeGalileoINAV(words)
	if err != nil {
		t.Fatalf("decode dummy: %v", err)
	}
	if w.HasOSNMA {
		t.Errorf("dummy message: HasOSNMA=%v, want false (OSNMA ICD: discard)", w.HasOSNMA)
	}
}

// TestDecodeGalileoINAVWord10GGTO guards regression fix (I/NAV side): word 10's
// GST-GPS conversion parameters decode at the GAL-OS-SIS-ICD-2.2 Table 51
// offsets (A0G@86 16 bits ×2⁻³⁵, A1G@102 12 bits ×2⁻⁵¹, t0G@114 8 bits ×3600,
// WN0G@122 6 bits) with two's-complement A0G/A1G, and §5.1.8's all-ones
// withdrawal sentinel requires ALL FOUR fields all-ones (A0G=−1 alone is a
// legal value, not a withdrawal).
func TestDecodeGalileoINAVWord10GGTO(t *testing.T) {
	p2m35 := 1.0 / float64(uint64(1)<<35)
	p2m51 := 1.0 / float64(uint64(1)<<51)

	content := make([]byte, 16)
	setContentBits(content, 0, 10, 6)              // word type = 10
	setContentBits(content, 86, 0xFFEC&0xFFFF, 16) // A0G raw = −20
	setContentBits(content, 102, 5, 12)            // A1G raw = +5
	setContentBits(content, 114, 100, 8)           // t0G raw = 100 → 360000 s
	setContentBits(content, 122, 37, 6)            // WN0G = 37
	w, err := DecodeGalileoINAV(buildGalileoINAVWords(content))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !w.HasGGTO || !w.GGTOValid {
		t.Fatalf("HasGGTO=%v GGTOValid=%v, want true/true", w.HasGGTO, w.GGTOValid)
	}
	if want := -20 * p2m35; w.A0G != want {
		t.Errorf("A0G = %v, want %v (signedness/scale)", w.A0G, want)
	}
	if want := 5 * p2m51; w.A1G != want {
		t.Errorf("A1G = %v, want %v", w.A1G, want)
	}
	if w.T0G != 360000 {
		t.Errorf("T0G = %v, want 360000 (raw 100 × 3600 s)", w.T0G)
	}
	if w.WN0G != 37 {
		t.Errorf("WN0G = %d, want 37", w.WN0G)
	}

	// All four fields all-ones: the §5.1.8 withdrawal.
	allOnes := make([]byte, 16)
	setContentBits(allOnes, 0, 10, 6)
	setContentBits(allOnes, 86, 0xFFFF, 16)
	setContentBits(allOnes, 102, 0xFFF, 12)
	setContentBits(allOnes, 114, 0xFF, 8)
	setContentBits(allOnes, 122, 0x3F, 6)
	w, err = DecodeGalileoINAV(buildGalileoINAVWords(allOnes))
	if err != nil {
		t.Fatalf("decode all-ones: %v", err)
	}
	if !w.HasGGTO || w.GGTOValid {
		t.Errorf("all-ones GGTO: HasGGTO=%v GGTOValid=%v, want true/false", w.HasGGTO, w.GGTOValid)
	}

	// A0G all-ones alone (raw −1) with the rest zero is a VALUE, not a withdrawal.
	minusOne := make([]byte, 16)
	setContentBits(minusOne, 0, 10, 6)
	setContentBits(minusOne, 86, 0xFFFF, 16)
	w, err = DecodeGalileoINAV(buildGalileoINAVWords(minusOne))
	if err != nil {
		t.Fatalf("decode A0G=-1: %v", err)
	}
	if !w.GGTOValid {
		t.Error("A0G=−1 with other fields zero flagged as withdrawal; the sentinel is the four-field conjunction")
	}
	if want := -1 * p2m35; w.A0G != want {
		t.Errorf("A0G = %v, want %v", w.A0G, want)
	}
}

// TestDecodeGalileoFNAVPage4GGTO guards regression fix (F/NAV side): page 4 transmits
// the same GGTO quartet at Table 33's offsets — and in a DIFFERENT field order
// than I/NAV (t0G@147 BEFORE A0G@155, then A1G@171, WN0G@183). Distinct values
// per field pin the order; the all-ones withdrawal must behave as on I/NAV.
//
// the offsets above are re-derived from GAL-OS-SIS-ICD-2.2 Table 33's
// own width ledger — Type(6) IODnav(10) Cic(16) Cis(16) A0(32) A1(24) ΔtLs(8)
// t0t(8) WN0t(8) WNLSF(8) DN(3) ΔtLSF(8) → t0G@147, +8 → A0G@155, +16 →
// A1G@171, +12 → WN0G@183. An abandoned first draft of this test wrote
// A1G@167/WN0G@179 (it advanced past A0G by 12 rather than its true 16 bits);
// the slip was caught before commit but survived in this comment until
// regression fix. The body and galileo_fnav.go have always been right — do NOT "fix"
// them to match an older comment.
func TestDecodeGalileoFNAVPage4GGTO(t *testing.T) {
	p2m35 := 1.0 / float64(uint64(1)<<35)
	p2m51 := 1.0 / float64(uint64(1)<<51)

	buf := make([]byte, 32)
	setFNAVBufBits(buf, 0, 4, 6)                // page type = 4
	setFNAVBufBits(buf, 147, 2, 8)              // t0G raw = 2 → 7200 s
	setFNAVBufBits(buf, 155, 0xFFFE&0xFFFF, 16) // A0G raw = −2
	setFNAVBufBits(buf, 171, 9, 12)             // A1G raw = +9
	setFNAVBufBits(buf, 183, 21, 6)             // WN0G = 21
	words := fnavBufToWords(buf)
	StampGalileoFNAVCRC(words)
	w, err := DecodeGalileoFNAV(words)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !w.HasGGTO || !w.GGTOValid {
		t.Fatalf("HasGGTO=%v GGTOValid=%v, want true/true", w.HasGGTO, w.GGTOValid)
	}
	if w.T0G != 7200 {
		t.Errorf("T0G = %v, want 7200", w.T0G)
	}
	if want := -2 * p2m35; w.A0G != want {
		t.Errorf("A0G = %v, want %v", w.A0G, want)
	}
	if want := 9 * p2m51; w.A1G != want {
		t.Errorf("A1G = %v, want %v", w.A1G, want)
	}
	if w.WN0G != 21 {
		t.Errorf("WN0G = %d, want 21", w.WN0G)
	}

	all := make([]byte, 32)
	setFNAVBufBits(all, 0, 4, 6)
	setFNAVBufBits(all, 147, 0xFF, 8)
	setFNAVBufBits(all, 155, 0xFFFF, 16)
	setFNAVBufBits(all, 171, 0xFFF, 12)
	setFNAVBufBits(all, 183, 0x3F, 6)
	words = fnavBufToWords(all)
	StampGalileoFNAVCRC(words)
	w, err = DecodeGalileoFNAV(words)
	if err != nil {
		t.Fatalf("decode all-ones: %v", err)
	}
	if !w.HasGGTO || w.GGTOValid {
		t.Errorf("all-ones GGTO: HasGGTO=%v GGTOValid=%v, want true/false", w.HasGGTO, w.GGTOValid)
	}
}

// TestAssembleGalileoFNAVBGD guards F/NAV page 1's BGD(E1,E5a) (bits
// 143–152, 10-bit two's complement × 2⁻³², GAL-OS-SIS-ICD-2.2 Table 30/72) must
// reach the assembled @3 clock's TGD — scaled by (f_E1/f_E5a)² per Eq. 19,
// because the tracked F/NAV signal (E5a) is the f2 of the (E1,E5a) clock pair —
// while the struct's BGDE1E5a keeps the raw broadcast value. A negative raw
// value pins the signedness (a sign-extension regression would produce a huge
// positive TGD).
func TestAssembleGalileoFNAVBGD(t *testing.T) {
	const iod = 9
	buf := make([]byte, 32)
	setFNAVBufBits(buf, 0, 1, 6)       // page type = 1
	setFNAVBufBits(buf, 12, iod, 10)   // IODnav
	setFNAVBufBits(buf, 143, 1021, 10) // BGD(E1,E5a) raw = −3 (10-bit two's complement 0b1111111101)
	words := fnavBufToWords(buf)
	StampGalileoFNAVCRC(words)
	p1, err := DecodeGalileoFNAV(words)
	if err != nil {
		t.Fatalf("decode page 1: %v", err)
	}
	bgdScale := 1.0 / float64(uint64(1)<<32)
	wantRaw := -3 * bgdScale
	if p1.BGDE1E5a != wantRaw {
		t.Errorf("BGDE1E5a = %v, want %v (raw broadcast value, unscaled)", p1.BGDE1E5a, wantRaw)
	}

	p2 := buildFNAVPage(t, 2, iod)
	p3 := buildFNAVPage(t, 3, iod)
	p4 := buildFNAVPage(t, 4, iod)
	_, clk, err := AssembleGalileoFNAV(14, p1, p2, p3, p4)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	wantTGD := wantRaw * clock.E5aGroupDelayFactor
	if clk.TGD != wantTGD {
		t.Errorf("TGD = %v, want %v (Eq. 19: (f_E1/f_E5a)²·BGD(E1,E5a))", clk.TGD, wantTGD)
	}
	if clk.TGD == p1.BGDE1E5a {
		t.Error("TGD equals the raw BGD — the Eq. 19 scaling for the E5a (f2) user is missing")
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
