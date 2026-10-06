package ingest

import (
	"bytes"
	"encoding/hex"
	"reflect"
	"testing"
	"time"
)

// TestNavfeederSolutionEndToEnd runs the real C navfeeder over the golden receiver-solution
// epoch and requires the collector to receive exactly what the Go dial-mode scanner lifts
// off the same bytes: the golden body, decoded identically, stamped by the feeder. This is
// the wire-level cross-oracle for telemetry 0x03 (common/receiver_solution.h on the edge,
// solution.go in the collector). Skips when the binary isn't built.
func TestNavfeederSolutionEndToEnd(t *testing.T) {
	bin := feederBinary(t)
	golden := readGolden(t)
	tow := golden["nav-pvt"][:4]
	var capture []byte
	capture = append(capture, ubxMsg(ubxClassNAV, ubxIDNAVPVT, golden["nav-pvt"])...)
	capture = append(capture, ubxMsg(ubxClassNAV, ubxIDNAVCLOCK, golden["nav-clock"])...)
	capture = append(capture, ubxMsg(ubxClassNAV, ubxIDNAVSTATUS, golden["nav-status"])...)
	capture = append(capture, ubxMsg(ubxClassNAV, ubxIDNAVEOE, tow)...)

	expected, errs := scanSolutions(t, capture)
	if len(expected) != 1 || len(errs) != 0 {
		t.Fatalf("Go scanner: %d frames, errors %v", len(expected), errs)
	}
	before := time.Now()
	got := feederFrames(t, bin, "solution-e2e", capture, 1)[0]
	if got.Solution == nil || got.MsgType != TelemReceiverSolution {
		t.Fatalf("collector frame = %+v", got)
	}
	if !reflect.DeepEqual(got.Solution, expected[0].Solution) {
		t.Fatalf("feeder solution differs from the Go scanner:\n got %+v\nwant %+v", got.Solution, expected[0].Solution)
	}
	if !bytes.Equal(got.Bytes, golden["body"]) {
		t.Fatalf("wire body = %s, want golden %s", hex.EncodeToString(got.Bytes), hex.EncodeToString(golden["body"]))
	}
	if !got.RecvStamped || got.Recv.Before(before.Add(-time.Second)) || got.Recv.After(time.Now()) {
		t.Fatalf("feeder stamp: stamped=%v recv=%v", got.RecvStamped, got.Recv)
	}
}
