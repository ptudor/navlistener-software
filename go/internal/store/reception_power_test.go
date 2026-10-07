package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

func TestIntegrationReceptionPowerModelUpsert(t *testing.T) {
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: testDSN(t)}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := fmt.Sprintf("power-model-test-%d", time.Now().UnixNano())
	collector := fmt.Sprintf("power-model-collector-%d", time.Now().UnixNano())
	first := ReceptionPowerModel{CollectorInstanceID: collector, SourceID: source, UpdatedAt: time.Now().UTC().Add(-time.Minute), ModelID: ^uint64(0), Data: []byte{1, 2, 3}}
	if err := s.SaveReceptionPowerModel(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
	second.ModelID = 9
	second.Data = []byte{4, 5}
	if err := s.SaveReceptionPowerModel(ctx, second); err != nil {
		t.Fatal(err)
	}
	// Another collector's model for the same station is a row of its own.
	other := first
	other.CollectorInstanceID = collector + "-other"
	other.ModelID = 7
	other.Data = []byte{6}
	if err := s.SaveReceptionPowerModel(ctx, other); err != nil {
		t.Fatal(err)
	}
	models, err := s.LoadReceptionPowerModels(ctx, collector)
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 {
		t.Fatalf("collector loaded %d models, want only its own", len(models))
	}
	got := models[0]
	if got.SourceID != source || got.CollectorInstanceID != collector || got.ModelID != second.ModelID || string(got.Data) != string(second.Data) || !got.UpdatedAt.Equal(second.UpdatedAt) {
		t.Fatalf("upserted model: %+v", got)
	}
	theirs, err := s.LoadReceptionPowerModels(ctx, other.CollectorInstanceID)
	if err != nil || len(theirs) != 1 || theirs[0].ModelID != 7 || string(theirs[0].Data) != string(other.Data) {
		t.Fatalf("other collector's model: %+v %v", theirs, err)
	}
	if _, err := s.LoadReceptionPowerModels(ctx, ""); err == nil {
		t.Fatal("load without a collector id accepted")
	}
	if err := s.SaveReceptionPowerModel(ctx, ReceptionPowerModel{SourceID: source, ModelID: 1, Data: []byte{1}}); err != ErrInvalidReceptionPowerModel {
		t.Fatalf("save without a collector id: %v", err)
	}
}
