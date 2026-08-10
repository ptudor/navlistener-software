package identity

import "testing"

func TestPrivateContextFailsClosed(t *testing.T) {
	c, err := NewPrivateContext("observer16", CredentialToken).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if c.OrganizationID != UnassignedOrganization || c.PublicEligible() || c.PublicAttributed() {
		t.Fatalf("private context widened unexpectedly: %+v", c)
	}
}

func TestNormalizeRejectsInvalidAndContradictoryPolicy(t *testing.T) {
	c := NewPrivateContext("observer16", CredentialToken)
	c.OrganizationID = "bad organization"
	if _, err := c.Normalize(); err == nil {
		t.Fatal("organization id with whitespace accepted")
	}

	c = NewPrivateContext("observer16", CredentialToken)
	c.Publication.AggregateUse = AggregatePublicAttributed
	if _, err := c.Normalize(); err == nil {
		t.Fatal("attributed public use without station metadata accepted")
	}
}

func TestPublicPolicyModes(t *testing.T) {
	anon := NewPrivateContext("observer16", CredentialToken)
	anon.Publication.AggregateUse = AggregatePublicAnonymous
	anon, err := anon.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !anon.PublicEligible() || anon.PublicAttributed() {
		t.Fatalf("anonymous policy classification wrong: %+v", anon)
	}

	attributed := anon
	attributed.Publication.AggregateUse = AggregatePublicAttributed
	attributed.Publication.StationMetadata = MetadataPseudonymous
	attributed, err = attributed.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !attributed.PublicEligible() || !attributed.PublicAttributed() {
		t.Fatalf("attributed policy classification wrong: %+v", attributed)
	}
}
