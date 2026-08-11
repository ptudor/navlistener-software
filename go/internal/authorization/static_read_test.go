package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/ptudor/navlistener/internal/identity"
)

func TestStaticReadAuthorizerRetainsNoPlaintextToken(t *testing.T) {
	sum := sha256.Sum256([]byte("secret"))
	a, err := NewStaticReadAuthorizer([]StaticReadGrant{{
		TokenSHA256: hex.EncodeToString(sum[:]),
		Principal:   identity.ReadPrincipal{ID: "viewer-a", Revision: "grant-v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceCollection, ID: "fleet-a"}}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	principal, ok := a.AuthorizeRead(context.Background(), "secret")
	if !ok || !principal.Allows(identity.Audience{Kind: identity.AudienceCollection, ID: "fleet-a"}) {
		t.Fatalf("static read grant rejected: %+v/%v", principal, ok)
	}
	if _, ok := a.byHash["secret"]; ok {
		t.Fatal("plaintext token retained as static map key")
	}
}
