package store

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
	poison := &pgconn.PgError{Code: "23502"} // integrity-constraint class
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
