package audience

import (
	"sort"
	"sync"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
)

// maxDynamicViews is a hard safety ceiling over server-owned organization and
// collection identifiers. Normal deployments are orders of magnitude smaller;
// reaching it means broken or hostile control-plane data and fails closed for
// additional views instead of exhausting memory.
const maxDynamicViews = 10_000

type View struct {
	Audience identity.Audience
	Store    *state.Store
	Sources  []config.Source
}

// ResetContext invalidates every audience that could contain contributions
// admitted under previous. Whole-view reset is intentional: state is merged and
// cannot safely subtract one observer after freshest-value/aggregate selection.
func (r *Registry) ResetContext(previous identity.ObserverContext) []identity.Audience {
	previous, err := previous.Normalize()
	if err != nil {
		return nil
	}
	audiences := []identity.Audience{{Kind: identity.AudienceOperator, ID: previous.CollectorInstanceID}}
	if previous.OrganizationID != identity.UnassignedOrganization {
		audiences = append(audiences, identity.Audience{Kind: identity.AudienceOrganization, ID: previous.OrganizationID})
	}
	for _, id := range previous.CollectionIDs {
		audiences = append(audiences, identity.Audience{Kind: identity.AudienceCollection, ID: id})
	}
	if previous.PublicEligible() {
		audiences = append(audiences, identity.Audience{Kind: identity.AudiencePublic})
	}
	seen := make(map[string]bool, len(audiences))
	affected := make([]identity.Audience, 0, len(audiences))
	r.mu.RLock()
	for _, selected := range audiences {
		key := selected.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		if view, ok := r.views[key]; ok {
			view.Store.Reset()
			affected = append(affected, selected)
		}
	}
	r.mu.RUnlock()
	sort.Slice(affected, func(i, j int) bool { return affected[i].Key() < affected[j].Key() })
	return affected
}

// Registry owns physically separated live state for organization/collection
// audiences. Fixed public/operator stores may be registered by main; dynamic
// views are created only from trusted ObserverContext membership, never from a
// read request.
type Registry struct {
	mu      sync.RWMutex
	shards  int
	sources []config.Source
	views   map[string]View
	dynamic int
}

func NewRegistry(shards int, sources []config.Source) *Registry {
	if shards <= 0 {
		shards = 1
	}
	return &Registry{shards: shards, sources: append([]config.Source(nil), sources...), views: make(map[string]View)}
}

func (r *Registry) Register(a identity.Audience, st *state.Store, sources []config.Source) {
	if st == nil {
		return
	}
	r.mu.Lock()
	r.views[a.Key()] = View{Audience: a, Store: st, Sources: append([]config.Source(nil), sources...)}
	r.mu.Unlock()
}

// ApplyPrivate admits a frame to its owning organization and each explicit
// collection membership. Public policy is intentionally irrelevant here: these
// are authenticated private audiences. Unassigned/invalid observations do not
// mint a tenant view.
func (r *Registry) ApplyPrivate(f *ingest.RawFrame) {
	if f == nil {
		return
	}
	c := f.Observer
	if c.ObserverID == "" {
		c = identity.NewPrivateContext(f.Source, identity.CredentialLocalDial)
	}
	var err error
	c, err = c.Normalize()
	if err != nil {
		return
	}
	audiences := make([]identity.Audience, 0, 1+len(c.CollectionIDs))
	if c.OrganizationID != identity.UnassignedOrganization {
		audiences = append(audiences, identity.Audience{Kind: identity.AudienceOrganization, ID: c.OrganizationID})
	}
	for _, id := range c.CollectionIDs {
		audiences = append(audiences, identity.Audience{Kind: identity.AudienceCollection, ID: id})
	}
	for _, a := range audiences {
		view, ok := r.ensureDynamic(a)
		if ok {
			view.Store.Apply(f)
		}
	}
}

func (r *Registry) ensureDynamic(a identity.Audience) (View, bool) {
	key := a.Key()
	r.mu.RLock()
	view, ok := r.views[key]
	r.mu.RUnlock()
	if ok {
		return view, true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if view, ok = r.views[key]; ok {
		return view, true
	}
	if r.dynamic >= maxDynamicViews {
		return View{}, false
	}
	view = View{Audience: a, Store: state.New(r.shards), Sources: sourcesForAudience(r.sources, a)}
	r.views[key] = view
	r.dynamic++
	return view, true
}

func sourcesForAudience(sources []config.Source, a identity.Audience) []config.Source {
	out := make([]config.Source, 0, len(sources))
	for _, source := range sources {
		c := source.ObserverContext
		if c.ObserverID == "" {
			c = identity.NewPrivateContext(source.Name, identity.CredentialLocalDial)
		}
		c, err := c.Normalize()
		if err == nil && a.Allows(c) {
			out = append(out, source)
		}
	}
	return out
}

// Resolve returns only a view already created from trusted ingest context or
// explicitly registered by main. A client-supplied audience can never allocate
// state or establish that the organization/collection exists.
func (r *Registry) Resolve(a identity.Audience) (*state.Store, []config.Source, bool) {
	r.mu.RLock()
	view, ok := r.views[a.Key()]
	r.mu.RUnlock()
	if !ok {
		return nil, nil, false
	}
	return view.Store, append([]config.Source(nil), view.Sources...), true
}

// Views snapshots every currently materialized view for scoped detector ticks.
func (r *Registry) Views() []View {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]View, 0, len(r.views))
	for _, view := range r.views {
		view.Sources = append([]config.Source(nil), view.Sources...)
		out = append(out, view)
	}
	return out
}

// Audiences returns canonical keys for every trusted materialized view. It is
// used for scoped snapshots/detectors, never to authorize a read principal.
func (r *Registry) Audiences() []identity.Audience {
	r.mu.RLock()
	out := make([]identity.Audience, 0, len(r.views))
	for _, view := range r.views {
		out = append(out, view.Audience)
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}
