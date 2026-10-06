package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

func TestBoardPersistenceMapping(t *testing.T) {
	now := time.Now()
	f := &ingest.RawFrame{Source: "test-board", Recv: now.Add(-time.Hour), RecvLocal: now,
		Session: "boot", Seq: 42, HasSeq: true, Bytes: []byte{1, 4},
		Observer: identity.NewPrivateContext("test-board", identity.CredentialLocalDial),
		Details:  &ingest.ObserverDetails{UptimeMS: 100, Timing: &ingest.BoardTiming{Clock: "esp_apb"}}}
	saved := frameForPersistence(f)
	if saved.Board == nil || saved.Board.Kind != "timing" || saved.Board.SampleTime != nil || !saved.ReceivedAt.Equal(now) || saved.SourceSeq != 42 || saved.Session != "boot" || string(saved.Raw) != string(f.Bytes) {
		t.Fatalf("mapping: %+v", saved)
	}
	var d ingest.ObserverDetails
	if err := json.Unmarshal(saved.Board.Data, &d); err != nil || d.UptimeMS != 100 || d.Timing.Clock != "esp_apb" {
		t.Fatalf("JSON: %+v %v", d, err)
	}
	f.BoardSampleStamped = true
	f.Details.Timing = nil
	saved = frameForPersistence(f)
	if saved.Board.Kind != "environment" || saved.Board.SampleTime == nil || !saved.Board.SampleTime.Equal(f.Recv) {
		t.Fatal("sample stamp or kind lost")
	}
}

func TestRFPersistenceMapping(t *testing.T) {
	now := time.Now()
	cx := identity.NewPrivateContext("test-rf", identity.CredentialLocalDial)
	sats := []ingest.SatCN0{{GnssID: 0, SvID: 12, Cn0: 43, ElevDeg: 51, Used: true}}
	f := &ingest.RawFrame{Source: "test-rf", Recv: now, Observer: cx, RF: &ingest.RawRF{Sats: sats}}
	saved := frameForPersistence(f)
	if saved.RF == nil || saved.RF.Kind != "reception" || saved.Board != nil || len(saved.Raw) == 0 {
		t.Fatalf("reception mapping: %+v", saved)
	}
	if want := ingest.EncodeReceptionData(sats); string(saved.Raw) != string(want) {
		t.Fatalf("normalized raw body = %x, want %x", saved.Raw, want)
	}
	var decoded ingest.RawRF
	if err := json.Unmarshal(saved.RF.Data, &decoded); err != nil || len(decoded.Sats) != 1 || decoded.Sats[0].Cn0 != 43 {
		t.Fatalf("decoded projection: %+v %v", decoded, err)
	}

	raw := ingest.EncodeJammingStats([]ingest.RFBand{{Block: 0, AGC: 2200, JamState: 2}})
	f = &ingest.RawFrame{Source: "test-rf", Recv: now, Observer: cx, Bytes: raw,
		RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 2200, JamState: 2}}}}
	saved = frameForPersistence(f)
	if saved.RF == nil || saved.RF.Kind != "jamming" || string(saved.Raw) != string(raw) {
		t.Fatalf("jamming mapping: %+v", saved)
	}
}

// TestSolutionPersistenceMapping: receiver solutions are private RF evidence of kind
// "solution". A push frame keeps its exact body; a dial frame stores the canonical one.
func TestSolutionPersistenceMapping(t *testing.T) {
	now := time.Now()
	cx := identity.NewPrivateContext("test-solution", identity.CredentialLocalDial)
	sol := &ingest.ReceiverSolution{
		PVT:   &ingest.SolutionPVT{TOWMS: 1000, FixType: 3, FixFlags: ingest.FixFlagOK, LatE7: 374219000, LonE7: -1220841000, HeightMM: 12500},
		Clock: &ingest.SolutionClock{TOWMS: 1000, BiasNS: -412_345, DriftNSS: 87},
	}
	saved := frameForPersistence(&ingest.RawFrame{Source: "test-solution", Recv: now, Observer: cx, MsgType: ingest.TelemReceiverSolution, Solution: sol})
	if saved.RF == nil || saved.RF.Kind != "solution" || saved.Board != nil {
		t.Fatalf("solution mapping: %+v", saved)
	}
	if want := ingest.EncodeReceiverSolution(sol); string(saved.Raw) != string(want) {
		t.Fatalf("canonical raw body = %x, want %x", saved.Raw, want)
	}
	var decoded ingest.ReceiverSolution
	if err := json.Unmarshal(saved.RF.Data, &decoded); err != nil || decoded.PVT == nil || decoded.Clock.BiasNS != -412_345 {
		t.Fatalf("decoded projection: %+v %v", decoded, err)
	}
	body := ingest.EncodeReceiverSolution(sol)
	saved = frameForPersistence(&ingest.RawFrame{Source: "test-solution", Recv: now, Observer: cx, Bytes: body,
		MsgType: ingest.TelemReceiverSolution, Solution: sol})
	if saved.RF == nil || string(saved.Raw) != string(body) {
		t.Fatalf("push body not kept exactly: %+v", saved)
	}
}
