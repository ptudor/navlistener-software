package identity

import "testing"

func TestReadPrincipalGrantsAreExplicitAndPrivate(t *testing.T) {
	p, err := NormalizeReadPrincipal(ReadPrincipal{
		ID: "viewer-a", Revision: "grant-v1",
		AudienceGrants: []Audience{{Kind: AudienceOrganization, ID: "customer-a"}, {Kind: AudienceCollection, ID: "fleet-a"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Allows(Audience{Kind: AudienceOrganization, ID: "customer-a"}) ||
		p.Allows(Audience{Kind: AudienceOrganization, ID: "customer-b"}) {
		t.Fatal("principal audience authorization mismatch")
	}
	if _, err := NormalizeReadPrincipal(ReadPrincipal{ID: "viewer-a", Revision: "grant-v1", AudienceGrants: []Audience{{Kind: AudiencePublic}}}); err == nil {
		t.Fatal("redundant public grant accepted as private authority")
	}
	changed := p
	changed.Revision = "grant-v2"
	if p.AuthorizationEqual(changed) {
		t.Fatal("read grant revision did not invalidate principal")
	}
}
