package store

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"
)

type ReceptionPowerModel struct {
	SourceID  string
	UpdatedAt time.Time
	ModelID   uint64
	Data      []byte
}

func (s *Store) LoadReceptionPowerModels(ctx context.Context) ([]ReceptionPowerModel, error) {
	rows, err := s.pool.Query(ctx, `SELECT source_id,updated_at,model_id,data FROM reception_power_models ORDER BY source_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []ReceptionPowerModel
	for rows.Next() {
		var model ReceptionPowerModel
		var id string
		if err := rows.Scan(&model.SourceID, &model.UpdatedAt, &id, &model.Data); err != nil {
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

func (s *Store) SaveReceptionPowerModel(ctx context.Context, model ReceptionPowerModel) error {
	if model.SourceID == "" || model.ModelID == 0 || len(model.Data) == 0 || len(model.Data) > 64<<20 {
		return ErrInvalidReceptionPowerModel
	}
	if model.UpdatedAt.IsZero() {
		model.UpdatedAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO reception_power_models(source_id,updated_at,model_id,data)
		VALUES($1,$2,$3,$4) ON CONFLICT(source_id) DO UPDATE
		SET updated_at=EXCLUDED.updated_at,model_id=EXCLUDED.model_id,data=EXCLUDED.data`,
		model.SourceID, model.UpdatedAt, strconv.FormatUint(model.ModelID, 10), model.Data)
	return err
}

var ErrInvalidReceptionPowerModel = errors.New("invalid reception power model")
