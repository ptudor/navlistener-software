package store

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

const MaxCurrentConditions = 10000

type ConditionSnapshot struct {
	Cursor int64         `json:"cursor"`
	Events []StoredEvent `json:"events"`
}

// CurrentConditions returns one latest transition (including resolutions) per
// station condition and its audience cursor from ONE PostgreSQL snapshot.
// gnss_events has no retention policy. Only the current policy epoch is visible;
// neither the history API's 24h default nor its pagination limits apply here.
// A timeout or too many conditions fails the whole snapshot, never truncates it.
func (s *Store) CurrentConditions(ctx context.Context, audience string, since time.Time) (ConditionSnapshot, error) {
	out := ConditionSnapshot{Events: []StoredEvent{}}
	if since.IsZero() {
		since = time.Unix(0, 0)
	}
	rows, err := s.pool.Query(ctx, `
 WITH visible AS MATERIALIZED (
   SELECT * FROM gnss_events WHERE audience=$1 AND time >= $2
 ), latest AS (
   SELECT DISTINCT ON (COALESCE(raw->>'station',sv),event_type,COALESCE(raw->>'gnss',''),COALESCE(raw->>'sig',''))
     audience_seq,time,sv,event_type,COALESCE(old_value,'') AS old_value,COALESCE(new_value,'') AS new_value,
     severity,COALESCE(message,'') AS message,raw
   FROM visible
   WHERE event_type IN ('station_offline','jamming_detected','spoofing_suspected','station_rf_degraded',
                        'antenna_fault','capability_signal_lost','capability_impossible')
   ORDER BY COALESCE(raw->>'station',sv),event_type,COALESCE(raw->>'gnss',''),COALESCE(raw->>'sig',''),audience_seq DESC
 ), bounded AS (SELECT * FROM latest ORDER BY audience_seq LIMIT $3)
 SELECT (SELECT COALESCE(max(audience_seq),0) FROM visible),
        COALESCE((SELECT jsonb_agg(to_jsonb(bounded)) FROM bounded),'[]'::jsonb)`,
		audience, since, MaxCurrentConditions+1)
	if err != nil {
		return out, fmt.Errorf("current conditions: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return out, fmt.Errorf("current conditions: missing snapshot: %v", rows.Err())
	}
	// SQL bounds the row count. Decode into the normal event shape after mapping
	// column names; raw params retain the same interpretation as query/SSE events.
	var data []byte
	if err := rows.Scan(&out.Cursor, &data); err != nil {
		return out, err
	}
	var values []struct {
		ID       int64           `json:"audience_seq"`
		Time     time.Time       `json:"time"`
		SV       string          `json:"sv"`
		Type     string          `json:"event_type"`
		Old      string          `json:"old_value"`
		New      string          `json:"new_value"`
		Severity int             `json:"severity"`
		Message  string          `json:"message"`
		Raw      json.RawMessage `json:"raw"`
	}
	if err := json.Unmarshal(data, &values); err != nil {
		return out, err
	}
	if len(values) > MaxCurrentConditions {
		return out, fmt.Errorf("current conditions exceeds %d", MaxCurrentConditions)
	}
	for _, v := range values {
		out.Events = append(out.Events, StoredEvent{ID: v.ID, Time: v.Time.UTC(), SV: v.SV, Type: v.Type, OldValue: v.Old, NewValue: v.New, Severity: v.Severity, Message: v.Message, Params: v.Raw})
	}
	return out, rows.Err()
}
