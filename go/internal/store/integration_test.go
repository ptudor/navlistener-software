package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// isolatedDSN returns the DSN for a database with **no navlistener tables** —
// distinct from testDSN's shared instance — for tests that need to control initial
// table state (e.g. simulating a pre-existing intsat deployment) before store.New
// applies schema.sql. Skips if NAVLISTENER_TEST_DSN_ISOLATED is unset.
//
// Setup, both parts required:
//
//	createdb navlistener_test_isolated
//	psql -d navlistener_test_isolated -c 'CREATE EXTENSION timescaledb'
//
// "Empty" means empty of *our* tables, not of the extension: store.New requires
// TimescaleDB to be present already and deliberately does not install it (that needs
// a superuser), so a genuinely bare database fails these tests on the extension check
// long before reaching the column-drift check they exist to exercise.
func isolatedDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("NAVLISTENER_TEST_DSN_ISOLATED")
	if dsn == "" {
		t.Skip("NAVLISTENER_TEST_DSN_ISOLATED not set; skipping isolated-database integration test")
	}
	return dsn
}

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
	mk := func(source, session string, seq uint64, hasSeq bool, raw []byte) *NavFrame {
		return &NavFrame{
			Ts: time.Now(), ReceivedAt: now, SourceID: source,
			GnssID: 0, SvID: 1, SigID: 0, MsgType: 1, Raw: raw,
			SourceSeq: seq, HasSourceSeq: hasSeq, Session: session,
		}
	}

	s.Enqueue(mk(obs, "boot-a", 100, true, []byte{1, 2, 3}))
	s.Enqueue(mk(dial, "", 0, false, []byte{4, 5, 6}))
	time.Sleep(250 * time.Millisecond)

	// Reconnect replay of seq 100, plus a genuinely new seq 101; a second dial-mode
	// frame with identical content (dial-mode never dedups). Then the regression fix
	// scenario itself: a REBOOTED feeder (fresh session) reusing seq 100 — a
	// different frame in a different sequence space that must persist, where the
	// pre-session ledger silently discarded it as a replay.
	s.Enqueue(mk(obs, "boot-a", 100, true, []byte{1, 2, 3}))
	s.Enqueue(mk(obs, "boot-a", 101, true, []byte{7, 8, 9}))
	s.Enqueue(mk(dial, "", 0, false, []byte{4, 5, 6}))
	s.Enqueue(mk(obs, "boot-b", 100, true, []byte{10, 11, 12}))
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

	// Three, not two: (boot-a,100), (boot-a,101), and (boot-b,100). The last is the
	// regression fix case this test's own body was extended to cover — a rebooted feeder
	// reusing seq 100 in a fresh sequence space — which the session dimension of the
	// dedup key must let through. The expectation was left at 2 when that case was
	// added, and no local TimescaleDB could run this test to catch it.
	if navCount != 3 {
		t.Errorf("nav_frames rows for %s = %d, want 3 (boot-a seq 100 once with its replay dropped, boot-a seq 101, and boot-b seq 100 from the rebooted feeder)", obs, navCount)
	}
	if dialCount != 2 {
		t.Errorf("nav_frames rows for %s = %d, want 2 (dial-mode frames are never deduplicated)", dial, dialCount)
	}
	if ledgerCount != 3 {
		t.Errorf("nav_frames_seq_seen rows for %s = %d, want 3 (boot-a seq 100, boot-a seq 101, boot-b seq 100 — the ledger is keyed on session too)", obs, ledgerCount)
	}
}

// TestIntegrationAtomicReplayClaim proves the replay claim is not stranded when
// CopyFrom rejects the corresponding raw row. The second attempt uses the same
// key with valid JSON and must persist exactly once.
func TestIntegrationAtomicReplayClaim(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	const source = "r002-atomic-integration"
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames WHERE source_id = $1`, source); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames_seq_seen WHERE source_id = $1`, source); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	frame := &NavFrame{Ts: now, ReceivedAt: now, SourceID: source, GnssID: 0, SvID: 1,
		SigID: 0, MsgType: 0x10, Raw: []byte{1}, Decoded: []byte(`{not-json`),
		SourceSeq: 1, HasSourceSeq: true, Session: "boot-r002"}
	if _, err := s.persistAtomicOnce(ctx, []*NavFrame{frame}); err == nil {
		t.Fatal("invalid JSON CopyFrom unexpectedly succeeded")
	}
	var claims int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames_seq_seen WHERE source_id = $1`, source).Scan(&claims); err != nil {
		t.Fatal(err)
	}
	if claims != 0 {
		t.Fatalf("dedup claims after failed CopyFrom = %d, want 0", claims)
	}

	frame.Decoded = []byte(`{"ok":true}`)
	if n, err := s.persistAtomicOnce(ctx, []*NavFrame{frame}); err != nil || n != 1 {
		t.Fatalf("retry persist = (%d, %v), want (1, nil)", n, err)
	}
	if n, err := s.persistAtomicOnce(ctx, []*NavFrame{frame}); err != nil || n != 0 {
		t.Fatalf("replay persist = (%d, %v), want (0, nil)", n, err)
	}
	var rows int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames WHERE source_id = $1`, source).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("nav_frames rows = %d, want exactly 1", rows)
	}
}

// TestIntegrationQueryEventsTotalPastLastPage guards QueryEvents' total is
// observed only via the paginated rows' count(*) OVER() -- a page past the last
// matching row (offset >= the matching count) returns zero rows, and without a
// fallback count total silently collapses to 0 even though rows genuinely match.
func TestIntegrationQueryEventsTotalPastLastPage(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	const sv = "R114-integration-sv"
	if _, err := s.pool.Exec(ctx, `DELETE FROM gnss_events WHERE sv = $1`, sv); err != nil {
		t.Fatalf("cleanup gnss_events: %v", err)
	}
	now := time.Now()
	for i := 0; i < 5; i++ {
		if _, err := s.WriteEvent(ctx, EventRow{
			Time: now.Add(time.Duration(i) * time.Second), SV: sv, Type: "orbit_disco", Severity: 1,
			DedupeKey: fmt.Sprintf("r114-test-%d", i),
		}); err != nil {
			t.Fatalf("WriteEvent %d: %v", i, err)
		}
	}

	events, total, err := s.QueryEvents(ctx, EventQuery{SV: sv, Limit: 100, Offset: 10})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("events at offset=10 = %d, want 0 (only 5 rows exist)", len(events))
	}
	if total != 5 {
		t.Errorf("total = %d, want 5 (the fix spec's exact verification: insert 5, offset=10, total must still read 5)", total)
	}

	// A within-range page still reports the same total.
	events, total, err = s.QueryEvents(ctx, EventQuery{SV: sv, Limit: 100, Offset: 0})
	if err != nil {
		t.Fatalf("QueryEvents: %v", err)
	}
	if len(events) != 5 || total != 5 {
		t.Errorf("first page = %d events / total %d, want 5/5", len(events), total)
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
	// session_id is part of the ledger's primary key since the regression fix revision
	// and is NOT NULL; seeding without it violated the constraint. The prune is
	// keyed on seen_at alone, so one session is enough to exercise it.
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO nav_frames_seq_seen (source_id, session_id, feeder_seq, seen_at) VALUES ($1, 'boot-prune', 1, $2), ($1, 'boot-prune', 2, $3)`,
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

// intsatGnssEventsDDL and intsatGnssSnapshotsDDL are copied verbatim from
// apps/intsat/go/migrations/001_init.sql (this repo's sibling product,
// docs/DESIGN.md's stated compatibility goal) to regression-test actual
// premise: today the two schemas are column-identical, but nothing enforces that
// going forward, and this pins the real shape rather than a hypothetical one.
const intsatGnssEventsDDL = `
CREATE TABLE IF NOT EXISTS gnss_events (
    id          BIGSERIAL,
    time        TIMESTAMPTZ NOT NULL,
    sv          TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    old_value   TEXT,
    new_value   TEXT,
    severity    SMALLINT NOT NULL DEFAULT 0,
    message     TEXT,
    raw         JSONB
);`

const intsatGnssSnapshotsDDL = `
CREATE TABLE IF NOT EXISTS gnss_snapshots (
    time        TIMESTAMPTZ NOT NULL,
    endpoint    TEXT NOT NULL,
    data        JSONB NOT NULL
);`

// TestIntegrationVerifyRequiredColumnsAcceptsIntsatSchema guards happy
// path: a database where gnss_events/gnss_snapshots were already created by
// intsat's actual 001_init.sql (verified byte-identical to navlistener's own
// column set as of this fix) must not be rejected by verifyRequiredColumns.
func TestIntegrationVerifyRequiredColumnsAcceptsIntsatSchema(t *testing.T) {
	dsn := isolatedDSN(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, intsatGnssEventsDDL); err != nil {
		t.Fatalf("pre-create gnss_events (intsat shape): %v", err)
	}
	if _, err := pool.Exec(ctx, intsatGnssSnapshotsDDL); err != nil {
		t.Fatalf("pre-create gnss_snapshots (intsat shape): %v", err)
	}

	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New rejected an intsat-pre-created schema it should accept: %v", err)
	}
	s.pool.Close()
}

// TestIntegrationVerifyRequiredColumnsFailsOnMissingColumn guards failure
// path: a pre-existing gnss_events missing a column this build requires (here,
// `raw`) must fail store.New with a clear, actionable error — not a working start
// that only fails later on the first WriteEvent/QueryEvents touching that column.
func TestIntegrationVerifyRequiredColumnsFailsOnMissingColumn(t *testing.T) {
	dsn := isolatedDSN(t)
	ctx := context.Background()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	// If TestIntegrationVerifyRequiredColumnsAcceptsIntsatSchema already ran against
	// this same isolated DB, gnss_events exists with the right shape; drop it so
	// this test controls the initial state.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS gnss_events CASCADE`); err != nil {
		t.Fatalf("drop gnss_events: %v", err)
	}
	if _, err := pool.Exec(ctx, `CREATE TABLE gnss_events (
		id BIGSERIAL, time TIMESTAMPTZ NOT NULL, sv TEXT NOT NULL, event_type TEXT NOT NULL,
		old_value TEXT, new_value TEXT, severity SMALLINT NOT NULL DEFAULT 0, message TEXT
		-- raw JSONB intentionally omitted
	)`); err != nil {
		t.Fatalf("pre-create incompatible gnss_events: %v", err)
	}

	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	_, err = New(ctx, cfg, integrationLog())
	if err == nil {
		t.Fatal("store.New succeeded against a gnss_events missing the raw column, want a fail-fast error")
	}
	if !strings.Contains(err.Error(), "gnss_events") || !strings.Contains(err.Error(), "raw") {
		t.Errorf("error = %q, want it to name the table and the missing column", err.Error())
	}
	t.Logf("got expected error: %v", err)

	// Restore a compatible table so a later run of the happy-path test against this
	// same isolated DB isn't left broken.
	if _, err := pool.Exec(ctx, `DROP TABLE IF EXISTS gnss_events CASCADE`); err != nil {
		t.Fatalf("cleanup gnss_events: %v", err)
	}
}

// TestIntegrationNotifyPayloadBounded guards an event with a multi-KB
// message must not fail its INSERT (pg_notify's ~8000-byte payload limit would
// raise inside the AFTER INSERT trigger and fail the whole transaction if message
// were still in the payload), and the fired notification's payload must be well
// under the limit regardless of message length.
func TestIntegrationNotifyPayloadBounded(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN gnss_event_v2"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	longMessage := strings.Repeat("x", 20_000) // well past pg_notify's ~8000-byte payload limit
	id, err := s.WriteEvent(ctx, EventRow{
		Audience: "public", RedactionClass: "public_policy_filtered",
		Time: time.Now(), SV: "G01-notify-test", Type: "test_event", Severity: 1, Message: longMessage,
		DedupeKey: fmt.Sprintf("notify-test-%d", time.Now().UnixNano()),
	})
	if err != nil {
		t.Fatalf("WriteEvent with a %d-byte message failed (pg_notify payload limit leaking into the INSERT): %v", len(longMessage), err)
	}

	nctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := conn.Conn().WaitForNotification(nctx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	if n.Channel != "gnss_event_v2" {
		t.Errorf("notification channel = %q, want gnss_event_v2", n.Channel)
	}
	if len(n.Payload) > 1000 {
		t.Errorf("notification payload is %d bytes, want well under pg_notify's ~8000-byte limit", len(n.Payload))
	}
	if strings.Contains(n.Payload, "xxxx") {
		t.Error("notification payload contains the long message; it must carry only id/sv/type/severity")
	}
	if !strings.Contains(n.Payload, fmt.Sprintf(`"id" : %d`, id)) { // json_build_object spaces its colons
		t.Errorf("notification payload = %q, want it to carry the inserted event's id (%d)", n.Payload, id)
	}
}

// TestIntegrationEventAudienceSequencesDoNotCross proves regression fix against the
// real cursor table: an operator-only event between two public events cannot
// create a gap in the public SSE/query id sequence.
func TestIntegrationEventAudienceSequencesDoNotCross(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	stamp := fmt.Sprintf("rju439-%d", time.Now().UnixNano())
	write := func(audience, suffix string) int64 {
		t.Helper()
		seq, err := s.WriteEvent(ctx, EventRow{
			Audience: audience, RedactionClass: "test", Time: time.Now(),
			SV: stamp, Type: "test_event", DedupeKey: stamp + "-" + suffix,
		})
		if err != nil {
			t.Fatalf("WriteEvent(%s): %v", audience, err)
		}
		return seq
	}
	public1 := write("public", "public-a")
	_ = write("operator:local", "private-between")
	public2 := write("public", "public-b")
	if public2 != public1+1 {
		t.Fatalf("public sequence jumped %d -> %d after private event", public1, public2)
	}

	pub, total, err := s.QueryEvents(ctx, EventQuery{Audience: "public", SV: stamp, Limit: 10})
	if err != nil || total != 2 || len(pub) != 2 {
		t.Fatalf("public query = %d/%d, %v", len(pub), total, err)
	}
	op, total, err := s.QueryEvents(ctx, EventQuery{Audience: "operator:local", SV: stamp, Limit: 10})
	if err != nil || total != 1 || len(op) != 1 {
		t.Fatalf("operator query = %d/%d, %v", len(op), total, err)
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM gnss_events WHERE sv = $1`, stamp); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
}

// TestIntegrationEventWriteIdempotent proves the regression fix contract against a
// live database: retrying WriteEvent with the identical row (the ambiguous
// commit-then-client-error case — PostgreSQL committed but the client saw a
// timeout) returns the SAME already-committed id and leaves exactly one row,
// while a distinct confirmation (its own dedupe key) still inserts freshly.
func TestIntegrationEventWriteIdempotent(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	const sv = "R152-integration-sv"
	if _, err := s.pool.Exec(ctx, `DELETE FROM gnss_events WHERE sv = $1`, sv); err != nil {
		t.Fatalf("cleanup gnss_events: %v", err)
	}

	row := EventRow{
		Time: time.Now(), SV: sv, Type: "orbit_disco", Severity: 2,
		DedupeKey: fmt.Sprintf("r152-test-%d", time.Now().UnixNano()),
	}
	id1, err := s.WriteEvent(ctx, row)
	if err != nil {
		t.Fatalf("first WriteEvent: %v", err)
	}
	id2, err := s.WriteEvent(ctx, row) // the retry after an ambiguous result
	if err != nil {
		t.Fatalf("retried WriteEvent: %v", err)
	}
	if id1 != id2 {
		t.Errorf("retry returned id %d, want the committed id %d", id2, id1)
	}
	var count int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM gnss_events WHERE sv = $1`, sv).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("rows after retry = %d, want 1", count)
	}

	// A different confirmation (fresh key) at the same instant still inserts.
	row2 := row
	row2.DedupeKey = row.DedupeKey + "-b"
	id3, err := s.WriteEvent(ctx, row2)
	if err != nil {
		t.Fatalf("distinct-key WriteEvent: %v", err)
	}
	if id3 == id1 {
		t.Errorf("distinct confirmation reused id %d", id1)
	}

	// An empty key is a caller bug and must be rejected, not silently unprotected.
	if _, err := s.WriteEvent(ctx, EventRow{Time: time.Now(), SV: sv, Type: "orbit_disco"}); err == nil {
		t.Error("WriteEvent accepted an empty dedupe key")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM gnss_events WHERE sv = $1`, sv); err != nil {
		t.Fatalf("cleanup gnss_events: %v", err)
	}
}

// TestIntegrationQueryNavFramesAcrossChunks exercises QueryNavFrames against a real
// hypertable, which is the only place its ordering contract can actually be tested.
// schema.sql sets chunk_time_interval to 1 hour, and a QA replay window spans many
// hours, so the rows a replay consumes are physically spread across chunks. A plain
// table — the workaround used while local TimescaleDB was Apache-licensed and could
// not apply schema.sql — stores them in one heap and would report a total order the
// hypertable does not owe us.
//
// It also pins the two properties a replay depends on and a unit test cannot reach:
// the window is half-open (Since inclusive, Until exclusive), and freqid survives the
// round trip, which is the whole reason the column exists (a GLONASS frame cannot be
// replayed without its FDMA channel, and it is absent from raw).
func TestIntegrationQueryNavFramesAcrossChunks(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	const source = "qnf-integration"
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames WHERE source_id = $1`, source); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	// Six frames at 90-minute spacing: with a 1-hour chunk interval every frame lands
	// in a different chunk, so a correct result cannot come from within-chunk order.
	base := time.Now().Add(-24 * time.Hour).Truncate(time.Hour)
	const step = 90 * time.Minute
	const n = 6
	rows := make([][]any, 0, n)
	for i := range n {
		at := base.Add(time.Duration(i) * step)
		rows = append(rows, navFrameToRow(&NavFrame{
			Ts: at, ReceivedAt: at, SourceID: source,
			GnssID: 6, SvID: 1 + i, SigID: 0, FreqID: i, MsgType: 0x30,
			Raw: []byte{byte(i), 0, 0, 0}, DecoderVer: "test",
		}))
	}
	// Insert in reverse reception-time order: regression fix now preserves this
	// first-storage admission order, even when the receiver clock goes backwards.
	for i := len(rows) - 1; i >= 0; i-- {
		if _, err := s.copyRows(ctx, rows[i:i+1]); err != nil {
			t.Fatalf("seed row %d: %v", i, err)
		}
	}

	glo := 6
	var got []StoredNavFrame
	// Half-open window: [base, base+step*(n-1)) must exclude the final frame.
	if err := s.QueryNavFrames(ctx, NavFrameQuery{
		GnssID: &glo, SourceID: source,
		Since: base, Until: base.Add(step * time.Duration(n-1)),
	}, func(f StoredNavFrame) error {
		got = append(got, f)
		return nil
	}); err != nil {
		t.Fatalf("QueryNavFrames: %v", err)
	}

	if len(got) != n-1 {
		t.Fatalf("got %d frames, want %d (Until is exclusive, so the last frame is out)", len(got), n-1)
	}
	for i, f := range got {
		index := n - 2 - i // last timestamp excluded; remaining admission order is descending
		if want := base.Add(time.Duration(index) * step); !f.ReceivedAt.Equal(want) {
			t.Errorf("frame %d received_at = %s, want %s (rows must retain first-storage order across chunks)",
				i, f.ReceivedAt, want)
		}
		if f.FreqID != index {
			t.Errorf("frame %d freqid = %d, want %d — the GLONASS channel did not survive the round trip", i, f.FreqID, index)
		}
		if f.SvID != 1+index {
			t.Errorf("frame %d svid = %d, want %d", i, f.SvID, 1+index)
		}
	}
}

// TestIntegrationQueryNavFramesLimitIsLoud pins that hitting the row cap reports
// ErrNavFrameLimit rather than truncating silently. A distribution built from a
// quietly clipped head is wrong in a way that still looks entirely plausible, so the
// caller has to be told.
func TestIntegrationQueryNavFramesLimitIsLoud(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.Close()

	const source = "qnf-limit-integration"
	if _, err := s.pool.Exec(ctx, `DELETE FROM nav_frames WHERE source_id = $1`, source); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	base := time.Now().Add(-3 * time.Hour).Truncate(time.Hour)
	for i := range 4 {
		at := base.Add(time.Duration(i) * time.Minute)
		if _, err := s.copyRows(ctx, [][]any{navFrameToRow(&NavFrame{
			Ts: at, ReceivedAt: at, SourceID: source,
			GnssID: 6, SvID: 1, SigID: 0, FreqID: 0, MsgType: 0x30,
			Raw: []byte{0, 0, 0, 0}, DecoderVer: "test",
		})}); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}

	seen := 0
	err = s.QueryNavFrames(ctx, NavFrameQuery{
		SourceID: source, Since: base, Limit: 2,
	}, func(StoredNavFrame) error { seen++; return nil })
	if !errors.Is(err, ErrNavFrameLimit) {
		t.Fatalf("err = %v, want ErrNavFrameLimit", err)
	}
	if seen != 2 {
		t.Errorf("delivered %d frames before the cap, want 2", seen)
	}
}
