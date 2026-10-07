package store

import (
	"context"
	"testing"
	"time"
)

// TestShutdownBudgetFollowsThePlan: the drain deadline is derived from the
// daemon's store phase, not a constant that happened to be shorter than it.
// With a 2 s phase and a persist that takes 3 s, Run must give up inside the
// phase (drain 1.5 s, the rest reserved for closing the pool) instead of
// outliving the daemon's wait.
func TestShutdownBudgetFollowsThePlan(t *testing.T) {
	if got := DrainBudget(2 * time.Second); got != 1500*time.Millisecond {
		t.Fatalf("DrainBudget(2s) = %v, want 1.5s", got)
	}
	if got := DrainBudget(200 * time.Millisecond); got != minDrainBudget {
		t.Fatalf("DrainBudget(200ms) = %v, want the %v floor", got, minDrainBudget)
	}
	slowCopy := func(ctx context.Context, rows [][]any) (int64, error) {
		select {
		case <-time.After(3 * time.Second):
			return int64(len(rows)), nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	s := &Store{
		in:         make(chan *NavFrame, 32),
		batchSize:  2,
		batchEvery: time.Hour,
		retry:      flushRetry{attempts: 3, backoff: 10 * time.Millisecond, attemptTO: 30 * time.Second},
		log:        quietLog(),
	}
	s.copy = slowCopy
	s.SetShutdownBudget(2 * time.Second)
	if s.shutdownBudget != 1500*time.Millisecond {
		t.Fatalf("shutdownBudget = %v after SetShutdownBudget(2s), want 1.5s", s.shutdownBudget)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); s.Run(ctx) }()
	for i := 0; i < 6; i++ {
		s.Enqueue(&NavFrame{SourceID: "obs", Raw: []byte{1}})
	}
	time.Sleep(20 * time.Millisecond)
	start := time.Now()
	cancel()
	select {
	case <-runDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Run outlived the 2 s store phase it was given")
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Fatalf("Run gave up after %v, before the 1.5 s drain budget it was given", elapsed)
	}
}
