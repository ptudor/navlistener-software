package audience

import (
	"fmt"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// regression fix (observability half): ApplyPrivate discarded ensureDynamic's
// refusal, so a collector at the dynamic-view ceiling silently stopped
// projecting valid private state — indistinguishable, from outside, from a
// tenant that simply had no observations. The refusal must be counted and
// logged, and the materialized count must be visible before the ceiling is hit.
func TestDynamicViewCeilingIsSurfaced(t *testing.T) {
	r := NewRegistry(1, nil)
	frameFor := func(org string) *ingest.RawFrame {
		c := identity.NewPrivateContext("obs1", identity.CredentialToken)
		c.OrganizationID = org
		return &ingest.RawFrame{Source: "obs1", Observer: c}
	}

	before := testutil.ToFloat64(metrics.AudienceViewsRefusedTotal)
	// Fill to the ceiling with distinct authorized organizations.
	for i := 0; i < maxDynamicViews; i++ {
		r.ApplyPrivate(frameFor(fmt.Sprintf("org-%d", i)))
	}
	if got := testutil.ToFloat64(metrics.AudienceViewsRefusedTotal) - before; got != 0 {
		t.Fatalf("refused %v views while still under the ceiling", got)
	}
	if got := testutil.ToFloat64(metrics.AudienceViewsMaterialized); got != float64(maxDynamicViews) {
		t.Errorf("materialized gauge = %v, want %d", got, maxDynamicViews)
	}

	// One past the ceiling: the observation is dropped, but never silently.
	r.ApplyPrivate(frameFor("org-over-the-ceiling"))
	if got := testutil.ToFloat64(metrics.AudienceViewsRefusedTotal) - before; got != 1 {
		t.Fatalf("ceiling refusal counted %v times, want 1", got)
	}
	if _, _, ok := r.Resolve(identity.Audience{
		Kind: identity.AudienceOrganization, ID: "org-over-the-ceiling"}); ok {
		t.Error("a refused audience was materialized anyway")
	}

	// An already-materialized audience keeps working at the ceiling.
	r.ApplyPrivate(frameFor("org-0"))
	if got := testutil.ToFloat64(metrics.AudienceViewsRefusedTotal) - before; got != 1 {
		t.Errorf("an existing view was refused at the ceiling (count %v)", got)
	}
}

// A read request must never be able to allocate a view or reveal that a tenant
// exists — the ceiling work must not have weakened that.
func TestResolveNeverAllocatesAView(t *testing.T) {
	r := NewRegistry(1, nil)
	a := identity.Audience{Kind: identity.AudienceOrganization, ID: "never-seen"}
	// The gauge is process-global, so compare a delta rather than an absolute.
	before := testutil.ToFloat64(metrics.AudienceViewsMaterialized)
	if _, _, ok := r.Resolve(a); ok {
		t.Fatal("Resolve materialized a view for an unseen audience")
	}
	if got := testutil.ToFloat64(metrics.AudienceViewsMaterialized) - before; got != 0 {
		t.Errorf("materialized gauge moved by %v after a read-only Resolve, want 0", got)
	}
}
