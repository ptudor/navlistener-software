package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
)

// notifyRecorder collects the (source, session, seq) triples the store reports
// durably resolved.
type notifyRecorder struct{ got []string }

func (r *notifyRecorder) fn(source, session string, seq uint64) {
	r.got = append(r.got, fmt.Sprintf("%s/%s/%d", source, session, seq))
}

func seqFrame(source, session string, seq uint64, raw byte) *NavFrame {
	return &NavFrame{SourceID: source, Session: session, SourceSeq: seq,
		HasSourceSeq: true, Raw: []byte{raw}}
}

// TestDurableNotifyOnCommit: a successful persist resolves every sequenced
// frame in the batch (dial-mode frames carry no sequence and are never
// reported); a retry-exhausted batch resolves NOTHING — its claims rolled
// back and the feeder's spool must keep it for reconnect replay.
func TestDurableNotifyOnCommit(t *testing.T) {
	rec := &notifyRecorder{}
	s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
		return int64(len(batch)), nil
	})
	s.SetDurableNotify(rec.fn)

	batch := []*NavFrame{
		seqFrame("obs1", "boot-a", 10, 1),
		{SourceID: "dial1", Raw: []byte{2}}, // no sequence: never notified
		seqFrame("obs1", "boot-a", 11, 3),
	}
	if w, d, aborted, _ := s.persistAtomicRetry(context.Background(), batch); w != 3 || d != 0 || aborted {
		t.Fatalf("persist = (%d, %d, %v), want (3, 0, false)", w, d, aborted)
	}
	want := []string{"obs1/boot-a/10", "obs1/boot-a/11"}
	if fmt.Sprint(rec.got) != fmt.Sprint(want) {
		t.Errorf("notified %v, want %v", rec.got, want)
	}

	// Retry exhaustion on a transient error: nothing resolved, nothing notified.
	rec.got = nil
	fail := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
		return 0, errors.New("transient DB failure")
	})
	fail.SetDurableNotify(rec.fn)
	if w, d, aborted, _ := fail.persistAtomicRetry(context.Background(),
		[]*NavFrame{seqFrame("obs1", "boot-a", 12, 4)}); w != 0 || d != 1 || aborted {
		t.Fatalf("failed persist = (%d, %d, %v), want (0, 1, false)", w, d, aborted)
	}
	if len(rec.got) != 0 {
		t.Errorf("retry-exhausted batch notified %v; the feeder must be left to replay it", rec.got)
	}
}

// TestDurableNotifyOnQuarantine: bisection quarantines the poison row, and the
// quarantine IS its durable disposition (deterministic row content — replaying
// it forever would only wedge the feeder's spool), so it is notified alongside
// the committed siblings.
func TestDurableNotifyOnQuarantine(t *testing.T) {
	rec := &notifyRecorder{}
	poison := &pgconn.PgError{Code: "22P02"} // data-exception class: the row's own content
	s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
		for _, f := range batch {
			if bytes.Equal(f.Raw, []byte{0xBA}) {
				return 0, poison
			}
		}
		return int64(len(batch)), nil
	})
	s.SetDurableNotify(rec.fn)

	batch := []*NavFrame{
		seqFrame("obs1", "boot-a", 20, 1),
		seqFrame("obs1", "boot-a", 21, 0xBA), // the poison row
		seqFrame("obs1", "boot-a", 22, 3),
	}
	w, d, aborted, _ := s.persistAtomicRetry(context.Background(), batch)
	if w != 2 || d != 1 || aborted {
		t.Fatalf("persist = (%d, %d, %v), want (2, 1, false)", w, d, aborted)
	}
	seen := map[string]bool{}
	for _, g := range rec.got {
		seen[g] = true
	}
	for _, want := range []string{"obs1/boot-a/20", "obs1/boot-a/21", "obs1/boot-a/22"} {
		if !seen[want] {
			t.Errorf("frame %s not notified (got %v)", want, rec.got)
		}
	}
	if len(rec.got) != 3 {
		t.Errorf("notified %d times, want 3: %v", len(rec.got), rec.got)
	}
}

// TestDurableNotifyOnNilRawReject: a nil-Raw frame is dropped at Enqueue as
// unfixable and must be resolved there, not left to wedge the watermark.
func TestDurableNotifyOnNilRawReject(t *testing.T) {
	rec := &notifyRecorder{}
	s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
		return int64(len(batch)), nil
	})
	s.SetDurableNotify(rec.fn)
	s.Enqueue(&NavFrame{SourceID: "obs1", Session: "boot-a", SourceSeq: 30, HasSourceSeq: true})
	if fmt.Sprint(rec.got) != fmt.Sprint([]string{"obs1/boot-a/30"}) {
		t.Errorf("nil-Raw reject notified %v, want the single frame resolved", rec.got)
	}
}

// TestSystemicConstraintFailureIsNotAcked guards a constraint every row trips
// (here 23502: a NOT NULL column a newer schema added) is not poison: no row is
// bisected, quarantined or acked — the feeder's spool keeps them for a build
// that can write them — and the cycle counts as a give-up, so two of them
// degrade /healthz instead of leaving it green while the record is discarded.
func TestSystemicConstraintFailureIsNotAcked(t *testing.T) {
	rec := &notifyRecorder{}
	calls := 0
	s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
		calls++
		return 0, &pgconn.PgError{Code: "23502", TableName: "nav_frames", ColumnName: "added_later",
			Message: `null value in column "added_later" violates not-null constraint`}
	})
	s.SetDurableNotify(rec.fn)
	batch := []*NavFrame{seqFrame("obs1", "boot-a", 1, 1), seqFrame("obs1", "boot-a", 2, 2),
		seqFrame("obs1", "boot-a", 3, 3), seqFrame("obs1", "boot-a", 4, 4)}
	deadline := func() time.Time { return time.Now().Add(time.Second) }
	droppedBefore := testutil.ToFloat64(metrics.StoreRetryDroppedTotal)

	s.flush(context.Background(), batch, deadline())
	if len(rec.got) != 0 {
		t.Fatalf("systemic failure acked %v; the feeder must keep these for replay", rec.got)
	}
	if calls != 1 {
		t.Errorf("persistOnce called %d times for one cycle, want 1 (no bisection or retry of a constraint every row trips)", calls)
	}
	if n := s.flushFailStreak.Load(); n != 1 {
		t.Errorf("flushFailStreak = %d after one systemic cycle, want 1", n)
	}
	if s.Degraded() != "" {
		t.Errorf("Degraded() = %q after one cycle, want empty", s.Degraded())
	}
	if d := testutil.ToFloat64(metrics.StoreRetryDroppedTotal) - droppedBefore; d != 4 {
		t.Errorf("StoreRetryDroppedTotal delta = %v, want 4 (left replayable, not quarantined)", d)
	}
	s.flush(context.Background(), batch, deadline())
	if s.Degraded() == "" {
		t.Error("Degraded() empty after two consecutive systemic cycles, want a reason")
	}
	if len(rec.got) != 0 {
		t.Errorf("second systemic cycle acked %v", rec.got)
	}
}

// TestConstraintViolationQuarantinedOnlyBesideCommittedSibling guards the
// unique/check-violation rule: a row that trips a constraint its siblings
// satisfy is that row's own content — quarantined and acked once a sibling of
// the cycle commits, whichever half commits first — while a cycle in which
// every row trips it is systemic: nothing acked, counted as a give-up.
func TestConstraintViolationQuarantinedOnlyBesideCommittedSibling(t *testing.T) {
	rec := &notifyRecorder{}
	check := &pgconn.PgError{Code: "23514", ConstraintName: "rf_samples_kind_check"}
	s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
		for _, f := range batch {
			if bytes.Equal(f.Raw, []byte{0xBA}) {
				return 0, check
			}
		}
		return int64(len(batch)), nil
	})
	s.SetDurableNotify(rec.fn)

	for name, batch := range map[string][]*NavFrame{
		"sibling commits first": {seqFrame("obs1", "boot-a", 40, 1), seqFrame("obs1", "boot-a", 41, 0xBA)},
		"sibling commits later": {seqFrame("obs1", "boot-a", 42, 0xBA), seqFrame("obs1", "boot-a", 43, 3)},
	} {
		rec.got = nil
		w, d, aborted, gaveUp := s.persistAtomicRetry(context.Background(), batch)
		if w != 1 || d != 1 || aborted || gaveUp {
			t.Fatalf("%s: persist = (%d, %d, %v, %v), want (1, 1, false, false)", name, w, d, aborted, gaveUp)
		}
		if len(rec.got) != 2 {
			t.Errorf("%s: notified %v, want both rows (the committed one and the quarantined one)", name, rec.got)
		}
	}

	rec.got = nil
	allBad := []*NavFrame{seqFrame("obs1", "boot-a", 44, 0xBA), seqFrame("obs1", "boot-a", 45, 0xBA)}
	w, d, aborted, gaveUp := s.persistAtomicRetry(context.Background(), allBad)
	if w != 0 || d != 2 || aborted || !gaveUp {
		t.Fatalf("every-row violation: persist = (%d, %d, %v, %v), want (0, 2, false, true)", w, d, aborted, gaveUp)
	}
	if len(rec.got) != 0 {
		t.Errorf("every-row constraint violation acked %v; nothing committed to prove it was the rows' fault", rec.got)
	}
}

// TestQuarantineLogCarriesRowIdentity guards the quarantine log line names the
// row — source, session, sequence, table, kind, satellite, message type, raw
// length — and the SQLSTATE: it is the only trace an acked-and-discarded frame
// leaves for triage.
func TestQuarantineLogCarriesRowIdentity(t *testing.T) {
	var buf bytes.Buffer
	s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
		return 0, &pgconn.PgError{Code: "22P02", Message: "invalid input syntax for type json"}
	})
	s.log = slog.New(slog.NewJSONHandler(&buf, nil))
	f := seqFrame("obs1", "boot-a", 77, 0xBA)
	f.GnssID, f.SvID, f.MsgType = 2, 11, 5
	f.Board = &BoardSample{Kind: "timing", Data: []byte(`{`)}
	if w, d, _, _ := s.persistAtomicRetry(context.Background(), []*NavFrame{f}); w != 0 || d != 1 {
		t.Fatalf("persist = (%d, %d), want the row quarantined", w, d)
	}
	var line map[string]any
	for _, raw := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var candidate map[string]any
		if err := json.Unmarshal(raw, &candidate); err != nil {
			t.Fatalf("log line %q: %v", raw, err)
		}
		if candidate["msg"] == "store quarantined a poison row" {
			line = candidate
		}
	}
	if line == nil {
		t.Fatalf("no quarantine log line in %q", buf.String())
	}
	for key, want := range map[string]any{
		"sqlstate": "22P02", "table": "observer_samples", "source": "obs1", "session": "boot-a",
		"seq": float64(77), "sequenced": true, "kind": "timing", "gnssid": float64(2), "svid": float64(11),
		"msg_type": float64(5), "raw_len": float64(1),
	} {
		if line[key] != want {
			t.Errorf("quarantine log %s = %v, want %v", key, line[key], want)
		}
	}
}
