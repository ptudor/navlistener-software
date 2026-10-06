package ingest

import (
	"encoding/hex"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestStoredFrameRebuildsEachKind(t *testing.T) {
	local := time.Unix(1_800_000_000, 0)
	stamp := local.Add(-300 * time.Millisecond)
	text, err := os.ReadFile("../../../testdata/observer_details_v1.hex")
	if err != nil {
		t.Fatal(err)
	}
	details, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	sol := sampleSolution(1000)
	sats := []SatCN0{{GnssID: 0, SvID: 4, Cn0: 41, ElevDeg: 30, Used: true}}
	bands := []RFBand{{Block: 0, AGC: 3000, JamState: 1, AntStatus: 2}}
	cases := []struct {
		in    StoredInput
		check func(*RawFrame) bool
	}{
		{StoredInput{Origin: "rf", Kind: "solution", Raw: EncodeReceiverSolution(sol)},
			func(f *RawFrame) bool {
				return reflect.DeepEqual(f.Solution, sol) && f.MsgType == TelemReceiverSolution
			}},
		{StoredInput{Origin: "rf", Kind: "reception", Raw: EncodeReceptionData(sats)},
			func(f *RawFrame) bool { return reflect.DeepEqual(f.RF.Sats, sats) }},
		{StoredInput{Origin: "rf", Kind: "jamming", Raw: EncodeJammingStats(bands)},
			func(f *RawFrame) bool { return reflect.DeepEqual(f.RF.Bands, bands) }},
		{StoredInput{Origin: "board", Kind: "environment", Raw: details},
			func(f *RawFrame) bool { return f.Details != nil && f.MsgType == TelemObserverDetails }},
	}
	for _, tc := range cases {
		in := tc.in
		in.Source, in.Local, in.WallClock, in.Session, in.Seq, in.HasSeq = "obs", local, &stamp, "boot", 9, true
		f, err := StoredFrame(in)
		if err != nil || !tc.check(f) {
			t.Fatalf("%s/%s: %+v %v", in.Origin, in.Kind, f, err)
		}
		if !f.LocalRecv().Equal(local) || f.Source != "obs" || f.Session != "boot" || f.Seq != 9 || !f.HasSeq {
			t.Fatalf("%s/%s: receipt context %+v", in.Origin, in.Kind, f)
		}
		if got, ok := f.WallClockStamp(); !ok || !got.Equal(stamp) || !f.BoardSampleStamped {
			t.Fatalf("%s/%s: wall clock %v %v", in.Origin, in.Kind, got, ok)
		}
		in.WallClock = nil
		f, err = StoredFrame(in)
		if _, ok := f.WallClockStamp(); err != nil || ok || !f.Recv.Equal(local) {
			t.Fatalf("%s/%s without a stamp: %+v %v", in.Origin, in.Kind, f, err)
		}
	}
}

func TestStoredFrameRejects(t *testing.T) {
	for _, in := range []StoredInput{
		{Origin: "rf", Kind: "combined", Raw: []byte(`{}`)},
		{Origin: "nav", Kind: "x"},
		{Origin: "rf", Kind: "solution", Raw: []byte{1, 0}},
		{Origin: "board", Kind: "timing", Raw: []byte{1}},
	} {
		if _, err := StoredFrame(in); err == nil {
			t.Errorf("%s/%s accepted", in.Origin, in.Kind)
		}
	}
}
