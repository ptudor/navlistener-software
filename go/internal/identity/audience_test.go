package identity

import "testing"

func TestAudienceAllowsOnlyItsResolvedScope(t *testing.T) {
	c := NewPrivateContext("obs-1", CredentialToken)
	c.OrganizationID = "customer-a"
	c.CollectorInstanceID = "airport-f"
	c.CollectionIDs = []string{"roof", "community"}
	c.Publication.AggregateUse = AggregatePublicAnonymous

	checks := map[string]bool{
		"public":                  true,
		"operator:airport-f":      true,
		"operator:other":          false,
		"organization:customer-a": true,
		"organization:other":      false,
		"collection:community":    true,
		"collection:other":        false,
	}
	for raw, want := range checks {
		a, err := ParseAudience(raw)
		if err != nil {
			t.Fatalf("ParseAudience(%q): %v", raw, err)
		}
		if got := a.Allows(c); got != want {
			t.Errorf("%s Allows = %v, want %v", raw, got, want)
		}
	}
}

func TestParseAudienceRejectsFreeFormScope(t *testing.T) {
	for _, raw := range []string{"", "customer-a", "public:any", "organization:", "unknown:x", "organization:a/b"} {
		if _, err := ParseAudience(raw); err == nil {
			t.Errorf("ParseAudience(%q) accepted", raw)
		}
	}
}
