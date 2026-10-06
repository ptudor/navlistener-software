package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AGCBaselineCheckpoint is one station's durable AGC baselines: the bands as live
// state exports them (JSON), and the antenna epoch they were learned under.
type AGCBaselineCheckpoint struct {
	SourceID  string
	UpdatedAt time.Time
	Epoch     string
	Data      json.RawMessage
}

// ErrInvalidAGCBaseline rejects a checkpoint the table must not hold.
var ErrInvalidAGCBaseline = errors.New("invalid AGC baseline checkpoint")

// LoadAGCBaselines returns every stored checkpoint.
func (s *Store) LoadAGCBaselines(ctx context.Context) ([]AGCBaselineCheckpoint, error) {
	rows, err := s.pool.Query(ctx, `SELECT source_id, updated_at, epoch, data FROM agc_baselines ORDER BY source_id`)
	if err != nil {
		return nil, fmt.Errorf("load AGC baselines: %w", err)
	}
	defer rows.Close()
	var out []AGCBaselineCheckpoint
	for rows.Next() {
		var c AGCBaselineCheckpoint
		if err := rows.Scan(&c.SourceID, &c.UpdatedAt, &c.Epoch, &c.Data); err != nil {
			return nil, fmt.Errorf("load AGC baselines: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveAGCBaseline upserts one station's checkpoint.
func (s *Store) SaveAGCBaseline(ctx context.Context, c AGCBaselineCheckpoint) error {
	if c.SourceID == "" || len(c.Data) == 0 || len(c.Data) > 4<<20 || !json.Valid(c.Data) {
		return ErrInvalidAGCBaseline
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO agc_baselines (source_id, updated_at, epoch, data)
		VALUES ($1, $2, $3, $4) ON CONFLICT (source_id) DO UPDATE
		SET updated_at = EXCLUDED.updated_at, epoch = EXCLUDED.epoch, data = EXCLUDED.data`,
		c.SourceID, c.UpdatedAt, c.Epoch, string(c.Data))
	return err
}
