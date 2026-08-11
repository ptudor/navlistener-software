package identity

import (
	"fmt"
	"sort"
	"strings"
)

// AudienceKind names an authorization boundary whose live state must be built
// only from observations admitted to that boundary. It is not a client-supplied
// organization filter: a read-side principal must first be granted the Audience.
type AudienceKind string

const (
	AudiencePublic       AudienceKind = "public"
	AudienceOperator     AudienceKind = "operator"
	AudienceOrganization AudienceKind = "organization"
	AudienceCollection   AudienceKind = "collection"
)

// Audience is a normalized cache/state authorization key. Public has no ID;
// every other kind is scoped to a trusted server-side identifier.
type Audience struct {
	Kind AudienceKind
	ID   string
}

// ReadPrincipal is the server-resolved identity behind a read bearer token.
// AudienceGrants are authorization output, never trusted request claims.
type ReadPrincipal struct {
	ID             string
	AudienceGrants []Audience
	Revision       string
}

// NormalizeReadPrincipal validates a control-plane/config principal and rejects
// redundant public grants. Public is always discoverable without a credential;
// every stored grant is therefore a private authorization boundary.
func NormalizeReadPrincipal(p ReadPrincipal) (ReadPrincipal, error) {
	p.AudienceGrants = append([]Audience(nil), p.AudienceGrants...)
	if !ValidScopeID(p.ID) {
		return p, fmt.Errorf("read principal id %q is invalid", p.ID)
	}
	if !ValidScopeID(p.Revision) {
		return p, fmt.Errorf("read principal revision %q is invalid", p.Revision)
	}
	if len(p.AudienceGrants) == 0 {
		return p, fmt.Errorf("read principal requires at least one private audience grant")
	}
	seen := make(map[string]bool, len(p.AudienceGrants))
	for _, grant := range p.AudienceGrants {
		key := grant.Key()
		parsed, err := ParseAudience(key)
		if err != nil || parsed != grant || grant.Kind == AudiencePublic {
			return p, fmt.Errorf("read audience grant %q is invalid or public", key)
		}
		if seen[key] {
			return p, fmt.Errorf("read audience grant %q is duplicated", key)
		}
		seen[key] = true
	}
	sort.Slice(p.AudienceGrants, func(i, j int) bool {
		return p.AudienceGrants[i].Key() < p.AudienceGrants[j].Key()
	})
	return p, nil
}

// Allows reports whether the principal was explicitly granted a private
// audience. Public requests do not need this method or a principal.
func (p ReadPrincipal) Allows(a Audience) bool {
	if a.Kind == AudiencePublic {
		return true
	}
	for _, grant := range p.AudienceGrants {
		if grant == a {
			return true
		}
	}
	return false
}

func (p ReadPrincipal) AuthorizationEqual(other ReadPrincipal) bool {
	if p.ID != other.ID || p.Revision != other.Revision || len(p.AudienceGrants) != len(other.AudienceGrants) {
		return false
	}
	for i := range p.AudienceGrants {
		if p.AudienceGrants[i] != other.AudienceGrants[i] {
			return false
		}
	}
	return true
}

// ParseAudience parses the canonical public or kind:id representation. Parsing
// validates syntax only; read-side authentication must separately grant it.
func ParseAudience(s string) (Audience, error) {
	if s == "public" {
		return Audience{Kind: AudiencePublic}, nil
	}
	kind, id, ok := strings.Cut(s, ":")
	if !ok || !ValidScopeID(id) {
		return Audience{}, fmt.Errorf("audience %q: want public or operator|organization|collection:<id>", s)
	}
	a := Audience{Kind: AudienceKind(kind), ID: id}
	switch a.Kind {
	case AudienceOperator, AudienceOrganization, AudienceCollection:
		return a, nil
	default:
		return Audience{}, fmt.Errorf("audience kind %q is invalid", kind)
	}
}

// Key returns the canonical cache/database representation.
func (a Audience) Key() string {
	if a.Kind == AudiencePublic {
		return "public"
	}
	return string(a.Kind) + ":" + a.ID
}

// Allows reports whether a resolved observation may contribute to this
// audience. It is deliberately fail-closed for zero/unknown audience kinds.
func (a Audience) Allows(c ObserverContext) bool {
	switch a.Kind {
	case AudiencePublic:
		return c.PublicEligible()
	case AudienceOperator:
		return a.ID != "" && c.CollectorInstanceID == a.ID
	case AudienceOrganization:
		return a.ID != "" && c.OrganizationID == a.ID
	case AudienceCollection:
		for _, id := range c.CollectionIDs {
			if id == a.ID {
				return true
			}
		}
	}
	return false
}
