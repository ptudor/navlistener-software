// Package audience projects trusted ingest frames into isolated authorization
// views before aggregation. Redacting a single global state at JSON time is not
// sufficient: private receivers would already have changed confidence, chosen
// ephemerides, counters, detector transitions, and timing.
package audience

import (
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

// AnonymousPublicSource is the deliberately non-station identity used inside
// the public state for every anonymous contributor. Collapsing them produces a
// conservative one-bucket confidence/receiver contribution and prevents source
// enumeration or differencing. It is filtered from the observer surface.
const AnonymousPublicSource = "__navlistener_public_anonymous__"

// IsAnonymousPublicSource reports whether id is the internal anonymous bucket.
func IsAnonymousPublicSource(id string) bool { return id == AnonymousPublicSource }

// ProjectPublic returns the frame that may enter public state. The returned
// frame is a shallow copy; the authenticated input is never mutated.
//
// Private/invalid context is rejected. Anonymous observations may affect
// satellite aggregate state but lose station telemetry, raw observables, and
// their source identity. Attributed public observations retain the canonical
// observer id and per-SV observations. RF security telemetry stays operator-only
// at every currently defined station-metadata tier.
func ProjectPublic(f *ingest.RawFrame) (*ingest.RawFrame, bool) {
	if f == nil {
		return nil, false
	}
	c := f.Observer
	if c.ObserverID == "" {
		c = identity.NewPrivateContext(f.Source, identity.CredentialLocalDial)
	}
	c, err := c.Normalize()
	if err != nil || !c.PublicEligible() {
		return nil, false
	}

	out := *f
	out.Observer = c
	out.RF = nil // no current policy grant exposes RF/security telemetry publicly
	switch c.Publication.AggregateUse {
	case identity.AggregatePublicAnonymous:
		out.Source = AnonymousPublicSource
		out.Obs = nil // perrecv/measured geometry identifies the station
	case identity.AggregatePublicAttributed:
		// Normalize already requires coarse/full. Both are deliberately locatable
		// product tiers; coarse affects future displayed coordinates, not geometry.
		out.Source = c.ObserverID
	default:
		return nil, false
	}
	return &out, true
}

// ProjectPublicEvents returns the contribution allowed to influence public
// detector state. Event permission is independent from public feed permission:
// EventsPrivate is rejected, PublicRedacted collapses source evidence into the
// anonymous bucket, and EventsPublic follows the ordinary public projection.
func ProjectPublicEvents(f *ingest.RawFrame) (*ingest.RawFrame, bool) {
	out, ok := ProjectPublic(f)
	if !ok {
		return nil, false
	}
	switch out.Observer.Publication.EventVisibility {
	case identity.EventsPrivate:
		return nil, false
	case identity.EventsPublicRedacted:
		out.Source = AnonymousPublicSource
		out.Obs = nil
	case identity.EventsPublic:
		// ProjectPublic already applied the aggregate attribution policy.
	default:
		return nil, false
	}
	return out, true
}

// PublicSources projects configured dial-source presentation metadata for the
// public observer feed. Anonymous/private sources have no row. Coarse stations
// omit the free-form site remark; full stations may publish it. Internal dial
// addresses are never copied by the serve layer regardless.
func PublicSources(sources []config.Source) []config.Source {
	out := make([]config.Source, 0, len(sources))
	for _, src := range sources {
		c := src.ObserverContext
		if c.ObserverID == "" {
			c = identity.NewPrivateContext(src.Name, identity.CredentialLocalDial)
		}
		c, err := c.Normalize()
		if err != nil || !c.PublicAttributed() {
			continue
		}
		projected := src
		projected.Name = c.ObserverID
		projected.ObserverContext = c
		if c.Publication.StationMetadata != identity.MetadataFull {
			projected.Remark = ""
		}
		out = append(out, projected)
	}
	return out
}
