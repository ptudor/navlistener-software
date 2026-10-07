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
	ObserverHistoryMaxLimit   = 500
	ObserverHistoryMaxOffset  = 100_000
	ObserverHistoryMaxWindow  = 30 * 24 * time.Hour
	ObserverHistoryMaxIDBytes = 4096
	// historyMaxSampleBytes caps one stored body (a sample's decoded JSON, an
	// evidence sample's raw wire body) the history readers will serve, and
	// historyMaxPageBytes caps the encoded bytes of one page. Both apply to
	// observer history and event evidence alike.
	historyMaxSampleBytes = 64 * 1024
	historyMaxPageBytes   = 4 * 1024 * 1024
)

// historyPageBudget bounds one page of stored samples — observer history and
// event evidence alike — by its encoded bytes, so a page of large stored rows is
// never assembled into one response the read API's write deadline cannot
// deliver. The reader stops before the sample that would cross the budget and
// reports HasMore; the row count limit alone cannot bound the bytes, because a
// stored body may be anything up to the per-sample cap.
type historyPageBudget struct{ used int }

// admit encodes sample as the page will and charges it to the budget. False
// means the page is full and sample belongs to the next one.
func (b *historyPageBudget) admit(sample any) (bool, error) {
	encoded, err := json.Marshal(sample)
	if err != nil {
		return false, err
	}
	if b.used+len(encoded) > historyMaxPageBytes {
		return false, nil
	}
	b.used += len(encoded)
	return true, nil
}

// validSampleBody reports whether a stored JSON body is present, within the
// per-sample cap and well formed. The readers' SQL nulls a body over the cap
// before it is transferred, so an absent body here is an oversized or corrupt
// stored record, and the page fails rather than silently skipping it.
func validSampleBody(body json.RawMessage) bool {
	return len(body) > 0 && len(body) <= historyMaxSampleBytes && json.Valid(body)
}

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
	// PostgreSQL stores microseconds. Round the lower bound up so a policy
	// transition with nanosecond precision cannot reveal an earlier receipt in
	// the same microsecond when the driver truncates the bound.
	since := q.Since.Truncate(time.Microsecond)
	if since.Before(q.Since) {
		since = since.Add(time.Microsecond)
	}
	args := []any{q.CollectorID, q.Observer, q.Kind, since, q.Until.Truncate(time.Microsecond), q.Limit + 1, q.Offset}
	args = append(args, historyMaxSampleBytes)
	scope := ""
	switch q.Audience.Kind {
	case identity.AudienceOrganization:
		scope = " AND organization_id = $9"
		args = append(args, q.Audience.ID)
	case identity.AudienceCollection:
		scope = " AND $9 = ANY(collection_ids)"
		args = append(args, q.Audience.ID)
	}
	// Protect the reader from oversized historical records before transferring
	// them to Go. A rejected record fails the page; it is never silently skipped.
	rows, err := s.pool.Query(ctx, `SELECT received_at, sample_time,
		CASE WHEN octet_length(source_session) <= 256 THEN source_session END,
		source_seq,
		CASE WHEN octet_length(hardware_trust) <= 32 THEN hardware_trust END,
		CASE WHEN octet_length(data::text) <= $8 AND
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
	var budget historyPageBudget
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
		if trust == nil || !validSampleBody(sample.Details) {
			return ObserverSamplePage{}, fmt.Errorf("observer history contains an invalid or oversized sample")
		}
		sample.HardwareTrust = *trust
		if seq != nil {
			value := strconv.FormatUint(uint64(*seq), 10)
			sample.Sequence = &value
		}
		fits, err := budget.admit(sample)
		if err != nil {
			return ObserverSamplePage{}, fmt.Errorf("observer history encode: %w", err)
		}
		if !fits {
			page.HasMore = true
			break
		}
		page.Samples = append(page.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		return ObserverSamplePage{}, fmt.Errorf("observer history read: %w", err)
	}
	return page, nil
}
