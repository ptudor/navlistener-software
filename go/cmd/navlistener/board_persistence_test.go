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
