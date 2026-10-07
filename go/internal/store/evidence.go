package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/ptudor/navlistener/internal/identity"
)

// EvidenceEventTypes are the station event types whose stored inputs are captured
// as durable evidence (docs/proposals/STATION-ASSURANCE.md §7, item 2.1).
var EvidenceEventTypes = []string{
	"spoofing_suspected", "station_assurance", "jamming_detected", "station_rf_degraded", "antenna_fault",
}

// EvidencePolicy bounds evidence capture.
type EvidencePolicy struct {
	// PreRoll and PostRoll are the stored inputs kept before and after the event.
	// Capture waits until the event is PostRoll old, which also lets the
	// historian commit the inputs that led to it.
	PreRoll, PostRoll time.Duration
	// Horizon is how far back capture looks. Events older than it are skipped,
	// because their inputs have expired; it must stay inside raw retention.
	Horizon time.Duration
	// Batch bounds the events captured in one sweep.
	Batch int
	// MaxSamples bounds the samples kept per origin per event; a bundle that hit
	// it is marked truncated.
	MaxSamples int
}

// Validate rejects a policy capture cannot apply.
func (p EvidencePolicy) Validate() error {
	if p.PreRoll <= 0 || p.PostRoll < 0 || p.Horizon <= p.PostRoll || p.Batch < 1 || p.MaxSamples < 1 {
		return fmt.Errorf("evidence policy needs positive pre-roll, batch and sample bound, and a horizon beyond the post-roll: %+v", p)
	}
	return nil
}

var evidenceColumns = []string{
	"audience", "audience_seq", "event_time", "collector_instance_id", "source_id",
	"window_start", "window_end", "captured_at", "rf_samples", "board_samples", "truncated",
}

// evidenceSampleColumns is the event key and origin, then the shared rf_samples /
// observer_samples layout and the two receipt clocks.
var evidenceSampleColumns = append([]string{"audience", "audience_seq", "event_time", "origin"}, rfColumns...)

// evidenceSelect is each origin's select list for evidenceSampleColumns. A board row's
// received_at is already the collector-local receipt and its sample_time the
// observer's stamp, which are exactly the two clocks an RF row stores separately.
var evidenceSelect = map[string]string{
	"rf":    strings.Join(rfColumns, ", "),
	"board": strings.Join(sampleColumns, ", ") + ", received_at, sample_time",
}

// pendingEvidence is one event whose evidence has not been captured.
type pendingEvidence struct {
	audience string
	seq      int64
	time     time.Time
	station  string
}

// CaptureEventEvidence copies the stored inputs behind this collector's recent
// station events into durable evidence and returns how many bundles it wrote.
// Each bundle holds the station's rf_samples and observer_samples received within
// [event − PreRoll, event + PostRoll], scoped to what the event's audience may
// see: an organization audience keeps its organization's samples, a collection
// audience its collection's, and an operator audience the collector's. A public
// event gets an empty bundle, because station telemetry is never public. Capture
// is idempotent and crash-safe: a bundle row is written once per event, so a
// restart resumes with the events still missing one.
func (s *Store) CaptureEventEvidence(ctx context.Context, collectorID string, now time.Time, p EvidencePolicy) (int, error) {
	if !identity.ValidScopeID(collectorID) {
		return 0, fmt.Errorf("evidence capture: invalid collector id")
	}
	if err := p.Validate(); err != nil {
		return 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT e.audience, e.audience_seq, e.time, e.sv FROM gnss_events e
		WHERE e.collector_instance_id = $1 AND e.event_type = ANY($2)
		AND e.time >= $3 AND e.time <= $4
		AND NOT EXISTS (SELECT 1 FROM event_evidence v WHERE v.audience = e.audience AND v.audience_seq = e.audience_seq)
		ORDER BY e.time, e.audience, e.audience_seq
		LIMIT $5`,
		collectorID, EvidenceEventTypes, now.Add(-p.Horizon), now.Add(-p.PostRoll), p.Batch)
	if err != nil {
		return 0, fmt.Errorf("evidence capture: pending events: %w", err)
	}
	var pending []pendingEvidence
	for rows.Next() {
		var e pendingEvidence
		if err := rows.Scan(&e.audience, &e.seq, &e.time, &e.station); err != nil {
			rows.Close()
			return 0, fmt.Errorf("evidence capture: scan: %w", err)
		}
		pending = append(pending, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("evidence capture: pending events: %w", err)
	}
	written := 0
	for _, e := range pending {
		ok, err := s.captureOne(ctx, collectorID, e, now, p)
		if err != nil {
			return written, err
		}
		if ok {
			written++
		}
	}
	return written, nil
}

// rfKinds and boardKinds name every kind of each source table, so the capture
// query can use the (source_id, kind, time) indexes.
var (
	rfKinds    = []string{"reception", "jamming", "combined", "solution"}
	boardKinds = []string{"environment", "timing"}
)

func (s *Store) captureOne(ctx context.Context, collectorID string, e pendingEvidence, now time.Time, p EvidencePolicy) (bool, error) {
	a, err := identity.ParseAudience(e.audience)
	if err != nil {
		return false, fmt.Errorf("evidence capture: event %s/%d: %w", e.audience, e.seq, err)
	}
	start, end := e.time.Add(-p.PreRoll), e.time.Add(p.PostRoll)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("evidence capture: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// The bundle row first: a concurrent capture of the same event waits on the
	// primary key and then finds it taken, so samples are never copied twice.
	tag, err := tx.Exec(ctx, `INSERT INTO event_evidence (`+strings.Join(evidenceColumns, ", ")+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 0, 0, false) ON CONFLICT DO NOTHING`,
		e.audience, e.seq, e.time, collectorID, e.station, start, end, now)
	if err != nil {
		return false, fmt.Errorf("evidence capture: bundle: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return false, nil
	}
	counts := map[string]int64{}
	truncated := false
	if a.Kind != identity.AudiencePublic {
		for _, src := range []struct {
			origin, table, timeColumn string
			kinds                     []string
		}{
			{"rf", "rf_samples", "sample_time", rfKinds},
			{"board", "observer_samples", "received_at", boardKinds},
		} {
			args := []any{collectorID, e.station, src.kinds, start, end}
			scope := ""
			switch a.Kind {
			case identity.AudienceOrganization:
				scope = " AND organization_id = $6"
				args = append(args, a.ID)
			case identity.AudienceCollection:
				scope = " AND $6 = ANY(collection_ids)"
				args = append(args, a.ID)
			}
			where := ` FROM ` + src.table + ` WHERE collector_instance_id = $1 AND source_id = $2
				AND kind = ANY($3) AND ` + src.timeColumn + ` >= $4 AND ` + src.timeColumn + ` <= $5` + scope
			// Newest first, so when the bound bites it is the oldest pre-roll samples
			// that go and the event instant and the post-roll survive — the part of
			// the window an investigation reads first.
			order := ` ORDER BY ` + src.timeColumn + ` DESC, ts DESC`
			n := len(args)
			p1, p2, p3, p4 := "$"+strconv.Itoa(n+1), "$"+strconv.Itoa(n+2), "$"+strconv.Itoa(n+3), "$"+strconv.Itoa(n+4)
			bound, probe := "$"+strconv.Itoa(n+5), "$"+strconv.Itoa(n+6)
			insertArgs := append(args, e.audience, e.seq, e.time, src.origin, p.MaxSamples, p.MaxSamples+1)
			// One statement, one snapshot: the candidates are one more than the
			// bound, the copy keeps the bound, and truncated is decided by the same
			// rows the copy saw. A separate count ran in its own snapshot, so a
			// sample committed between the two left a bundle that hit the bound
			// unmarked, and a window that exactly filled the bound cannot be told
			// from one that overflowed it by RowsAffected alone.
			var kept, seen int64
			if err := tx.QueryRow(ctx, `WITH candidates (`+strings.Join(rfColumns, ", ")+`) AS (
					SELECT `+evidenceSelect[src.origin]+where+order+` LIMIT `+probe+`
				), kept AS (
					INSERT INTO event_evidence_samples (`+strings.Join(evidenceSampleColumns, ", ")+`)
					SELECT `+p1+`, `+p2+`, `+p3+`, `+p4+`, `+strings.Join(rfColumns, ", ")+`
					FROM candidates`+order+` LIMIT `+bound+`
					RETURNING 1
				)
				SELECT (SELECT count(*) FROM kept), (SELECT count(*) FROM candidates)`, insertArgs...).Scan(&kept, &seen); err != nil {
				return false, fmt.Errorf("evidence capture: copy %s: %w", src.table, err)
			}
			counts[src.origin] = kept
			if seen > kept {
				truncated = true
			}
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE event_evidence SET rf_samples = $3, board_samples = $4, truncated = $5
		WHERE audience = $1 AND audience_seq = $2`, e.audience, e.seq, counts["rf"], counts["board"], truncated); err != nil {
		return false, fmt.Errorf("evidence capture: counts: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("evidence capture: commit: %w", err)
	}
	return true, nil
}

// EvidenceMaxLimit bounds one page of evidence samples.
const EvidenceMaxLimit = 500

// EvidenceQuery names one event's evidence for a server-authorized private
// audience; the scope is never taken from query parameters.
type EvidenceQuery struct {
	Audience      identity.Audience
	Seq           int64
	Limit, Offset int
}

// Validate rejects a query the store must not run.
func (q EvidenceQuery) Validate() error {
	a, err := identity.ParseAudience(q.Audience.Key())
	if err != nil || a != q.Audience || a.Kind == identity.AudiencePublic {
		return fmt.Errorf("private audience required")
	}
	if q.Seq < 1 || q.Limit < 1 || q.Limit > EvidenceMaxLimit || q.Offset < 0 || q.Offset > ObserverHistoryMaxOffset {
		return fmt.Errorf("id must be positive, limit 1..%d and offset 0..%d", EvidenceMaxLimit, ObserverHistoryMaxOffset)
	}
	return nil
}

// EventEvidence is one event with its captured evidence window and a page of samples.
type EventEvidence struct {
	Event        StoredEvent      `json:"event"`
	WindowStart  time.Time        `json:"window_start"`
	WindowEnd    time.Time        `json:"window_end"`
	CapturedAt   time.Time        `json:"captured_at"`
	RFSamples    int              `json:"rf_samples"`
	BoardSamples int              `json:"board_samples"`
	Truncated    bool             `json:"truncated"`
	Samples      []EvidenceSample `json:"samples"`
	HasMore      bool             `json:"has_more"`
}

// EvidenceSample is one copied input: its origin table and kind, the exact stored
// body and its decoded projection, and its receipt evidence. Sequence is decimal
// text so browsers keep all 64 bits.
type EvidenceSample struct {
	Origin     string     `json:"origin"`
	Kind       string     `json:"kind"`
	ReceivedAt time.Time  `json:"received_at"`
	SampleTime *time.Time `json:"sample_time"`
	// LocalReceivedAt is the collector-local receipt instant and WallClockStamp the
	// independent stamp the station checks used; both null on rows stored before
	// they were recorded.
	LocalReceivedAt *time.Time      `json:"local_received_at"`
	WallClockStamp  *time.Time      `json:"wall_clock_stamp"`
	Session         *string         `json:"session"`
	Sequence        *string         `json:"sequence"`
	HardwareTrust   string          `json:"hardware_trust"`
	Raw             []byte          `json:"raw"`
	Data            json.RawMessage `json:"data"`
}

// ErrNoEvidence reports an event without captured evidence in the audience: no such
// event, an event of a type that is not captured, or one not captured yet.
var ErrNoEvidence = errors.New("no evidence for this event")

// QueryEventEvidence returns one event's evidence and a page of its samples, in
// origin and receipt order. The audience is the event's own, so the copied samples
// are exactly those that audience was allowed to see when they were captured.
func (s *Store) QueryEventEvidence(ctx context.Context, q EvidenceQuery) (EventEvidence, error) {
	var out EventEvidence
	if err := q.Validate(); err != nil {
		return out, err
	}
	key := q.Audience.Key()
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT e.audience_seq, e.time, e.sv, e.event_type, COALESCE(e.old_value, ''),
			COALESCE(e.new_value, ''), e.severity, COALESCE(e.message, ''), e.raw,
			v.window_start, v.window_end, v.captured_at, v.rf_samples, v.board_samples, v.truncated
		FROM event_evidence v
		JOIN gnss_events e ON e.audience = v.audience AND e.audience_seq = v.audience_seq AND e.time = v.event_time
		WHERE v.audience = $1 AND v.audience_seq = $2`, key, q.Seq).Scan(
		&out.Event.ID, &out.Event.Time, &out.Event.SV, &out.Event.Type, &out.Event.OldValue, &out.Event.NewValue,
		&out.Event.Severity, &out.Event.Message, &raw,
		&out.WindowStart, &out.WindowEnd, &out.CapturedAt, &out.RFSamples, &out.BoardSamples, &out.Truncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, ErrNoEvidence
	}
	if err != nil {
		return out, fmt.Errorf("evidence query: %w", err)
	}
	if len(raw) > 0 {
		out.Event.Params = json.RawMessage(raw)
	}
	// The copied bodies are only as bounded as the wire that carried them was
	// when they were stored, so the same guards as the observer-history reader
	// apply: a body over the per-sample cap is nulled before transfer and fails
	// the page, and the page stops at the shared byte budget with HasMore.
	rows, err := s.pool.Query(ctx, `SELECT origin, kind, received_at, sample_time, source_session, source_seq,
			hardware_trust,
			CASE WHEN octet_length(raw) <= $6 THEN raw END,
			CASE WHEN octet_length(data::text) <= $6 THEN data END,
			local_received_at, wall_clock_stamp
		FROM event_evidence_samples
		WHERE audience = $1 AND audience_seq = $2 AND event_time = $3
		ORDER BY origin DESC, received_at, ts, source_session COLLATE "C" NULLS FIRST, source_seq NULLS FIRST
		LIMIT $4 OFFSET $5`, key, q.Seq, out.Event.Time, q.Limit+1, q.Offset, historyMaxSampleBytes)
	if err != nil {
		return out, fmt.Errorf("evidence samples: %w", err)
	}
	defer rows.Close()
	out.Samples = []EvidenceSample{}
	var budget historyPageBudget
	for rows.Next() {
		if len(out.Samples) == q.Limit {
			out.HasMore = true
			break
		}
		var sample EvidenceSample
		var seq *int64
		if err := rows.Scan(&sample.Origin, &sample.Kind, &sample.ReceivedAt, &sample.SampleTime, &sample.Session, &seq,
			&sample.HardwareTrust, &sample.Raw, &sample.Data, &sample.LocalReceivedAt, &sample.WallClockStamp); err != nil {
			return out, fmt.Errorf("evidence samples scan: %w", err)
		}
		// raw is NOT NULL in the table, so a nil here is the size guard above
		// (pgx scans a NULL bytea as nil and an empty one as a non-nil empty slice).
		if sample.Raw == nil || !validSampleBody(sample.Data) {
			return EventEvidence{}, fmt.Errorf("event evidence contains an invalid or oversized sample")
		}
		if seq != nil {
			value := strconv.FormatUint(uint64(*seq), 10)
			sample.Sequence = &value
		}
		fits, err := budget.admit(sample)
		if err != nil {
			return EventEvidence{}, fmt.Errorf("evidence samples encode: %w", err)
		}
		if !fits {
			out.HasMore = true
			break
		}
		out.Samples = append(out.Samples, sample)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("evidence samples read: %w", err)
	}
	return out, nil
}
