package frame

import "math/bits"

// GPS/QZSS LNAV parity (IS-GPS-200N §20.3.5). Each 30-bit word carries 24 data bits
// d1..d24 (MSB first) and 6 parity bits D25..D30, computed over the data bits and
// the last two parity bits of the previous word (D29*, D30*). If D30* is set, the
// 24 data bits are transmitted complemented and must be inverted before use.
//
// Two delivery conventions reach the decoder. A raw-signal source hands over the
// transmitted word as broadcast (GPSParity). A u-blox receiver (UBX-RXM-SFRBX, the
// fleet's source) normalises each word before delivery: measured on 3 130/3 130
// words of two independent captures, delivered = tx XOR (D30* ? 0x3FFFFFFF : 0) —
// the whole word, parity bits included, is inverted when the previous transmitted
// D30 was 1, so the 24 data bits arrive un-complemented and the six parity bits
// arrive inverted. Carrying the transmitted D29*/D30* through the subframe (word 1
// starts from 0/0, which the ICD guarantees by solving words 2 and 10 to D29=D30=0)
// verifies every delivered word; lnavParityOK accepts a subframe under either
// convention and rejects one that verifies under neither.

// parityMasks[k] selects, over the 24 data bits, the bits that XOR into parity bit
// D(25+k). Bit (24−i) of the mask corresponds to data bit d_i (d1 is the MSB).
var parityMasks = [6]uint32{
	maskFromIdx(1, 2, 3, 5, 6, 10, 11, 12, 13, 14, 17, 18, 20, 23),    // D25
	maskFromIdx(2, 3, 4, 6, 7, 11, 12, 13, 14, 15, 18, 19, 21, 24),    // D26
	maskFromIdx(1, 3, 4, 5, 7, 8, 12, 13, 14, 15, 16, 19, 20, 22),     // D27
	maskFromIdx(2, 4, 5, 6, 8, 9, 13, 14, 15, 16, 17, 20, 21, 23),     // D28
	maskFromIdx(1, 3, 5, 6, 7, 9, 10, 14, 15, 16, 17, 18, 21, 22, 24), // D29
	maskFromIdx(3, 5, 6, 8, 9, 10, 11, 13, 15, 19, 22, 23, 24),        // D30
}

// prevBit[k] is which previous-word parity bit (D29* or D30*) XORs into parity bit
// D(25+k): false = D29*, true = D30* (IS-GPS-200N Table 20-XIV).
var prevIsD30 = [6]bool{false, true, false, true, true, false}

func maskFromIdx(idx ...int) uint32 {
	var m uint32
	for _, i := range idx {
		m |= 1 << uint(24-i)
	}
	return m
}

func parity(d, mask uint32) uint32 {
	return uint32(bits.OnesCount32(d&mask) & 1)
}

// parityBits computes the six transmitted parity bits D25..D30 (D25 the MSB) over
// the 24 source data bits d — the ICD's d1..d24, un-complemented — and the
// previous word's transmitted D29*, D30* (IS-GPS-200N Table 20-XIV).
func parityBits(d, d29star, d30star uint32) uint32 {
	d29star &= 1
	d30star &= 1
	var computed uint32
	for k := 0; k < 6; k++ {
		prev := d29star
		if prevIsD30[k] {
			prev = d30star
		}
		computed = (computed << 1) | (prev ^ parity(d, parityMasks[k]))
	}
	return computed
}

// GPSParity verifies the parity of a 30-bit LNAV word as transmitted (the raw-signal
// form), given the previous word's last two parity bits D29* and D30* (0 or 1). It
// returns the 24 data bits (already un-complemented per D30*) and whether the six
// parity bits check out. A word failing parity must be dropped, never assembled
// (docs/CONSTELLATIONS.md §2).
func GPSParity(word uint32, d29star, d30star uint32) (data uint32, ok bool) {
	word &= 0x3FFFFFFF // 30 bits
	raw := (word >> 6) & 0xFFFFFF
	recv := word & 0x3F // D25..D30, D25 is the MSB of these six
	d30star &= 1

	d := raw
	if d30star != 0 {
		d = ^raw & 0xFFFFFF // data bits are complemented when D30* = 1
	}
	return d, parityBits(d, d29star, d30star) == recv
}

// lnavParityOK verifies a ten-word LNAV subframe under either delivery
// convention: the receiver-normalised form the fleet's u-blox receivers deliver,
// or the raw transmitted form. Both chains start from D29* = D30* = 0 at word 1.
func lnavParityOK(words []uint32) bool {
	return lnavNormalizedParityOK(words) || lnavRawParityOK(words)
}

// lnavNormalizedParityOK verifies words delivered as tx XOR (D30* ? 0x3FFFFFFF : 0):
// the data bits are the source bits, the expected parity is the transmitted parity
// inverted when D30* = 1, and the transmitted D29/D30 carried to the next word are
// recovered by undoing that inversion.
func lnavNormalizedParityOK(words []uint32) bool {
	var d29, d30 uint32
	for _, w := range words {
		w &= 0x3FFFFFFF
		expect := parityBits((w>>6)&0xFFFFFF, d29, d30)
		if d30 == 1 {
			expect ^= 0x3F
		}
		if w&0x3F != expect {
			return false
		}
		tx := w & 0x3F
		if d30 == 1 {
			tx ^= 0x3F
		}
		d29, d30 = (tx>>1)&1, tx&1
	}
	return true
}

// lnavRawParityOK verifies words delivered as transmitted, carrying each word's own
// parity bits D29/D30 into the next (GPSParity's model).
func lnavRawParityOK(words []uint32) bool {
	var d29, d30 uint32
	for _, w := range words {
		if _, ok := GPSParity(w, d29, d30); !ok {
			return false
		}
		d29, d30 = (w>>1)&1, w&1
	}
	return true
}

// StampGPSLNAVParity writes receiver-normalised parity into a ten-word LNAV
// subframe whose 24 data bits per word are already in bits 29..6, overwriting bits
// 5..0 — the delivery form DecodeGPSLNAV verifies. For building test fixtures and
// synthetic frames, like StampGalileoINAVCRC; a real receiver stamps its own.
func StampGPSLNAVParity(words []uint32) {
	var d29, d30 uint32
	for i := range words {
		data := (words[i] >> 6) & 0xFFFFFF
		tx := parityBits(data, d29, d30)
		delivered := tx
		if d30 == 1 {
			delivered ^= 0x3F
		}
		words[i] = data<<6 | delivered
		d29, d30 = (tx>>1)&1, tx&1
	}
}
