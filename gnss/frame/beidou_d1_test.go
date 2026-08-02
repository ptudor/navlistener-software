package frame

import "testing"

func d1TestWords(fraID int) []uint32 {
	info := make([]byte, 28)
	setBits(info, 15, 3, uint64(fraID))
	r := NewBitReaderN(info, 224)
	words := make([]uint32, 10)
	v, _ := r.Bits(0, 26)
	words[0] = uint32(v) << 4
	for i := 1; i < 10; i++ {
		v, _ = r.Bits(26+(i-1)*22, 22)
		words[i] = uint32(v) << 8
	}
	StampBeiDouD1BCH(words)
	return words
}

func TestDecodeBeiDouD1BCH(t *testing.T) {
	good := d1TestWords(1)
	if _, err := DecodeBeiDouD1(good); err != nil {
		t.Fatalf("valid BCH frame rejected: %v", err)
	}
	infoFlip := append([]uint32(nil), good...)
	infoFlip[1] ^= 1 << 29
	if _, err := DecodeBeiDouD1(infoFlip); err != ErrBadBCH {
		t.Errorf("info-bit flip error = %v, want ErrBadBCH", err)
	}
	parityFlip := append([]uint32(nil), good...)
	parityFlip[1] ^= 1
	if _, err := DecodeBeiDouD1(parityFlip); err != ErrBadBCH {
		t.Errorf("parity-bit flip error = %v, want ErrBadBCH", err)
	}
}

// TestAssembleBeiDouSOWAdjacency guards D1 carries no AODE-style pairing
// tag, so SOW adjacency (sf1/sf2/sf3 each 6s apart within one 30s D1 frame) is the
// only guard against splicing toe/orbital-element halves from different data
// sets. A spliced sf2/sf3 (e.g. sf2 from a stale broadcast, sf3 from a fresh one)
// must be rejected rather than silently assembled into a garbage ephemeris.
func TestAssembleBeiDouSOWAdjacency(t *testing.T) {
	sf1 := &BeiDouSubframe{FraID: 1, SOW: 100}
	sf2 := &BeiDouSubframe{FraID: 2, SOW: 106}
	sf3 := &BeiDouSubframe{FraID: 3, SOW: 112}
	if _, _, err := AssembleBeiDou(1, sf1, sf2, sf3); err != nil {
		t.Errorf("adjacent triple (100,106,112) rejected: %v", err)
	}

	// sf3 spliced from a broadcast 30s later (e.g. the next frame's subframe 3,
	// after a subframe-2 loss stranded a stale sf2) — must be rejected.
	spliced3 := &BeiDouSubframe{FraID: 3, SOW: 142}
	if _, _, err := AssembleBeiDou(1, sf1, sf2, spliced3); err != errBeiDouSOWGap {
		t.Errorf("err = %v, want errBeiDouSOWGap (sf3 30s late)", err)
	}

	// sf2 spliced from a stale broadcast (e.g. the prior hour's data set) paired
	// with a fresh sf1/sf3 — the exact hourly-changeover failure mode described.
	staleSf2 := &BeiDouSubframe{FraID: 2, SOW: 76} // 24s behind sf1, not +6
	if _, _, err := AssembleBeiDou(1, sf1, staleSf2, sf3); err != errBeiDouSOWGap {
		t.Errorf("err = %v, want errBeiDouSOWGap (sf2 stale)", err)
	}

	// nil inputs still hit the existing short-frame guard, unchanged.
	if _, _, err := AssembleBeiDou(1, nil, sf2, sf3); err != ErrShortFrame {
		t.Errorf("err = %v, want ErrShortFrame for nil sf1", err)
	}
}

// TestAssembleBeiDouRejectsWrongSlots covers the FraID slot assertion: D1
// carries no cross-subframe pairing tag, so a transposed argument list
// whose SOWs still march +6/+6 passes the adjacency rule — only the FraID
// identity check can reject it before toe splices sf2/sf3 fields from the
// wrong pages.
func TestAssembleBeiDouRejectsWrongSlots(t *testing.T) {
	sf1 := &BeiDouSubframe{FraID: 1, SOW: 100}
	sf2 := &BeiDouSubframe{FraID: 2, SOW: 106}
	sf3 := &BeiDouSubframe{FraID: 3, SOW: 112}
	if _, _, err := AssembleBeiDou(1, sf1, sf2, sf3); err != nil {
		t.Fatalf("correct slots rejected: %v", err)
	}
	// The dangerous transposition: subframe 3 then 2 with SOWs crafted to stay
	// +6/+6, so the timing rule cannot catch it.
	x3 := &BeiDouSubframe{FraID: 3, SOW: 106}
	x2 := &BeiDouSubframe{FraID: 2, SOW: 112}
	if _, _, err := AssembleBeiDou(1, sf1, x3, x2); err != ErrWrongMsgType {
		t.Errorf("err = %v, want ErrWrongMsgType (3/2 transposed, clean SOWs)", err)
	}
	// An almanac page (FraID 4) in an ephemeris slot.
	alm := &BeiDouSubframe{FraID: 4, SOW: 106}
	if _, _, err := AssembleBeiDou(1, sf1, alm, sf3); err != ErrWrongMsgType {
		t.Errorf("err = %v, want ErrWrongMsgType (FraID 4 in slot 2)", err)
	}
}

// TestAssembleBeiDouSOWWeekRollover guards the one legitimate D1 frame
// per week straddles the BDT SOW rollover (604794 → 0 → 6) and must assemble;
// the adjacency comparison wraps mod 604800 rather than subtracting raw SOWs.
// Sets that are only "adjacent" under naive modulo arithmetic must still fail,
// and an out-of-domain SOW must be rejected, not normalized.
func TestAssembleBeiDouSOWWeekRollover(t *testing.T) {
	sf1 := &BeiDouSubframe{FraID: 1, SOW: 604794}
	sf2 := &BeiDouSubframe{FraID: 2, SOW: 0}
	sf3 := &BeiDouSubframe{FraID: 3, SOW: 6}
	if _, _, err := AssembleBeiDou(1, sf1, sf2, sf3); err != nil {
		t.Errorf("rollover triple (604794,0,6) rejected: %v", err)
	}

	// sf2 straddling the boundary too: (604788, 604794, 0) is a valid frame.
	early1 := &BeiDouSubframe{FraID: 1, SOW: 604788}
	early2 := &BeiDouSubframe{FraID: 2, SOW: 604794}
	early3 := &BeiDouSubframe{FraID: 3, SOW: 0}
	if _, _, err := AssembleBeiDou(1, early1, early2, early3); err != nil {
		t.Errorf("rollover triple (604788,604794,0) rejected: %v", err)
	}

	// Reordered across the boundary (sf2 from the new week, sf3 from the old)
	// is a splice, not a frame — the wrap must not admit it. Tags are correct
	// per slot so the timing rule, not the FraID identity check, rejects it.
	splice2 := &BeiDouSubframe{FraID: 2, SOW: 6}
	splice3 := &BeiDouSubframe{FraID: 3, SOW: 0}
	if _, _, err := AssembleBeiDou(1, sf1, splice2, splice3); err != errBeiDouSOWGap {
		t.Errorf("err = %v, want errBeiDouSOWGap (reordered rollover)", err)
	}

	// A stale sf2 30 s behind sf1 sits near the boundary under the wrap but is
	// −30 s, not +6 s — still rejected.
	weekStale := &BeiDouSubframe{FraID: 2, SOW: 604764} // 604794 − 30
	if _, _, err := AssembleBeiDou(1, sf1, weekStale, sf3); err != errBeiDouSOWGap {
		t.Errorf("err = %v, want errBeiDouSOWGap (week-boundary stale sf2)", err)
	}

	// SOW 604800 is outside the transmitted domain [0, 604800): a corrupt
	// field, not one second into the new week — sowDelta rejects it even
	// though (604794, 604800, 6) looks adjacent under naive modulo math.
	outOfDomain := &BeiDouSubframe{FraID: 2, SOW: 604800}
	if _, _, err := AssembleBeiDou(1, sf1, outOfDomain, sf3); err != errBeiDouSOWGap {
		t.Errorf("err = %v, want errBeiDouSOWGap (out-of-domain SOW)", err)
	}
}
