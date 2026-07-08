package frame

import "math/bits"

// GPS/QZSS LNAV parity (IS-GPS-200 §20.3.5). Each 30-bit word carries 24 data bits
// d1..d24 (MSB first) and 6 parity bits D25..D30, computed over the data bits and
// the last two parity bits of the previous word (D29*, D30*). If D30* is set, the
// 24 data bits are transmitted complemented and must be inverted before use.

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
// D(25+k): false = D29*, true = D30* (IS-GPS-200 Table 20-XIV).
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

// GPSParity verifies the parity of a 30-bit LNAV word given the previous word's
// last two parity bits D29* and D30* (0 or 1). It returns the 24 data bits
// (already un-complemented per D30*) and whether the six parity bits check out. A
// word failing parity must be dropped, never assembled (docs/CONSTELLATIONS.md §2).
func GPSParity(word uint32, d29star, d30star uint32) (data uint32, ok bool) {
	word &= 0x3FFFFFFF // 30 bits
	raw := (word >> 6) & 0xFFFFFF
	recv := word & 0x3F // D25..D30, D25 is the MSB of these six
	d29star &= 1
	d30star &= 1

	d := raw
	if d30star != 0 {
		d = ^raw & 0xFFFFFF // data bits are complemented when D30* = 1
	}

	var computed uint32
	for k := 0; k < 6; k++ {
		prev := d29star
		if prevIsD30[k] {
			prev = d30star
		}
		p := prev ^ parity(d, parityMasks[k])
		computed = (computed << 1) | p
	}
	return d, computed == recv
}
