package store

import (
	"context"
	"encoding/json"
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

// EventQuery filters a historical events query (docs/OUTPUT.md §2.1/§3). A zero-value
// string/severity field is "any"; a zero Since/Until is "unbounded on that side". Limit
// and Offset paginate; the caller clamps them.
type EventQuery struct {
	SV          string
	Type        string
	MinSeverity int
	Since       time.Time
	Until       time.Time
	Limit       int
	Offset      int
}

// StoredEvent is one persisted integrity event as read back for the query API. Params is
// the interpolation object (stored in the raw column by the detector); it is emitted as
// `params` to mirror the SSE event shape (docs/OUTPUT.md §3).
type StoredEvent struct {
	ID       int64           `json:"id"`
	Time     time.Time       `json:"time"`
	SV       string          `json:"sv"`
	Type     string          `json:"type"`
	OldValue string          `json:"old_value,omitempty"`
	NewValue string          `json:"new_value,omitempty"`
	Severity int             `json:"severity"`
	Message  string          `json:"message,omitempty"`
	Params   json.RawMessage `json:"params,omitempty"`
}

// EventSummary aggregates the events in a window (docs/OUTPUT.md §2.1).
type EventSummary struct {
	TotalEvents     int
	ActiveCritical  int
	ActiveWarnings  int
	LastCritical    *time.Time
	ByType          map[string]int
	ByConstellation map[string]int
}

// QueryEvents returns the events matching q, newest first, plus the total matching the
// filters before Limit/Offset (for pagination). The filters are parameterized (never
// interpolated) — untrusted query input cannot reach the SQL. The window is [Since, Until];
// an unset bound is treated as open on that side.
func (s *Store) QueryEvents(ctx context.Context, q EventQuery) ([]StoredEvent, int, error) {
	since := q.Since
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	until := q.Until
	if until.IsZero() {
		until = time.Now().Add(24 * time.Hour) // generous open upper bound
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, time, sv, event_type, COALESCE(old_value,''), COALESCE(new_value,''),
		        severity, COALESCE(message,''), raw, count(*) OVER()
		   FROM gnss_events
		  WHERE ($1 = '' OR sv = $1)
		    AND ($2 = '' OR event_type = $2)
		    AND severity >= $3
		    AND time >= $4 AND time <= $5
		  ORDER BY time DESC, id DESC
		  LIMIT $6 OFFSET $7`,
		q.SV, q.Type, int16(q.MinSeverity), since, until, q.Limit, q.Offset)
	if err != nil {
		return nil, 0, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []StoredEvent
	total := 0
	for rows.Next() {
		var e StoredEvent
		var sev int16
		var raw []byte
		if err := rows.Scan(&e.ID, &e.Time, &e.SV, &e.Type, &e.OldValue, &e.NewValue,
			&sev, &e.Message, &raw, &total); err != nil {
			return nil, 0, fmt.Errorf("scan event: %w", err)
		}
		e.Severity = int(sev)
		e.Time = e.Time.UTC()
		if len(raw) > 0 {
			e.Params = json.RawMessage(raw)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate events: %w", err)
	}
	return out, total, nil
}

// constellationByLetter maps a RINEX SV-name letter to a constellation name for the
// summary's by-constellation breakdown (docs/CONSTELLATIONS.md §0).
var constellationByLetter = map[byte]string{
	'G': "gps", 'R': "glonass", 'E': "galileo", 'C': "beidou",
	'J': "qzss", 'I': "navic", 'S': "sbas",
}

// SummarizeEvents aggregates the events in [since, until] by type, constellation, and
// severity, with the most recent critical timestamp. The counting is done in SQL (a small
// grouped result) so the window can be large without streaming every row.
func (s *Store) SummarizeEvents(ctx context.Context, since, until time.Time) (EventSummary, error) {
	sum := EventSummary{ByType: map[string]int{}, ByConstellation: map[string]int{}}
	rows, err := s.pool.Query(ctx,
		`SELECT event_type, LEFT(sv,1), severity, count(*)
		   FROM gnss_events
		  WHERE time >= $1 AND time <= $2
		  GROUP BY event_type, LEFT(sv,1), severity`,
		since, until)
	if err != nil {
		return sum, fmt.Errorf("summarize events: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var typ, letter string
		var sev int16
		var n int
		if err := rows.Scan(&typ, &letter, &sev, &n); err != nil {
			return sum, fmt.Errorf("scan summary: %w", err)
		}
		sum.TotalEvents += n
		sum.ByType[typ] += n
		if letter != "" {
			if name, ok := constellationByLetter[letter[0]]; ok {
				sum.ByConstellation[name] += n
			}
		}
		switch {
		case sev >= 2:
			sum.ActiveCritical += n
		case sev >= 1:
			sum.ActiveWarnings += n
		}
	}
	if err := rows.Err(); err != nil {
		return sum, fmt.Errorf("iterate summary: %w", err)
	}

	// max(time) is NULL when the window has no critical events — scan into a pointer.
	var t *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT max(time) FROM gnss_events WHERE severity >= 2 AND time >= $1 AND time <= $2`,
		since, until).Scan(&t); err != nil {
		return sum, fmt.Errorf("summarize last_critical: %w", err)
	}
	if t != nil {
		u := t.UTC()
		sum.LastCritical = &u
	}
	return sum, nil
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
