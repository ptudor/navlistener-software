package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// ReceptionPowerModel is one station's checkpointed received-power model.
// CollectorInstanceID is the collector that learned it; models are keyed by it,
// so collectors sharing a database keep and restore their own.
type ReceptionPowerModel struct {
	CollectorInstanceID string
	SourceID            string
	UpdatedAt           time.Time
	ModelID             uint64
	Data                []byte
}

// LoadReceptionPowerModels returns every model the collector stored.
func (s *Store) LoadReceptionPowerModels(ctx context.Context, collectorID string) ([]ReceptionPowerModel, error) {
	if !identity.ValidScopeID(collectorID) {
		return nil, fmt.Errorf("load reception power models: invalid collector id")
	}
	rows, err := s.pool.Query(ctx, `SELECT collector_instance_id,source_id,updated_at,model_id,data FROM reception_power_models
		WHERE collector_instance_id = $1 ORDER BY source_id`, collectorID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []ReceptionPowerModel
	for rows.Next() {
		var model ReceptionPowerModel
		var id string
		if err := rows.Scan(&model.CollectorInstanceID, &model.SourceID, &model.UpdatedAt, &id, &model.Data); err != nil {
			return nil, err
		}
		model.ModelID, err = strconv.ParseUint(id, 10, 64)
		if err != nil || model.ModelID == 0 {
			return nil, fmt.Errorf("invalid reception power model id for %q", model.SourceID)
		}
		models = append(models, model)
	}
	return models, rows.Err()
}

// SaveReceptionPowerModel upserts one station's model under its collector.
func (s *Store) SaveReceptionPowerModel(ctx context.Context, model ReceptionPowerModel) error {
	if !identity.ValidScopeID(model.CollectorInstanceID) || model.SourceID == "" || model.ModelID == 0 || len(model.Data) == 0 || len(model.Data) > 64<<20 {
		return ErrInvalidReceptionPowerModel
	}
	if model.UpdatedAt.IsZero() {
		model.UpdatedAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO reception_power_models(collector_instance_id,source_id,updated_at,model_id,data)
		VALUES($1,$2,$3,$4,$5) ON CONFLICT(collector_instance_id,source_id) DO UPDATE
		SET updated_at=EXCLUDED.updated_at,model_id=EXCLUDED.model_id,data=EXCLUDED.data`,
		model.CollectorInstanceID, model.SourceID, model.UpdatedAt, strconv.FormatUint(model.ModelID, 10), model.Data)
	return err
}

var ErrInvalidReceptionPowerModel = errors.New("invalid reception power model")
