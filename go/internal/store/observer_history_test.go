package store

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/identity"
)

func historyQuery() ObserverSampleQuery {
	return ObserverSampleQuery{
		Audience:    identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"},
		CollectorID: "collector-a", Observer: "receiver: 001/東京", Kind: "environment",
		Since: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
		Until: time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC), Limit: 100,
	}
}

func TestObserverHistoryValidation(t *testing.T) {
	if err := historyQuery().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ObserverSampleQuery){
		"public":      func(q *ObserverSampleQuery) { q.Audience = identity.Audience{Kind: identity.AudiencePublic} },
		"empty scope": func(q *ObserverSampleQuery) { q.Audience = identity.Audience{} },
		"unassigned":  func(q *ObserverSampleQuery) { q.Audience.ID = identity.UnassignedOrganization },
		"other collector": func(q *ObserverSampleQuery) {
			q.Audience = identity.Audience{Kind: identity.AudienceOperator, ID: "collector-b"}
		},
		"missing collector": func(q *ObserverSampleQuery) { q.CollectorID = "" },
		"empty observer":    func(q *ObserverSampleQuery) { q.Observer = "" },
		"nul observer":      func(q *ObserverSampleQuery) { q.Observer = "a\x00b" },
		"invalid UTF8":      func(q *ObserverSampleQuery) { q.Observer = "\xff" },
		"large observer":    func(q *ObserverSampleQuery) { q.Observer = strings.Repeat("a", ObserverHistoryMaxIDBytes+1) },
		"kind":              func(q *ObserverSampleQuery) { q.Kind = "raw" },
		"zero time":         func(q *ObserverSampleQuery) { q.Since = time.Time{} },
		"inverted":          func(q *ObserverSampleQuery) { q.Since = q.Until.Add(time.Second) },
		"large window":      func(q *ObserverSampleQuery) { q.Since = q.Until.Add(-ObserverHistoryMaxWindow - time.Second) },
		"limit":             func(q *ObserverSampleQuery) { q.Limit = ObserverHistoryMaxLimit + 1 },
		"zero limit":        func(q *ObserverSampleQuery) { q.Limit = 0 },
		"offset":            func(q *ObserverSampleQuery) { q.Offset = ObserverHistoryMaxOffset + 1 },
		"negative offset":   func(q *ObserverSampleQuery) { q.Offset = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			q := historyQuery()
			change(&q)
			// An invalid scope must fail before dereferencing the database pool.
			if _, err := (&Store{}).QueryObserverSamples(context.Background(), q); err == nil {
				t.Fatal("invalid query accepted")
			}
		})
	}
}

// These query tests need only PostgreSQL. They use the production table DDL in
// an isolated schema, without changing existing tables or Timescale policies.
func observerHistoryTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("NAVLISTENER_OBSERVER_TEST_DSN")
	if dsn == "" {
		t.Skip("set NAVLISTENER_OBSERVER_TEST_DSN for PostgreSQL sensor-history query tests")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	schema := pgx.Identifier{fmt.Sprintf("observer_history_test_%d", time.Now().UnixNano())}.Sanitize()
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE"); err != nil {
			t.Error(err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	start := strings.Index(schemaSQL, "CREATE TABLE IF NOT EXISTS observer_samples (")
	if start < 0 {
		t.Fatal("observer_samples DDL missing")
	}
	end := strings.Index(schemaSQL[start:], "\n);")
	if end < 0 {
		t.Fatal("observer_samples DDL incomplete")
	}
	if _, err := pool.Exec(ctx, schemaSQL[start:start+end+3]); err != nil {
		t.Fatal(err)
	}
	return &Store{pool: pool}
}

func TestObserverHistoryPostgres(t *testing.T) {
	s := observerHistoryTestStore(t)
	ctx := context.Background()
	base := historyQuery()
	stamp := base.Since.Add(time.Hour)
	insert := func(collector, org, source, kind string, collections []string, at time.Time, seq any, data string) {
		t.Helper()
		_, err := s.pool.Exec(ctx, `INSERT INTO observer_samples
			(ts,received_at,source_id,collector_instance_id,organization_id,collection_ids,kind,raw,data,hardware_trust,source_seq)
			VALUES ($1,$1,$2,$3,$4,$5,$6,'secret raw wire',$7,'trusted',$8)`, at, source, collector, org, collections, kind, data, seq)
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("collector-a", "org-a", base.Observer, "environment", []string{"group-a"}, stamp, int64(-1), `{"environment":{"mcp9808_c":24.5,"humidity_percent":null}}`)
	insert("collector-a", "org-b", base.Observer, "environment", []string{"group-b"}, stamp, nil, `{"marker":"other owner"}`)
	insert("collector-b", "org-a", base.Observer, "environment", []string{"group-a"}, stamp, nil, `{"marker":"other collector"}`)
	insert("collector-a", "org-a", "other receiver", "environment", []string{"group-a"}, stamp, nil, `{}`)
	insert("collector-a", "org-a", base.Observer, "timing", []string{"group-a"}, stamp, nil, `{"timing":{}}`)
	insert("collector-a", "org-a", base.Observer, "environment", []string{"group-a"}, base.Since.Add(-time.Second), nil, `{}`)
	insert("collector-a", "org-a", base.Observer, "environment", []string{"group-a"}, base.Until.Add(time.Second), nil, `{}`)
	for name, scope := range map[string]identity.Audience{
		"organization": base.Audience,
		"collection":   {Kind: identity.AudienceCollection, ID: "group-a"},
	} {
		t.Run(name, func(t *testing.T) {
			q := base
			q.Audience = scope
			page, err := s.QueryObserverSamples(ctx, q)
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Samples) != 1 || page.HasMore {
				t.Fatalf("scope leaked or lost rows: %+v", page)
			}
			r := page.Samples[0]
			if r.Sequence == nil || *r.Sequence != "18446744073709551615" || r.Session != nil || r.SampleTime != nil || r.HardwareTrust != "trusted" || !strings.Contains(string(r.Details), `"humidity_percent": null`) {
				t.Fatalf("evidence changed: %+v", r)
			}
		})
	}
	t.Run("operator and tied pagination", func(t *testing.T) {
		q := base
		q.Audience = identity.Audience{Kind: identity.AudienceOperator, ID: "collector-a"}
		q.Limit = 1
		first, err := s.QueryObserverSamples(ctx, q)
		if err != nil || len(first.Samples) != 1 || !first.HasMore {
			t.Fatalf("first: %+v %v", first, err)
		}
		q.Offset = 1
		second, err := s.QueryObserverSamples(ctx, q)
		if err != nil || len(second.Samples) != 1 || second.HasMore {
			t.Fatalf("second: %+v %v", second, err)
		}
		if string(first.Samples[0].Details) == string(second.Samples[0].Details) {
			t.Fatal("repeated tied row")
		}
	})
	t.Run("empty and kind", func(t *testing.T) {
		q := base
		q.Observer = "unseen"
		p, err := s.QueryObserverSamples(ctx, q)
		if err != nil || p.Samples == nil || len(p.Samples) != 0 {
			t.Fatalf("empty: %+v %v", p, err)
		}
		q = base
		q.Kind = "timing"
		p, err = s.QueryObserverSamples(ctx, q)
		if err != nil || len(p.Samples) != 1 || p.Samples[0].Sequence != nil {
			t.Fatalf("timing: %+v %v", p, err)
		}
	})
	t.Run("cancellation", func(t *testing.T) {
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err := s.QueryObserverSamples(canceled, base); err == nil {
			t.Fatal("cancellation ignored")
		}
	})
	t.Run("oversized sample", func(t *testing.T) {
		q := base
		q.Observer = "oversized"
		insert("collector-a", "org-a", q.Observer, q.Kind, []string{}, stamp, nil, `{"value":"`+strings.Repeat("a", observerHistoryMaxSampleBytes)+`"}`)
		if _, err := s.QueryObserverSamples(ctx, q); err == nil {
			t.Fatal("oversized record accepted")
		}
	})
	t.Run("page byte budget", func(t *testing.T) {
		q := base
		q.Observer = "large-page"
		q.Limit = 500
		for i := 0; i < 100; i++ {
			insert("collector-a", "org-a", q.Observer, q.Kind, []string{}, stamp.Add(time.Duration(i)*time.Second), nil, `{"value":"`+strings.Repeat("<", 32*1024)+`"}`)
		}
		p, err := s.QueryObserverSamples(ctx, q)
		if err != nil || !p.HasMore || len(p.Samples) == 0 || len(p.Samples) >= 100 {
			t.Fatalf("unbounded page: count=%d more=%v err=%v", len(p.Samples), p.HasMore, err)
		}
		q.Offset = len(p.Samples)
		next, err := s.QueryObserverSamples(ctx, q)
		if err != nil || len(next.Samples) == 0 || !next.Samples[0].ReceivedAt.After(p.Samples[len(p.Samples)-1].ReceivedAt) {
			t.Fatalf("byte-limited pagination failed: %v", err)
		}
	})
}
