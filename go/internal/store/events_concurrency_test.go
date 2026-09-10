package store

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
)

// TestIntegrationEventCursorHasNoHolesUnderConcurrency proves regression fix
// against a live database. Before the fix, WriteEvent decided "does this
// (time, dedupe_key) exist?" from a statement snapshot and then incremented the
// audience cursor whenever that snapshot was empty. Two concurrent writers of
// the SAME event could both miss, both consume a sequence, and then race at the
// unique index: the loser returned the winner's audience_seq while its own
// increment stayed committed. The burned sequence is a hole no event will ever
// fill, and SSE replay reads a hole as a lost event — a false replay_gap on a
// client that missed nothing.
func TestIntegrationEventCursorHasNoHolesUnderConcurrency(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond,
		RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	// Audiences unique to this run so the assertions can own the whole cursor.
	stamp := time.Now().UnixNano()
	audienceA := fmt.Sprintf("operator:dayblue-a-%d", stamp)
	audienceB := fmt.Sprintf("operator:dayblue-b-%d", stamp)
	sv := fmt.Sprintf("RDAYBLUEX005-%d", stamp)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM gnss_events WHERE sv = $1`, sv)
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM gnss_event_audience_cursors WHERE audience = ANY($1)`,
			[]string{audienceA, audienceB})
	})

	// A cursor entry is created on first use, so both audiences start absent.
	cursor := func(audience string) int64 {
		t.Helper()
		var last int64
		err := s.pool.QueryRow(ctx,
			`SELECT last_seq FROM gnss_event_audience_cursors WHERE audience = $1`, audience).Scan(&last)
		if err != nil {
			t.Fatalf("cursor(%s): %v", audience, err)
		}
		return last
	}
	maxSeq := func(audience string) int64 {
		t.Helper()
		var highest int64
		err := s.pool.QueryRow(ctx,
			`SELECT coalesce(max(audience_seq), 0) FROM gnss_events WHERE audience = $1 AND sv = $2`,
			audience, sv).Scan(&highest)
		if err != nil {
			t.Fatalf("max(audience_seq) for %s: %v", audience, err)
		}
		return highest
	}
	// seqs returns every audience_seq this run wrote, ordered.
	seqs := func(audience string) []int64 {
		t.Helper()
		rows, err := s.pool.Query(ctx,
			`SELECT audience_seq FROM gnss_events WHERE audience = $1 AND sv = $2 ORDER BY audience_seq`,
			audience, sv)
		if err != nil {
			t.Fatalf("seqs(%s): %v", audience, err)
		}
		defer rows.Close()
		var out []int64
		for rows.Next() {
			var v int64
			if err := rows.Scan(&v); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, v)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("rows: %v", err)
		}
		return out
	}

	// (1) The defect itself: many goroutines released at once on ONE identity.
	// Every call must return the same sequence, exactly one row must exist, and
	// the cursor must have advanced by exactly one.
	const racers = 32
	eventTime := time.Now()
	row := EventRow{
		Audience: audienceA, RedactionClass: "test", Time: eventTime,
		SV: sv, Type: "orbit_disco", Severity: 2,
		DedupeKey: fmt.Sprintf("dayblue-005-same-%d", stamp),
	}
	start := make(chan struct{})
	results := make([]int64, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], errs[i] = s.WriteEvent(ctx, row)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	for i, got := range results {
		if got != results[0] {
			t.Fatalf("racer %d returned sequence %d, racer 0 returned %d; one event must have one sequence",
				i, got, results[0])
		}
	}
	if got := seqs(audienceA); len(got) != 1 {
		t.Fatalf("%d rows for one dedupe identity, want 1: %v", len(got), got)
	}
	if got := cursor(audienceA); got != maxSeq(audienceA) {
		t.Fatalf("cursor for %s is %d but the highest event sequence is %d: %d sequence(s) consumed "+
			"without producing an event — SSE replay reads that hole as a lost event",
			audienceA, got, maxSeq(audienceA), got-maxSeq(audienceA))
	}

	// (2) Valid concurrency must still work: distinct keys in the same audience
	// get distinct, contiguous sequences with no holes and no duplicates.
	const distinct = 24
	wg = sync.WaitGroup{}
	distinctErrs := make([]error, distinct)
	for i := range distinct {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, distinctErrs[i] = s.WriteEvent(ctx, EventRow{
				Audience: audienceA, RedactionClass: "test", Time: time.Now(),
				SV: sv, Type: "orbit_disco", Severity: 1,
				DedupeKey: fmt.Sprintf("dayblue-005-distinct-%d-%d", stamp, i),
			})
		}()
	}
	wg.Wait()
	for i, err := range distinctErrs {
		if err != nil {
			t.Fatalf("distinct writer %d: %v", i, err)
		}
	}
	got := seqs(audienceA)
	if len(got) != distinct+1 {
		t.Fatalf("rows in %s = %d, want %d", audienceA, len(got), distinct+1)
	}
	for i := 1; i < len(got); i++ {
		if got[i] != got[i-1]+1 {
			t.Fatalf("hole in %s between %d and %d: %v", audienceA, got[i-1], got[i], got)
		}
	}
	if last, highest := cursor(audienceA), got[len(got)-1]; last != highest {
		t.Fatalf("cursor for %s = %d, want max(audience_seq) = %d", audienceA, last, highest)
	}

	// (3) Sequences stay isolated per audience: a second audience racing the same
	// dedupe *string* is a different identity and gets its own cursor from 1.
	wg = sync.WaitGroup{}
	otherRow := row
	otherRow.Audience = audienceB
	otherErrs := make([]error, racers)
	otherResults := make([]int64, racers)
	start = make(chan struct{})
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			otherResults[i], otherErrs[i] = s.WriteEvent(ctx, otherRow)
		}()
	}
	close(start)
	wg.Wait()
	for i, err := range otherErrs {
		if err != nil {
			t.Fatalf("audience-B racer %d: %v", i, err)
		}
	}
	// Same (time, dedupe_key) as row, so this is the SAME event — the unique index
	// is on (time, dedupe_key) alone. It resolves to the already-committed row in
	// audience A and must consume nothing in audience B.
	for i, got := range otherResults {
		if got != results[0] {
			t.Fatalf("audience-B racer %d returned %d, want the committed sequence %d "+
				"(the dedupe identity is (time, dedupe_key), not the audience)", i, got, results[0])
		}
	}
	var bCursorRows int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM gnss_event_audience_cursors WHERE audience = $1`, audienceB).Scan(&bCursorRows); err != nil {
		t.Fatalf("audience-B cursor count: %v", err)
	}
	if bCursorRows != 0 {
		t.Fatalf("audience %s allocated a cursor for an event that already existed elsewhere", audienceB)
	}
}

// TestIntegrationEventWriteSerializesOnDedupeIdentity is the deterministic half
// of regression fix. The stress test above can only sample the race window; this
// one pins the mechanism directly by holding the dedupe advisory lock from an
// independent connection: a writer of that identity must wait, a writer of any
// other identity must not, and the waiter must resolve to the already-committed
// sequence without consuming a new one.
func TestIntegrationEventWriteSerializesOnDedupeIdentity(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond,
		RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	stamp := time.Now().UnixNano()
	audience := fmt.Sprintf("operator:dayblue-lock-%d", stamp)
	sv := fmt.Sprintf("RDAYBLUEX005LOCK-%d", stamp)
	t.Cleanup(func() {
		_, _ = s.pool.Exec(context.Background(), `DELETE FROM gnss_events WHERE sv = $1`, sv)
		_, _ = s.pool.Exec(context.Background(),
			`DELETE FROM gnss_event_audience_cursors WHERE audience = $1`, audience)
	})

	eventTime := time.Now()
	row := EventRow{
		Audience: audience, RedactionClass: "test", Time: eventTime,
		SV: sv, Type: "orbit_disco", Severity: 2,
		DedupeKey: fmt.Sprintf("dayblue-005-lock-%d", stamp),
	}
	first, err := s.WriteEvent(ctx, row)
	if err != nil {
		t.Fatalf("first WriteEvent: %v", err)
	}

	// Hold this identity's lock from a connection the store knows nothing about.
	holder, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire holder: %v", err)
	}
	held, err := holder.Begin(ctx)
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := held.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`,
		eventDedupeLockKey(row.Time, row.DedupeKey)); err != nil {
		t.Fatalf("hold dedupe lock: %v", err)
	}

	blocked := make(chan int64, 1)
	blockedErr := make(chan error, 1)
	go func() {
		seq, err := s.WriteEvent(ctx, row)
		if err != nil {
			blockedErr <- err
			return
		}
		blocked <- seq
	}()
	select {
	case seq := <-blocked:
		t.Fatalf("WriteEvent returned %d while another session held this dedupe identity's lock; "+
			"dedupe resolution and sequence allocation are not serialized", seq)
	case err := <-blockedErr:
		t.Fatalf("blocked WriteEvent failed: %v", err)
	case <-time.After(400 * time.Millisecond):
	}

	// A different identity must not be caught behind it: the lock is keyed, not global.
	other := row
	other.DedupeKey = row.DedupeKey + "-other"
	otherDone := make(chan struct{})
	go func() {
		defer close(otherDone)
		if _, err := s.WriteEvent(ctx, other); err != nil {
			t.Errorf("unrelated WriteEvent failed: %v", err)
		}
	}()
	select {
	case <-otherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("an unrelated dedupe identity blocked behind this one; the advisory lock is not keyed per identity")
	}

	held.Rollback(ctx)
	holder.Release()

	select {
	case seq := <-blocked:
		if seq != first {
			t.Fatalf("serialized WriteEvent returned %d, want the committed sequence %d", seq, first)
		}
	case err := <-blockedErr:
		t.Fatalf("released WriteEvent failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("WriteEvent never completed after the dedupe lock was released")
	}

	// One row for this identity, and the cursor advanced only for the two real
	// events (the original and the unrelated one) — never for the retry.
	var rows, cursorLast int64
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM gnss_events WHERE sv = $1 AND dedupe_key = $2`,
		sv, row.DedupeKey).Scan(&rows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if rows != 1 {
		t.Fatalf("rows for one dedupe identity = %d, want 1", rows)
	}
	if err := s.pool.QueryRow(ctx,
		`SELECT last_seq FROM gnss_event_audience_cursors WHERE audience = $1`, audience).Scan(&cursorLast); err != nil {
		t.Fatalf("cursor: %v", err)
	}
	var highest int64
	if err := s.pool.QueryRow(ctx,
		`SELECT max(audience_seq) FROM gnss_events WHERE audience = $1`, audience).Scan(&highest); err != nil {
		t.Fatalf("max seq: %v", err)
	}
	if cursorLast != highest {
		t.Fatalf("cursor = %d but the highest event sequence is %d: %d sequence(s) consumed without an event",
			cursorLast, highest, cursorLast-highest)
	}
}
