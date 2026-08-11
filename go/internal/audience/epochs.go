package audience

import (
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

type policyEpoch struct {
	generation uint64
	visibleAt  time.Time
}

// PolicyEpochs is the in-process current-policy intersection for derived
// history and pending event publication. Its conservative startup boundary
// means this collector never serves event rows from an earlier process whose
// source policy cannot be reconstructed from the aggregate event alone.
type PolicyEpochs struct {
	mu      sync.RWMutex
	started time.Time
	epochs  map[string]policyEpoch
}

func NewPolicyEpochs(started time.Time) *PolicyEpochs {
	if started.IsZero() {
		started = time.Now()
	}
	return &PolicyEpochs{started: started.UTC(), epochs: make(map[string]policyEpoch)}
}

func (p *PolicyEpochs) Current(audience string) (generation uint64, visibleAt time.Time) {
	p.mu.RLock()
	epoch, ok := p.epochs[audience]
	started := p.started
	p.mu.RUnlock()
	if !ok {
		return 0, started
	}
	return epoch.generation, epoch.visibleAt
}

// Advance makes every pre-transition derived event invisible and invalidates
// pending publication generations. It returns canonical keys actually changed.
func (p *PolicyEpochs) Advance(audiences []identity.Audience, at time.Time) []string {
	if at.IsZero() {
		at = time.Now()
	}
	at = at.UTC()
	seen := make(map[string]bool, len(audiences))
	changed := make([]string, 0, len(audiences))
	p.mu.Lock()
	for _, selected := range audiences {
		key := selected.Key()
		if seen[key] {
			continue
		}
		seen[key] = true
		epoch, exists := p.epochs[key]
		if !exists {
			epoch.visibleAt = p.started
		}
		epoch.generation++
		if epoch.visibleAt.Before(at) {
			epoch.visibleAt = at
		}
		p.epochs[key] = epoch
		changed = append(changed, key)
	}
	p.mu.Unlock()
	return changed
}
