package store

import (
	"context"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

// TestIntegrationPruneSweepIsChunkedAndKeepsTheWriterLive seeds two million
// expired ledger rows (about a day of a twenty-receiver fleet) and sweeps them
// under a ten-second budget. One whole-table DELETE under a ten-second timeout
// used to be cancelled and rolled back entirely, deleting nothing and leaving
// the next hour's backlog larger; the chunked sweep keeps every finished chunk
// and the next sweep continues. It also runs on its own goroutine: frames
// enqueued while a sweep is in flight are still flushed on the writer's cadence.
func TestIntegrationPruneSweepIsChunkedAndKeepsTheWriterLive(t *testing.T) {
	dsn := isolatedDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()
	const source = "prune-sweep-integration"
	const seeded = 2_000_000
	clearLedger(t, s.pool, source)
	key := seedSession(t, s.pool, source, "boot-expired")
	seedCtx, seedCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer seedCancel()
	if _, err := s.pool.Exec(seedCtx,
		`INSERT INTO nav_frames_seq_seen (session_key, feeder_seq, seen_at)
		 SELECT $1, g, now() - interval '30 days' FROM generate_series(1, $2) AS g`, key, seeded); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := ledgerClaims(t, s.pool, source); got != seeded {
		t.Fatalf("seeded %d rows, want %d", got, seeded)
	}

	// First sweep under a short budget: progress must be durable per chunk.
	s.pruneSweepBudget = 10 * time.Second
	start := time.Now()
	s.pruneSeqSeen(ctx)
	elapsed := time.Since(start)
	remaining := ledgerClaims(t, s.pool, source)
	deleted := seeded - remaining
	if deleted == 0 {
		t.Fatalf("sweep deleted nothing in %v", elapsed)
	}
	if remaining > 0 && deleted%pruneChunkRows != 0 {
		t.Fatalf("partial sweep deleted %d rows, not a whole number of %d-row chunks", deleted, pruneChunkRows)
	}
	if elapsed > 10*time.Second+pruneChunkTimeout {
		t.Fatalf("sweep ran %v past its 10 s budget", elapsed)
	}
	t.Logf("first sweep: %d rows in %d chunks over %v (%d remaining)", deleted, deleted/pruneChunkRows, elapsed, remaining)

	// The writer keeps consuming while a sweep runs: a sweep on its own
	// goroutine against whatever is left, and a frame enqueued meanwhile lands.
	const live = "prune-sweep-live-integration"
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames WHERE source_id = $1`, live); err != nil {
		t.Fatal(err)
	}
	clearLedger(t, s.pool, live)
	if remaining == 0 {
		// Reseed so the concurrent sweep has real work.
		if _, err := s.pool.Exec(seedCtx,
			`INSERT INTO nav_frames_seq_seen (session_key, feeder_seq, seen_at)
			 SELECT $1, g, now() - interval '30 days' FROM generate_series(1, $2) AS g`, key, seeded); err != nil {
			t.Fatalf("reseed: %v", err)
		}
	}
	s.pruneSweepBudget = 2 * time.Minute
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); s.Run(runCtx) }()
	s.startPrune(runCtx)
	if !s.pruning.Load() {
		t.Fatal("sweep did not start")
	}
	now := time.Now()
	s.Enqueue(&NavFrame{Ts: now, ReceivedAt: now, SourceID: live, SvID: 1, MsgType: 1, Raw: []byte{1, 2, 3},
		SourceSeq: 1, HasSourceSeq: true, Session: "boot-live"})
	deadline := time.Now().Add(5 * time.Second)
	var rows int
	for rows == 0 && time.Now().Before(deadline) {
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE source_id = $1`, live).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows == 0 {
			time.Sleep(50 * time.Millisecond)
		}
	}
	sweeping := s.pruning.Load()
	if rows != 1 {
		t.Fatalf("frame enqueued during a sweep was not persisted within 5 s (sweep still running: %t)", sweeping)
	}
	if !sweeping {
		t.Fatalf("the sweep finished before the frame was flushed, so this run proved nothing about concurrency; %d rows had been left for it", remaining)
	}
	// Let the sweep finish, then stop: the whole backlog is gone across sweeps.
	for s.pruning.Load() && time.Now().Before(time.Now().Add(2*time.Minute)) {
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Store.Run did not shut down")
	}
	verify, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("verify store.New: %v", err)
	}
	defer verify.Close()
	if left := ledgerClaims(t, verify.pool, source); left != 0 {
		t.Errorf("%d expired rows left after the sweeps", left)
	}
	// With no claims left and a first_seen inside the window, the seed session
	// is kept; it is retired only once it is older than the window too.
	if _, err := verify.pool.Exec(ctx, `UPDATE nav_frames_sessions SET first_seen = now() - interval '30 days' WHERE session_key = $1`, key); err != nil {
		t.Fatal(err)
	}
	verify.pruneSeqSeen(ctx)
	var sessions int
	if err := verify.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames_sessions WHERE session_key = $1`, key).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if sessions != 0 {
		t.Errorf("expired session with no claims survived the sweep")
	}
	if _, err := verify.pool.Exec(ctx, `DELETE FROM nav_frames WHERE source_id = $1`, live); err != nil {
		t.Fatal(err)
	}
	clearLedger(t, verify.pool, live)
}
