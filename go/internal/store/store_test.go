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
)

var testRetry = flushRetry{attempts: 3, backoff: time.Millisecond, attemptTO: time.Second}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

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
	firstSeen := &NavFrame{SourceID: "obs1", SourceSeq: 10, HasSourceSeq: true, Raw: []byte{2}}
	replayed := &NavFrame{SourceID: "obs1", SourceSeq: 9, HasSourceSeq: true, Raw: []byte{3}}

	fresh := map[seqKey]bool{{source: "obs1", seq: 10}: true} // 9 already in the ledger
	out := dedupBatch([]*NavFrame{dial, firstSeen, replayed}, fresh)

	if len(out) != 2 {
		t.Fatalf("dedupBatch returned %d frames, want 2 (dial + first-seen)", len(out))
	}
	for _, f := range out {
		if f == replayed {
			t.Error("replayed sequence was not dropped")
		}
	}
	if out[0] != dial || out[1] != firstSeen {
		t.Errorf("dedupBatch = %+v, want [dial, firstSeen] in order", out)
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
		GnssID: 0, SvID: 5, SigID: 0, MsgType: 0x10,
		Raw: []byte{1, 2, 3}, Decoded: []byte(`{"a":1}`), DecoderVer: "v1",
	}
	r := navFrameToRow(f)
	if len(r) != len(copyColumns) {
		t.Fatalf("row has %d cols, want %d", len(r), len(copyColumns))
	}
	if r[3] != int16(0) || r[4] != int16(5) || r[6] != int16(0x10) {
		t.Errorf("id columns wrong: %v", r[3:7])
	}
	if r[8] != `{"a":1}` {
		t.Errorf("decoded = %v, want the JSON string", r[8])
	}
	// nil decoded when empty.
	f.Decoded = nil
	if navFrameToRow(f)[8] != nil {
		t.Error("empty decoded should map to nil")
	}
}
