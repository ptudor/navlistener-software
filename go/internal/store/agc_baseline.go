package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// AGCBaselineCheckpoint is one station's durable AGC baselines: the bands as live
// state exports them (JSON), and the antenna epoch they were learned under.
// CollectorInstanceID is the collector that learned them; checkpoints are keyed
// by it, so collectors sharing a database keep and restore their own.
type AGCBaselineCheckpoint struct {
	CollectorInstanceID string
	SourceID            string
	UpdatedAt           time.Time
	Epoch               string
	Data                json.RawMessage
}

// ErrInvalidAGCBaseline rejects a checkpoint the table must not hold.
var ErrInvalidAGCBaseline = errors.New("invalid AGC baseline checkpoint")

// LoadAGCBaselines returns every checkpoint the collector stored.
func (s *Store) LoadAGCBaselines(ctx context.Context, collectorID string) ([]AGCBaselineCheckpoint, error) {
	if !identity.ValidScopeID(collectorID) {
		return nil, fmt.Errorf("load AGC baselines: invalid collector id")
	}
	rows, err := s.pool.Query(ctx, `SELECT collector_instance_id, source_id, updated_at, epoch, data FROM agc_baselines
		WHERE collector_instance_id = $1 ORDER BY source_id`, collectorID)
	if err != nil {
		return nil, fmt.Errorf("load AGC baselines: %w", err)
	}
	defer rows.Close()
	var out []AGCBaselineCheckpoint
	for rows.Next() {
		var c AGCBaselineCheckpoint
		if err := rows.Scan(&c.CollectorInstanceID, &c.SourceID, &c.UpdatedAt, &c.Epoch, &c.Data); err != nil {
			return nil, fmt.Errorf("load AGC baselines: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SaveAGCBaseline upserts one station's checkpoint under its collector.
func (s *Store) SaveAGCBaseline(ctx context.Context, c AGCBaselineCheckpoint) error {
	if !identity.ValidScopeID(c.CollectorInstanceID) || c.SourceID == "" || len(c.Data) == 0 || len(c.Data) > 4<<20 || !json.Valid(c.Data) {
		return ErrInvalidAGCBaseline
	}
	if c.UpdatedAt.IsZero() {
		c.UpdatedAt = time.Now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO agc_baselines (collector_instance_id, source_id, updated_at, epoch, data)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT (collector_instance_id, source_id) DO UPDATE
		SET updated_at = EXCLUDED.updated_at, epoch = EXCLUDED.epoch, data = EXCLUDED.data`,
		c.CollectorInstanceID, c.SourceID, c.UpdatedAt, c.Epoch, string(c.Data))
	return err
}
