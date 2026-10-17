package ingest

import (
	"bytes"
	"testing"
)

func TestSolutionV2TimestampProvenance(t *testing.T) {
	for source := StampUnknown; source <= StampFactory; source++ {
		in := &ReceiverSolution{PVT: &SolutionPVT{FixType: 3}, Clock: &SolutionClock{}, Status: &SolutionStatus{}, StampSource: source}
		encoded := EncodeReceiverSolution(in)
		if encoded[0] != 2 || encoded[len(encoded)-1] != source {
			t.Fatalf("bad V2 header/trailer: %x", encoded)
		}
		out, err := decodeReceiverSolution(encoded)
		if err != nil {
			t.Fatal(err)
		}
		if out.StampSource != source || !bytes.Equal(EncodeReceiverSolution(out), encoded) {
			t.Fatalf("provenance lost in persistence/reconstruction: %+v", out)
		}
		if _, err := decodeReceiverSolution(encoded[:len(encoded)-1]); err == nil {
			t.Fatal("truncated V2 accepted")
		}
		encoded[len(encoded)-1] = 0
		if _, err := decodeReceiverSolution(encoded); err == nil {
			t.Fatal("V2 allowed unqualified legacy source")
		}
	}
	legacy := EncodeReceiverSolution(&ReceiverSolution{Clock: &SolutionClock{}})
	if legacy[0] != 1 {
		t.Fatal("legacy encoder changed")
	}
	if _, err := decodeReceiverSolution(legacy); err != nil {
		t.Fatal(err)
	}
}
