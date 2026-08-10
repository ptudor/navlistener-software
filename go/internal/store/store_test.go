package store

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
)

var testRetry = flushRetry{attempts: 3, backoff: time.Millisecond, attemptTO: time.Second}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestRequiredColumnsCoversNavFrames guards verifyRequiredColumns must fail fast on a
// drifted nav_frames / nav_frames_seq_seen (not just the intsat-shared tables), so a
// self-upgrade against an older deployed raw-frame table is caught at startup rather than
// silently dropping every CopyFrom batch. The nav_frames list must stay exactly copyColumns.
func TestRequiredColumnsCoversNavFrames(t *testing.T) {
	nf, ok := requiredColumns["nav_frames"]
	if !ok {
		t.Fatal("requiredColumns is missing nav_frames ")
	}
	if len(nf) != len(copyColumns) {
		t.Fatalf("requiredColumns[nav_frames] = %v, want copyColumns %v", nf, copyColumns)
	}
	for i := range copyColumns {
		if nf[i] != copyColumns[i] {
			t.Errorf("nav_frames column %d = %q, want %q (must track copyColumns)", i, nf[i], copyColumns[i])
		}
	}
	seq, ok := requiredColumns["nav_frames_seq_seen"]
	if !ok {
		t.Fatal("requiredColumns is missing nav_frames_seq_seen ")
	}
	want := []string{"source_id", "session_id", "feeder_seq", "seen_at"} // session_id: regression fix
	if len(seq) != len(want) {
		t.Fatalf("requiredColumns[nav_frames_seq_seen] = %v, want %v", seq, want)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Errorf("nav_frames_seq_seen column %d = %q, want %q", i, seq[i], want[i])
		}
	}
}

// row builds a CopyFrom row whose source_id (index 2) marks it poison when "bad".
func row(id string) []any { return []any{nil, nil, id} }

func TestPersistRetrySuccess(t *testing.T) {
	copy := func(context.Context, [][]any) (int64, error) { return 3, nil }
	w, d := persistRetry(context.Background(), testRetry, copy, [][]any{row("a"), row("b"), row("c")}, quietLog())
	if w != 3 || d != 0 {
		t.Errorf("written=%d dropped=%d, want 3/0", w, d)
	}
}

func TestPersistRetryTransientThenSuccess(t *testing.T) {
	var calls int32
	copy := func(_ context.Context, rows [][]any) (int64, error) {
		if atomic.AddInt32(&calls, 1) < 3 {
			return 0, errors.New("connection reset") // retryable (non-PgError)
		}
		return int64(len(rows)), nil
	}
	w, d := persistRetry(context.Background(), testRetry, copy, [][]any{row("a"), row("b")}, quietLog())
	if w != 2 || d != 0 {
		t.Errorf("written=%d dropped=%d, want 2/0 after retries", w, d)
	}
	if calls != 3 {
		t.Errorf("expected 3 attempts, got %d", calls)
	}
}

func TestPersistRetryGivesUp(t *testing.T) {
	copy := func(context.Context, [][]any) (int64, error) { return 0, errors.New("db down") }
	w, d := persistRetry(context.Background(), testRetry, copy, [][]any{row("a"), row("b")}, quietLog())
	if w != 0 || d != 2 {
		t.Errorf("written=%d dropped=%d, want 0/2 (dropped after retries)", w, d)
	}
}

// TestPersistRetryQuarantinesPoison: a deterministic (data-exception) error on one
// row must isolate that row by bisection and still persist the rest.
func TestPersistRetryQuarantinesPoison(t *testing.T) {
	copy := func(_ context.Context, rows [][]any) (int64, error) {
		for _, r := range rows {
			if r[2] == "bad" {
				return 0, &pgconn.PgError{Code: "22P02", Message: "invalid input"}
			}
		}
		return int64(len(rows)), nil
	}
	rows := [][]any{row("a"), row("b"), row("bad"), row("d")}
	w, d := persistRetry(context.Background(), testRetry, copy, rows, quietLog())
	if w != 3 || d != 1 {
		t.Errorf("written=%d dropped=%d, want 3/1 (poison isolated, rest written)", w, d)
	}
}

func TestIsPoison(t *testing.T) {
	if !isPoison(&pgconn.PgError{Code: "23505"}) { // unique violation
		t.Error("23xxx should be poison")
	}
	if !isPoison(&pgconn.PgError{Code: "22P02"}) { // invalid text
		t.Error("22xxx should be poison")
	}
	if isPoison(&pgconn.PgError{Code: "08006"}) { // connection failure
		t.Error("08xxx (connection) should be retryable, not poison")
	}
	if isPoison(errors.New("timeout")) {
		t.Error("non-PgError should be retryable, not poison")
	}
}

// TestDedupBatchDropsReplayedSeq guards a push-path frame whose
// (source, feeder_seq) is not in `fresh` (already persisted on a prior flush) must
// be dropped, while dial-mode frames (HasSourceSeq false) always pass through
// regardless of fresh, and a first-seen sequence (present in fresh) passes too.
func TestDedupBatchDropsReplayedSeq(t *testing.T) {
	dial := &NavFrame{SourceID: "dial1", Raw: []byte{1}} // no sequence: always kept
	firstSeen := &NavFrame{SourceID: "obs1", Session: "boot-a", SourceSeq: 10, HasSourceSeq: true, Raw: []byte{2}}
	replayed := &NavFrame{SourceID: "obs1", Session: "boot-a", SourceSeq: 9, HasSourceSeq: true, Raw: []byte{3}}
	// same source and seq as the replayed frame, but a NEW session —
	// a rebooted feeder's fresh sequence space, never a replay of boot-a's seq 9.
	rebooted := &NavFrame{SourceID: "obs1", Session: "boot-b", SourceSeq: 9, HasSourceSeq: true, Raw: []byte{4}}

	fresh := map[seqKey]bool{
		{source: "obs1", session: "boot-a", seq: 10}: true, // (boot-a, 9) already in the ledger
		{source: "obs1", session: "boot-b", seq: 9}:  true, // the fresh session's claim is new
	}
	out := dedupBatch([]*NavFrame{dial, firstSeen, replayed, rebooted}, fresh)

	if len(out) != 3 {
		t.Fatalf("dedupBatch returned %d frames, want 3 (dial + first-seen + rebooted)", len(out))
	}
	for _, f := range out {
		if f == replayed {
			t.Error("replayed sequence was not dropped")
		}
	}
	if out[0] != dial || out[1] != firstSeen || out[2] != rebooted {
		t.Errorf("dedupBatch = %+v, want [dial, firstSeen, rebooted] in order", out)
	}
}

// TestDedupBatchDoesNotAliasInput guards against a subtle regression: dedupBatch
// must return a fresh slice, not a view over the caller's backing array (the
// caller reuses `batch` across flushes via batch = batch[:0]).
func TestDedupBatchDoesNotAliasInput(t *testing.T) {
	batch := make([]*NavFrame, 0, 4)
	batch = append(batch, &NavFrame{SourceID: "a"}, &NavFrame{SourceID: "b"})
	out := dedupBatch(batch, nil)
	out[0] = &NavFrame{SourceID: "mutated"}
	if batch[0].SourceID == "mutated" {
		t.Error("dedupBatch aliased the input slice's backing array")
	}
}

func TestParseSimpleInterval(t *testing.T) {
	cases := map[string]time.Duration{
		"7 days":    7 * 24 * time.Hour,
		"1 day":     24 * time.Hour,
		"12 hours":  12 * time.Hour,
		"30 minute": 30 * time.Minute,
		"2 weeks":   2 * 7 * 24 * time.Hour,
	}
	for in, want := range cases {
		got, err := parseSimpleInterval(in)
		if err != nil {
			t.Errorf("parseSimpleInterval(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSimpleInterval(%q) = %v, want %v", in, got, want)
		}
	}
	if _, err := parseSimpleInterval("not an interval"); err == nil {
		t.Error("expected an error for a malformed interval")
	}
}

func TestNavFrameToRow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	f := &NavFrame{
		Ts: now, ReceivedAt: now, SourceID: "obs1",
		GnssID: 0, SvID: 5, SigID: 0, FreqID: 0, MsgType: 0x10,
		Raw: []byte{1, 2, 3}, Decoded: []byte(`{"a":1}`), DecoderVer: "v1",
	}
	r := navFrameToRow(f)
	if len(r) != len(copyColumns) {
		t.Fatalf("row has %d cols, want %d", len(r), len(copyColumns))
	}
	idx := func(col string) int {
		for i, c := range copyColumns {
			if c == col {
				return i
			}
		}
		t.Fatalf("copyColumns has no %q", col)
		return -1
	}
	if r[idx("gnssid")] != int16(0) || r[idx("svid")] != int16(5) || r[idx("msg_type")] != int16(0x10) {
		t.Errorf("id columns wrong: gnssid=%v svid=%v msg_type=%v",
			r[idx("gnssid")], r[idx("svid")], r[idx("msg_type")])
	}
	if r[idx("decoded")] != `{"a":1}` {
		t.Errorf("decoded = %v, want the JSON string", r[idx("decoded")])
	}
	// nil decoded when empty.
	f.Decoded = nil
	if navFrameToRow(f)[idx("decoded")] != nil {
		t.Error("empty decoded should map to nil")
	}
}

// TestNavFrameRowMatchesColumnOrder pins each value to the column it is named for,
// by position. CopyFrom binds positionally, so inserting a column mid-list (freqid
// went in before msg_type) silently shifts every later value into the wrong column
// unless navFrameToRow moves with it — a corruption Postgres cannot catch when the
// neighbours share a type, as SMALLINT sigid/freqid/msg_type do.
func TestNavFrameRowMatchesColumnOrder(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// Distinct values so a transposition cannot coincidentally pass.
	f := &NavFrame{
		Ts: now, ReceivedAt: now.Add(time.Second), SourceID: "obs1",
		OrganizationID: "customer-a", EnrollmentID: "enroll-9", CollectorInstanceID: "hosted-west",
		Provenance: "local", CredentialTier: "hardware_mtls", AttestationTier: "verified_v2_complete",
		AggregateUse: "private", StationMetadata: "none", EventVisibility: "private", PolicyRevision: "policy-7",
		GnssID: 6, SvID: 5, SigID: 2, FreqID: 9, MsgType: 0x10,
		Raw: []byte{1, 2, 3}, Decoded: []byte(`{"a":1}`), DecoderVer: "v1",
	}
	r := navFrameToRow(f)
	if len(r) != len(copyColumns) {
		t.Fatalf("row has %d cols, want %d", len(r), len(copyColumns))
	}
	want := map[string]any{
		"ts": now, "received_at": now.Add(time.Second), "source_id": "obs1",
		"organization_id": "customer-a", "enrollment_id": "enroll-9",
		"collector_instance_id": "hosted-west", "provenance": "local",
		"credential_tier": "hardware_mtls", "attestation_tier": "verified_v2_complete",
		"aggregate_use": "private", "station_metadata": "none", "event_visibility": "private", "policy_revision": "policy-7",
		"gnssid": int16(6), "svid": int16(5), "sigid": int16(2),
		"freqid": int16(9), "msg_type": int16(0x10),
		"decoded": `{"a":1}`, "decoder_ver": "v1",
	}
	for i, col := range copyColumns {
		w, ok := want[col]
		if !ok {
			continue // raw is a byte slice, not comparable with !=
		}
		if r[i] != w {
			t.Errorf("column %q (index %d) = %v, want %v", col, i, r[i], w)
		}
	}
}

// TestShutdownFlushSharesOneDeadline guards drain() + the final flush()
// must share ONE shutdown deadline, not a fresh per-chunk budget. With batchSize=2
// and 10 queued frames (5 chunks) and a copy that takes 1s per chunk if allowed to
// run to completion, a per-chunk budget would let total shutdown flush time run to
// ~5s; a shared ~500ms deadline must cut it off well short of that. The margin
// between "bounded" and "unbounded" is kept deliberately huge (an order of
// magnitude) so this isn't flaky under -race/system load or when run alongside
// this package's live-DB integration tests, which add real scheduling contention.
func TestShutdownFlushSharesOneDeadline(t *testing.T) {
	slowCopy := func(ctx context.Context, rows [][]any) (int64, error) {
		select {
		case <-time.After(time.Second):
			return int64(len(rows)), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s := &Store{
		in:             make(chan *NavFrame, 32),
		batchSize:      2,
		batchEvery:     time.Hour, // never fires on its own; only shutdown drives flushes here
		retry:          flushRetry{attempts: 3, backoff: 10 * time.Millisecond, attemptTO: 30 * time.Second},
		log:            quietLog(),
		shutdownBudget: 500 * time.Millisecond,
	}
	s.copy = slowCopy

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); s.Run(ctx) }()

	for i := 0; i < 10; i++ {
		s.Enqueue(&NavFrame{SourceID: "obs", Raw: []byte{1}})
	}
	time.Sleep(20 * time.Millisecond) // let Enqueue's sends land before shutdown begins

	start := time.Now()
	cancel()
	select {
	case <-runDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return — shutdown deadline was not enforced")
	}
	elapsed := time.Since(start)

	// 5 unbounded chunk flushes at 1s each would take ~5s; the shared 500ms
	// deadline must cut it off well before that — 3s leaves generous headroom for
	// scheduling contention while still clearly failing if the budget were not
	// actually shared (i.e. not bounded at all).
	if elapsed > 3*time.Second {
		t.Errorf("shutdown took %v, want well under 3s (5 unbounded chunks would take ~5s) — "+
			"the shared deadline does not appear to be bounding total drain time", elapsed)
	}
}

// TestEnqueueDropsNilRaw guards a nil Raw maps to SQL NULL and trips
// nav_frames.raw's NOT NULL constraint, poison-quarantining the whole batch by
// bisection. Enqueue must drop it before it ever reaches the queue, counting a
// specific metric rather than surfacing as a generic quarantine. A non-nil empty
// []byte{} stores fine as an empty bytea and must NOT be dropped.
func TestEnqueueDropsNilRaw(t *testing.T) {
	s := &Store{in: make(chan *NavFrame, 4), log: quietLog()}

	before := testutil.ToFloat64(metrics.StoreEmptyRawTotal)
	s.Enqueue(&NavFrame{SourceID: "obs", Raw: nil})
	after := testutil.ToFloat64(metrics.StoreEmptyRawTotal)
	if after-before != 1 {
		t.Errorf("StoreEmptyRawTotal delta = %v, want 1", after-before)
	}
	select {
	case f := <-s.in:
		t.Fatalf("nil-Raw frame reached the queue: %+v", f)
	default:
	}

	// A non-nil empty Raw is not dropped.
	s.Enqueue(&NavFrame{SourceID: "obs", Raw: []byte{}})
	select {
	case <-s.in:
	default:
		t.Error("non-nil empty Raw was dropped, want it enqueued")
	}
}
