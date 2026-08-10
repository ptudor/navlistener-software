package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// EventRow is one confirmed integrity event to persist (docs/OUTPUT.md §3/§4). Raw
// is the event's params as a JSON document (or nil). The DB assigns an internal
// row id and an audience-local cursor; only public rows notify the scoped v2
// external channel. The native SSE broker publishes directly after commit.
//
// DedupeKey  is the internal idempotency identity: an opaque value
// generated exactly once per confirmed transition, before the first write
// attempt, and carried unchanged across retries. It is never served — the
// public StoredEvent/EventMsg/SSE shapes do not expose it.
type EventRow struct {
	Audience       string
	RedactionClass string
	Time           time.Time
	SV             string
	Type           string
	OldValue       string
	NewValue       string
	Severity       int
	Message        string
	Raw            []byte // JSON, or nil
	DedupeKey      string // required; see the type comment
}

const defaultEventAudience = "operator:local"

// WriteEvent inserts one integrity event and returns its audience-local sequence.
// Events are low-rate and individually meaningful, so they are written with a plain INSERT
// (not the batched CopyFrom path the high-rate nav frames use). The insert fires
// the notify trigger inside the same transaction.
//
// regression fix — the write is idempotent on (time, dedupe_key): PostgreSQL can
// commit the INSERT while this client observes a timeout or connection error,
// and the caller's bounded retry (cmd writeEventRetry) then re-runs it. The
// existing CTE finds the already committed row before incrementing the audience
// cursor, and the conflict-safe INSERT returns its audience_seq instead of
// inserting or notifying twice. This is load-bearing for an ambiguous
// commit may not create a visible gap in the public cursor.
func (s *Store) WriteEvent(ctx context.Context, e EventRow) (int64, error) {
	if e.DedupeKey == "" {
		return 0, fmt.Errorf("write event: missing dedupe key (every event needs a retry-stable identity)")
	}
	var raw any
	if len(e.Raw) > 0 {
		raw = string(e.Raw)
	}
	audience := e.Audience
	if audience == "" {
		audience = defaultEventAudience
	}
	redaction := e.RedactionClass
	if redaction == "" {
		redaction = "private"
	}
	var audienceSeq int64
	err := s.pool.QueryRow(ctx,
		`WITH existing AS MATERIALIZED (
		     SELECT audience_seq FROM gnss_events WHERE time = $1 AND dedupe_key = $11
		 ), next_seq AS (
		     INSERT INTO gnss_event_audience_cursors (audience, last_seq)
		     SELECT $2, 1 WHERE NOT EXISTS (SELECT 1 FROM existing)
		     ON CONFLICT (audience) DO UPDATE
		       SET last_seq = gnss_event_audience_cursors.last_seq + 1
		     RETURNING last_seq
		 ), chosen AS (
		     SELECT audience_seq FROM existing
		     UNION ALL SELECT last_seq FROM next_seq
		     LIMIT 1
		 )
		 INSERT INTO gnss_events
		        (time, audience, audience_seq, redaction_class, sv, event_type,
		         old_value, new_value, severity, message, raw, dedupe_key)
		 SELECT $1, $2, chosen.audience_seq, $3, $4, $5, $6, $7, $8, $9, $10, $11
		   FROM chosen
		 ON CONFLICT (time, dedupe_key) DO UPDATE SET dedupe_key = EXCLUDED.dedupe_key
		 RETURNING audience_seq`,
		e.Time, audience, redaction, e.SV, e.Type, nilIfEmpty(e.OldValue), nilIfEmpty(e.NewValue),
		int16(e.Severity), nilIfEmpty(e.Message), raw, e.DedupeKey,
	).Scan(&audienceSeq)
	if err != nil {
		return 0, fmt.Errorf("write event: %w", err)
	}
	return audienceSeq, nil
}

// clampSeverity bounds a caller-supplied MinSeverity into the valid 0..2 (info/warning/
// critical) range before it is cast to the DB column's int16 : an out-of-range
// value like 65538 wraps to 2 and 65536 wraps to 0 under a raw int16 cast, silently
// remapping the filter to a plausible-looking but wrong severity instead of the caller's
// actual (invalid) request.
func clampSeverity(v int) int {
	if v < 0 {
		return 0
	}
	if v > 2 {
		return 2
	}
	return v
}

// EventQuery filters a historical events query (docs/OUTPUT.md §2.1/§3). A zero-value
// string/severity field is "any"; a zero Since/Until is "unbounded on that side". Limit
// and Offset paginate; the caller clamps them.
type EventQuery struct {
	Audience    string
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

// EventSummary aggregates the events in a window (docs/OUTPUT.md §2.1). The
// severity buckets are historical event counts over the window — every row at
// that severity, including superseded conditions and recovery transitions
//. They are deliberately NOT named "active": the schema has no
// resolved marker and no latest-state-per-detector query, so a truthful
// current-incident count does not exist here yet.
type EventSummary struct {
	TotalEvents     int
	CriticalEvents  int
	WarningEvents   int
	LastCritical    *time.Time
	ByType          map[string]int
	ByConstellation map[string]int
}

// QueryEvents returns the events matching q, newest first, plus the total matching the
// filters before Limit/Offset (for pagination). The filters are parameterized (never
// interpolated) — untrusted query input cannot reach the SQL. The window is [Since, Until];
// an unset bound is treated as open on that side.
func (s *Store) QueryEvents(ctx context.Context, q EventQuery) ([]StoredEvent, int, error) {
	audience := q.Audience
	if audience == "" {
		audience = defaultEventAudience
	}
	since := q.Since
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	until := q.Until
	if until.IsZero() {
		until = time.Now().Add(24 * time.Hour) // generous open upper bound
	}
	rows, err := s.pool.Query(ctx,
		`SELECT audience_seq, time, sv, event_type, COALESCE(old_value,''), COALESCE(new_value,''),
		        severity, COALESCE(message,''), raw, count(*) OVER()
		   FROM gnss_events
		  WHERE audience = $1
		    AND ($2 = '' OR sv = $2)
		    AND ($3 = '' OR event_type = $3)
		    AND severity >= $4
		    AND time >= $5 AND time <= $6
		  ORDER BY time DESC, audience_seq DESC
		  LIMIT $7 OFFSET $8`,
		audience, q.SV, q.Type, int16(clampSeverity(q.MinSeverity)), since, until, q.Limit, q.Offset)
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
	// count(*) OVER() is observed only via the returned rows, so a page past the
	// last row (offset >= the matching count -- including the routine "probe one past
	// the last page") returns zero rows and total silently collapses to 0, though
	// thousands of rows may still match the filters. Run a separate, unpaginated count
	// with the same filters whenever the page came back empty.
	if len(out) == 0 {
		if err := s.pool.QueryRow(ctx,
			`SELECT count(*) FROM gnss_events
			  WHERE audience = $1
			    AND ($2 = '' OR sv = $2)
			    AND ($3 = '' OR event_type = $3)
			    AND severity >= $4
			    AND time >= $5 AND time <= $6`,
			audience, q.SV, q.Type, int16(clampSeverity(q.MinSeverity)), since, until,
		).Scan(&total); err != nil {
			return nil, 0, fmt.Errorf("count events: %w", err)
		}
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
	return s.SummarizeEventsForAudience(ctx, defaultEventAudience, since, until)
}

// SummarizeEventsForAudience aggregates only one authorized event stream.
func (s *Store) SummarizeEventsForAudience(ctx context.Context, audience string, since, until time.Time) (EventSummary, error) {
	if audience == "" {
		audience = defaultEventAudience
	}
	sum := EventSummary{ByType: map[string]int{}, ByConstellation: map[string]int{}}
	// only SV-scoped events (sv shaped like G05@0 / E14@1) contribute to the
	// by-constellation breakdown. Station-scoped events (jamming/spoofing/capability) store
	// the station id in sv, so a bare LEFT(sv,1) would misattribute a station named
	// "Gateway1"/"Roof2"/"East…" to gps/glonass/…; the SV-name-shape regex excludes them
	// (their letter comes back '' and is skipped below) while they still count in the total,
	// by-type, and severity tallies.
	rows, err := s.pool.Query(ctx,
		`SELECT event_type,
		        CASE WHEN sv ~ '^[GRECJIS][0-9]{2}@' THEN LEFT(sv,1) ELSE '' END,
		        severity, count(*)
		   FROM gnss_events
		  WHERE audience = $1 AND time >= $2 AND time <= $3
		  GROUP BY event_type, 2, severity`,
		audience, since, until)
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
			sum.CriticalEvents += n
		case sev >= 1:
			sum.WarningEvents += n
		}
	}
	if err := rows.Err(); err != nil {
		return sum, fmt.Errorf("iterate summary: %w", err)
	}

	// max(time) is NULL when the window has no critical events — scan into a pointer.
	var t *time.Time
	if err := s.pool.QueryRow(ctx,
		`SELECT max(time) FROM gnss_events WHERE audience = $1 AND severity >= 2 AND time >= $2 AND time <= $3`,
		audience, since, until).Scan(&t); err != nil {
		return sum, fmt.Errorf("summarize last_critical: %w", err)
	}
	if t != nil {
		u := t.UTC()
		sum.LastCritical = &u
	}
	return sum, nil
}

// WriteSnapshot stores one feed dump for replay/backfill (docs/OUTPUT.md §4).
// audience is the materialized authorization view and endpoint is the feed
// name; data is the marshalled JSON body. Keeping audience in the row and index
// prevents an operator snapshot from ever being replayed as public.
func (s *Store) WriteSnapshot(ctx context.Context, at time.Time, audience, endpoint string, data []byte) error {
	if audience == "" {
		return fmt.Errorf("write snapshot: audience is required")
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO gnss_snapshots (time, audience, endpoint, data) VALUES ($1, $2, $3, $4)`,
		at, audience, endpoint, string(data))
	if err != nil {
		return fmt.Errorf("write snapshot: %w", err)
	}
	return nil
}
