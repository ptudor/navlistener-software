package frame

import "testing"

func TestBitReaderMSBFirst(t *testing.T) {
	// 0xA5 = 1010_0101. Read from bit 0.
	r := NewBitReader([]byte{0xA5})
	if v, _ := r.Bits(0, 4); v != 0xA { // 1010
		t.Errorf("Bits(0,4) = %#x, want 0xA", v)
	}
	if v, _ := r.Bits(4, 4); v != 0x5 { // 0101
		t.Errorf("Bits(4,4) = %#x, want 0x5", v)
	}
	if v, _ := r.Bits(0, 8); v != 0xA5 {
		t.Errorf("Bits(0,8) = %#x, want 0xA5", v)
	}
}

func TestBitReaderCrossByte(t *testing.T) {
	// 0x0F 0xF0 → bits 4..12 span the byte boundary: 1111_1111 = 0xFF.
	r := NewBitReader([]byte{0x0F, 0xF0})
	if v, _ := r.Bits(4, 8); v != 0xFF {
		t.Errorf("cross-byte Bits(4,8) = %#x, want 0xFF", v)
	}
}

func TestBitReaderSigned(t *testing.T) {
	// 4-bit two's complement: 1111 = −1, 1000 = −8, 0111 = +7.
	r := NewBitReader([]byte{0xF8, 0x70})
	if v, _ := r.Signed(0, 4); v != -1 {
		t.Errorf("Signed 1111 = %d, want -1", v)
	}
	if v, _ := r.Signed(4, 4); v != -8 {
		t.Errorf("Signed 1000 = %d, want -8", v)
	}
	if v, _ := r.Signed(8, 4); v != 7 {
		t.Errorf("Signed 0111 = %d, want 7", v)
	}
}

func TestBitReaderSignMag(t *testing.T) {
	// sign-magnitude 4-bit: 1001 = −1, 0011 = +3.
	r := NewBitReader([]byte{0x93})
	if v, _ := r.SignMag(0, 4); v != -1 {
		t.Errorf("SignMag 1001 = %d, want -1", v)
	}
	if v, _ := r.SignMag(4, 4); v != 3 {
		t.Errorf("SignMag 0011 = %d, want 3", v)
	}
}

func TestBitReaderConcat(t *testing.T) {
	// hi=0xA (bits 0..4), lo=0x5 (bits 4..8) → 0xA5.
	r := NewBitReader([]byte{0xA5})
	if v, _ := r.Concat(0, 4, 4, 4); v != 0xA5 {
		t.Errorf("Concat = %#x, want 0xA5", v)
	}
}

func TestBitReaderBounds(t *testing.T) {
	r := NewBitReader([]byte{0xFF})
	for _, c := range []struct{ start, n int }{{0, 9}, {8, 1}, {-1, 4}, {0, 65}, {0, -1}} {
		if _, err := r.Bits(c.start, c.n); err != ErrOutOfRange {
			t.Errorf("Bits(%d,%d) err = %v, want ErrOutOfRange", c.start, c.n, err)
		}
	}
}

func TestCRC24QKnownAnswer(t *testing.T) {
	if got := CRC24Q([]byte("123456789")); got != 0xCDE703 {
		t.Errorf("CRC-24Q(123456789) = %#06X, want 0xCDE703", got)
	}
	if got := CRC24Q(nil); got != 0 {
		t.Errorf("CRC-24Q(empty) = %#X, want 0", got)
	}
}

func TestCheckCRC24QRoundTrip(t *testing.T) {
	msg := []byte{0x11, 0x22, 0x33, 0x44}
	c := CRC24Q(msg)
	full := append(msg, byte(c>>16), byte(c>>8), byte(c))
	if !CheckCRC24Q(full) {
		t.Error("valid CRC-24Q frame should check out")
	}
	full[0] ^= 0x01 // corrupt a bit
	if CheckCRC24Q(full) {
		t.Error("corrupted frame must fail CRC-24Q")
	}
	if CheckCRC24Q([]byte{0x00, 0x00}) {
		t.Error("a buffer shorter than the CRC can't be valid")
	}
}

// validParity finds the unique 6-bit parity value that makes GPSParity accept a
// word with the given 24 data bits and previous parity bits, treating GPSParity as
// the oracle. Exactly one such value must exist (parity is deterministic).
func validParity(t *testing.T, data24, d29s, d30s uint32) uint32 {
	t.Helper()
	found := -1
	for p := uint32(0); p < 64; p++ {
		word := (data24 << 6) | p
		if _, ok := GPSParity(word, d29s, d30s); ok {
			if found >= 0 {
				t.Fatalf("more than one valid parity for data %#x", data24)
			}
			found = int(p)
		}
	}
	if found < 0 {
		t.Fatalf("no valid parity found for data %#x", data24)
	}
	return uint32(found)
}

func TestGPSParityAcceptsAndDetectsErrors(t *testing.T) {
	// With D30* = 0 (no complement), build a valid word and check acceptance,
	// then confirm every single-bit flip across all 30 bits breaks parity.
	data := uint32(0x5A5A5A) & 0xFFFFFF
	p := validParity(t, data, 0, 0)
	word := (data << 6) | p
	if _, ok := GPSParity(word, 0, 0); !ok {
		t.Fatal("constructed word should pass parity")
	}
	for b := 0; b < 30; b++ {
		flipped := word ^ (1 << uint(b))
		if _, ok := GPSParity(flipped, 0, 0); ok {
			t.Errorf("single-bit flip at %d not detected", b)
		}
	}
}

func TestGPSParityD30Complement(t *testing.T) {
	// When D30* = 1 the data bits are complemented; GPSParity must return the
	// un-complemented data.
	data := uint32(0x123456) & 0xFFFFFF
	p := validParity(t, data, 0, 1) // build under D30*=1
	word := (data << 6) | p
	got, ok := GPSParity(word, 0, 1)
	if !ok {
		t.Fatal("word should pass parity under D30*=1")
	}
	if got != (^data & 0xFFFFFF) {
		t.Errorf("D30*=1 data = %#x, want complemented %#x", got, ^data&0xFFFFFF)
	}
}
