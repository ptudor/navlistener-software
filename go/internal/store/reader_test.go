package store

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"net/url"
	"strings"
)

func TestIntegrationReaderPreservesPoliciesAndEvidence(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	w, err := New(ctx, config.Store{DSN: dsn, RawRetention: "90 days", CompressAfter: "30 days"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	// This role has no authority to migrate, prune, or install policies.
	// Roles are cluster-wide: two suites on one cluster can both pass the
	// existence check, so tolerate the losing CREATE ROLE.
	_, err = w.pool.Exec(ctx, `DO $$ BEGIN
	BEGIN
	CREATE ROLE navlistener_review_reader LOGIN;
	EXCEPTION WHEN duplicate_object THEN NULL;
	END; END $$;
	GRANT USAGE ON SCHEMA public TO navlistener_review_reader;
	GRANT SELECT ON nav_frames TO navlistener_review_reader;
	INSERT INTO nav_frames (ts, received_at, source_id, gnssid, svid, sigid, freqid, msg_type, raw, decoder_ver)
	VALUES (now()-interval '60 days', now()-interval '60 days', 'reader-policy-test', 6, 1, 0, 7, 1, '\x0102', 'test')`)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		t.Helper()
		var policies, columns []byte
		var count int64
		// next_start is scheduler-owned and moves on its own once the job is
		// picked up; everything else in the row is policy configuration.
		if err := w.pool.QueryRow(ctx, `SELECT coalesce(jsonb_agg((to_jsonb(j) - 'next_start') ORDER BY job_id), '[]') FROM timescaledb_information.jobs j WHERE hypertable_name='nav_frames'`).Scan(&policies); err != nil {
			t.Fatal(err)
		}
		if err := w.pool.QueryRow(ctx, `SELECT jsonb_agg(to_jsonb(c) ORDER BY ordinal_position) FROM information_schema.columns c WHERE table_name='nav_frames'`).Scan(&columns); err != nil {
			t.Fatal(err)
		}
		if err := w.pool.QueryRow(ctx, `SELECT count(*) FROM nav_frames`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal([]any{string(policies), string(columns), count})
		return string(b)
	}
	before := snapshot()
	readerDSN := dsn + " user=navlistener_review_reader"
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		u.User = url.User("navlistener_review_reader")
		readerDSN = u.String()
	}
	r, err := OpenReader(ctx, readerDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	n := 0
	if err := r.QueryNavFrames(ctx, NavFrameQuery{Since: time.Now().Add(-80 * 24 * time.Hour), SourceID: "reader-policy-test"}, func(StoredNavFrame) error { n++; return nil }); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("old evidence missing")
	}
	if after := snapshot(); before != after {
		t.Fatalf("reader changed schema, policies or evidence:\nbefore %s\nafter %s", before, after)
	}
	var readOnly string
	if err := r.pool.QueryRow(ctx, `SHOW default_transaction_read_only`).Scan(&readOnly); err != nil || readOnly != "on" {
		t.Fatalf("read-only setting: %q %v", readOnly, err)
	}
	// Writer initialization still honors an explicit retention change.
	w2, err := New(ctx, config.Store{DSN: dsn, RawRetention: "91 days", CompressAfter: "30 days"}, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	w2.Close()
	if snapshot() == before {
		t.Fatal("writer no longer applies configured policies")
	}
}
