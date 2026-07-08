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
