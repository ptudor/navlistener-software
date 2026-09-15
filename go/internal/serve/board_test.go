package serve

import (
	"encoding/json"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"strings"
	"testing"
	"time"
)

func TestObserversBoardDetailsPrivateOnly(t *testing.T) {
	s := testServer(nil)
	now := time.Now()
	s.store.Apply(&ingest.RawFrame{Source: "sensor-only", Recv: now, Details: &ingest.ObserverDetails{Environment: &ingest.BoardEnvironment{}, EEPROM: &ingest.BoardEEPROM{EUI64: "0200000000000001"}}})
	private := s.observers(now, s.store, nil, identity.Audience{Kind: identity.AudienceOperator})
	if len(private) != 1 || private[0].Board == nil || private[0].ID != "sensor-only" {
		t.Fatal("sensor-only push station absent")
	}
	data, err := json.Marshal(private)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"mcp9808_c":null`) || !strings.Contains(string(data), `"eui64":"0200000000000001"`) {
		t.Fatal("unknown measurement/identity lost")
	}
	s.store.Apply(&ingest.RawFrame{Source: "timing-only", Recv: now, Details: &ingest.ObserverDetails{Timing: &ingest.BoardTiming{Clock: "esp_apb"}}})
	private = s.observers(now, s.store, nil, identity.Audience{Kind: identity.AudienceOperator})
	data, err = json.Marshal(private)
	if err != nil || !strings.Contains(string(data), `"clock":"esp_apb"`) {
		t.Fatal("private timing-only observer absent")
	}
	public := s.observers(now, s.store, nil, identity.Audience{Kind: identity.AudiencePublic})
	if len(public) != 0 {
		t.Fatal("public observer enumeration from private board")
	}
}
