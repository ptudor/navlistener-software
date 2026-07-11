package frame

import "testing"

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

func TestAssembleBeiDouSOWAdjacencyAcrossWeek(t *testing.T) {
	sf1 := &BeiDouSubframe{FraID: 1, SOW: 604794}
	sf2 := &BeiDouSubframe{FraID: 2, SOW: 0}
	sf3 := &BeiDouSubframe{FraID: 3, SOW: 6}
	if _, _, err := AssembleBeiDou(1, sf1, sf2, sf3); err != nil {
		t.Fatalf("rollover set rejected: %v", err)
	}
	sf2.SOW = 604788
	if _, _, err := AssembleBeiDou(1, sf1, sf2, sf3); err != errBeiDouSOWGap {
		t.Fatalf("reordered set error = %v", err)
	}
	sf2.SOW = 604800
	if _, _, err := AssembleBeiDou(1, sf1, sf2, sf3); err != errBeiDouSOWGap {
		t.Fatalf("out-of-domain SOW error = %v", err)
	}
}
