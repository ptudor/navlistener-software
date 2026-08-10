package store

import (
	"context"
	"fmt"
	"time"
)

// NavFrameQuery selects persisted raw nav frames for replay.
//
// This is the read side of the historian's central promise (docs/DESIGN.md
// §1): raw frames are stored undecoded so a decoder fix can be re-run over history.
// Without a query path that promise is only aspirational — the frames go in and the
// only replayable artefact is whatever someone happened to `cat` to a file at the
// time.
type NavFrameQuery struct {
	// GnssID restricts to one constellation (u-blox numbering). Nil means all.
	GnssID *int
	// SourceID restricts to one ingest source / observer. Empty means all — note
	// that N receivers seeing one SV is the normal case and the duplicates are
	// meaningful, so a replay that wants one receiver's view must say so.
	SourceID string
	// Since and Until bound reception time (received_at), not ingest time: replay
	// cares when the receiver heard the frame, not when the writer flushed it.
	// Until zero means "no upper bound".
	Since, Until time.Time
	// Limit caps rows returned; 0 applies defaultNavFrameLimit. A replay that
	// silently stops at a cap would understate a distribution, so ErrNavFrameLimit
	// is returned when the cap is actually hit rather than truncating quietly.
	Limit int
}

// defaultNavFrameLimit bounds an unbounded query. Roughly a day of one receiver's
// full-constellation output at observed rates (~26 SFRBX/s) — high enough that a
// normal QA window is never clipped, low enough that a mistyped range cannot pull a
// whole hypertable into memory.
const defaultNavFrameLimit = 2_500_000

// ErrNavFrameLimit reports that the query stopped at Limit with more rows matching.
// Callers must treat partial results as unfit for a distribution: a histogram built
// from a silently truncated head is wrong in a way that looks plausible.
var ErrNavFrameLimit = fmt.Errorf("nav frame query hit its row limit; narrow the range or raise Limit")

// StoredNavFrame is one persisted frame, shaped for reconstruction into an
// ingest.RawFrame. Words are not stored as such — Raw holds the frame bytes, which
// for a word-oriented source is the big-endian nav words back to back.
type StoredNavFrame struct {
	ReceivedAt          time.Time
	SourceID            string
	OrganizationID      string
	EnrollmentID        string
	CollectorInstanceID string
	CollectionIDs       []string
	Provenance          string
	CredentialTier      string
	AttestationTier     string
	AggregateUse        string
	StationMetadata     string
	PolicyRevision      string
	GnssID              int
	SvID                int
	SigID               int
	FreqID              int // GLONASS FDMA channel (k = FreqID - 7); 0 for other constellations
	MsgType             int
	Raw                 []byte
}

// Close releases the connection pool.
//
// The daemon does not need this — Run closes the pool on its shutdown path — but a
// read-only consumer (a replay or export tool) never starts Run and would otherwise
// leak the pool. Guarded by a sync.Once so calling it alongside a Run shutdown is
// safe rather than a double close.
func (s *Store) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.closeOnce.Do(s.pool.Close)
}

// QueryNavFrames streams matching frames in reception order, calling fn for each.
//
// Streaming rather than returning a slice is deliberate: a multi-hour GLONASS window
// is hundreds of thousands of frames, and a replay consumes them strictly in order —
// materialising the whole set would cost hundreds of megabytes to no purpose. An
// error from fn aborts the scan and is returned unchanged.
func (s *Store) QueryNavFrames(ctx context.Context, q NavFrameQuery, fn func(StoredNavFrame) error) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("query nav frames: no database configured")
	}
	if q.Since.IsZero() {
		return fmt.Errorf("query nav frames: Since is required (an unbounded scan of a hypertable is never what a replay wants)")
	}
	if !q.Until.IsZero() && !q.Until.After(q.Since) {
		return fmt.Errorf("query nav frames: Until (%s) must be after Since (%s)", q.Until, q.Since)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultNavFrameLimit
	}

	// Ordered by received_at to reproduce the receiver's arrival order, which is what
	// live state saw; ts (the ingest-time hypertable dimension) can reorder across a
	// feeder reconnect replay. The trailing keys make the order total, so a rerun of
	// the same window replays identically rather than permuting frames that share a
	// timestamp.
	sql := `SELECT received_at, source_id, organization_id, enrollment_id,
	               collector_instance_id, collection_ids, provenance, credential_tier,
	               attestation_tier, aggregate_use, station_metadata, policy_revision,
	               gnssid, svid, sigid, freqid, msg_type, raw
	          FROM nav_frames
	         WHERE received_at >= $1
	           AND ($2::timestamptz IS NULL OR received_at < $2)
	           AND ($3::smallint   IS NULL OR gnssid = $3)
	           AND ($4::text       IS NULL OR source_id = $4)
	         ORDER BY received_at, source_id, gnssid, svid, sigid
	         LIMIT $5`

	var until, gnssID, source any
	if !q.Until.IsZero() {
		until = q.Until
	}
	if q.GnssID != nil {
		gnssID = int16(*q.GnssID)
	}
	if q.SourceID != "" {
		source = q.SourceID
	}

	rows, err := s.pool.Query(ctx, sql, q.Since, until, gnssID, source, limit+1)
	if err != nil {
		return fmt.Errorf("query nav frames: %w", err)
	}
	defer rows.Close()

	n := 0
	for rows.Next() {
		n++
		if n > limit {
			return ErrNavFrameLimit // the +1 row proves more matched
		}
		var (
			f                         StoredNavFrame
			gid, sv, sig, freq, mtype int16
		)
		if err := rows.Scan(
			&f.ReceivedAt, &f.SourceID, &f.OrganizationID, &f.EnrollmentID,
			&f.CollectorInstanceID, &f.CollectionIDs, &f.Provenance, &f.CredentialTier,
			&f.AttestationTier, &f.AggregateUse, &f.StationMetadata, &f.PolicyRevision,
			&gid, &sv, &sig, &freq, &mtype, &f.Raw,
		); err != nil {
			return fmt.Errorf("query nav frames: scan: %w", err)
		}
		f.GnssID, f.SvID, f.SigID = int(gid), int(sv), int(sig)
		f.FreqID, f.MsgType = int(freq), int(mtype)
		if err := fn(f); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("query nav frames: %w", err)
	}
	return nil
}
