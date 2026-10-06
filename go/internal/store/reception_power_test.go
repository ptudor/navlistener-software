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
	first := ReceptionPowerModel{SourceID: source, UpdatedAt: time.Now().UTC().Add(-time.Minute), ModelID: ^uint64(0), Data: []byte{1, 2, 3}}
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
	models, err := s.LoadReceptionPowerModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var got *ReceptionPowerModel
	for i := range models {
		if models[i].SourceID == source {
			got = &models[i]
			break
		}
	}
	if got == nil || got.ModelID != second.ModelID || string(got.Data) != string(second.Data) || !got.UpdatedAt.Equal(second.UpdatedAt) {
		t.Fatalf("upserted model: %+v", got)
	}
}
