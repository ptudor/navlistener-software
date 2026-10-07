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
		"no collector":  {SourceID: "a", Data: json.RawMessage(`[]`)},
		"bad collector": {CollectorInstanceID: "not a scope id", SourceID: "a", Data: json.RawMessage(`[]`)},
		"no source":     {CollectorInstanceID: "c1", Data: json.RawMessage(`[]`)},
		"no data":       {CollectorInstanceID: "c1", SourceID: "a"},
		"not json":      {CollectorInstanceID: "c1", SourceID: "a", Data: json.RawMessage(`[`)},
	} {
		if err := s.SaveAGCBaseline(context.Background(), c); err != ErrInvalidAGCBaseline {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := s.LoadAGCBaselines(context.Background(), ""); err == nil {
		t.Error("load without a collector id accepted")
	}
}

// TestIntegrationAGCBaselines: a station's checkpoint upserts in place, and two
// collectors sharing the database each keep and load their own row for the same
// station.
func TestIntegrationAGCBaselines(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: testDSN(t)}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	id := fmt.Sprintf("agc-baseline-%d", time.Now().UnixNano())
	collectorA := fmt.Sprintf("agc-collector-a-%d", time.Now().UnixNano())
	collectorB := fmt.Sprintf("agc-collector-b-%d", time.Now().UnixNano())
	for _, epoch := range []string{"antenna-1", "antenna-2"} {
		if err := s.SaveAGCBaseline(ctx, AGCBaselineCheckpoint{CollectorInstanceID: collectorA, SourceID: id, Epoch: epoch, Data: json.RawMessage(`[{"block":0,"baseline":4000}]`)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SaveAGCBaseline(ctx, AGCBaselineCheckpoint{CollectorInstanceID: collectorB, SourceID: id, Epoch: "antenna-9", Data: json.RawMessage(`[{"block":0,"baseline":1000}]`)}); err != nil {
		t.Fatal(err)
	}
	for collector, epoch := range map[string]string{collectorA: "antenna-2", collectorB: "antenna-9"} {
		all, err := s.LoadAGCBaselines(ctx, collector)
		if err != nil {
			t.Fatal(err)
		}
		if len(all) != 1 || all[0].SourceID != id || all[0].CollectorInstanceID != collector || all[0].Epoch != epoch || len(all[0].Data) == 0 {
			t.Fatalf("%s loaded %+v, want its own checkpoint at %s", collector, all, epoch)
		}
	}
}
