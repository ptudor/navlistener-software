package frame

import (
	"math"
	"testing"
)

// TestBitsRejectsOverflowingStart guards BitReader.Bits's bounds check used
// to compute start+n before validating start against r.nbits, so a start near
// math.MaxInt overflowed the signed addition and could slip past the check into an
// out-of-bounds r.data index (the finding's PoC:
// NewBitReader([]byte{0xFF,0xFF}).Bits(math.MaxInt-32, 64) panicked). Every read
// whose start already exceeds the frame must error before any addition happens.
func TestBitsRejectsOverflowingStart(t *testing.T) {
	r := NewBitReader([]byte{0xFF, 0xFF})
	for _, tt := range []struct {
		name  string
		start int
		n     int
	}{
		{"the finding's exact PoC", math.MaxInt - 32, 64},
		{"fix spec case 1", math.MaxInt - 1, 64},
		{"fix spec case 2", math.MaxInt, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := r.Bits(tt.start, tt.n); err != ErrOutOfRange {
				t.Errorf("Bits(%d, %d) = %v, want ErrOutOfRange", tt.start, tt.n, err)
			}
		})
	}
}

// TestBitsValidReadsUnaffected confirms the fix changes no result for any
// valid read: once start <= nbits is established, start+n cannot overflow, so the
// added start > r.nbits check must be a pure no-op for in-range reads.
func TestBitsValidReadsUnaffected(t *testing.T) {
	r := NewBitReader([]byte{0xA5, 0x3C})
	got, err := r.Bits(4, 8)
	if err != nil {
		t.Fatalf("Bits(4,8): %v", err)
	}
	if want := uint64(0x53); got != want {
		t.Errorf("Bits(4,8) = %#x, want %#x", got, want)
	}
	// The exact boundary: start+n == nbits is valid (reads the last bit).
	if _, err := r.Bits(15, 1); err != nil {
		t.Errorf("Bits(15,1) at the exact boundary: %v", err)
	}
	// One bit past the end must still be rejected (unchanged existing behavior).
	if _, err := r.Bits(16, 1); err != ErrOutOfRange {
		t.Errorf("Bits(16,1) past the end = %v, want ErrOutOfRange", err)
	}
}

// TestConcatRejectsOverwideTotal guards Concat/ConcatSigned silently
// truncated when hiN+loN > 64 -- with loN == 64, Go defines hi << 64 as 0 (shift
// amounts >= the operand's bit width yield 0, not a compile/runtime error), so the
// result silently became just lo with hiN bits dropped and no error, unlike Bits's
// own n > 64 rejection.
func TestConcatRejectsOverwideTotal(t *testing.T) {
	r := NewBitReader(make([]byte, 32)) // 256 bits, plenty for any width tested here
	if _, err := r.Concat(0, 64, 0, 64); err != ErrOutOfRange {
		t.Errorf("Concat(0,64,0,64) = %v, want ErrOutOfRange (the fix spec's exact case)", err)
	}
	if _, err := r.Concat(0, 40, 0, 40); err != ErrOutOfRange {
		t.Errorf("Concat(0,40,0,40) (total 80 > 64) = %v, want ErrOutOfRange", err)
	}
	if _, err := r.ConcatSigned(0, 64, 0, 64); err != ErrOutOfRange {
		t.Errorf("ConcatSigned(0,64,0,64) = %v, want ErrOutOfRange", err)
	}
	// A total width of exactly 64 (the boundary) must still succeed -- must not
	// change results for any valid width <= 64.
	if _, err := r.Concat(0, 32, 32, 32); err != nil {
		t.Errorf("Concat(0,32,32,32) (total exactly 64) = %v, want success", err)
	}
}
