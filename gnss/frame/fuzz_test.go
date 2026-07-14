package frame

import (
	"testing"

	"github.com/ptudor/gnss"
)

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
	f.Add(uint32(0x22c00000), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3 uint32) {
		words := []uint32{w0, w1, w2, w3, w0, w1, w2, w3, w0, w1}
		if sf, err := DecodeGPSLNAV(words); err == nil && sf == nil {
			t.Fatal("nil subframe without error")
		}
	})
}

// FuzzGLONASSAlmanac asserts the almanac-pair decoder never panics on arbitrary word
// content — a short or malformed pair returns an error or an in-range struct, never a
// crash or an out-of-bounds bit read.
func FuzzGLONASSAlmanac(f *testing.F) {
	f.Add(uint32(0x60000000), uint32(0), uint32(0), uint32(0),
		uint32(0x70000000), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, a0, a1, a2, a3, b0, b1, b2, b3 uint32) {
		e, err := DecodeGLONASSAlmanac([]uint32{a0, a1, a2, a3}, []uint32{b0, b1, b2, b3}, 100)
		if err != nil {
			return
		}
		if e.Alm.Slot < 0 || e.Alm.Slot > 31 {
			t.Fatalf("slot out of range: %d", e.Alm.Slot)
		}
	})
}

// every decoder gets a fuzz target, not just BitReader/CRC24Q/GPSLNAV/
// GLONASSAlmanac -- "every decoder is fuzzed" (the package doc's own claim) was
// false for 7 of 9 decoders. Each target below follows the existing style: no
// panic on arbitrary word content, and a nil result must come with a non-nil
// error (never a silent zero value mistaken for a decoded message). Go's fuzzing
// engine doesn't support []uint32 directly, so each word is its own fuzzed
// uint32 parameter, assembled into the slice the decoder expects inside the
// closure (the same pattern FuzzGPSLNAV/FuzzGLONASSAlmanac already use).

// FuzzDecodeGPSCNAV asserts the GPS/QZSS CNAV decoder never panics.
func FuzzDecodeGPSCNAV(f *testing.F) {
	f.Add(uint8(gnss.GPS), uint32(0x8B000000), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, id uint8, w0, w1, w2, w3, w4, w5, w6, w7, w8, w9 uint32) {
		words := []uint32{w0, w1, w2, w3, w4, w5, w6, w7, w8, w9}
		if m, err := DecodeGPSCNAV(gnss.GNSSID(id), words); err == nil && m == nil {
			t.Fatal("nil message without error")
		}
	})
}

// FuzzDecodeGalileoINAV asserts the Galileo I/NAV word decoder never panics.
func FuzzDecodeGalileoINAV(f *testing.F) {
	f.Add(uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3, w4, w5, w6, w7 uint32) {
		words := []uint32{w0, w1, w2, w3, w4, w5, w6, w7}
		if m, err := DecodeGalileoINAV(words); err == nil && m == nil {
			t.Fatal("nil message without error")
		}
	})
}

// FuzzDecodeGalileoFNAV asserts the Galileo F/NAV page decoder never panics.
func FuzzDecodeGalileoFNAV(f *testing.F) {
	f.Add(uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3, w4, w5, w6, w7 uint32) {
		words := []uint32{w0, w1, w2, w3, w4, w5, w6, w7}
		if m, err := DecodeGalileoFNAV(words); err == nil && m == nil {
			t.Fatal("nil message without error")
		}
	})
}

// FuzzDecodeGLONASSString asserts the GLONASS nav-string decoder never panics.
func FuzzDecodeGLONASSString(f *testing.F) {
	f.Add(uint32(0x08000000), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3 uint32) {
		words := []uint32{w0, w1, w2, w3}
		if s, err := DecodeGLONASSString(words); err == nil && s == nil {
			t.Fatal("nil string without error")
		}
	})
}

// FuzzDecodeGLONASSFrameNA asserts the GLONASS frame-day (string 5) decoder never
// panics.
func FuzzDecodeGLONASSFrameNA(f *testing.F) {
	f.Add(uint32(0), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3 uint32) {
		words := []uint32{w0, w1, w2, w3}
		_, _ = DecodeGLONASSFrameNA(words)
	})
}

// FuzzDecodeBeiDouD1 asserts the BeiDou D1 subframe decoder never panics.
func FuzzDecodeBeiDouD1(f *testing.F) {
	f.Add(uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3, w4, w5, w6, w7, w8, w9 uint32) {
		words := []uint32{w0, w1, w2, w3, w4, w5, w6, w7, w8, w9}
		if sf, err := DecodeBeiDouD1(words); err == nil && sf == nil {
			t.Fatal("nil subframe without error")
		}
	})
}

// FuzzDecodeBeiDouBCNAV2 asserts the BeiDou B-CNAV2 message decoder never panics.
func FuzzDecodeBeiDouBCNAV2(f *testing.F) {
	f.Add(uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, w0, w1, w2, w3, w4, w5, w6, w7, w8 uint32) {
		words := []uint32{w0, w1, w2, w3, w4, w5, w6, w7, w8}
		if m, err := DecodeBeiDouBCNAV2(words); err == nil && m == nil {
			t.Fatal("nil message without error")
		}
	})
}

// FuzzDecodeSBASL1 asserts the SBAS L1 message decoder never panics.
func FuzzDecodeSBASL1(f *testing.F) {
	f.Add(1, uint32(0x53000000), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, prn int, w0, w1, w2, w3, w4, w5, w6, w7 uint32) {
		words := []uint32{w0, w1, w2, w3, w4, w5, w6, w7}
		if m, err := DecodeSBASL1(prn, words); err == nil && m == nil {
			t.Fatal("nil message without error")
		}
	})
}

// FuzzGPSParity asserts the LNAV Hamming-parity check never panics on arbitrary
// words -- it's the untrusted-input entry point every LNAV word passes through
// before any field is read.
func FuzzGPSParity(f *testing.F) {
	f.Add(uint32(0x22c000), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, word, d29star, d30star uint32) {
		_, _ = GPSParity(word, d29star, d30star)
	})
}
