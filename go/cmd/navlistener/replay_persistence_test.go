package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

func TestIntegrationReplayPersistsWithoutLiveEffects(t *testing.T) {
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
	source := fmt.Sprintf("replay-receipt-%d", time.Now().UnixNano())
	f := lifecycleCNAV(10, time.Now(), identity.NewPrivateContext(source, identity.CredentialToken))
	f.Recv = f.Recv.Add(-12 * time.Hour).Truncate(time.Microsecond)
	f.Session, f.Seq, f.HasSeq = "boot", 42, true
	raw := f.RawBytes()
	tr := ingest.NewDurableTracker()
	if !tr.Received(source, f.Session, f.Seq, true) {
		t.Fatal("receipt rejected")
	}
	resolved := false
	historian.SetDurableNotify(func(source, session string, seq uint64) {
		var got []byte
		var stamp time.Time
		var claimed bool
		err := db.QueryRow(ctx, `SELECT n.raw,n.received_at,
   EXISTS(SELECT 1 FROM nav_frames_seq_seen l JOIN nav_frames_sessions s USING(session_key)
    WHERE s.source_id=n.source_id AND s.session_id=n.source_session AND l.feeder_seq=n.source_seq)
   FROM nav_frames n WHERE n.source_id=$1 AND n.source_session=$2 AND n.source_seq=$3`, source, session, int64(seq)).Scan(&got, &stamp, &claimed)
		if err != nil || !claimed || !bytes.Equal(got, raw) || !stamp.Equal(f.Recv) {
			t.Errorf("replay resolved without original durable evidence: claimed=%v stamp=%v error=%v", claimed, stamp, err)
			return
		}
		resolved = true
		tr.Resolved(source, session, seq)
	})
	frames := make(chan *ingest.RawFrame, 1)
	frames <- f
	close(frames)
	live := state.New(1)
	var last atomic.Int64
	decodeLoop(frames, live, state.New(1), state.New(1), historian, log, &last, nil, nil, nil)
	if tr.Watermark(source, f.Session) != 41 || last.Load() != 0 || len(live.MonitoringSatellites(time.Now())) != 0 {
		t.Fatal("replay became live or was acknowledged before persistence")
	}
	runCtx, cancel := context.WithCancel(ctx)
	cancel()
	historian.Run(runCtx)
	if !resolved || tr.Watermark(source, f.Session) != 42 {
		t.Fatal("history-only replay was not committed and acknowledged")
	}
}
