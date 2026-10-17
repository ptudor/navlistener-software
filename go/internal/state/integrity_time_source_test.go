package state

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

func TestReplayDoesNotInventIndependentTime(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	for _, source := range []uint8{ingest.StampUnknown, ingest.StampRTC, ingest.StampGNSS, ingest.StampNetwork, ingest.StampFactory, 0} {
		f := &ingest.RawFrame{Recv: now.Add(-time.Hour), RecvLocal: now, RecvStamped: true, Solution: &ingest.ReceiverSolution{StampSource: source}}
		stamp, local := utcReference(f)
		independent := source == 0 || source == ingest.StampNetwork || source == ingest.StampFactory
		if local || stamp.IsZero() == independent {
			t.Fatalf("source %d replay reference=%s local=%t", source, stamp, local)
		}
		f.Recv = now.Add(-time.Second)
		stamp, local = utcReference(f)
		if stamp != now || !local {
			t.Fatalf("source %d lost collector live reference", source)
		}
	}
}
