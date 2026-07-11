package frame

import (
	"math/rand"
	"testing"
)

// TestCRC24QBitsMatchesByteWiseCRC24Q guards regression fix/CRC24QBits (used where
// the ICD's CRC coverage isn't byte-aligned, e.g. GPS/QZSS CNAV) must agree with
// the existing, already-relied-upon byte-wise CRC24Q for a byte-aligned,
// whole-byte range — this is the ground-truth check that the bit-serial
// reimplementation is correct, not just plausible.
func TestCRC24QBitsMatchesByteWiseCRC24Q(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	for _, n := range []int{1, 3, 8, 9, 36, 40} {
		buf := make([]byte, n)
		r.Read(buf)
		want := CRC24Q(buf)
		got := CRC24QBits(buf, 0, n*8)
		if got != want {
			t.Errorf("len=%d: CRC24QBits = 0x%06x, want 0x%06x (CRC24Q)", n, got, want)
		}
	}
}

// TestCheckCRC24QBitsRejectsFlippedBit guards regression fix/actual use: a
// self-consistent (data+CRC) bit range passes, and flipping any single bit in
// it (data or CRC) must fail.
func TestCheckCRC24QBitsRejectsFlippedBit(t *testing.T) {
	buf := make([]byte, 40)
	r := rand.New(rand.NewSource(2))
	r.Read(buf[:37]) // leave room to overwrite the last 3 bytes with a real CRC
	payload := buf[:37]
	crc := CRC24Q(payload)
	buf[37] = byte(crc >> 16)
	buf[38] = byte(crc >> 8)
	buf[39] = byte(crc)

	if !CheckCRC24QBits(buf, 0, 320) {
		t.Fatal("self-consistent buffer must pass CheckCRC24QBits")
	}
	for bit := 0; bit < 320; bit += 37 { // sample across the whole range, not exhaustively
		flipped := append([]byte(nil), buf...)
		flipped[bit>>3] ^= 1 << uint(7-(bit&7))
		if CheckCRC24QBits(flipped, 0, 320) {
			t.Errorf("flipping bit %d must fail the CRC check", bit)
		}
	}
}
