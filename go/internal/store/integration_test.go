package store

import (
	"context"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

// testDSN returns the DSN for a live TimescaleDB instance, or skips the test if
// NAVLISTENER_TEST_DSN is unset. These tests exercise the real schema (schema.sql)
// and the regression fix replay-dedup ledger end to end — the pure-Go unit tests elsewhere
// in this package (dedupBatch, persistRetry) cover the same logic without a
// database, but only a live TimescaleDB can confirm the SQL itself is correct
// (the unnest/ON CONFLICT/RETURNING upsert, the hypertable + plain-table mix, the
// prune DELETE).
func testDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NAVLISTENER_TEST_DSN")
	if dsn == "" {
		t.Skip("NAVLISTENER_TEST_DSN not set; skipping live-TimescaleDB integration test")
	}
	return dsn
}

func integrationLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestIntegrationSchemaApplies confirms store.New connects, requires TimescaleDB,
// applies schema.sql (including the regression fix nav_frames_seq_seen ledger), and installs
// the compression/retention policies without error against a real database.
func TestIntegrationSchemaApplies(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	var toRegclass string
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('nav_frames_seq_seen')::text`).Scan(&toRegclass); err != nil {
		t.Fatalf("query nav_frames_seq_seen: %v", err)
	}
	if toRegclass != "nav_frames_seq_seen" {
		t.Errorf("nav_frames_seq_seen table not created")
	}
}

// TestIntegrationReplayDedup drives the full regression fix path against a live database: a
// push-path frame replayed on reconnect (same source, same feeder_seq) must not
// duplicate in nav_frames, a genuinely new sequence must persist, and dial-mode
// frames (no feeder_seq) must never be deduplicated against each other.
func TestIntegrationReplayDedup(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	const obs, dial = "obsA-integration", "dial1-integration"
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames WHERE source_id = ANY($1)`, []string{obs, dial}); err != nil {
		t.Fatalf("cleanup nav_frames: %v", err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames_seq_seen WHERE source_id = $1`, obs); err != nil {
		t.Fatalf("cleanup nav_frames_seq_seen: %v", err)
	}

	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.Run(runCtx) }()

	now := time.Now()
	mk := func(source string, seq uint64, hasSeq bool, raw []byte) *NavFrame {
		return &NavFrame{
			Ts: time.Now(), ReceivedAt: now, SourceID: source,
			GnssID: 0, SvID: 1, SigID: 0, MsgType: 1, Raw: raw,
			SourceSeq: seq, HasSourceSeq: hasSeq,
		}
	}

	s.Enqueue(mk(obs, 100, true, []byte{1, 2, 3}))
	s.Enqueue(mk(dial, 0, false, []byte{4, 5, 6}))
	time.Sleep(250 * time.Millisecond)

	// Reconnect replay of seq 100, plus a genuinely new seq 101; a second dial-mode
	// frame with identical content (dial-mode never dedups).
	s.Enqueue(mk(obs, 100, true, []byte{1, 2, 3}))
	s.Enqueue(mk(obs, 101, true, []byte{7, 8, 9}))
	s.Enqueue(mk(dial, 0, false, []byte{4, 5, 6}))
	time.Sleep(250 * time.Millisecond)

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Store.Run did not shut down")
	}

	// A fresh pool for verification, since s.pool was closed by Run's shutdown.
	verify, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("verify store.New: %v", err)
	}
	defer verify.pool.Close()

	var navCount, dialCount, ledgerCount int
	if err := verify.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE source_id = $1`, obs).Scan(&navCount); err != nil {
		t.Fatal(err)
	}
	if err := verify.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE source_id = $1`, dial).Scan(&dialCount); err != nil {
		t.Fatal(err)
	}
	if err := verify.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames_seq_seen WHERE source_id = $1`, obs).Scan(&ledgerCount); err != nil {
		t.Fatal(err)
	}

	if navCount != 2 {
		t.Errorf("nav_frames rows for %s = %d, want 2 (seq 100 once, seq 101 once — the replay of 100 must be dropped)", obs, navCount)
	}
	if dialCount != 2 {
		t.Errorf("nav_frames rows for %s = %d, want 2 (dial-mode frames are never deduplicated)", dial, dialCount)
	}
	if ledgerCount != 2 {
		t.Errorf("nav_frames_seq_seen rows for %s = %d, want 2 (seq 100 and 101)", obs, ledgerCount)
	}
}

// TestIntegrationPruneSeqSeen confirms pruneSeqSeen deletes ledger entries older
// than the configured raw-retention window and leaves recent ones alone.
func TestIntegrationPruneSeqSeen(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	const source = "prune-integration"
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames_seq_seen WHERE source_id = $1`, source); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	recent := time.Now()
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO nav_frames_seq_seen (source_id, feeder_seq, seen_at) VALUES ($1, 1, $2), ($1, 2, $3)`,
		source, old, recent); err != nil {
		t.Fatalf("seed: %v", err)
	}

	s.pruneSeqSeen(ctx)

	var remaining int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames_seq_seen WHERE source_id = $1`, source).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 1 {
		t.Errorf("remaining ledger rows = %d, want 1 (only the recent entry should survive a 7-day prune)", remaining)
	}
}
