package frame

import "testing"

// FuzzBitReader asserts the reader never panics or reads out of bounds for any
// input bytes and any offset/length — the untrusted-input contract
// (docs/INTEGRITY.md §9). Reads that fall outside the frame must return
// ErrOutOfRange, not crash.
func FuzzBitReader(f *testing.F) {
	f.Add([]byte{0xA5, 0x5A}, 3, 10)
	f.Add([]byte{}, 0, 8)
	f.Add([]byte{0xFF}, -1, 100)
	f.Fuzz(func(t *testing.T, data []byte, start, n int) {
		r := NewBitReader(data)
		if v, err := r.Bits(start, n); err == nil {
			// A successful read must not exceed the requested width.
			if n < 64 && v >= (1<<uint(n)) {
				t.Fatalf("Bits(%d,%d) = %#x exceeds width", start, n, v)
			}
		}
		_, _ = r.Signed(start, n)
		_, _ = r.SignMag(start, n)
		_, _ = r.Concat(start, n, start, n)

		// A bounded reader must never allow a read past its limit.
		rn := NewBitReaderN(data, n)
		if rn.Len() > len(data)*8 {
			t.Fatalf("NewBitReaderN len %d exceeds data", rn.Len())
		}
	})
}

// FuzzCRC24Q asserts the checksum never panics on arbitrary input.
func FuzzCRC24Q(f *testing.F) {
	f.Add([]byte("123456789"))
	f.Fuzz(func(t *testing.T, data []byte) {
		if got := CRC24Q(data); got > 0xFFFFFF {
			t.Fatalf("CRC-24Q result %#x exceeds 24 bits", got)
		}
		_ = CheckCRC24Q(data)
	})
}

// FuzzGPSLNAV asserts the LNAV subframe decoder never panics on arbitrary word
// content — malformed frames must return an error (parity/short), never crash.
func FuzzGPSLNAV(f *testing.F) {
	f.Add(uint32(0x22c000), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3 uint32) {
		words := []uint32{w0, w1, w2, w3, w0, w1, w2, w3, w0, w1}
		if sf, err := DecodeGPSLNAV(words); err == nil && sf == nil {
			t.Fatal("nil subframe without error")
		}
	})
}
