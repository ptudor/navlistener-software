package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/gnss"
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

// maxNavFrameLimit is the largest limit the query accepts. It is
// far above any plausible replay window and far below the point where limit+1
// could overflow, so the probe row that detects "more matched" is always safe.
const maxNavFrameLimit = 100_000_000

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
	// ReceiptOrder is the database's first-storage admission sequence. Nil marks
	// pre-migration history whose original order cannot be reconstructed.
	ReceiptOrder *int64
	Session      string
	SourceSeq    uint64
	HasSourceSeq bool

	ReceivedAt            time.Time
	SourceID              string
	OrganizationID        string
	EnrollmentID          string
	CollectorInstanceID   string
	CollectionIDs         []string
	Provenance            string
	CredentialTier        string
	CredentialFingerprint string
	AttestationTier       string
	// HardwareTrust and CommissioningFingerprint are the collector's verified
	// hardware evidence at receipt; "none" and empty for rows stored before the
	// columns existed and for every source that presented no evidence.
	HardwareTrust            string
	ManufacturerAuthorityID  string
	CommissioningFingerprint string
	AggregateUse             string
	StationMetadata          string
	EventVisibility          string
	RawExport                string
	FederationPeers          []string
	PublishSignals           []string
	PolicyRevision           string
	GnssID                   int
	SvID                     int
	SigID                    int
	FreqID                   int // GLONASS FDMA channel (k = FreqID - 7); 0 for other constellations
	MsgType                  int
	SBFHeader                []byte // nil means legacy/unknown SBF revision and header
	Raw                      []byte
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

// QueryNavFrames streams matching frames in stable first-storage admission order.
// Receipt timestamps only select the half-open window; rollback does not reorder
// new rows. Legacy rows precede new rows using the documented deterministic
// timestamp/content fallback, which cannot reconstruct their original arrival.
//
// Streaming rather than returning a slice is deliberate: a multi-hour GLONASS window
// is hundreds of thousands of frames, and a replay consumes them strictly in order —
// materialising the whole set would cost hundreds of megabytes to no purpose. An
// error from fn aborts the scan and is returned unchanged.
func (s *Store) QueryNavFrames(ctx context.Context, q NavFrameQuery, fn func(StoredNavFrame) error) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("query nav frames: no database configured")
	}
	return queryNavFrames(ctx, s.pool, q, fn)
}

const navFrameSelect = `SELECT received_at, source_id, organization_id, enrollment_id,
	               collector_instance_id, collection_ids, provenance, credential_tier,
	               credential_fingerprint, attestation_tier, hardware_trust, manufacturer_authority_id, commissioning_fingerprint,
	               aggregate_use, station_metadata, event_visibility,
	               raw_export, federation_peers, publish_signals, policy_revision,
	               gnssid, svid, sigid, freqid, msg_type, raw, sbf_header, receipt_order, COALESCE(source_session, ''), source_seq
	          FROM nav_frames`

func queryNavFrames(ctx context.Context, pool *pgxpool.Pool, q NavFrameQuery, fn func(StoredNavFrame) error) error {
	if q.Since.IsZero() {
		return fmt.Errorf("query nav frames: Since is required (an unbounded scan of a hypertable is never what a replay wants)")
	}
	if !q.Until.IsZero() && !q.Until.After(q.Since) {
		return fmt.Errorf("query nav frames: Until (%s) must be after Since (%s)", q.Until, q.Since)
	}
	// this is a reusable exported query type, so the caller's
	// numeric fields are validated before they reach SQL rather than relying on
	// today's replay callers happening to pass small values. An out-of-range GNSS
	// id used to be cast straight to int16 — 65,536 wrapping to 0 silently
	// answered a *different* constellation's question — and a MaxInt limit
	// overflowed at limit+1, producing a database error instead of the API's
	// documented limit behavior.
	if q.GnssID != nil {
		id := *q.GnssID
		if id < 0 || id > int(gnss.NavIC) || !gnss.GNSSID(id).Valid() {
			return fmt.Errorf("query nav frames: gnss id %d is outside the constellation domain "+
				"(0..%d minus IMES; docs/CONSTELLATIONS.md §0)", id, int(gnss.NavIC))
		}
	}
	limit := q.Limit
	if limit <= 0 {
		limit = defaultNavFrameLimit
	}
	// The +1 probe row below must not overflow, and a limit far past any plausible
	// replay is a caller bug worth naming rather than a query that cannot run.
	if limit > maxNavFrameLimit {
		return fmt.Errorf("query nav frames: limit %d exceeds the maximum supported limit %d",
			limit, maxNavFrameLimit)
	}

	// New rows use the persistent first-storage sequence, stable across chunks,
	// reconnect dedup, compression, process restart and receiver-clock rollback.
	// Legacy records have no recoverable original order: put them first, ordered
	// by reception/ingest time then the complete immutable forensic tuple under C
	// collation. Exact duplicate legacy tuples are interchangeable for replay; no
	// arbitrary message-type sort is described as original arrival order. Exclude
	// mutable decoded projections/version from the legacy tie breaker.
	sql := navFrameSelect + `
	         WHERE received_at >= $1
	           AND ($2::timestamptz IS NULL OR received_at < $2)
	           AND ($3::smallint   IS NULL OR gnssid = $3)
	           AND ($4::text       IS NULL OR source_id = $4)
	         ORDER BY receipt_order NULLS FIRST,
              CASE WHEN receipt_order IS NULL THEN received_at END,
              CASE WHEN receipt_order IS NULL THEN ts END,
              CASE WHEN receipt_order IS NULL THEN
                  (to_jsonb(nav_frames) - 'decoded' - 'decoder_ver')::text END COLLATE "C"
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

	rows, err := pool.Query(ctx, sql, q.Since, until, gnssID, source, limit+1)
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
			sourceSeq                 *int64
			f                         StoredNavFrame
			gid, sv, sig, freq, mtype int16
		)
		if err := rows.Scan(
			&f.ReceivedAt, &f.SourceID, &f.OrganizationID, &f.EnrollmentID,
			&f.CollectorInstanceID, &f.CollectionIDs, &f.Provenance, &f.CredentialTier,
			&f.CredentialFingerprint, &f.AttestationTier, &f.HardwareTrust, &f.ManufacturerAuthorityID, &f.CommissioningFingerprint,
			&f.AggregateUse, &f.StationMetadata, &f.EventVisibility,
			&f.RawExport, &f.FederationPeers, &f.PublishSignals, &f.PolicyRevision,
			&gid, &sv, &sig, &freq, &mtype, &f.Raw, &f.SBFHeader, &f.ReceiptOrder, &f.Session, &sourceSeq,
		); err != nil {
			return fmt.Errorf("query nav frames: scan: %w", err)
		}
		if sourceSeq != nil {
			f.SourceSeq, f.HasSourceSeq = uint64(*sourceSeq), true
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
