package frame

import "testing"

func TestBCNAV2ShortFrame(t *testing.T) {
	if _, err := DecodeBeiDouBCNAV2([]uint32{1, 2, 3}); err != ErrShortFrame {
		t.Errorf("err = %v, want ErrShortFrame", err)
	}
}

func TestBCNAV2Header(t *testing.T) {
	// PRN@0(6)=30, MesType@6(6)=10 → word 0 top 12 bits = 011110 001010.
	words := make([]uint32, 9)
	words[0] = (uint32(30) << 26) | (uint32(10) << 20)
	m, err := DecodeBeiDouBCNAV2(words)
	if err != nil {
		t.Fatal(err)
	}
	if m.PRN != 30 || m.MesType != 10 {
		t.Errorf("PRN=%d MesType=%d, want 30/10", m.PRN, m.MesType)
	}
}
