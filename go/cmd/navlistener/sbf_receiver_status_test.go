package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

// sbfBenchStatus is a ReceiverStatus block (4014, revision 1) carrying the fields a
// mosaic-G5 P3 on firmware 1.0.0 reported with an L1/L5 antenna and a fix. The observer
// logged the decoded fields, not the bytes, so the block was packed from them, with its
// own time stamp, and CRC'd by Python's struct and binascii.crc_hqx.
const sbfBenchStatus = "24401a91ae2f440000ccbf198d090500b1020000300000000000000009041d7f" +
	"05246000062e640103336801043266000c32600002336b0100286601012c64000b296400"

func scanOne(t *testing.T, block []byte, at time.Time) *ingest.RawFrame {
	t.Helper()
	var frames []*ingest.RawFrame
	err := ingest.ReplaySBF(bytes.NewReader(block), "p3", func() time.Time { return at },
		func(f *ingest.RawFrame) { frames = append(frames, f) }, func(kind string) { t.Errorf("scan: %s", kind) })
	if err != nil || len(frames) != 1 {
		t.Fatalf("scanner frames=%d err=%v", len(frames), err)
	}
	return frames[0]
}

// A ReceiverStatus block is receiver telemetry: it is stored with the RF evidence the
// station checks replay, as the whole block, and the stored row rebuilds the frame live
// state applied.
func TestReceiverStatusPersistsAsRFEvidence(t *testing.T) {
	block, err := hex.DecodeString(sbfBenchStatus)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Truncate(time.Microsecond)
	f := scanOne(t, block, at)
	if frameKind(f) != "rf" {
		t.Fatalf("frame kind %q, want rf", frameKind(f))
	}
	saved := frameForPersistence(f)
	if saved.RF == nil || saved.RF.Kind != "receiver_status" || saved.Board != nil {
		t.Fatalf("persisted as %+v, want an rf sample of kind receiver_status", saved.RF)
	}
	if !bytes.Equal(saved.Raw, block) {
		t.Fatalf("raw %x is not the whole block", saved.Raw)
	}
	if !saved.RF.LocalReceivedAt.Equal(at) || saved.RF.WallClockStamp == nil || !saved.RF.WallClockStamp.Equal(at) || !saved.ReceivedAt.Equal(at) {
		t.Fatalf("clocks: local %v stamp %v received %v", saved.RF.LocalReceivedAt, saved.RF.WallClockStamp, saved.ReceivedAt)
	}

	var data struct {
		UptimeS      uint32 `json:"uptime_s"`
		TemperatureC *int   `json:"temperature_c"`
		CPULoad      *int   `json:"cpu_load_percent"`
		RxState      uint32 `json:"rx_state"`
		FrontEnds    []struct {
			ID             int  `json:"id"`
			Locked         bool `json:"locked"`
			GainDB         *int `json:"gain_db"`
			SampleVariance *int `json:"sample_variance"`
			Blanked        int  `json:"blanked_percent"`
		} `json:"front_ends"`
	}
	if err := json.Unmarshal(saved.RF.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.UptimeS != 689 || data.TemperatureC == nil || *data.TemperatureC != 27 || data.CPULoad == nil || *data.CPULoad != 5 ||
		data.RxState != 0x30 || len(data.FrontEnds) != 9 {
		t.Fatalf("decoded projection = %s", saved.RF.Data)
	}
	// The receiver's order: L5 first, then E5b; L1 is seventh.
	if first, l1 := data.FrontEnds[0], data.FrontEnds[6]; first.ID != 5 || *first.GainDB != 36 || l1.ID != 0 || *l1.GainDB != 40 ||
		*l1.SampleVariance != 102 || l1.Blanked != 1 || !l1.Locked {
		t.Fatalf("front ends = %s", saved.RF.Data)
	}

	replayed, err := ingest.StoredFrame(ingest.StoredInput{Origin: "rf", Kind: saved.RF.Kind, Raw: saved.Raw, Source: saved.SourceID,
		Local: saved.RF.LocalReceivedAt, WallClock: saved.RF.WallClockStamp})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replayed.RF, f.RF) || !bytes.Equal(replayed.Bytes, f.Bytes) || !bytes.Equal(replayed.SBFHeader, f.SBFHeader) {
		t.Fatal("the stored row does not rebuild the frame that was applied")
	}
}

// A status frame without its original header cannot be stored as a whole block, so it
// stays the captured block it would have been: nothing is invented and nothing lost.
func TestReceiverStatusWithoutHeaderStaysCaptured(t *testing.T) {
	block, err := hex.DecodeString(sbfBenchStatus)
	if err != nil {
		t.Fatal(err)
	}
	f := scanOne(t, block, time.Now())
	f.SBFHeader = nil
	saved := frameForPersistence(f)
	if saved.RF != nil || saved.MsgType != ingest.SBFReceiverStatusBlock || !bytes.Equal(saved.Raw, block[8:]) || saved.SBFHeader != nil {
		t.Fatalf("header-less status persisted as RF %+v, msg_type %d, raw %d bytes", saved.RF, saved.MsgType, len(saved.Raw))
	}
}

// Every other SBF block still lands with the navigation captures, header beside body.
func TestOtherSBFBlocksStayCaptured(t *testing.T) {
	wires, frames := sbfCaptureFixtures(t)
	for i, f := range frames {
		if f.RF != nil || f.MsgType != 4017 || !bytes.Equal(append(append([]byte(nil), f.SBFHeader...), f.Raw...), wires[i]) {
			t.Fatalf("block 4017 persisted as RF %+v, msg_type %d", f.RF, f.MsgType)
		}
	}
}
