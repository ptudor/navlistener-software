package frame

import (
	"errors"
	"testing"
)

// TestDecodeNavICSPSDeferred documents that NavIC is a known, explicitly-deferred stub
// : the placeholder decoder always reports the deferral, never a spurious success, so a
// future wiring mistake that calls it expecting a decode fails loudly.
func TestDecodeNavICSPSDeferred(t *testing.T) {
	if err := DecodeNavICSPS([]uint32{0, 0, 0, 0, 0, 0, 0, 0}); !errors.Is(err, ErrNavICDeferred) {
		t.Fatalf("DecodeNavICSPS = %v, want ErrNavICDeferred", err)
	}
}
