package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ptudor/navlistener/internal/identity"
)

// StaticReadGrant is the already-validated bootstrap form used by standalone
// collectors without the shared control plane.
type StaticReadGrant struct {
	TokenSHA256 string
	Principal   identity.ReadPrincipal
}

type StaticReadAuthorizer struct {
	byHash map[string]identity.ReadPrincipal
}

func NewStaticReadAuthorizer(grants []StaticReadGrant) (*StaticReadAuthorizer, error) {
	byHash := make(map[string]identity.ReadPrincipal, len(grants))
	for i, grant := range grants {
		digest := strings.ToLower(grant.TokenSHA256)
		decoded, err := hex.DecodeString(digest)
		if err != nil || len(decoded) != sha256.Size {
			return nil, fmt.Errorf("static read grant %d token digest is not SHA-256 hex", i)
		}
		if _, exists := byHash[digest]; exists {
			return nil, fmt.Errorf("static read grant %d duplicates a token digest", i)
		}
		principal, err := identity.NormalizeReadPrincipal(grant.Principal)
		if err != nil {
			return nil, fmt.Errorf("static read grant %d: %w", i, err)
		}
		byHash[digest] = cloneReadPrincipal(principal)
	}
	return &StaticReadAuthorizer{byHash: byHash}, nil
}

func (a *StaticReadAuthorizer) AuthorizeRead(_ context.Context, token string) (identity.ReadPrincipal, bool) {
	sum := sha256.Sum256([]byte(token))
	principal, ok := a.byHash[hex.EncodeToString(sum[:])]
	return cloneReadPrincipal(principal), ok
}
