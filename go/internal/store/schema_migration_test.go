package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/config"
)

// TestIntegrationEventsStartupDDLRunsOnce: the gnss_events migration that takes
// ACCESS EXCLUSIVE locks — adding audience_seq, SET NOT NULL, seeding the
// audience cursors, creating the notify trigger — runs on the first start
// against a table that pre-dates it and never again. A second start must leave
// the trigger (same OID) and the cursor table untouched, so a collector restart
// in a shared database does not block the other collectors' writes.
func TestIntegrationEventsStartupDDLRunsOnce(t *testing.T) {
	dsn := isolatedDSN(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer pool.Close()
	for _, sql := range []string{
		`DROP TABLE IF EXISTS gnss_event_audience_cursors CASCADE`,
		`DROP TABLE IF EXISTS gnss_events CASCADE`,
		// The shape before audience-local cursors: no audience_seq, no cursor table.
		`CREATE TABLE gnss_events (
			id BIGSERIAL, time TIMESTAMPTZ NOT NULL, audience TEXT NOT NULL DEFAULT 'legacy-operator',
			redaction_class TEXT NOT NULL DEFAULT 'private', sv TEXT NOT NULL, event_type TEXT NOT NULL,
			old_value TEXT, new_value TEXT, severity SMALLINT NOT NULL DEFAULT 0, message TEXT, raw JSONB, dedupe_key TEXT)`,
		`SELECT create_hypertable('gnss_events', 'time')`,
		`INSERT INTO gnss_events (time, sv, event_type) VALUES (now() - interval '2 hours', 'G01', 'health_change'), (now() - interval '1 hour', 'G02', 'health_change')`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("legacy gnss_events: %v", err)
		}
	}

	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New against the legacy shape: %v", err)
	}
	s.Close()

	var mismatched int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM gnss_events WHERE audience_seq IS DISTINCT FROM id`).Scan(&mismatched); err != nil || mismatched != 0 {
		t.Fatalf("legacy rows not given their id as audience_seq: %d %v", mismatched, err)
	}
	var notNull bool
	if err := pool.QueryRow(ctx, `SELECT attnotnull FROM pg_attribute WHERE attrelid = 'gnss_events'::regclass AND attname = 'audience_seq'`).Scan(&notNull); err != nil || !notNull {
		t.Fatalf("audience_seq NOT NULL = %v %v", notNull, err)
	}
	var cursor int64
	if err := pool.QueryRow(ctx, `SELECT last_seq FROM gnss_event_audience_cursors WHERE audience = 'legacy-operator'`).Scan(&cursor); err != nil || cursor != 2 {
		t.Fatalf("legacy cursor seeded to %d %v, want the highest legacy id", cursor, err)
	}
	triggerOID := func() uint32 {
		t.Helper()
		var oids []uint32
		rows, err := pool.Query(ctx, `SELECT oid FROM pg_trigger WHERE tgrelid = 'gnss_events'::regclass AND tgname = 'gnss_event_notify'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var oid uint32
			if err := rows.Scan(&oid); err != nil {
				t.Fatal(err)
			}
			oids = append(oids, oid)
		}
		if len(oids) != 1 {
			t.Fatalf("notify trigger present %d times on gnss_events, want once", len(oids))
		}
		return oids[0]
	}
	first := triggerOID()

	// Move the cursor below the table's maximum: a re-run of the seed would
	// raise it back, so an unchanged value proves the seed did not run again.
	if _, err := pool.Exec(ctx, `UPDATE gnss_event_audience_cursors SET last_seq = 1 WHERE audience = 'legacy-operator'`); err != nil {
		t.Fatal(err)
	}
	s, err = New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("second store.New: %v", err)
	}
	s.Close()
	if second := triggerOID(); second != first {
		t.Fatalf("notify trigger recreated on restart: oid %d -> %d", first, second)
	}
	if err := pool.QueryRow(ctx, `SELECT last_seq FROM gnss_event_audience_cursors WHERE audience = 'legacy-operator'`).Scan(&cursor); err != nil || cursor != 1 {
		t.Fatalf("cursor re-seeded on restart: %d %v", cursor, err)
	}
}
