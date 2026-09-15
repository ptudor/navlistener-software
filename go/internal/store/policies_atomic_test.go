package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/config"
)

// policySet is the installed compression/retention configuration for the three
// hypertables, as the extension's own job metadata reports it. This is the state
// regression fix is about: a partial set here means a hypertable is running with
// no retention or no compression.
func policySet(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT hypertable_name, proc_name, config::text
		   FROM timescaledb_information.jobs
		  WHERE hypertable_name IN ('nav_frames', 'gnss_snapshots', 'gnss_events', 'observer_samples')`)
	if err != nil {
		t.Fatalf("read policy jobs: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var table, proc, cfg string
		if err := rows.Scan(&table, &proc, &cfg); err != nil {
			t.Fatalf("scan job: %v", err)
		}
		out = append(out, fmt.Sprintf("%s/%s/%s", table, proc, cfg))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("job rows: %v", err)
	}
	sort.Strings(out)
	return out
}

// TestIntegrationPolicyReplacementIsAtomic proves regression fix against a live
// database. applyPolicies replaces six policies as remove-then-add pairs; run as
// independent autocommit statements, any failure or cancellation between a remove
// and its add left that hypertable with no compression or no retention while
// startup merely returned an error. Every failure point must now leave the
// previous policy set completely intact.
func TestIntegrationPolicyReplacementIsAtomic(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()

	// Install a known-good baseline through the normal path.
	baseCfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond,
		RawRetention: "21 days", CompressAfter: "2 days"}
	s, err := New(ctx, baseCfg, integrationLog())
	if err != nil {
		t.Fatalf("baseline store.New: %v", err)
	}
	pool := s.pool
	defer pool.Close()

	baseline := policySet(t, ctx, pool)
	if len(baseline) == 0 {
		t.Fatal("baseline installed no policies; the fixture is not exercising anything")
	}

	// Each of these statements is one of applyPolicies' remove/add boundaries.
	// Cancelling at each in turn covers "after every remove/add boundary".
	boundaries := []string{
		"remove_compression_policy('nav_frames'",
		"add_compression_policy('nav_frames'",
		"remove_retention_policy('nav_frames'",
		"add_retention_policy('nav_frames'",
		"remove_compression_policy('gnss_snapshots'",
		"add_compression_policy('gnss_snapshots'",
		"remove_retention_policy('gnss_snapshots'",
		"add_retention_policy('gnss_snapshots'",
		"remove_compression_policy('gnss_events'",
		"add_compression_policy('gnss_events'",
		"remove_compression_policy('observer_samples'",
		"add_compression_policy('observer_samples'",
		"remove_retention_policy('observer_samples'",
		"add_retention_policy('observer_samples'",
	}
	// A different interval, so a partial application would be visibly different
	// from the baseline rather than coincidentally identical.
	changed := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond,
		RawRetention: "13 days", CompressAfter: "3 hours"}

	for i, boundary := range boundaries {
		t.Run(fmt.Sprintf("fail_after_%d_%s", i, strings.SplitN(boundary, "(", 2)[0]), func(t *testing.T) {
			failCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			// Cancel the moment this boundary's statement has been issued, which
			// aborts the transaction partway through the replacement sequence.
			hook := func(sql string) {
				if strings.Contains(sql, boundary) {
					cancel()
				}
			}
			err := applyPoliciesWithHook(failCtx, pool, integrationLog(), changed, hook)
			if err == nil {
				t.Fatal("cancelled policy application reported success")
			}
			if got := policySet(t, ctx, pool); !equalSets(got, baseline) {
				t.Fatalf("policy set changed after a failure at %q:\n got: %v\nwant: %v",
					boundary, got, baseline)
			}
		})
	}

	// A successful application replaces the whole set, and repeating it is a
	// no-op — restart idempotency.
	if err := applyPolicies(ctx, pool, integrationLog(), changed); err != nil {
		t.Fatalf("applyPolicies: %v", err)
	}
	applied := policySet(t, ctx, pool)
	if equalSets(applied, baseline) {
		t.Fatal("successful application left the baseline policies in place")
	}
	if !containsPolicy(applied, "nav_frames", "13 days") {
		t.Fatalf("nav_frames retention was not replaced: %v", applied)
	}
	if err := applyPolicies(ctx, pool, integrationLog(), changed); err != nil {
		t.Fatalf("second applyPolicies (restart idempotency): %v", err)
	}
	if again := policySet(t, ctx, pool); !equalSets(again, applied) {
		t.Fatalf("re-applying the same config changed the policy set:\n got: %v\nwant: %v", again, applied)
	}

	// regression fix still holds: events are compression-only, never retention.
	for _, p := range applied {
		if strings.HasPrefix(p, "gnss_events/policy_retention") {
			t.Fatalf("a retention policy was installed on gnss_events: %v", applied)
		}
	}

	// Restore the baseline so a shared test database is left as it was found.
	if err := applyPolicies(ctx, pool, integrationLog(), baseCfg); err != nil {
		t.Fatalf("restore baseline: %v", err)
	}
}

func equalSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsPolicy(set []string, table, want string) bool {
	for _, p := range set {
		if strings.HasPrefix(p, table+"/") && strings.Contains(p, want) {
			return true
		}
	}
	return false
}

// The policy replacement must fit comfortably inside its own reserved budget.
// regression fix split the single 10 s startup context in two precisely because a
// slow ping/extension-check/schema-migration could otherwise leave this step with
// almost nothing; the reservation is only meaningful if the work it protects
// genuinely fits.
func TestIntegrationPolicyBudgetIsSufficient(t *testing.T) {
	dsn := testDSN(t)
	ctx := context.Background()
	cfg := config.Store{DSN: dsn, BatchSize: 100, BatchEvery: 50 * time.Millisecond,
		RawRetention: "21 days", CompressAfter: "2 days"}
	s, err := New(ctx, cfg, integrationLog())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	defer s.pool.Close()

	if schemaSetupBudget <= 0 || policySetupBudget <= 0 {
		t.Fatal("both startup budgets must be positive")
	}
	// Run under exactly the reserved budget, as Store.New does, and measure.
	budgeted, cancel := context.WithTimeout(ctx, policySetupBudget)
	defer cancel()
	start := time.Now()
	if err := applyPolicies(budgeted, s.pool, integrationLog(), cfg); err != nil {
		t.Fatalf("applyPolicies did not complete within its documented %v budget: %v", policySetupBudget, err)
	}
	if took := time.Since(start); took > policySetupBudget/3 {
		t.Errorf("policy replacement took %v of its %v budget; the reservation is too tight to absorb "+
			"a loaded database", took, policySetupBudget)
	}
}
