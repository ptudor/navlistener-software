// Package frame decodes raw broadcast navigation frames — the untrusted bytes a
// receiver hands us — into the typed nav messages the propagators consume. Every
// length, index, and bit read is bounds-checked and every decoder is fuzzed
// (docs/CONSTELLATIONS.md §2, docs/INTEGRITY.md §9).
//
// The per-constellation frame layouts and the integrity checks (Hamming parity,
// CRC-24Q, BCH) live in this package; the field meanings are docs/MATH.md.
package frame

import "errors"

// ErrOutOfRange is returned by BitReader when a read would fall outside the frame.
var ErrOutOfRange = errors.New("frame: bit read out of range")

// BitReader extracts fields from a navigation frame in MSB-first bit order — the
// order the ICDs specify — over a fixed byte slice. All reads are by absolute bit
// offset (ICD field maps are naturally expressed that way) and are bounds-checked,
// so a short or malformed frame yields an error, never a panic or an out-of-bounds
// read.
type BitReader struct {
	data  []byte
	nbits int
}

// NewBitReader wraps data, treating all of it as readable bits.
func NewBitReader(data []byte) *BitReader {
	return &BitReader{data: data, nbits: len(data) * 8}
}

// NewBitReaderN wraps data but limits reads to the first nbits (for a frame whose
// meaningful length is not a whole number of bytes). nbits is clamped to the
// slice length.
func NewBitReaderN(data []byte, nbits int) *BitReader {
	max := len(data) * 8
	if nbits < 0 {
		nbits = 0
	}
	if nbits > max {
		nbits = max
	}
	return &BitReader{data: data, nbits: nbits}
}

// Len returns the number of readable bits.
func (r *BitReader) Len() int { return r.nbits }

// Bits reads n bits (0..64) starting at absolute bit offset start, MSB-first, and
// returns them right-aligned in a uint64. It errors if the range is invalid or
// extends past the frame.
func (r *BitReader) Bits(start, n int) (uint64, error) {
	// start > r.nbits must be checked (and short-circuit) before start+n is
	// ever computed -- an attacker-supplied start near math.MaxInt overflows the
	// signed-int addition, wrapping start+n negative and slipping past the bound
	// check into an out-of-bounds r.data index. Once start <= r.nbits is
	// established here, start+n cannot overflow (nbits is a real frame's bit
	// length and n <= 64).
	if n < 0 || n > 64 || start < 0 || start > r.nbits || start+n > r.nbits {
		return 0, ErrOutOfRange
	}
	var v uint64
	for i := 0; i < n; i++ {
		bit := start + i
		b := (r.data[bit>>3] >> (7 - uint(bit&7))) & 1
		v = (v << 1) | uint64(b)
	}
	return v, nil
}

// Signed reads n bits at start as a two's-complement signed integer.
func (r *BitReader) Signed(start, n int) (int64, error) {
	u, err := r.Bits(start, n)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	// sign-extend via a left-then-arithmetic-right shift rather than
	// int64(u) - (1<<n) — the subtraction form overflows at n==63 (1<<63 ==
	// math.MinInt64, and subtracting that from a positive int64(u) wraps). The
	// shift form is exact for every 1 <= n <= 64.
	return int64(u<<(64-uint(n))) >> (64 - uint(n)), nil
}

// SignMag reads n bits at start as a sign-magnitude integer: the top bit is the
// sign, the remaining n−1 bits the magnitude. Several GLONASS and BeiDou fields
// use this encoding rather than two's complement (docs/MATH.md, per-ICD).
func (r *BitReader) SignMag(start, n int) (int64, error) {
	u, err := r.Bits(start, n)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, nil
	}
	mag := int64(u & ((1 << (uint(n) - 1)) - 1))
	if u&(1<<(uint(n)-1)) != 0 {
		return -mag, nil
	}
	return mag, nil
}

// Concat reads two bit ranges and concatenates them with the first as the high
// part — for a field the ICD splits across non-adjacent words (common in LNAV and
// GLONASS). The result is (hi << loN) | lo.
func (r *BitReader) Concat(hiStart, hiN, loStart, loN int) (uint64, error) {
	// reject before shifting -- with loN == 64, hi << 64 is 0 in Go (shift
	// amounts >= the operand's bit width are defined to yield 0, not an error), so
	// the result would silently become just lo with no signal that hiN bits were
	// dropped; any total width in (64,128] is likewise wrong instead of erroring,
	// unlike Bits's own n > 64 rejection.
	if hiN+loN > 64 {
		return 0, ErrOutOfRange
	}
	hi, err := r.Bits(hiStart, hiN)
	if err != nil {
		return 0, err
	}
	lo, err := r.Bits(loStart, loN)
	if err != nil {
		return 0, err
	}
	return (hi << uint(loN)) | lo, nil
}

// ConcatSigned is Concat interpreted as a two's-complement value of width hiN+loN.
func (r *BitReader) ConcatSigned(hiStart, hiN, loStart, loN int) (int64, error) {
	u, err := r.Concat(hiStart, hiN, loStart, loN)
	if err != nil {
		return 0, err
	}
	n := hiN + loN
	if n == 0 {
		return 0, nil
	}
	// same overflow-safe sign-extend as Signed above.
	return int64(u<<(64-uint(n))) >> (64 - uint(n)), nil
}
