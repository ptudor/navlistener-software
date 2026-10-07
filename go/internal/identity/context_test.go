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

// TestSignalPolicyIsCanonical pins the single canonical signal space: a grant or
// declaration spelled with the receiver's secondary tag ("2:1", Galileo E1-B as
// u-blox delivers it) is the same selector as the primary "2:0"; AllowsSignal
// admits the raw tag against the canonical grant; two spellings of one signal
// collapse to one entry; GLONASS L1OF and L2OF stay distinct bands.
func TestSignalPolicyIsCanonical(t *testing.T) {
	for _, tc := range []struct{ gnss, sig, want int }{
		{0, 4, 3}, {0, 7, 6}, {0, 0, 0}, {0, 3, 3},
		{2, 1, 0}, {2, 4, 3}, {2, 6, 5}, {2, 5, 5},
		{3, 1, 0}, {3, 3, 2}, {3, 6, 5}, {3, 7, 8}, {3, 8, 8},
		{5, 5, 4}, {5, 9, 8}, {5, 1, 1},
		{6, 0, 0}, {6, 2, 2}, {1, 0, 0}, {7, 0, 0},
	} {
		if got := CanonicalSigID(tc.gnss, tc.sig); got != tc.want {
			t.Errorf("CanonicalSigID(%d, %d) = %d, want %d", tc.gnss, tc.sig, got, tc.want)
		}
	}

	alias := NewPrivateContext("obs", CredentialToken)
	alias.Publication.Signals = []Signal{{GnssID: 2, SigID: 1}}
	alias.DeclaredCapabilities = []Signal{{GnssID: 2, SigID: 1}, {GnssID: 2, SigID: 0}, {GnssID: 6, SigID: 2}, {GnssID: 6, SigID: 0}}
	alias, err := alias.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	primary := NewPrivateContext("obs", CredentialToken)
	primary.Publication.Signals = []Signal{{GnssID: 2, SigID: 0}}
	primary.DeclaredCapabilities = []Signal{{GnssID: 6, SigID: 0}, {GnssID: 2, SigID: 0}, {GnssID: 6, SigID: 2}}
	primary, err = primary.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if !alias.AuthorizationEqual(primary) {
		t.Fatalf("alias and primary spellings normalized differently:\n%+v\n%+v", alias.Publication.Signals, primary.Publication.Signals)
	}
	wantDeclared := []Signal{{GnssID: 2, SigID: 0}, {GnssID: 6, SigID: 0}, {GnssID: 6, SigID: 2}}
	if !equalSignals(alias.DeclaredCapabilities, wantDeclared) {
		t.Fatalf("declared capabilities = %+v, want %+v (aliases collapsed, GLONASS bands kept)", alias.DeclaredCapabilities, wantDeclared)
	}
	// A literal repeat is still an authoring error.
	dup := NewPrivateContext("obs", CredentialToken)
	dup.DeclaredCapabilities = []Signal{{GnssID: 2, SigID: 1}, {GnssID: 2, SigID: 1}}
	if _, err := dup.Normalize(); err == nil {
		t.Fatal("literal duplicate declared capability accepted")
	}

	policy := primary.Publication
	if !policy.AllowsSignal(2, 1) || !policy.AllowsSignal(2, 0) {
		t.Fatal("canonical 2:0 grant refused a Galileo E1-B frame (raw sigId 1) or the primary tag")
	}
	if policy.AllowsSignal(2, 3) || policy.AllowsSignal(0, 0) {
		t.Fatal("2:0 grant admitted another signal")
	}
	glonass := PublicationPolicy{Signals: []Signal{{GnssID: 6, SigID: 0}}}
	if !glonass.AllowsSignal(6, 0) || glonass.AllowsSignal(6, 2) {
		t.Fatal("GLONASS L1OF grant must not admit L2OF")
	}
	l2c := PublicationPolicy{Signals: []Signal{{GnssID: 0, SigID: 3}}}
	if !l2c.AllowsSignal(0, 4) || l2c.AllowsSignal(0, 6) {
		t.Fatal("GPS L2C grant 0:3 must admit the L2 CM tag (0,4) and nothing else")
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
