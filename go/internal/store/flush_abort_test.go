package store

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/metrics"
)

// regression fix guard tests: a normal-path flush runs under storeCtx, so storeCancel()
// interrupts it mid-retry instead of blocking shutdown for up to ~30 s; the
// interrupted batch (nothing resolved, transaction rolled back) is RETAINED and
// re-flushed by the shutdown drain under the single bounded shutdownBudget —
// dial-mode frames have no replay source, so discarding them there was silent
// permanent forensic loss on the most common operator action (a restart during
// DB trouble). Wall-budget exhaustion with a live parent keeps the pre-regression fix
// drop-counted policy so the writer stays memory-bounded through a DB outage.

// atomicStore builds a Store on the atomic-persist path with a fake persistOnce,
// mirroring how New wires production (pool-free, like the legacy copy-seam tests).
func atomicStore(persistOnce func(ctx context.Context, batch []*NavFrame) (int64, error)) *Store {
	s := &Store{
		in:             make(chan *NavFrame, 64),
		batchSize:      2,
		batchEvery:     time.Hour, // never fires on its own in these tests
		retry:          flushRetry{attempts: 3, backoff: time.Millisecond, attemptTO: time.Second},
		log:            quietLog(),
		atomicPersist:  true,
		shutdownBudget: time.Second,
	}
	s.persistOnce = persistOnce
	return s
}

// TestNormalFlushInterruptedByShutdownRetainsBatchForDrain: cancel storeCtx while
// a normal flush is blocked on a slow DB; every enqueued frame must still be
// persisted exactly once by the bounded shutdown drain, with zero drops.
func TestNormalFlushInterruptedByShutdownRetainsBatchForDrain(t *testing.T) {
	var mu sync.Mutex
	var persisted []*NavFrame
	firstCallStarted := make(chan struct{})
	var once sync.Once
	fake := func(ctx context.Context, batch []*NavFrame) (int64, error) {
		once.Do(func() { close(firstCallStarted) })
		select {
		case <-ctx.Done():
			return 0, ctx.Err() // the "hung DB attempt" the cancellation interrupts
		case <-time.After(30 * time.Millisecond): // a slow-but-working DB (well under shutdownBudget)
			mu.Lock()
			persisted = append(persisted, batch...)
			mu.Unlock()
			return int64(len(batch)), nil
		}
	}
	s := atomicStore(fake)

	droppedBefore := testutil.ToFloat64(metrics.StoreQuarantinedTotal)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan struct{})
	go func() { defer close(runDone); s.Run(ctx) }()

	// Two frames trip the size flush (batchSize=2) whose first attempt blocks in
	// fake; two more queue behind it for the drain.
	for i := 0; i < 4; i++ {
		s.Enqueue(&NavFrame{SourceID: "dial", Raw: []byte{byte(i)}})
	}
	select {
	case <-firstCallStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("normal flush never started")
	}
	start := time.Now()
	cancel() // interrupts the in-flight attempt; ctx.Err() != nil ⇒ retain, not drop

	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("shutdown took %v, want well under 2s (bounded drain)", elapsed)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(persisted) != 4 {
		t.Errorf("persisted %d frames, want all 4 (retained batch + queued frames re-flushed by the drain)", len(persisted))
	}
	seen := map[byte]int{}
	for _, f := range persisted {
		seen[f.Raw[0]]++
	}
	for b, n := range seen {
		if n != 1 {
			t.Errorf("frame %d persisted %d times, want exactly once (atomic rollback must make retention duplicate-free)", b, n)
		}
	}
	if d := testutil.ToFloat64(metrics.StoreQuarantinedTotal) - droppedBefore; d != 0 {
		t.Errorf("StoreQuarantinedTotal delta = %v, want 0 (nothing may be dropped on an orderly shutdown with a working DB)", d)
	}
}

// TestDegradedAfterConsecutiveFlushGiveUps guards two consecutive
// retry-exhausted flush cycles (≈ a minute-plus of DB failure) must surface via
// Degraded() so /healthz reports the historian dropping the forensic record; a
// single give-up must NOT degrade (no flapping), and one successful persist
// clears the streak.
func TestDegradedAfterConsecutiveFlushGiveUps(t *testing.T) {
	fail := errors.New("db down")
	shouldFail := true
	fake := func(ctx context.Context, batch []*NavFrame) (int64, error) {
		if shouldFail {
			return 0, fail
		}
		return int64(len(batch)), nil
	}
	s := atomicStore(fake)
	s.retry = flushRetry{attempts: 2, backoff: time.Millisecond, attemptTO: time.Second}
	batch := []*NavFrame{{SourceID: "dial", Raw: []byte{1}}}
	deadline := func() time.Time { return time.Now().Add(time.Second) }

	if s.flush(context.Background(), batch, deadline()); s.Degraded() != "" {
		t.Errorf("Degraded() = %q after ONE give-up, want empty (a single blip must not degrade health)", s.Degraded())
	}
	if s.flush(context.Background(), batch, deadline()); s.Degraded() == "" {
		t.Error("Degraded() empty after two consecutive give-ups, want a reason")
	}
	shouldFail = false
	if s.flush(context.Background(), batch, deadline()); s.Degraded() != "" {
		t.Errorf("Degraded() = %q after a successful persist, want empty (streak must reset)", s.Degraded())
	}
}

// TestDegradedClearedByIdleHealthProbe guards flushFailStreak used to be
// cleared ONLY by a successful non-empty persist, so a DB that failed for ≥2
// cycles and then recovered while no frames were arriving left /healthz reporting
// the historian degraded indefinitely. An empty-batch cycle now probes the pool
// directly; a passing probe clears the streak, a failing one leaves it (the
// latched status is then true).
func TestDegradedClearedByIdleHealthProbe(t *testing.T) {
	degrade := func(s *Store) {
		batch := []*NavFrame{{SourceID: "dial", Raw: []byte{1}}}
		for i := 0; i < 2; i++ {
			s.flush(context.Background(), batch, time.Now().Add(time.Second))
		}
		if s.Degraded() == "" {
			t.Fatal("setup: two consecutive give-ups did not degrade")
		}
	}
	// runIdle runs the writer loop with an EMPTY queue until cond or a timeout,
	// exercising exactly the idle path (the batch timer firing with no work).
	runIdle := func(t *testing.T, s *Store, budget time.Duration, cond func() bool) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { defer close(done); s.Run(ctx) }()
		deadline := time.Now().Add(budget)
		for time.Now().Before(deadline) && !cond() {
			time.Sleep(2 * time.Millisecond)
		}
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return")
		}
	}

	t.Run("healthy probe clears", func(t *testing.T) {
		s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
			return 0, errors.New("db down")
		})
		s.retry = flushRetry{attempts: 2, backoff: time.Millisecond, attemptTO: time.Second}
		s.batchEvery = 2 * time.Millisecond
		degrade(s)
		// the probe clears only after a genuinely quiet window with no
		// write attempts; model that the failing flushes stopped a while ago.
		s.lastWriteAttempt = time.Now().Add(-idleQuietWindow)
		var pings atomic.Int64
		s.ping = func(ctx context.Context) error { pings.Add(1); return nil }
		runIdle(t, s, 2*time.Second, func() bool { return s.Degraded() == "" })
		if s.Degraded() != "" {
			t.Errorf("Degraded() = %q after the DB became reachable during ingest silence, want empty", s.Degraded())
		}
		if pings.Load() == 0 {
			t.Error("idle cycles never probed the pool")
		}
	})

	t.Run("failing probe keeps the streak", func(t *testing.T) {
		s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
			return 0, errors.New("db down")
		})
		s.retry = flushRetry{attempts: 2, backoff: time.Millisecond, attemptTO: time.Second}
		s.batchEvery = 2 * time.Millisecond
		degrade(s)
		s.lastWriteAttempt = time.Now().Add(-idleQuietWindow) // quiet: probe may run
		var pings atomic.Int64
		s.ping = func(ctx context.Context) error { pings.Add(1); return errors.New("db still down") }
		runIdle(t, s, 2*time.Second, func() bool { return pings.Load() > 0 })
		if s.Degraded() == "" {
			t.Error("Degraded() cleared on a FAILING health probe — the historian is still down")
		}
	})

	// under a write-side-only fault (read-only standby, disk full,
	// revoked INSERT) Ping passes while every flush fails. With write attempts
	// still recent, empty-batch cycles must NOT probe or clear — otherwise, at
	// any frame rate low enough for the batch to empty between failing cycles,
	// the probe zeroes the streak between them and Degraded() never latches.
	t.Run("recent failing writes gate the probe", func(t *testing.T) {
		s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
			return 0, errors.New("cannot execute COPY in a read-only transaction")
		})
		s.retry = flushRetry{attempts: 2, backoff: time.Millisecond, attemptTO: time.Second}
		s.batchEvery = 2 * time.Millisecond
		degrade(s) // the last failing write attempt was moments ago
		var pings atomic.Int64
		s.ping = func(ctx context.Context) error { pings.Add(1); return nil }
		runIdle(t, s, 300*time.Millisecond, func() bool { return s.Degraded() == "" })
		if s.Degraded() == "" {
			t.Error("idle probe cleared the streak while writes were actively failing — the regression fix masking regression")
		}
		if n := pings.Load(); n != 0 {
			t.Errorf("probe ran %d times inside the quiet window, want 0 (recent attempts own the verdict)", n)
		}
	})

	t.Run("healthy idle daemon never probes", func(t *testing.T) {
		s := atomicStore(func(ctx context.Context, batch []*NavFrame) (int64, error) {
			return int64(len(batch)), nil
		})
		s.batchEvery = 2 * time.Millisecond
		var pings atomic.Int64
		s.ping = func(ctx context.Context) error { pings.Add(1); return nil }
		runIdle(t, s, 200*time.Millisecond, func() bool { return pings.Load() > 0 })
		if n := pings.Load(); n != 0 {
			t.Errorf("probed %d times with a zero failure streak, want 0 (an idle healthy daemon must not poll the DB)", n)
		}
	})
}

// TestPoisonBisectionCountsOneFlushFailure guards a poison-row
// bisection whose sub-batches then fail transiently used to Add(1) per sub-batch,
// so ONE flush cycle reached Degraded()'s ≥2 threshold — undercutting the
// two-consecutive-cycles anti-flap intent. The streak must advance by exactly one
// per top-level flush.
func TestPoisonBisectionCountsOneFlushFailure(t *testing.T) {
	// Whole batch → poison (bisect); each half → transient failure (give up).
	fake := func(ctx context.Context, batch []*NavFrame) (int64, error) {
		if len(batch) == 4 {
			return 0, &pgconn.PgError{Code: "23505", Message: "duplicate key"}
		}
		return 0, errors.New("connection reset")
	}
	s := atomicStore(fake)
	s.retry = flushRetry{attempts: 2, backoff: time.Millisecond, attemptTO: time.Second}
	batch := []*NavFrame{
		{SourceID: "dial", Raw: []byte{1}}, {SourceID: "dial", Raw: []byte{2}},
		{SourceID: "dial", Raw: []byte{3}}, {SourceID: "dial", Raw: []byte{4}},
	}

	s.flush(context.Background(), batch, time.Now().Add(5*time.Second))
	if n := s.flushFailStreak.Load(); n != 1 {
		t.Errorf("flushFailStreak = %d after ONE bisecting flush cycle, want 1", n)
	}
	if s.Degraded() != "" {
		t.Errorf("Degraded() = %q after one flush cycle, want empty (two consecutive cycles is the promised threshold)", s.Degraded())
	}
	s.flush(context.Background(), batch, time.Now().Add(5*time.Second))
	if n := s.flushFailStreak.Load(); n != 2 {
		t.Errorf("flushFailStreak = %d after two failing cycles, want 2", n)
	}
	if s.Degraded() == "" {
		t.Error("Degraded() empty after two consecutive failing cycles, want a reason")
	}
}

// TestNormalFlushBudgetExhaustionStillDrops: when the wall budget (deadline)
// expires with the parent context still live — a long DB outage, no shutdown —
// the batch must be dropped and counted exactly as before regression fix, so the writer
// stays live and memory-bounded rather than retrying one batch forever.
func TestNormalFlushBudgetExhaustionStillDrops(t *testing.T) {
	fake := func(ctx context.Context, batch []*NavFrame) (int64, error) {
		<-ctx.Done() // every attempt hangs until its per-attempt/deadline context cuts it
		return 0, ctx.Err()
	}
	s := atomicStore(fake)
	s.retry = flushRetry{attempts: 3, backoff: time.Millisecond, attemptTO: 20 * time.Millisecond}

	droppedBefore := testutil.ToFloat64(metrics.StoreQuarantinedTotal)
	batch := []*NavFrame{{SourceID: "dial", Raw: []byte{1}}, {SourceID: "dial", Raw: []byte{2}}}
	completed := s.flush(context.Background(), batch, time.Now().Add(50*time.Millisecond))
	if !completed {
		t.Error("flush returned false (retain) on wall-budget expiry with a live parent; want completed=true with the batch dropped")
	}
	if d := testutil.ToFloat64(metrics.StoreQuarantinedTotal) - droppedBefore; d != 2 {
		t.Errorf("StoreQuarantinedTotal delta = %v, want 2 (budget-exhausted batch dropped and counted)", d)
	}
	// regression fix names the wall budget as a failure class the streak must count;
	// this pin was missing (mutating the budget-expiry branch's gaveUp=true to
	// false left the suite green).
	if n := s.flushFailStreak.Load(); n != 1 {
		t.Errorf("flushFailStreak = %d after a wall-budget give-up, want 1", n)
	}
}

// TestPersistAtomicRetryAbortAfterPartialBisectionDropsRemainder: once a poison
// bisection has committed part of the batch, an interruption may no longer
// signal abort (retention would re-flush — and duplicate — the committed
// dial-mode rows); the cut-short remainder is counted dropped instead.
func TestPersistAtomicRetryAbortAfterPartialBisectionDropsRemainder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	fake := func(fctx context.Context, batch []*NavFrame) (int64, error) {
		calls++
		switch calls {
		case 1: // whole batch: poison → bisect
			return 0, &pgconn.PgError{Code: "22P02", Message: "invalid input"}
		case 2: // first half commits
			return int64(len(batch)), nil
		default: // second half: the shutdown lands here
			cancel()
			return 0, errors.New("connection reset")
		}
	}
	s := atomicStore(fake)

	batch := []*NavFrame{
		{SourceID: "dial", Raw: []byte{1}}, {SourceID: "dial", Raw: []byte{2}},
		{SourceID: "dial", Raw: []byte{3}}, {SourceID: "dial", Raw: []byte{4}},
	}
	written, dropped, aborted, _ := s.persistAtomicRetry(ctx, batch)
	if aborted {
		t.Fatal("aborted=true after the first half committed — retention here would duplicate committed rows")
	}
	if written != 2 || dropped != 2 {
		t.Errorf("written=%d dropped=%d, want 2/2 (first half committed, interrupted remainder dropped)", written, dropped)
	}
}

// TestNormalFlushSkippedOnceShutdownBegun: a size-threshold flush reached after
// storeCtx is cancelled must not start a fresh normal-budget flush — the batch
// falls through to the ctx.Done() drain ("check ctx before flushing").
func TestNormalFlushSkippedOnceShutdownBegun(t *testing.T) {
	var mu sync.Mutex
	var ctxErrsAtCall []error // ctx.Err() sampled AT call time (flush cancels its ctx on return)
	var rows int
	fake := func(ctx context.Context, batch []*NavFrame) (int64, error) {
		mu.Lock()
		ctxErrsAtCall = append(ctxErrsAtCall, ctx.Err())
		rows += len(batch)
		mu.Unlock()
		return int64(len(batch)), nil
	}
	s := atomicStore(fake)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // shutdown already begun before Run ever sees a frame
	for i := 0; i < 3; i++ {
		s.Enqueue(&NavFrame{SourceID: "dial", Raw: []byte{byte(i)}})
	}
	runDone := make(chan struct{})
	go func() { defer close(runDone); s.Run(ctx) }()
	select {
	case <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}

	mu.Lock()
	defer mu.Unlock()
	if rows != 3 {
		t.Fatalf("persisted %d rows, want 3 — the drain must still flush the queue", rows)
	}
	// Every flush must have come from the drain (detached context, live at call
	// time), never from a normal-path flush under the already-cancelled ctx.
	for i, err := range ctxErrsAtCall {
		if errors.Is(err, context.Canceled) {
			t.Errorf("persist call %d ran under the cancelled store context — normal flush was not skipped after shutdown", i)
		}
	}
}
