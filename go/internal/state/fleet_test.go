package state

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/wire"
)

const fleetBoard = "board-0003-00112233445566778899aabbccddeeff"

func enrolled(observer, authority, fingerprint string, trust identity.HardwareTrust) identity.ObserverContext {
	c := identity.NewPrivateContext(observer, identity.CredentialToken)
	c.ManufacturerAuthorityID = authority
	c.HardwareTrust = trust
	c.CommissioningFingerprint = fingerprint
	return c
}

// A field report lists only boards enrolled under the authority asked for, with
// the collector's evidence for their session and what they reported in it.
func TestFleetBoardsCarryEvidenceAndOnlyThatManufacturersBoards(t *testing.T) {
	s := New(1)
	now := time.Now()
	fingerprint := "c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00c0ffee00"
	update := &wire.UpdateStatus{Channel: "stable", Mode: "install", State: "idle", Profile: "trusted", Running: 17, Error: "OK"}
	apply := func(source, session string, seq uint64, c identity.ObserverContext, details ingest.ObserverDetails, at time.Time) {
		details.UptimeMS = seq * 1000
		s.Apply(&ingest.RawFrame{Source: source, Observer: c, Session: session, Seq: seq, HasSeq: true, Recv: at, RecvLocal: at, Details: &details})
	}
	board := enrolled(fleetBoard, "example-manufacturing", fingerprint, identity.HardwareTrustTrusted)
	apply(fleetBoard, "boot-a", 1, board, ingest.ObserverDetails{Firmware: "0.1.0+1.b8e037c", Update: update}, now.Add(-2*time.Minute))
	// A later report without the firmware tag does not erase what the session said.
	apply(fleetBoard, "boot-a", 2, board, ingest.ObserverDetails{Timing: &ingest.BoardTiming{}}, now)
	apply("board-0003-ffeeddccbbaa99887766554433221100", "boot-a", 1,
		enrolled("board-0003-ffeeddccbbaa99887766554433221100", "someone-else", fingerprint, identity.HardwareTrustOpen),
		ingest.ObserverDetails{Firmware: "other"}, now)
	apply("software-station", "boot-a", 1, enrolled("software-station", "example-manufacturing", "", identity.HardwareTrustNone),
		ingest.ObserverDetails{Firmware: "feeder"}, now)

	boards := s.FleetBoards("example-manufacturing")
	if len(boards) != 1 {
		t.Fatalf("boards = %+v, want only %s", boards, fleetBoard)
	}
	b := boards[0]
	if b.ObserverID != fleetBoard || b.HardwareTrust != identity.HardwareTrustTrusted || b.CommissioningFingerprint != fingerprint {
		t.Fatalf("evidence = %+v", b)
	}
	if b.Firmware != "0.1.0+1.b8e037c" || b.Update == nil || b.Update.Running != 17 || !b.LastReceived.Equal(now) {
		t.Fatalf("board account = %+v", b)
	}

	// A new session starts again: its firmware is unknown until it says.
	apply(fleetBoard, "boot-b", 1, board, ingest.ObserverDetails{Timing: &ingest.BoardTiming{}}, now.Add(time.Minute))
	if b := s.FleetBoards("example-manufacturing")[0]; b.Firmware != "" || b.Update != nil {
		t.Fatalf("after reboot = %+v", b)
	}
}
