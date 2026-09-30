package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

const (
	ObserverHistoryMaxLimit       = 500
	ObserverHistoryMaxOffset      = 100_000
	ObserverHistoryMaxWindow      = 30 * 24 * time.Hour
	ObserverHistoryMaxIDBytes     = 4096
	observerHistoryMaxSampleBytes = 64 * 1024
	observerHistoryMaxPageBytes   = 4 * 1024 * 1024
)

// ObserverSampleQuery carries a server-authorized audience, never a scope taken
// directly from query parameters. The caller must enforce current read grants
// and the audience's policy-history boundary before calling the store.
type ObserverSampleQuery struct {
	Audience      identity.Audience
	CollectorID   string
	Observer      string
	Kind          string
	Since, Until  time.Time
	Limit, Offset int
}

func (q ObserverSampleQuery) Validate() error {
	a, err := identity.ParseAudience(q.Audience.Key())
	if err != nil || a != q.Audience || a.Kind == identity.AudiencePublic {
		return fmt.Errorf("private audience required")
	}
	if !identity.ValidScopeID(q.CollectorID) || (a.Kind == identity.AudienceOperator && a.ID != q.CollectorID) {
		return fmt.Errorf("collector scope mismatch")
	}
	if a.Kind == identity.AudienceOrganization && a.ID == identity.UnassignedOrganization {
		return fmt.Errorf("unassigned organization has no owner history")
	}
	if !identity.ValidOpaqueObserverID(q.Observer) || strings.ContainsRune(q.Observer, 0) || len(q.Observer) > ObserverHistoryMaxIDBytes {
		return fmt.Errorf("observer must be nonempty UTF-8, without NUL, at most %d bytes", ObserverHistoryMaxIDBytes)
	}
	if q.Kind != "environment" && q.Kind != "timing" {
		return fmt.Errorf("kind must be environment or timing")
	}
	if q.Since.IsZero() || q.Until.IsZero() || q.Since.After(q.Until) || q.Until.Sub(q.Since) > ObserverHistoryMaxWindow {
		return fmt.Errorf("history window must be ordered and at most 30 days")
	}
	if q.Limit < 1 || q.Limit > ObserverHistoryMaxLimit || q.Offset < 0 || q.Offset > ObserverHistoryMaxOffset {
		return fmt.Errorf("limit must be 1..500 and offset 0..100000")
	}
	return nil
}

// StoredObserverSample exposes decoded measurements and receipt evidence only.
// Sequence is decimal text so browsers preserve all 64 bits. Nullable source
// metadata remains null; a missing feeder UTC stamp is never replaced by receipt time.
type StoredObserverSample struct {
	ReceivedAt    time.Time       `json:"received_at"`
	SampleTime    *time.Time      `json:"sample_time"`
	Session       *string         `json:"session"`
	Sequence      *string         `json:"sequence"`
	HardwareTrust string          `json:"hardware_trust"`
	Details       json.RawMessage `json:"details"`
}

type ObserverSamplePage struct {
	Samples []StoredObserverSample
	HasMore bool
}

// QueryObserverSamples applies immutable receipt-time scope in SQL, including
// the collector boundary when multiple collectors share a database. It does not
// read raw wire bodies, credentials, commissioning records or authority evidence.
func (s *Store) QueryObserverSamples(ctx context.Context, q ObserverSampleQuery) (ObserverSamplePage, error) {
	page := ObserverSamplePage{Samples: []StoredObserverSample{}}
	if err := q.Validate(); err != nil {
		return page, err
	}
	args := []any{q.CollectorID, q.Observer, q.Kind, q.Since, q.Until, q.Limit + 1, q.Offset}
	scope := ""
	switch q.Audience.Kind {
	case identity.AudienceOrganization:
		scope = " AND organization_id = $8"
		args = append(args, q.Audience.ID)
	case identity.AudienceCollection:
		scope = " AND $8 = ANY(collection_ids)"
		args = append(args, q.Audience.ID)
	}
	// Protect the reader from oversized historical records before transferring
	// them to Go. A rejected record fails the page; it is never silently skipped.
	rows, err := s.pool.Query(ctx, `SELECT received_at, sample_time,
		CASE WHEN octet_length(source_session) <= 256 THEN source_session END,
		source_seq,
		CASE WHEN octet_length(hardware_trust) <= 32 THEN hardware_trust END,
		CASE WHEN octet_length(data::text) <= 65536 AND
			(source_session IS NULL OR octet_length(source_session) <= 256)
			THEN data END
		FROM observer_samples
		WHERE collector_instance_id = $1 AND source_id = $2 AND kind = $3
		AND received_at >= $4 AND received_at <= $5`+scope+`
		ORDER BY received_at, ts, source_session COLLATE "C" NULLS FIRST,
			source_seq NULLS FIRST, sample_time NULLS FIRST, hardware_trust COLLATE "C", data::text COLLATE "C"
		LIMIT $6 OFFSET $7`, args...)
	if err != nil {
		return page, fmt.Errorf("observer history query: %w", err)
	}
	defer rows.Close()
	pageBytes := 0
	for rows.Next() {
		if len(page.Samples) == q.Limit {
			page.HasMore = true
			break
		}
		var sample StoredObserverSample
		var seq *int64
		var trust *string
		if err := rows.Scan(&sample.ReceivedAt, &sample.SampleTime, &sample.Session, &seq, &trust, &sample.Details); err != nil {
			return ObserverSamplePage{}, fmt.Errorf("observer history scan: %w", err)
		}
		if trust == nil || len(sample.Details) == 0 || len(sample.Details) > observerHistoryMaxSampleBytes || !json.Valid(sample.Details) {
			return ObserverSamplePage{}, fmt.Errorf("observer history contains an invalid or oversized sample")
		}
		sample.HardwareTrust = *trust
		if seq != nil {
			value := strconv.FormatUint(uint64(*seq), 10)
			sample.Sequence = &value
		}
		encoded, err := json.Marshal(sample)
		if err != nil {
			return ObserverSamplePage{}, fmt.Errorf("observer history encode: %w", err)
		}
		if pageBytes+len(encoded) > observerHistoryMaxPageBytes {
			page.HasMore = true
			break
		}
		pageBytes += len(encoded)
		page.Samples = append(page.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		return ObserverSamplePage{}, fmt.Errorf("observer history read: %w", err)
	}
	return page, nil
}
