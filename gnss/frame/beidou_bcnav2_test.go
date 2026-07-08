package frame

import (
	"encoding/binary"
	"testing"
)

func TestBCNAV2ShortFrame(t *testing.T) {
	if _, err := DecodeBeiDouBCNAV2([]uint32{1, 2, 3}); err != ErrShortFrame {
		t.Errorf("err = %v, want ErrShortFrame", err)
	}
}

// TestBCNAV2RejectsBadCRC confirms a frame failing its CRC-24Q is rejected, never
// partially decoded (the real capture's frames all pass — see TestRealBeiDouBCNAV2).
func TestBCNAV2RejectsBadCRC(t *testing.T) {
	words := make([]uint32, 9)
	words[0] = (uint32(30) << 26) | (uint32(10) << 20) // PRN 30, MesType 10, junk CRC
	if _, err := DecodeBeiDouBCNAV2(words); err != ErrBadCRC {
		t.Errorf("err = %v, want ErrBadCRC", err)
	}
}

// TestBCNAV2AcceptsValidCRC builds a CRC-24Q-valid frame and checks the header
// decodes (message type 10).
func TestBCNAV2AcceptsValidCRC(t *testing.T) {
	buf := make([]byte, 36)
	// PRN 30 @0(6), MesType 10 @6(6): top 12 bits = 011110 001010.
	binary.BigEndian.PutUint32(buf[0:], (uint32(30)<<26)|(uint32(10)<<20))
	// Append a valid CRC-24Q over the leading 264 bits into the trailing 24.
	c := CRC24Q(buf[:33])
	buf[33] = byte(c >> 16)
	buf[34] = byte(c >> 8)
	buf[35] = byte(c)
	words := make([]uint32, 9)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	m, err := DecodeBeiDouBCNAV2(words)
	if err != nil {
		t.Fatalf("valid-CRC frame rejected: %v", err)
	}
	if m.PRN != 30 || m.MesType != 10 {
		t.Errorf("PRN=%d MesType=%d, want 30/10", m.PRN, m.MesType)
	}
}

// TestBCNAV2PairAdjacency confirms the type-10/11 assembler rejects a stale pair:
// type 11 carries no IODE, so broadcast adjacency (±3 s) is the only guard against
// stitching elements across an ephemeris changeover.
func TestBCNAV2PairAdjacency(t *testing.T) {
	m10 := &BeiDouBCNAV2{MesType: 10, SOW: 100}
	m11 := &BeiDouBCNAV2{MesType: 11, SOW: 103, hasEph2: true}
	if _, _, err := AssembleBeiDouBCNAV2(28, m10, m11, nil); err != nil {
		t.Errorf("adjacent pair rejected: %v", err)
	}
	m11.SOW = 130 // a type 11 from a previous broadcast group
	if _, _, err := AssembleBeiDouBCNAV2(28, m10, m11, nil); err != errPairSOW {
		t.Errorf("err = %v, want errPairSOW", err)
	}
}
