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
	attributed.Publication.StationMetadata = MetadataCoarse
	attributed, err = attributed.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !attributed.PublicEligible() || !attributed.PublicAttributed() {
		t.Fatalf("attributed policy classification wrong: %+v", attributed)
	}
}

func TestPublicEventsRequirePublicAggregate(t *testing.T) {
	c := NewPrivateContext("obs", CredentialToken)
	c.Publication.EventVisibility = EventsPublic
	if _, err := c.Normalize(); err == nil {
		t.Fatal("private aggregate accepted public event visibility")
	}

	c.Publication.AggregateUse = AggregatePublicAnonymous
	if got, err := c.Normalize(); err != nil || got.Publication.EventVisibility != EventsPublic {
		t.Fatalf("public event policy rejected: %+v, %v", got, err)
	}
}

func TestRawExportAndSignalPolicyValidation(t *testing.T) {
	c := NewPrivateContext("obs", CredentialHardwareMTLS)
	c.Publication.RawExport = RawExportNamedPeers
	if _, err := c.Normalize(); err == nil {
		t.Fatal("named_peers without a peer accepted")
	}
	c.Publication.FederationPeers = []string{"peer-b"}
	c.Publication.Signals = []Signal{{GnssID: 0, SigID: 0}, {GnssID: 2, SigID: 3}}
	c, err := c.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !c.Publication.NamesFederationPeer("peer-b") || c.Publication.NamesFederationPeer("peer-c") {
		t.Fatal("peer allow-list mismatch")
	}
	if !c.Publication.AllowsSignal(2, 3) || c.Publication.AllowsSignal(6, 0) {
		t.Fatal("signal allow-list mismatch")
	}
}

func TestCredentialFingerprintAndAuthorizationEquality(t *testing.T) {
	c := NewPrivateContext("obs", CredentialSoftwareMTLS)
	c.CredentialFingerprint = "ABC"
	if _, err := c.Normalize(); err == nil {
		t.Fatal("malformed certificate fingerprint accepted")
	}
	c.CredentialFingerprint = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	c, err := c.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !c.AuthorizationEqual(c) {
		t.Fatal("identical authorization contexts differ")
	}
	changed := c
	changed.Publication.Revision = "withdrawn-v2"
	if c.AuthorizationEqual(changed) {
		t.Fatal("policy revision change did not invalidate authorization")
	}
}

func TestReceiptEvidenceIsRequiredCanonicalAndAuthorizationRelevant(t *testing.T) {
	c := NewPrivateContext("obs", CredentialToken)
	c.FeedGrants = []string{"rtcm", "ubx"}
	c.DeclaredCapabilities = []Signal{{GnssID: 2, SigID: 3}, {GnssID: 0, SigID: 0}}
	c.CollectionIDs = []string{"fleet-b", "fleet-a"}
	c, err := c.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if c.FeedGrants[0] != "rtcm" || c.CollectionIDs[0] != "fleet-a" || c.DeclaredCapabilities[0] != (Signal{GnssID: 0, SigID: 0}) {
		t.Fatalf("receipt evidence was not canonicalized: %+v", c)
	}
	changed := c
	changed.FeedGrants = []string{"ubx"}
	if c.AuthorizationEqual(changed) {
		t.Fatal("feed grant change did not invalidate active authority")
	}
	c.FeedGrants = nil
	if _, err := c.Normalize(); err == nil {
		t.Fatal("context without retained feed authority was accepted")
	}
}

func TestSessionEvidenceIsValidatedAndNeverAuthorization(t *testing.T) {
	base, err := NewPrivateContext("00-04-a3-12-34-56-78-90", CredentialToken).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if base.HardwareTrust != HardwareTrustNone || base.ManufacturerAuthorityID != "" || base.CommissioningFingerprint != "" {
		t.Fatalf("resolved context carries evidence: %+v", base)
	}
	fingerprint := "aa" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcd"
	base.ManufacturerAuthorityID = "test-manufacturer"
	trusted, err := base.WithSessionEvidence(HardwareTrustTrusted, "test-manufacturer", fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if trusted.HardwareTrust != HardwareTrustTrusted || trusted.ManufacturerAuthorityID != "test-manufacturer" || trusted.CommissioningFingerprint != fingerprint {
		t.Fatalf("session evidence = %+v", trusted)
	}
	if base.HardwareTrust != HardwareTrustNone {
		t.Fatal("stamping a session changed the resolved context it was copied from")
	}
	// A periodic recheck resolves no evidence; it must still equal the admitted
	// session, and two sessions with different evidence share one policy.
	if !trusted.AuthorizationEqual(base) || !base.AuthorizationEqual(trusted) {
		t.Fatal("session evidence changed authorization equality")
	}

	for name, c := range map[string]struct {
		trust       HardwareTrust
		authority   string
		fingerprint string
	}{
		"unknown trust":                {"verified", "test-manufacturer", fingerprint},
		"trust without an authority":   {HardwareTrustOpen, "", fingerprint},
		"trust without a record":       {HardwareTrustOpen, "test-manufacturer", ""},
		"record that proved nothing":   {HardwareTrustNone, "", fingerprint},
		"different enrolled authority": {HardwareTrustNone, "other-manufacturer", ""},
		"invalid authority":            {HardwareTrustTest, "not valid!", fingerprint},
		"uppercase fingerprint":        {HardwareTrustTest, "test-manufacturer", "AA" + fingerprint[2:]},
		"short fingerprint":            {HardwareTrustTest, "test-manufacturer", fingerprint[:32]},
		"empty trust with a record":    {"", "test-manufacturer", fingerprint},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := base.WithSessionEvidence(c.trust, c.authority, c.fingerprint); err == nil {
				t.Fatal("malformed session evidence accepted")
			}
		})
	}
}
