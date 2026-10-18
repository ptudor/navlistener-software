package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
)

func TestIntegrationNetworkHistoryMigration(t *testing.T) {
	dsn := isolatedDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond, RawRetention: "7 days", CompressAfter: "1 day"}
	open := func() {
		t.Helper()
		s, err := New(ctx, cfg, integrationLog())
		if err != nil {
			t.Fatal(err)
		}
		s.Close()
	}
	open()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err = pool.Exec(ctx, `DELETE FROM observer_samples WHERE source_id='network-migration'`); err != nil {
		t.Fatal(err)
	}
	// Recreate the legacy check after a chunk exists, then exercise normal startup.
	_, err = pool.Exec(ctx, `INSERT INTO observer_samples(ts,received_at,source_id,collector_instance_id,kind,raw,data)
 VALUES(now(),now(),'network-migration','collector-a','environment','\x0104','{}');
 ALTER TABLE observer_samples DROP CONSTRAINT observer_samples_kind_check;
 ALTER TABLE observer_samples ADD CONSTRAINT observer_samples_kind_check CHECK(kind IN ('environment','timing'));`)
	if err != nil {
		t.Fatal(err)
	}
	open()
	var firstOID, secondOID uint32
	query := `SELECT oid FROM pg_constraint WHERE conrelid='observer_samples'::regclass AND conname='observer_samples_kind_check'`
	if err = pool.QueryRow(ctx, query).Scan(&firstOID); err != nil {
		t.Fatal(err)
	}
	_, err = pool.Exec(ctx, `INSERT INTO observer_samples(ts,received_at,source_id,collector_instance_id,kind,raw,data)
 VALUES(now(),now(),'network-migration','collector-a','network','\x0104','{"network":{"counters":{"wifi_disconnects":3}}}')`)
	if err != nil {
		t.Fatal("migrated constraint rejects network", err)
	}
	open()
	if err = pool.QueryRow(ctx, query).Scan(&secondOID); err != nil || firstOID != secondOID {
		t.Fatal("migration repeats on restart", err)
	}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	page, err := s.QueryObserverSamples(ctx, ObserverSampleQuery{Audience: identity.Audience{Kind: identity.AudienceOperator, ID: "collector-a"}, CollectorID: "collector-a", Observer: "network-migration", Kind: "network", Since: time.Now().Add(-time.Hour), Until: time.Now().Add(time.Minute), Limit: 10})
	if err != nil || len(page.Samples) != 1 {
		t.Fatalf("network history: %+v %v", page, err)
	}
}
