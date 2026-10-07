package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

func poisonReceipt(source string, seq uint64) *ingest.RawFrame {
	bad := math.NaN()
	return &ingest.RawFrame{Source: source, Session: "boot", Seq: seq, HasSeq: true, Recv: time.Now(),
		Observer: identity.NewPrivateContext(source, identity.CredentialToken), MsgType: ingest.TelemObserverDetails,
		Bytes: []byte{1, 4, 9, 8}, Details: &ingest.ObserverDetails{Environment: &ingest.BoardEnvironment{MCP9808C: &bad}}}
}

func TestPersistencePanicRetainsRawReceiptAndIdentity(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	f := poisonReceipt("poison-board", 42)
	before := testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues(""))
	saved, quarantined := safeFrameForPersistence(f, log)
	if !quarantined || saved.Board != nil || saved.RF != nil || !bytes.Equal(saved.Raw, f.Bytes) || saved.SourceID != f.Source || saved.Session != f.Session || saved.SourceSeq != f.Seq || !saved.HasSourceSeq || !saved.ReceivedAt.Equal(f.Recv) || saved.RawExport != string(f.Observer.Publication.RawExport) || saved.PolicyRevision != f.Observer.Publication.Revision {
		t.Fatalf("raw fallback lost receipt: %+v", saved)
	}
	if !bytes.Contains(saved.Decoded, []byte("persistence_projection_panic")) {
		t.Fatalf("quarantine reason missing: %s", saved.Decoded)
	}
	if testutil.ToFloat64(metrics.DecodePanicsTotal.WithLabelValues(""))-before != 1 {
		t.Fatal("serialization panic not counted")
	}
	for _, want := range []string{"source=poison-board", "session=boot", "sequence=42"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("log missing %s: %s", want, logs.String())
		}
	}
}

func TestIntegrationPersistenceQuarantineCommitsBeforeResolution(t *testing.T) {
	dsn := os.Getenv("NAVLISTENER_TEST_DSN")
	if dsn == "" {
		t.Skip("requires disposable TimescaleDB NAVLISTENER_TEST_DSN")
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	historian, err := store.New(ctx, config.Store{DSN: dsn, BatchSize: 100, BatchEvery: time.Second}, log)
	if err != nil {
		t.Fatal(err)
	}
	defer historian.Close()
	db, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close(ctx)
	tr := ingest.NewDurableTracker()
	source := fmt.Sprintf("poison-receipt-%d", time.Now().UnixNano())
	var callbacks int
	historian.SetDurableNotify(func(gotSource, session string, seq uint64) {
		var raw []byte
		var reason, policy string
		var claimed bool
		err := db.QueryRow(ctx, `SELECT n.raw,n.decoded->>'quarantine_reason',n.policy_revision,
   EXISTS(SELECT 1 FROM nav_frames_seq_seen l JOIN nav_frames_sessions s USING(session_key)
    WHERE s.source_id=n.source_id AND s.session_id=n.source_session AND l.feeder_seq=n.source_seq)
   FROM nav_frames n WHERE n.source_id=$1 AND n.source_session=$2 AND n.source_seq=$3`, gotSource, session, int64(seq)).Scan(&raw, &reason, &policy, &claimed)
		if err != nil || !claimed || !bytes.Equal(raw, []byte{1, 4, 9, 8}) || reason == "" || policy == "" {
			t.Errorf("resolution before durable raw row and ledger: raw=%x reason=%q policy=%q claimed=%v error=%v", raw, reason, policy, claimed, err)
			return
		}
		callbacks++
		tr.Resolved(gotSource, session, seq)
	})
	frames := make(chan *ingest.RawFrame, 2)
	for seq := uint64(1); seq <= 2; seq++ {
		f := poisonReceipt(source, seq)
		if seq == 2 {
			f.QuarantineReason = "reception_check_panic"
		}
		if !tr.Received(source, "boot", seq, true) {
			t.Fatal("receipt rejected")
		}
		frames <- f
	}
	close(frames)
	live := state.New(1)
	var lastFrame atomic.Int64
	decodeLoop(frames, live, state.New(1), state.New(1), historian, log, &lastFrame, nil, nil, nil)
	if got := tr.Watermark(source, "boot"); got != 0 {
		t.Fatalf("pre-commit watermark=%d", got)
	}
	if len(live.FeedStationBoards(time.Now())) != 0 {
		t.Fatal("quarantined projection reached live state")
	}
	runCtx, cancel := context.WithCancel(ctx)
	cancel()
	historian.Run(runCtx)
	if callbacks != 2 || tr.Watermark(source, "boot") != 2 {
		t.Fatalf("callbacks=%d watermark=%d, want 2 after commit", callbacks, tr.Watermark(source, "boot"))
	}
}
