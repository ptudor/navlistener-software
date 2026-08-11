package audience

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

func TestRegistryBuildsPrivateViewsOnlyFromTrustedFrameScope(t *testing.T) {
	ctx := identity.NewPrivateContext("observer-a", identity.CredentialToken)
	ctx.OrganizationID = "customer-a"
	ctx.CollectionIDs = []string{"fleet-a"}
	registry := NewRegistry(1, []config.Source{{Name: "observer-a", ObserverContext: ctx}})
	registry.ApplyPrivate(&ingest.RawFrame{
		Recv: time.Now(), Source: "observer-a", Observer: ctx,
		RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 100}}},
	})

	for _, a := range []identity.Audience{{Kind: identity.AudienceOrganization, ID: "customer-a"}, {Kind: identity.AudienceCollection, ID: "fleet-a"}} {
		st, sources, ok := registry.Resolve(a)
		if !ok || st == nil || len(sources) != 1 {
			t.Fatalf("scoped view %s missing: store=%v sources=%d ok=%v", a.Key(), st, len(sources), ok)
		}
		if _, ok := st.FeedStationRF(time.Now())["observer-a"]; !ok {
			t.Fatalf("scoped view %s lacks admitted station state", a.Key())
		}
	}
	if _, _, ok := registry.Resolve(identity.Audience{Kind: identity.AudienceOrganization, ID: "customer-b"}); ok {
		t.Fatal("read lookup minted an unknown organization view")
	}
}

func TestRegistryDoesNotMintUnassignedOrganizationAudience(t *testing.T) {
	ctx := identity.NewPrivateContext("observer-a", identity.CredentialToken)
	registry := NewRegistry(1, nil)
	registry.ApplyPrivate(&ingest.RawFrame{Recv: time.Now(), Source: "observer-a", Observer: ctx, RF: &ingest.RawRF{}})
	if _, _, ok := registry.Resolve(identity.Audience{Kind: identity.AudienceOrganization, ID: identity.UnassignedOrganization}); ok {
		t.Fatal("unassigned migration bucket became a tenant audience")
	}
}

func TestRegistryPolicyResetIsConservativeAndAudienceScoped(t *testing.T) {
	ctxA := identity.NewPrivateContext("observer-a", identity.CredentialToken)
	ctxA.OrganizationID = "customer-a"
	ctxA.CollectionIDs = []string{"fleet-a"}
	ctxB := identity.NewPrivateContext("observer-b", identity.CredentialToken)
	ctxB.OrganizationID = "customer-b"
	registry := NewRegistry(1, nil)
	now := time.Now()
	registry.ApplyPrivate(&ingest.RawFrame{Recv: now, Source: "observer-a", Observer: ctxA, RF: &ingest.RawRF{Bands: []ingest.RFBand{{AGC: 100}}}})
	registry.ApplyPrivate(&ingest.RawFrame{Recv: now, Source: "observer-b", Observer: ctxB, RF: &ingest.RawRF{Bands: []ingest.RFBand{{AGC: 200}}}})
	audienceA := identity.Audience{Kind: identity.AudienceOrganization, ID: "customer-a"}
	audienceB := identity.Audience{Kind: identity.AudienceOrganization, ID: "customer-b"}
	storeA, _, _ := registry.Resolve(audienceA)
	before := storeA.Generation()
	affected := registry.ResetContext(ctxA)
	if len(affected) != 2 || affected[0].Key() != "collection:fleet-a" || affected[1] != audienceA {
		t.Fatalf("affected audiences = %+v", affected)
	}
	if storeA.Generation() != before+1 || len(storeA.FeedStationRF(now)) != 0 {
		t.Fatal("withdrawn organization retained materialized state")
	}
	storeB, _, _ := registry.Resolve(audienceB)
	if _, ok := storeB.FeedStationRF(now)["observer-b"]; !ok {
		t.Fatal("unrelated organization was reset")
	}
}
