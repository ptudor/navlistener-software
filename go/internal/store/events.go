package store

import (
	"context"
	"fmt"
	"time"
)

// EventRow is one confirmed integrity event to persist (docs/OUTPUT.md §3/§4). Raw
// is the event's params as a JSON document (or nil). The DB assigns the id and the
// AFTER INSERT trigger fires pg_notify, so serve-side LISTENers see it immediately.
type EventRow struct {
	Time     time.Time
	SV       string
	Type     string
	OldValue string
	NewValue string
	Severity int
	Message  string
	Raw      []byte // JSON, or nil
}

// WriteEvent inserts one integrity event and returns its assigned id. Events are
// low-rate and individually meaningful, so they are written with a plain INSERT
// (not the batched CopyFrom path the high-rate nav frames use). The insert fires
// the notify trigger inside the same transaction.
func (s *Store) WriteEvent(ctx context.Context, e EventRow) (int64, error) {
	var raw any
	if len(e.Raw) > 0 {
		raw = string(e.Raw)
	}
	var id int64
	err := s.pool.QueryRow(ctx,
		`INSERT INTO gnss_events (time, sv, event_type, old_value, new_value, severity, message, raw)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		e.Time, e.SV, e.Type, nilIfEmpty(e.OldValue), nilIfEmpty(e.NewValue),
		int16(e.Severity), nilIfEmpty(e.Message), raw,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("write event: %w", err)
	}
	return id, nil
}

// WriteSnapshot stores one feed dump for replay/backfill (docs/OUTPUT.md §4).
// endpoint is the feed name; data is the marshalled JSON body.
func (s *Store) WriteSnapshot(ctx context.Context, at time.Time, endpoint string, data []byte) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO gnss_snapshots (time, endpoint, data) VALUES ($1, $2, $3)`,
		at, endpoint, string(data))
	if err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	return nil
}
