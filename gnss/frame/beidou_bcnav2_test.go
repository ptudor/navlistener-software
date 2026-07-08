package frame

import "testing"

// TestBCNAV2IsFlaggedUnimplemented pins the compliance-gap contract: the decoder
// reads the header message type but returns ErrBCNAV2Unimplemented rather than a
// fabricated ephemeris. When the BDS-SIS-ICD-B2a §6.2 layout is implemented, this
// test is replaced by a real cross-check against the D1 decoder (see the package
// doc in beidou_bcnav2.go).
func TestBCNAV2IsFlaggedUnimplemented(t *testing.T) {
	// A minimal 9-word frame with message type 10 in bits 6..11 (0b001010).
	words := make([]uint32, 9)
	words[0] = uint32(10) << (32 - 12) // MesType at bit 6, width 6

	got, err := DecodeBeiDouBCNAV2(words)
	if err != ErrBCNAV2Unimplemented {
		t.Fatalf("err = %v, want ErrBCNAV2Unimplemented (the flagged gap)", err)
	}
	if got == nil || got.MesType != 10 {
		t.Fatalf("MesType = %v, want the reliably-read header value 10", got)
	}
}

func TestBCNAV2ShortFrame(t *testing.T) {
	if _, err := DecodeBeiDouBCNAV2([]uint32{1, 2, 3}); err != ErrShortFrame {
		t.Errorf("err = %v, want ErrShortFrame", err)
	}
}
