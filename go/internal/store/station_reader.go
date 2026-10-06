package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StationInput is one stored station input for replay: an rf_samples or
// observer_samples row with the clocks the live checks used.
type StationInput struct {
	Origin, Kind string
	Raw          []byte
	ReceivedAt   time.Time
	// Local is the collector-local receipt instant. Rows stored before
	// local_received_at existed fall back to their persistence time and set
	// Approximate.
	Local       time.Time
	Approximate bool
	// WallClock is the independent stamp, nil when there was none or the row
	// predates its column.
	WallClock *time.Time
	Session   string
	Seq       uint64
	HasSeq    bool
}

// StationInputQuery selects one station's inputs received within [Since, Until]. An
// empty CollectorID reads every collector's rows for the station.
type StationInputQuery struct {
	CollectorID  string
	Station      string
	Since, Until time.Time
}

const stationInputColumns = `origin, kind, raw, received_at, local, approximate, wall, source_session, source_seq`

// QueryStationInputs streams a station's stored inputs in collector receipt order,
// the order live state applied them.
func (r *Reader) QueryStationInputs(ctx context.Context, q StationInputQuery, fn func(StationInput) error) error {
	if q.Station == "" || q.Since.IsZero() || q.Until.IsZero() || q.Until.Before(q.Since) {
		return fmt.Errorf("station input query needs a station and an ordered window")
	}
	collector := ""
	args := []any{q.Station, q.Since, q.Until}
	if q.CollectorID != "" {
		collector = " AND collector_instance_id = $4"
		args = append(args, q.CollectorID)
	}
	rows, err := r.pool.Query(ctx, `SELECT `+stationInputColumns+` FROM (
		SELECT 'rf' AS origin, kind, raw, received_at, COALESCE(local_received_at, ts) AS local,
			local_received_at IS NULL AS approximate, wall_clock_stamp AS wall, source_session, source_seq, ts
		FROM rf_samples WHERE source_id = $1 AND sample_time >= $2 AND sample_time <= $3`+collector+`
		UNION ALL
		SELECT 'board', kind, raw, received_at, received_at, false, sample_time, source_session, source_seq, ts
		FROM observer_samples WHERE source_id = $1 AND received_at >= $2 AND received_at <= $3`+collector+`
	) inputs ORDER BY local, ts, origin, source_session COLLATE "C" NULLS FIRST, source_seq NULLS FIRST`, args...)
	if err != nil {
		return fmt.Errorf("station inputs: %w", err)
	}
	return scanStationInputs(rows, fn)
}

// EvidenceWindow is a captured event's station and input window.
type EvidenceWindow struct {
	Station    string
	EventTime  time.Time
	Start, End time.Time
}

// QueryEvidenceInputs streams one captured event's evidence samples in collector
// receipt order and returns its window. It returns ErrNoEvidence when the event has
// no captured evidence.
func (r *Reader) QueryEvidenceInputs(ctx context.Context, audience string, seq int64, fn func(StationInput) error) (EvidenceWindow, error) {
	var w EvidenceWindow
	err := r.pool.QueryRow(ctx, `SELECT source_id, event_time, window_start, window_end FROM event_evidence
		WHERE audience = $1 AND audience_seq = $2`, audience, seq).Scan(&w.Station, &w.EventTime, &w.Start, &w.End)
	if errors.Is(err, pgx.ErrNoRows) {
		return w, ErrNoEvidence
	}
	if err != nil {
		return w, fmt.Errorf("evidence window: %w", err)
	}
	rows, err := r.pool.Query(ctx, `SELECT origin, kind, raw, received_at, COALESCE(local_received_at, ts),
			local_received_at IS NULL, wall_clock_stamp, source_session, source_seq
		FROM event_evidence_samples WHERE audience = $1 AND audience_seq = $2 AND event_time = $3
		ORDER BY COALESCE(local_received_at, ts), ts, origin, source_session COLLATE "C" NULLS FIRST, source_seq NULLS FIRST`,
		audience, seq, w.EventTime)
	if err != nil {
		return w, fmt.Errorf("evidence inputs: %w", err)
	}
	return w, scanStationInputs(rows, fn)
}

func scanStationInputs(rows pgx.Rows, fn func(StationInput) error) error {
	defer rows.Close()
	for rows.Next() {
		var in StationInput
		var session *string
		var seq *int64
		if err := rows.Scan(&in.Origin, &in.Kind, &in.Raw, &in.ReceivedAt, &in.Local, &in.Approximate, &in.WallClock, &session, &seq); err != nil {
			return fmt.Errorf("station inputs scan: %w", err)
		}
		if session != nil {
			in.Session = *session
		}
		if seq != nil {
			in.Seq, in.HasSeq = uint64(*seq), true
		}
		if err := fn(in); err != nil {
			return err
		}
	}
	return rows.Err()
}

// StoredStationEvent is a stored station event to compare a replay against.
type StoredStationEvent struct {
	Audience string
	Time     time.Time
	Type     string
	OldValue string
	NewValue string
	Severity int
}

// QueryStationEvents returns the station's stored events of the given types within
// [since, until], in time order. A non-empty audience restricts them to that audience.
func (r *Reader) QueryStationEvents(ctx context.Context, station, audience string, types []string, since, until time.Time) ([]StoredStationEvent, error) {
	args := []any{station, types, since, until}
	filter := ""
	if audience != "" {
		filter = " AND audience = $5"
		args = append(args, audience)
	}
	rows, err := r.pool.Query(ctx, `SELECT audience, time, event_type, COALESCE(old_value, ''), COALESCE(new_value, ''), severity
		FROM gnss_events WHERE sv = $1 AND event_type = ANY($2) AND time >= $3 AND time <= $4`+filter+`
		ORDER BY time, audience, audience_seq`, args...)
	if err != nil {
		return nil, fmt.Errorf("station events: %w", err)
	}
	defer rows.Close()
	var out []StoredStationEvent
	for rows.Next() {
		var e StoredStationEvent
		if err := rows.Scan(&e.Audience, &e.Time, &e.Type, &e.OldValue, &e.NewValue, &e.Severity); err != nil {
			return nil, fmt.Errorf("station events scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
