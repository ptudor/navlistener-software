package frame

import "testing"

// TestGLONASSAlmanacShortFrame confirms the almanac decoder rejects a truncated pair
// (untrusted-input discipline) rather than reading out of bounds.
func TestGLONASSAlmanacShortFrame(t *testing.T) {
	if _, err := DecodeGLONASSAlmanac([]uint32{1, 2, 3}, []uint32{1, 2, 3, 4}, 100); err != ErrShortFrame {
		t.Errorf("first short: err = %v, want ErrShortFrame", err)
	}
	if _, err := DecodeGLONASSAlmanac([]uint32{1, 2, 3, 4}, []uint32{1, 2}, 100); err != ErrShortFrame {
		t.Errorf("second short: err = %v, want ErrShortFrame", err)
	}
	if _, err := DecodeGLONASSFrameNA([]uint32{1, 2, 3}); err != ErrShortFrame {
		t.Errorf("NA short: err = %v, want ErrShortFrame", err)
	}
}

// TestGLONASSHnChannel checks the FDMA channel mapping (ICD Table 4.10) and the
// regression fix rejection of the dead HnA codespace 7..24 (no valid channel under the
// post-2005 frequency plan, ICD §3.3.1.1).
func TestGLONASSHnChannel(t *testing.T) {
	for h, want := range map[int]int{0: 0, 6: 6, 25: -7, 31: -1} {
		got, ok := gloHnToChannel(h)
		if !ok || got != want {
			t.Errorf("gloHnToChannel(%d) = %d/%v, want %d/true", h, got, ok, want)
		}
	}
	for _, h := range []int{7, 15, 24, 32, -1} {
		if _, ok := gloHnToChannel(h); ok {
			t.Errorf("gloHnToChannel(%d) ok = true, want rejection", h)
		}
	}
}
