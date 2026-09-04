package ingest

import (
	"context"
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// Optional database capability: recheck a previously authenticated credential
// digest without retaining plaintext tokens or requiring a new device session.
type observerReconciler interface {
	ReconcileObserver(context.Context, string, string, string) (identity.ObserverContext, bool)
}

const reconciliationBudget = 5 * time.Second

func (p *PushServer) runReconciliation(ctx context.Context) {
	r, ok := p.auth.(observerReconciler)
	if !ok || p.reauthorizeEvery <= 0 {
		return
	}
	ticker := time.NewTicker(p.reauthorizeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.reconcileOnce(ctx, r)
		}
	}
}

func (p *PushServer) reconcileOnce(ctx context.Context, r observerReconciler) {
	type retained struct {
		admission  *Admission
		previous   identity.ObserverContext
		credential policyCredential
	}
	p.authorizationMu.Lock()
	policies := make([]*observerPolicy, 0, len(p.policies))
	for _, policy := range p.policies {
		policies = append(policies, policy)
	}
	p.authorizationMu.Unlock()
	var tasks []retained
	for _, policy := range policies {
		policy.mu.Lock()
		for credential := range policy.credentials {
			tasks = append(tasks, retained{&Admission{policy: policy, generation: policy.generation.Load()}, policy.current, credential})
		}
		policy.mu.Unlock()
	}
	// One whole-sweep deadline, never N independent five-second stalls. Entries
	// that cannot be reconciled within it lose derived visibility fail closed.
	checkCtx, cancel := context.WithTimeout(ctx, reconciliationBudget)
	defer cancel()
	jobs := make(chan retained, len(tasks))
	withdraw := make(chan *Admission, len(tasks))
	for _, task := range tasks {
		jobs <- task
	}
	close(jobs)
	var workers sync.WaitGroup
	for range min(16, len(tasks)) {
		workers.Go(func() {
			for task := range jobs {
				var current identity.ObserverContext
				var allowed bool
				if checkCtx.Err() == nil {
					current, allowed = r.ReconcileObserver(checkCtx, task.credential.digest, task.previous.ObserverID, task.credential.feed)
				}
				// Reapply the original session's already-verified transport proof.
				// It can never elevate a new credential or accept a changed leaf.
				if current.CredentialTier == identity.CredentialToken && task.previous.CredentialTier == identity.CredentialSoftwareMTLS && current.CredentialFingerprint == "" {
					current.CredentialTier = task.previous.CredentialTier
					current.CredentialFingerprint = task.previous.CredentialFingerprint
				}
				if !allowed || checkCtx.Err() != nil || !task.previous.AuthorizationEqual(current) {
					withdraw <- task.admission
				}
			}
		})
	}
	workers.Wait()
	close(withdraw)
	for admission := range withdraw {
		if ctx.Err() != nil {
			return
		}
		// Uses the same generation and ordered reset protocol as live sessions.
		// Multiple credential failures and late sweeps cannot repeat a transition.
		p.changeAdmission(ctx, admission, identity.ObserverContext{})
	}
}
