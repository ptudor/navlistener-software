package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

func TestSaveAGCBaselineRejects(t *testing.T) {
	s := &Store{}
	for name, c := range map[string]AGCBaselineCheckpoint{
		"no source": {Data: json.RawMessage(`[]`)},
		"no data":   {SourceID: "a"},
		"not json":  {SourceID: "a", Data: json.RawMessage(`[`)},
	} {
		if err := s.SaveAGCBaseline(context.Background(), c); err != ErrInvalidAGCBaseline {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestIntegrationAGCBaselines(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: testDSN(t)}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := fmt.Sprintf("agc-baseline-%d", time.Now().UnixNano())
	for _, epoch := range []string{"antenna-1", "antenna-2"} {
		if err := s.SaveAGCBaseline(ctx, AGCBaselineCheckpoint{SourceID: id, Epoch: epoch, Data: json.RawMessage(`[{"block":0,"baseline":4000}]`)}); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.LoadAGCBaselines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, c := range all {
		if c.SourceID == id {
			found++
			if c.Epoch != "antenna-2" || len(c.Data) == 0 {
				t.Fatalf("checkpoint = %+v", c)
			}
		}
	}
	if found != 1 {
		t.Fatalf("found %d checkpoints for the station", found)
	}
}
