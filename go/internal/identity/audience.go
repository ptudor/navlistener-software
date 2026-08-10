package identity

import (
	"fmt"
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
