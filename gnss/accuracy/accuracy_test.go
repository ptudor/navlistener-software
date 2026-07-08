package accuracy

import (
	"math"
	"testing"
)

func TestURAMeters(t *testing.T) {
	// The IS-GPS-200 nominal URA values at representative indices.
	cases := map[int]float64{0: 2.0, 1: 2.8284, 2: 4.0, 4: 8.0, 6: 16.0, 7: 32.0, 14: 4096.0}
	for n, want := range cases {
		got, ok := URAMeters(n)
		if !ok {
			t.Errorf("URA %d: unexpectedly invalid", n)
			continue
		}
		if math.Abs(got-want) > 1e-3 {
			t.Errorf("URA %d = %v, want %v", n, got, want)
		}
	}
	if _, ok := URAMeters(15); ok {
		t.Error("URA 15 must be 'no accuracy' (invalid)")
	}
	if _, ok := URAMeters(-1); ok {
		t.Error("negative URA must be invalid")
	}
}

func TestGalileoSISABands(t *testing.T) {
	cases := map[int]float64{
		0: 0.0, 49: 0.49, // band 1
		50: 0.5, 74: 0.98, // band 2
		75: 1.0, 99: 1.96, // band 3
		100: 2.0, 125: 6.0, // band 4
	}
	for n, want := range cases {
		got, ok := GalileoSISA(n)
		if !ok || math.Abs(got-want) > 1e-9 {
			t.Errorf("SISA %d = %v (ok=%v), want %v", n, got, ok, want)
		}
	}
	if _, ok := GalileoSISA(255); ok {
		t.Error("SISA 255 must be 'NO SISA AVAILABLE' (invalid)")
	}
	if _, ok := GalileoSISA(200); ok {
		t.Error("SISA in the 126–254 spare range must be invalid")
	}
}

func TestGlonassFT(t *testing.T) {
	if v, ok := GlonassFT(0); !ok || v != 1.0 {
		t.Errorf("F_T 0 = %v (%v), want 1.0", v, ok)
	}
	if v, ok := GlonassFT(6); !ok || v != 10.0 {
		t.Errorf("F_T 6 = %v, want 10.0", v)
	}
	if _, ok := GlonassFT(15); ok {
		t.Error("F_T 15 must be 'not used' (invalid)")
	}
}

// TestURAMonotonic sanity-checks that the decoded accuracy is non-decreasing in
// the index (a worse index is never a better accuracy).
func TestURAMonotonic(t *testing.T) {
	prev := 0.0
	for n := 0; n <= 14; n++ {
		v, _ := URAMeters(n)
		if v < prev {
			t.Errorf("URA not monotonic at %d: %v < %v", n, v, prev)
		}
		prev = v
	}
}
