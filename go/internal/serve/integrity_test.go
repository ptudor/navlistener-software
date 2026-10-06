package serve

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

// TestObserversIntegrityPrivateOnly: the integrity assessment is evaluated from the
// receiver's own solution, so it is served to private audiences only and a
// solution-only station is not enumerated publicly.
func TestObserversIntegrityPrivateOnly(t *testing.T) {
	s := testServer(nil)
	now := time.Now()
	s.store.Apply(&ingest.RawFrame{Source: "solution-only", Recv: now, RecvLocal: now, MsgType: ingest.TelemReceiverSolution,
		Solution: &ingest.ReceiverSolution{PVT: &ingest.SolutionPVT{TOWMS: 1000, FixType: 3, FixFlags: ingest.FixFlagOK,
			LatE7: 374219000, LonE7: -1220841000, HeightMM: 12500, HAccMM: 1500, VAccMM: 2500}}})
	private := s.observers(now, s.store, nil, identity.Audience{Kind: identity.AudienceOperator})
	if len(private) != 1 || private[0].ID != "solution-only" || private[0].Integrity == nil {
		t.Fatalf("private observers = %+v", private)
	}
	data, err := json.Marshal(private)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"integrity":{`) || !strings.Contains(string(data), `"config_hash":"sha256:`) {
		t.Fatalf("integrity block missing: %s", data)
	}
	if strings.Contains(string(data), "37.4219") || strings.Contains(string(data), "374219000") {
		t.Fatalf("private feed carries the receiver's coordinates: %s", data)
	}
	if public := s.observers(now, s.store, nil, identity.Audience{Kind: identity.AudiencePublic}); len(public) != 0 {
		t.Fatalf("public observers enumerate a solution-only station: %+v", public)
	}
}
