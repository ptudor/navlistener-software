package ingest

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

func TestDurableOutageAndChurnBounds(t *testing.T) {
	tr := NewDurableTracker()
	tr.maxSessions = 16
	tr.maxPerSession = 64
	tr.maxOutstanding = 512
	// Four hours at 100 attempted frames/sec, no persistence at all. Include
	// sparse/nonmonotone sequences and adversarial per-frame session churn.
	for tick := 0; tick < 4*3600*100; tick++ {
		sess := fmt.Sprint(tick % 8)
		tr.Received("offline", sess, uint64(tick/8)*3+1, true)
		if tick%100 == 0 {
			tr.Received("churn", fmt.Sprint(tick), uint64(tick)+1, true)
		}
		if tick%1000 == 0 {
			if tr.outstanding > 512 || len(tr.m) > 16 {
				t.Fatal("budget exceeded")
			}
			for _, s := range tr.m {
				if len(s.pending) > 64 || len(s.outstanding) != len(s.pending) {
					t.Fatal("per-session budget exceeded")
				}
			}
		}
	}
	// Silence inside the abandonment window cannot discard the only record of
	// an unresolved hole (a live feeder replays well within it).
	tr.mu.Lock()
	for _, s := range tr.m {
		s.touched = time.Now().Add(-durableHoleAbandonAfter / 2)
	}
	tr.pruneLocked(time.Now())
	tr.mu.Unlock()
	if tr.outstanding != 512 {
		t.Fatalf("forgot holes: %d", tr.outstanding)
	}
	// Unaffected ACK queries stay prompt while other producers pound full budgets.
	tr.Received("healthy", "session", 100, false)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				tr.Received("offline", "0", 999999, true)
			}
		}
	}()
	start := time.Now()
	for i := 0; i < 10000; i++ {
		if got := tr.Watermark("healthy", "session"); got != 100 {
			t.Fatalf("healthy watermark %d", got)
		}
	}
	close(stop)
	wg.Wait()
	if time.Since(start) > 2*time.Second {
		t.Fatal("unrelated ACK lookup stalled")
	}
	// Already-tracked replay remains admissible at capacity. Commit in arbitrary
	// order and verify the watermark against the actual remaining minimum.
	for key, s := range tr.m {
		original := append([]uint64(nil), s.pending...)
		rand.New(rand.NewSource(6)).Shuffle(len(original), func(i, j int) { original[i], original[j] = original[j], original[i] })
		for _, seq := range original {
			if !tr.Received(key.source, key.session, seq, true) {
				t.Fatal("full budget rejected tracked replay")
			}
			tr.Resolved(key.source, key.session, seq)
			want := s.highest
			for n := range s.outstanding {
				if n-1 < want {
					want = n - 1
				}
			}
			if got := tr.Watermark(key.source, key.session); got != want {
				t.Fatalf("watermark=%d, want %d", got, want)
			}
		}
	}
	if tr.outstanding != 0 {
		t.Fatal("commits did not release budget")
	}
	if !tr.Received("offline", "0", 999999, true) {
		t.Fatal("recovered persistence cannot admit new data")
	}
}

func TestDurableSessionChurnAndSparseReplay(t *testing.T) {
	tr := NewDurableTracker()
	tr.maxSessions = 4
	for i := 0; i < 4; i++ {
		if !tr.Received("obs", fmt.Sprint(i), uint64(i+1)*100, true) {
			t.Fatal("initial admission")
		}
	}
	for i := 4; i < 10000; i++ {
		if tr.Received("obs", fmt.Sprint(i), 1, true) {
			t.Fatal("unbounded session admission")
		}
	}
	if !tr.Received("obs", "0", 3, true) || tr.Watermark("obs", "0") != 2 {
		t.Fatal("sparse low replay lost")
	}
	tr.Resolved("obs", "0", 3)
	if tr.Watermark("obs", "0") != 99 {
		t.Fatal("never-received gaps should be skipped")
	}
	tr.Resolved("obs", "0", 100)
	tr.mu.Lock()
	tr.m[durableKey{"obs", "0"}].touched = time.Now().Add(-8 * 24 * time.Hour)
	tr.nextPrune = time.Time{}
	tr.mu.Unlock()
	if !tr.Received("obs", "new", 1, true) || len(tr.m) != 4 {
		t.Fatal("resolved idle session not reclaimed")
	}
}

func TestPushTrackingBudgetStopsBeforeHandoff(t *testing.T) {
	tr := NewDurableTracker()
	tr.maxPerSession = 1
	server, client := net.Pipe()
	defer client.Close()
	out := make(chan *RawFrame, 4)
	p := &PushServer{out: out, ackInterval: time.Millisecond, durable: tr, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		p.stream(context.Background(), server, &connWriter{c: server}, identity.NewPrivateContext("obs", identity.CredentialToken), "ubx", "session")
	}()
	rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 1, Raw: make([]byte, 40)}
	for _, seq := range []uint64{10, 20} {
		if err := wire.WriteFrame(client, wire.Data, wire.EncodeData(seq, rec)); err != nil {
			t.Fatal(err)
		}
	}
	// Sparse seq 10 may safely produce ACK 9, but never ACK 10 or later.
	_ = client.SetReadDeadline(time.Now().Add(time.Second))
	for {
		ft, b, err := wire.ReadFrame(client)
		if err != nil {
			break
		}
		if ft == wire.Ack {
			seq, _ := wire.DecodeAck(b)
			if seq >= 10 {
				t.Fatalf("ACK beyond unresolved hole: %d", seq)
			}
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("budget did not close stream")
	}
	if len(out) != 1 || (<-out).Seq != 10 {
		t.Fatal("over-budget frame reached decoder")
	}
	if tr.Watermark("obs", "session") != 9 {
		t.Fatal("old unresolved receipt was forgotten")
	}
}

// TestDurableAbandonsSilentHolesLoudlyAndBoundsPerObserver pins the two
// reclamation rules the astra-6 verification added to one observer's
// session churn cannot fill the table for everyone, and a session whose holes
// stay unresolved while it is silent past durableHoleAbandonAfter is abandoned
// — counted, never acknowledged — so unrecoverable holes cannot consume the
// budget until a collector restart.
func TestDurableAbandonsSilentHolesLoudlyAndBoundsPerObserver(t *testing.T) {
	tr := NewDurableTracker()
	tr.maxPerSource = 4
	abandoned := metrics.DurableHolesAbandonedTotal.WithLabelValues("loop")
	before := testutil.ToFloat64(abandoned)
	for i := 0; i < 4; i++ {
		if !tr.Received("loop", fmt.Sprint(i), 1, true) {
			t.Fatalf("session %d refused under the per-observer cap", i)
		}
	}
	if tr.Received("loop", "4", 1, true) {
		t.Fatal("per-observer session cap not enforced")
	}
	if !tr.Received("other", "s", 1, true) {
		t.Fatal("unrelated observer denied by a crash-looping neighbour")
	}
	if got := tr.Watermark("loop", "0"); got != 0 {
		t.Fatalf("hole did not hold the watermark: %d", got)
	}
	// An active session is never abandoned however old its holes are: only
	// silence (time since the last receipt or resolution) counts.
	tr.mu.Lock()
	tr.pruneLocked(time.Now())
	tr.mu.Unlock()
	if tr.outstanding != 5 {
		t.Fatalf("recently touched sessions abandoned: outstanding %d", tr.outstanding)
	}
	// Silence past the window: holes abandoned loudly, budget released.
	tr.mu.Lock()
	for k, s := range tr.m {
		if k.source == "loop" {
			s.touched = time.Now().Add(-durableHoleAbandonAfter - time.Minute)
		}
	}
	tr.nextPrune = time.Time{}
	tr.mu.Unlock()
	if !tr.Received("loop", "4", 1, true) {
		t.Fatal("abandoned sessions did not release the observer's budget")
	}
	if got := testutil.ToFloat64(abandoned) - before; got != 4 {
		t.Fatalf("abandoned holes counted %v, want 4", got)
	}
	if tr.outstanding != 2 || tr.perSource["loop"] != 1 || tr.perSource["other"] != 1 {
		t.Fatalf("accounting after abandonment: outstanding=%d perSource=%v", tr.outstanding, tr.perSource)
	}
	// A feeder that reappears with an abandoned session is re-tracked from
	// what it replays; nothing was acknowledged in the meantime.
	if !tr.Received("loop", "0", 1, true) || tr.Watermark("loop", "0") != 0 {
		t.Fatal("abandoned hole acknowledged or replay refused")
	}
}

// regression fix (observability half): observer policies are retained for the
// process lifetime and the 1,025th identity is refused until restart. The
// capacity gauge exists so that ceiling is visible in advance rather than
// discovered when a valid new observer is turned away.
func TestPushObserverPolicyCapacityIsVisible(t *testing.T) {
	p := &PushServer{policies: make(map[string]*observerPolicy),
		collectorInstanceID: identity.LocalCollectorInstance,
		log:                 slog.New(slog.NewTextHandler(io.Discard, nil))}
	// The gauge is an absolute Set of this server's policy count; production runs
	// exactly one push server, so absolute values are the right assertion here.
	ctx := context.Background()
	admit := func(id string) (*Admission, string) {
		c := identity.NewPrivateContext(id, identity.CredentialToken)
		c, err := c.Normalize()
		if err != nil {
			t.Fatalf("normalize %q: %v", id, err)
		}
		return p.admit(ctx, c, 0, func() {}, policyCredential{digest: "d-" + id, feed: "ubx"})
	}
	for i := range 5 {
		if a, reason := admit(fmt.Sprintf("obs-%d", i)); a == nil {
			t.Fatalf("admission %d refused: %s", i, reason)
		}
	}
	if got := testutil.ToFloat64(metrics.PushObserverPoliciesTracked); got != 5 {
		t.Fatalf("tracked-policies gauge = %v, want 5", got)
	}
	// Re-admitting an existing identity must not inflate the capacity reading.
	if a, reason := admit("obs-0"); a == nil {
		t.Fatalf("re-admission refused: %s", reason)
	}
	if got := testutil.ToFloat64(metrics.PushObserverPoliciesTracked); got != 5 {
		t.Errorf("gauge = %v after a repeat identity; it must count distinct policies", got)
	}
	// And the documented ceiling still fails closed, with its existing reason,
	// while every retained policy was admitted recently (none is reclaimable).
	p.policyRetention = defaultPolicyRetention
	p.authorizationMu.Lock()
	for i := range maxTrackedObserverPolicies {
		p.policies[fmt.Sprintf("filler-%d", i)] = &observerPolicy{sessions: map[*Admission]context.CancelFunc{}, lastAdmitted: time.Now()}
	}
	p.authorizationMu.Unlock()
	if a, reason := admit("one-too-many"); a != nil || reason != "observer_ceiling" {
		t.Errorf("admission past the ceiling = %v/%q, want a refusal with observer_ceiling", a, reason)
	}
}

// TestIdlePoliciesReclaimedAtCeiling guards the ceiling is not a lockout: a
// policy with no live session whose last admission is older than the policy
// retention window is reclaimed when the table is full, so a new observer is
// admitted and the gauge follows; a policy admitted recently, or one with a
// live session, stays.
func TestIdlePoliciesReclaimedAtCeiling(t *testing.T) {
	p := newPushServer("", &tls.Config{}, make(chan *RawFrame, 8), nil, time.Second, 8, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()
	for i := range maxTrackedObserverPolicies {
		c := identity.NewPrivateContext(fmt.Sprintf("obs-%d", i), identity.CredentialToken)
		a, reason := p.admit(ctx, c, 0, func() {})
		if a == nil {
			t.Fatalf("admission %d refused: %s", i, reason)
		}
		if i != 0 { // obs-0 keeps a live session
			a.release()
		}
	}
	stale := func(observer string) {
		policy := p.policies[observer]
		policy.mu.Lock()
		policy.lastAdmitted = time.Now().Add(-p.policyRetention - time.Minute)
		policy.mu.Unlock()
	}
	stale("obs-0") // live session: must survive however old its admission
	stale("obs-1") // idle and past retention: reclaimable
	newcomer := identity.NewPrivateContext("replacement-board", identity.CredentialToken)
	a, reason := p.admit(ctx, newcomer, 0, func() {})
	if a == nil {
		t.Fatalf("new observer refused at the ceiling although an idle policy was reclaimable: %s", reason)
	}
	if _, kept := p.policies["obs-1"]; kept {
		t.Fatal("idle policy past retention was not reclaimed")
	}
	if _, kept := p.policies["obs-0"]; !kept {
		t.Fatal("policy with a live session was reclaimed")
	}
	if _, kept := p.policies["obs-2"]; !kept {
		t.Fatal("recently admitted policy was reclaimed")
	}
	if got := testutil.ToFloat64(metrics.PushObserverPoliciesTracked); got != maxTrackedObserverPolicies {
		t.Fatalf("tracked-policies gauge = %v after reclaim and admission, want %d", got, maxTrackedObserverPolicies)
	}
	// With nothing reclaimable the ceiling still refuses.
	if b, reason := p.admit(ctx, identity.NewPrivateContext("one-too-many", identity.CredentialToken), 0, func() {}); b != nil || reason != "observer_ceiling" {
		t.Fatalf("admission past a full table of live or recent policies = %v/%q, want observer_ceiling", b, reason)
	}
}

// TestDurablePerSourceCapEvictsResolvedSessions guards a restart-looping
// station is not locked out of durable ingest: at the per-observer session
// cap, the oldest fully resolved session is evicted (counted) and the new
// session's receipt admitted; when every retained session has an unresolved
// hole the receipt is still refused.
func TestDurablePerSourceCapEvictsResolvedSessions(t *testing.T) {
	tr := NewDurableTracker()
	evicted := metrics.DurableSessionsEvictedTotal.WithLabelValues("looper")
	before := testutil.ToFloat64(evicted)
	for i := 0; i < maxDurableSessionsPerSource; i++ {
		session := fmt.Sprint("boot-", i)
		if !tr.Received("looper", session, 1, true) {
			t.Fatalf("session %d refused under the cap", i)
		}
		tr.Resolved("looper", session, 1)
		tr.mu.Lock()
		tr.m[durableKey{"looper", session}].touched = time.Now().Add(-time.Duration(maxDurableSessionsPerSource-i) * time.Second)
		tr.mu.Unlock()
	}
	if !tr.Received("looper", "boot-new", 1, true) {
		t.Fatal("new session refused although every retained session was resolved")
	}
	if _, kept := tr.m[durableKey{"looper", "boot-0"}]; kept {
		t.Fatal("the oldest resolved session was not the one evicted")
	}
	if _, kept := tr.m[durableKey{"looper", "boot-new"}]; !kept || tr.perSource["looper"] != maxDurableSessionsPerSource {
		t.Fatalf("accounting after eviction: kept=%v perSource=%d", kept, tr.perSource["looper"])
	}
	if got := testutil.ToFloat64(evicted) - before; got != 1 {
		t.Fatalf("evictions counted %v, want 1", got)
	}
	// Every retained session holding a hole: the cap stands.
	holes := NewDurableTracker()
	for i := 0; i < maxDurableSessionsPerSource; i++ {
		if !holes.Received("looper", fmt.Sprint("boot-", i), 1, true) {
			t.Fatalf("session %d refused under the cap", i)
		}
	}
	if holes.Received("looper", "boot-new", 1, true) {
		t.Fatal("a session with an unresolved hole was evicted for a new one")
	}
	// A resolved session is reclaimed after a short idle, not a week.
	tr.mu.Lock()
	tr.m[durableKey{"looper", "boot-new"}].touched = time.Now().Add(-durableResolvedIdle - time.Minute)
	tr.pruneLocked(time.Now())
	_, kept := tr.m[durableKey{"looper", "boot-new"}]
	tr.mu.Unlock()
	if !kept {
		t.Fatal("a session with an outstanding receipt was reclaimed as resolved")
	}
	tr.Resolved("looper", "boot-new", 1)
	tr.mu.Lock()
	tr.m[durableKey{"looper", "boot-new"}].touched = time.Now().Add(-durableResolvedIdle - time.Minute)
	tr.pruneLocked(time.Now())
	_, kept = tr.m[durableKey{"looper", "boot-new"}]
	tr.mu.Unlock()
	if kept {
		t.Fatalf("resolved session idle past %v was not reclaimed", durableResolvedIdle)
	}
}
