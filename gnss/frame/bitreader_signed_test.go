package frame

import (
	"math"
	"math/big"
	"testing"
)

// bigSignedRef computes the two's-complement value of the low n bits of u using
// arbitrary-precision arithmetic (raw - 2^n when the sign bit is set) — the same
// mathematical definition Signed/ConcatSigned implement, but computed in a domain
// that cannot overflow, so it is an independent reference for the overflow-prone
// int64(u) - (1<<n) form regression fix replaced.
func bigSignedRef(u uint64, n int) int64 {
	if n == 0 {
		return 0
	}
	raw := new(big.Int).SetUint64(u)
	if u&(1<<(uint(n)-1)) != 0 {
		raw.Sub(raw, new(big.Int).Lsh(big.NewInt(1), uint(n)))
	}
	if !raw.IsInt64() {
		panic("bigSignedRef: result does not fit in int64")
	}
	return raw.Int64()
}

// TestSignedAndConcatSignedAtN63 guards int64(u) - (1<<n) overflows at
// n==63 (1<<63 == math.MinInt64; subtracting that from a positive int64(u)
// wraps). The two patterns that exercise the overflow are the max-negative bit
// pattern (only the sign bit set) and all-ones (-1); both must match the
// arbitrary-precision reference.
func TestSignedAndConcatSignedAtN63(t *testing.T) {
	buf := make([]byte, 16)
	set := func(u uint64, n int) {
		for i := 0; i < len(buf); i++ {
			buf[i] = 0
		}
		for i := 0; i < n; i++ {
			bit := i
			b := (u >> uint(n-1-i)) & 1
			if b != 0 {
				buf[bit>>3] |= 1 << (7 - uint(bit&7))
			}
		}
	}

	cases := []struct {
		name string
		u    uint64
		n    int
	}{
		{"n63 max-negative (sign bit only)", 1 << 62, 63},
		{"n63 all-ones (-1)", (1 << 63) - 1, 63},
		{"n63 zero", 0, 63},
		{"n64 max-negative", 1 << 63, 64},
		{"n64 all-ones (-1)", math.MaxUint64, 64},
		{"n1 zero", 0, 1},
		{"n1 one (-1)", 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			want := bigSignedRef(c.u, c.n)

			set(c.u, c.n)
			r := NewBitReader(buf)
			got, err := r.Signed(0, c.n)
			if err != nil {
				t.Fatalf("Signed: %v", err)
			}
			if got != want {
				t.Errorf("Signed(%d) = %d, want %d", c.n, got, want)
			}

			// ConcatSigned must agree when the field is split arbitrarily across the
			// hi/lo boundary — split at n/2.
			hiN := c.n / 2
			loN := c.n - hiN
			gotC, err := r.ConcatSigned(0, hiN, hiN, loN)
			if err != nil {
				t.Fatalf("ConcatSigned: %v", err)
			}
			if gotC != want {
				t.Errorf("ConcatSigned(hiN=%d,loN=%d) = %d, want %d", hiN, loN, gotC, want)
			}
		})
	}
}

// FuzzSignedAgainstReference extends the bitreader fuzz coverage to check
// correctness, not just no-panic: for every width 0..64, Signed's result must
// equal the arbitrary-precision reference. This is the fuzz FuzzBitReader was
// missing — it only asserted Signed didn't crash, which would not have caught
// the n=63 overflow.
func FuzzSignedAgainstReference(f *testing.F) {
	f.Add(uint64(1<<62), 63)
	f.Add(uint64((1<<63)-1), 63)
	f.Add(uint64(1<<63), 64)
	f.Fuzz(func(t *testing.T, u uint64, n int) {
		if n < 0 || n > 64 {
			return
		}
		buf := make([]byte, 8)
		for i := 0; i < n; i++ {
			bit := i
			b := (u >> uint(n-1-i)) & 1
			if b != 0 {
				buf[bit>>3] |= 1 << (7 - uint(bit&7))
			}
		}
		mask := uint64(math.MaxUint64)
		if n < 64 {
			mask = (uint64(1) << uint(n)) - 1
		}
		want := bigSignedRef(u&mask, n)

		r := NewBitReader(buf)
		got, err := r.Signed(0, n)
		if err != nil {
			t.Fatalf("Signed(%d): %v", n, err)
		}
		if got != want {
			t.Fatalf("Signed(%d) = %d, want %d (u=%#x)", n, got, want, u&mask)
		}
	})
}
