package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

func TestIntegrationBoardSamplesAtomicReplay(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	s, err := New(ctx, config.Store{DSN: dsn}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	source := fmt.Sprintf("board-history-test-%d", time.Now().UnixNano())
	now := time.Now().UTC().Truncate(time.Microsecond)
	frame := func(seq uint64, kind string) *NavFrame {
		f := &NavFrame{Ts: now, ReceivedAt: now, SourceID: source, OrganizationID: "test-org",
			Session: "boot-a", SourceSeq: seq, HasSourceSeq: true, Raw: []byte{1, 4}, PolicyRevision: "test-policy"}
		if kind != "" {
			f.Board = &BoardSample{Kind: kind, Data: []byte(`{"uptime_ms":123,"environment":{"mcp9808_c":25.5}}`)}
		}
		return f
	}
	nav, env, timing := frame(1, ""), frame(2, "environment"), frame(3, "timing")
	timing.Board.SampleTime = &now
	w, err := s.persistAtomicOnce(ctx, []*NavFrame{nav, env, timing, env})
	if err != nil || w != 3 {
		t.Fatalf("mixed batch: %d %v", w, err)
	}
	w, err = s.persistAtomicOnce(ctx, []*NavFrame{nav, env, timing})
	if err != nil || w != 0 {
		t.Fatalf("replay: %d %v", w, err)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM observer_samples WHERE source_id=$1`, source).Scan(&count); err != nil || count != 2 {
		t.Fatalf("board rows: %d %v", count, err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE source_id=$1`, source).Scan(&count); err != nil || count != 1 {
		t.Fatalf("board polluted nav history: %d %v", count, err)
	}
	var sample *time.Time
	var temp float64
	var org, revision string
	var raw []byte
	if err := s.pool.QueryRow(ctx, `SELECT sample_time, (data->'environment'->>'mcp9808_c')::float8, organization_id, policy_revision, raw FROM observer_samples WHERE source_id=$1 AND kind='environment'`, source).Scan(&sample, &temp, &org, &revision, &raw); err != nil {
		t.Fatal(err)
	}
	if sample != nil || temp != 25.5 || org != "test-org" || revision != "test-policy" || string(raw) != string(env.Raw) {
		t.Fatal("lost unknown time, values, bytes or receipt provenance")
	}
	if err := s.pool.QueryRow(ctx, `SELECT sample_time FROM observer_samples WHERE source_id=$1 AND kind='timing'`, source).Scan(&sample); err != nil || sample == nil || !sample.Equal(now) {
		t.Fatalf("known sample time: %v %v", sample, err)
	}
	// A malformed board row must roll back both the nav row and replay claims.
	bad, next := frame(4, "timing"), frame(5, "")
	bad.Board.Data = []byte(`{invalid`)
	if _, err := s.persistAtomicOnce(ctx, []*NavFrame{next, bad}); err == nil {
		t.Fatal("bad JSON committed")
	}
	bad.Board.Data = []byte(`{"uptime_ms":124}`)
	w, err = s.persistAtomicOnce(ctx, []*NavFrame{next, bad})
	if err != nil || w != 2 {
		t.Fatalf("rolled-back claims prevented replay: %d %v", w, err)
	}
	// Reboot sequence reuse is a new sample, including every uint64 sequence bit.
	reboot := frame(^uint64(0), "environment")
	reboot.Session = "boot-b"
	w, err = s.persistAtomicOnce(ctx, []*NavFrame{reboot})
	if err != nil || w != 1 {
		t.Fatalf("new boot: %d %v", w, err)
	}
	// Close/reopen models daemon restart; persisted rows and ledger survive.
	s.Close()
	s, err = New(ctx, config.Store{DSN: dsn}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err = s.persistAtomicOnce(ctx, []*NavFrame{reboot})
	if err != nil || w != 0 {
		t.Fatalf("restart duplicated sample: %d %v", w, err)
	}
}
