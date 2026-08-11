package authorization

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

func testContext() identity.ObserverContext {
	c := identity.NewPrivateContext("observer-a", identity.CredentialHardwareMTLS)
	c.OrganizationID = "customer-a"
	c.EnrollmentID = "enrollment-a"
	c.CollectorInstanceID = "collector-a"
	c.CollectionIDs = []string{"fleet-a"}
	c.FeedGrants = []string{"ubx", "rtcm"}
	c.DeclaredCapabilities = []identity.Signal{{GnssID: 0, SigID: 0}}
	c.CredentialFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	c.AttestationTier = identity.AttestationVerifiedV2
	c.Publication.Revision = "policy-v1"
	return c
}

func TestObserverAuthorizationCachesDigestAndInvalidates(t *testing.T) {
	var calls int
	p := newProvider(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(_ context.Context, digest, station, feed string) (identity.ObserverContext, bool, error) {
			calls++
			if digest == "secret" {
				t.Fatal("plaintext token reached lookup")
			}
			if station != "observer-a" || feed != "ubx" {
				return identity.ObserverContext{}, false, nil
			}
			return testContext(), true, nil
		})
	now := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }

	first, ok := p.Authenticate(context.Background(), "secret", "observer-a", "ubx")
	if !ok || first.OrganizationID != "customer-a" || calls != 1 {
		t.Fatalf("first authorization = %+v/%v calls=%d", first, ok, calls)
	}
	first.CollectionIDs[0] = "mutated"
	first.FeedGrants[0] = "mutated"
	first.DeclaredCapabilities[0].SigID = 99
	second, ok := p.Authenticate(context.Background(), "secret", "observer-a", "ubx")
	if !ok || second.CollectionIDs[0] != "fleet-a" || second.FeedGrants[0] != "rtcm" ||
		second.DeclaredCapabilities[0].SigID != 0 || calls != 1 {
		t.Fatalf("cache was not defensive: %+v/%v calls=%d", second, ok, calls)
	}
	p.InvalidateAll()
	if _, ok := p.Authenticate(context.Background(), "secret", "observer-a", "ubx"); !ok || calls != 2 {
		t.Fatalf("invalidation did not force lookup, calls=%d", calls)
	}
}

func TestObserverAuthorizationFailsClosedAndDoesNotCacheDatabaseErrors(t *testing.T) {
	var calls int
	p := newProvider(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
			calls++
			return identity.ObserverContext{}, false, errors.New("database unavailable")
		})
	for range 2 {
		if got, ok := p.Authenticate(context.Background(), "secret", "observer-a", "ubx"); ok || got.ObserverID != "" {
			t.Fatalf("database error authorized observer: %+v/%v", got, ok)
		}
	}
	if calls != 2 {
		t.Fatalf("transient database errors were cached, calls=%d", calls)
	}
}

func TestObserverAuthorizationRejectsMalformedControlPlaneContext(t *testing.T) {
	p := newProvider(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
			c := testContext()
			c.Publication.AggregateUse = "everyone"
			return c, true, nil
		})
	if _, ok := p.Authenticate(context.Background(), "secret", "observer-a", "ubx"); ok {
		t.Fatal("malformed control-plane policy authorized observer")
	}
}

func TestInvalidationRacingLookupCannotRepopulateStaleAuthority(t *testing.T) {
	var p *Provider
	var calls int
	p = newProvider(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)),
		func(context.Context, string, string, string) (identity.ObserverContext, bool, error) {
			calls++
			if calls == 1 {
				p.InvalidateAll()
			}
			return testContext(), true, nil
		})
	if _, ok := p.Authenticate(context.Background(), "secret", "observer-a", "ubx"); ok {
		t.Fatal("lookup racing invalidation returned stale authority")
	}
	if _, ok := p.Authenticate(context.Background(), "secret", "observer-a", "ubx"); !ok || calls != 2 {
		t.Fatalf("fresh post-invalidation lookup failed, calls=%d", calls)
	}
}

func TestReadAuthorizationIsDigestCachedAndExplicit(t *testing.T) {
	var calls int
	p := newProvider(time.Minute, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	p.lookupRead = func(_ context.Context, digest string) (identity.ReadPrincipal, bool, error) {
		calls++
		if digest == "secret" {
			t.Fatal("plaintext read token reached lookup")
		}
		return identity.ReadPrincipal{ID: "viewer-a", Revision: "grant-v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOrganization, ID: "customer-a"}}}, true, nil
	}
	p.now = func() time.Time { return time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC) }
	first, ok := p.AuthorizeRead(context.Background(), "secret")
	if !ok || !first.Allows(identity.Audience{Kind: identity.AudienceOrganization, ID: "customer-a"}) || calls != 1 {
		t.Fatalf("read authorization = %+v/%v calls=%d", first, ok, calls)
	}
	first.AudienceGrants[0].ID = "mutated"
	second, ok := p.AuthorizeRead(context.Background(), "secret")
	if !ok || second.AudienceGrants[0].ID != "customer-a" || calls != 1 {
		t.Fatalf("read cache was not defensive: %+v/%v calls=%d", second, ok, calls)
	}
	p.InvalidateAll()
	if _, ok := p.AuthorizeRead(context.Background(), "secret"); !ok || calls != 2 {
		t.Fatalf("read invalidation did not force lookup, calls=%d", calls)
	}
}
