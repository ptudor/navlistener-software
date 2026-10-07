package ingest

import (
	"context"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
)

// Optional database capability: recheck a previously authenticated credential
// digest without retaining plaintext tokens or requiring a new device session.
// The error distinguishes a check the control plane could not answer (a
// transport failure, a timeout, a lookup that raced an invalidation), which
// leaves the contributor's policy as it is, from an authoritative verdict:
// ok=false with a nil error is the control plane's own denial.
type observerReconciler interface {
	ReconcileObserver(context.Context, string, string, string) (identity.ObserverContext, bool, error)
}

// reconciliationBudget is one whole-sweep deadline, never N independent
// five-second stalls. A task the budget cuts off is skipped this sweep, not
// withdrawn, and is first in line for the next one.
const reconciliationBudget = 5 * time.Second

// maxReconcileTasksPerSweep caps the control-plane queries one sweep issues;
// the rest wait for later sweeps, least recently verified first. It keeps a
// sweep well inside reconciliationBudget on a healthy control plane, and it
// bounds the standing load a long-retired fleet (1024 policies × 8
// credentials) puts on the database to this many queries per recheck interval
// instead of all of them.
const maxReconcileTasksPerSweep = 1024

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
		verifiedAt time.Time
	}
	now := time.Now()
	p.authorizationMu.Lock()
	policies := make([]*observerPolicy, 0, len(p.policies))
	for _, policy := range p.policies {
		policies = append(policies, policy)
	}
	p.authorizationMu.Unlock()
	var tasks []retained
	for _, policy := range policies {
		policy.mu.Lock()
		// A policy the control plane confirmed within the authority TTL — at
		// an admission, by its live sessions' rechecks, or in an earlier sweep
		// — is not queried again: the sweep's value is the contributors nothing
		// else is checking, and a withdrawal is still seen within one TTL.
		if now.Sub(policy.verifiedAt) < p.authorityTTL {
			policy.mu.Unlock()
			continue
		}
		for credential := range policy.credentials {
			tasks = append(tasks, retained{&Admission{policy: policy, generation: policy.generation.Load()}, policy.current, credential, policy.verifiedAt})
		}
		policy.mu.Unlock()
	}
	// Least recently verified first, so a fleet larger than one sweep's cap is
	// covered round-robin across sweeps rather than the same policies each time.
	sort.SliceStable(tasks, func(i, j int) bool { return tasks[i].verifiedAt.Before(tasks[j].verifiedAt) })
	if len(tasks) > maxReconcileTasksPerSweep {
		tasks = tasks[:maxReconcileTasksPerSweep]
	}
	checkCtx, cancel := context.WithTimeout(ctx, p.reconcileBudget)
	defer cancel()
	jobs := make(chan retained, len(tasks))
	withdraw := make(chan *Admission, len(tasks))
	verified := make(chan *Admission, len(tasks))
	for _, task := range tasks {
		jobs <- task
	}
	close(jobs)
	var unverified atomic.Int64
	var workers sync.WaitGroup
	for range min(16, len(tasks)) {
		workers.Go(func() {
			for task := range jobs {
				if checkCtx.Err() != nil {
					// The sweep budget is spent. Nothing was learned about this
					// contributor, so nothing changes for it; it is first in
					// line next sweep.
					unverified.Add(1)
					continue
				}
				current, allowed, err := r.ReconcileObserver(checkCtx, task.credential.digest, task.previous.ObserverID, task.credential.feed)
				if err != nil || checkCtx.Err() != nil {
					unverified.Add(1)
					continue
				}
				// Reapply the original session's already-verified transport proof.
				// It can never elevate a new credential or accept a changed leaf.
				if current.CredentialTier == identity.CredentialToken && task.previous.CredentialTier == identity.CredentialSoftwareMTLS && current.CredentialFingerprint == "" {
					current.CredentialTier = task.previous.CredentialTier
					current.CredentialFingerprint = task.previous.CredentialFingerprint
				}
				if !allowed || !task.previous.AuthorizationEqual(current) {
					withdraw <- task.admission
					continue
				}
				verified <- task.admission
			}
		})
	}
	workers.Wait()
	close(withdraw)
	close(verified)
	if n := unverified.Load(); n > 0 {
		metrics.PushAuthorizationUnavailableTotal.WithLabelValues("sweep").Add(float64(n))
		p.log.Warn("reconciliation sweep could not verify retained contributors; their policies stand until a later sweep answers",
			"unverified", n, "swept", len(tasks))
	}
	for admission := range verified {
		admission.verified(now)
	}
	for admission := range withdraw {
		if ctx.Err() != nil {
			return
		}
		// Uses the same generation and ordered reset protocol as live sessions.
		// Multiple credential failures and late sweeps cannot repeat a transition.
		p.changeAdmission(ctx, admission, identity.ObserverContext{})
	}
}
