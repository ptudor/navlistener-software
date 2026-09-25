package authority_test

import (
	"crypto/x509"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/authority"
	"github.com/ptudor/navlistener/internal/testauthority"
)

func TestExactIssuingKeysAndPairings(t *testing.T) {
	ab, cd := testauthority.New(t, "navlisten", "ab"), testauthority.New(t, "customer", "ab", "cd")
	set, err := authority.New([]authority.Operational{ab.Config, cd.Config}, map[string]bool{"ab": true, "cd": true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*testauthority.Pair{ab, cd} {
		leaf, _, _ := p.Leaf(t, "board-0003-00112233445566778899aabbccddeeff")
		for _, issuer := range p.Issuers {
			chains := [][]*x509.Certificate{{leaf, issuer}}
			pin, err := set.MatchIssuer(chains, p.Config.ID)
			if err != nil || pin != authority.Fingerprint(issuer.RawSubjectPublicKeyInfo) {
				t.Fatalf("cross-signed issuer: %s %v", pin, err)
			}
			other := "navlisten"
			if p == ab {
				other = "customer"
			}
			if _, err := set.MatchIssuer(chains, other); err == nil {
				t.Fatal("cross-authority credential accepted")
			}
		}
	}
	if err := set.Allows("customer", "ab"); err != nil {
		t.Fatal(err)
	}
	if err := set.Allows("navlisten", "cd"); err == nil {
		t.Fatal("unregistered pairing accepted")
	}
	if err := set.Allows("device-claimed", ""); err == nil {
		t.Fatal("unknown authority accepted")
	}
	if _, err := set.MatchIssuer(nil, "navlisten"); err == nil {
		t.Fatal("unverified chain accepted")
	}
	if _, err := set.MatchIssuer([][]*x509.Certificate{{ab.Roots[0], ab.Roots[0]}}, "navlisten"); err == nil {
		t.Fatal("root substituted for issuing intermediate")
	}
	// Another project signed by the same approved roots is still unregistered.
	otherKey := testauthority.Key(t)
	other := testauthority.Cert(t, ab.Issuers[0], ab.Roots[0], &otherKey.PublicKey, ab.RootKeys[0])
	otherProject := *ab
	otherProject.IssuingKey, otherProject.Issuers[0] = otherKey, other
	leaf, _, _ := otherProject.Leaf(t, "board-0003-00112233445566778899aabbccddeeff")
	roots, intermediates := x509.NewCertPool(), x509.NewCertPool()
	roots.AddCert(ab.Roots[0])
	intermediates.AddCert(other)
	chains, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := set.MatchIssuer(chains, "navlisten"); err == nil {
		t.Fatal("different issuing key accepted under the same root")
	}
}

func TestAmbiguousAndDisabledAuthorities(t *testing.T) {
	p := testauthority.New(t, "one")
	duplicate := p.Config
	duplicate.ID = "two"
	if _, err := authority.New([]authority.Operational{p.Config, duplicate}, nil, time.Now()); err == nil {
		t.Fatal("same issuing key registered under two authorities")
	}
	p.Config.Enabled = false
	s, err := authority.New([]authority.Operational{p.Config}, nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if s.Allows("one", "") == nil {
		t.Fatal("disabled authority admitted")
	}
}
