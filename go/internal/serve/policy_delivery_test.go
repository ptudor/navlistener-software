package serve

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/store"
)

func resetTestAudience(s *Server) {
	s.store.Reset()
	s.policyEpochs.Advance([]identity.Audience{s.audience}, time.Now())
	s.InvalidateAudiences([]identity.Audience{s.audience})
}

func TestResetBetweenRenderAndCacheAdmission(t *testing.T) {
	s := testServer(nil)
	s.store.Apply(&ingest.RawFrame{Source: "withdrawn", Recv: time.Now(), RF: &ingest.RawRF{Bands: []ingest.RFBand{{AGC: 100}}}})
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s.beforeCacheAdmission = func() { close(ready); <-release }
	go func() { defer close(done); s.refresh("observers") }()
	<-ready
	resetTestAudience(s)
	close(release)
	<-done
	s.beforeCacheAdmission = nil
	s.mu.RLock()
	body := s.cache[cacheKey{audience: s.audience.Key(), feed: "observers"}].body
	s.mu.RUnlock()
	if body != nil {
		t.Fatal("stale render admitted after reset")
	}
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest("GET", "/", nil))
	if strings.Contains(rr.Body.String(), "withdrawn") {
		t.Fatal("new request received withdrawn state")
	}
}

// TestConcurrentMissesAfterResetRenderOnce guards the on-demand refresh is
// single-flight: after a reset invalidates the warmed body, a burst of
// concurrent public requests performs exactly one render and every request is
// answered from it, instead of N shard walks racing on admission.
func TestConcurrentMissesAfterResetRenderOnce(t *testing.T) {
	s := testServer(nil)
	s.refresh("observers")
	var renders atomic.Int64
	s.onBuildFeed = func(feed string, _ identity.Audience) {
		if feed == "observers" {
			renders.Add(1)
		}
	}
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.beforeCacheAdmission = func() { once.Do(func() { close(ready) }); <-release }
	resetTestAudience(s)
	const n = 16
	codes := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rr := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
			codes[i] = rr.Code
		}(i)
	}
	<-ready // one render is in flight and parked before admission
	close(release)
	wg.Wait()
	s.beforeCacheAdmission = nil
	if got := renders.Load(); got != 1 {
		t.Fatalf("%d concurrent misses rendered %d times, want exactly 1", n, got)
	}
	for i, code := range codes {
		if code != http.StatusOK {
			t.Errorf("request %d: status %d, want 200 from the shared render", i, code)
		}
	}
}

func TestResetBetweenAuthorizationAndBrokerAdmission(t *testing.T) {
	s := testServer(nil)
	b := s.broker
	ready, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	b.beforePublish = func() { close(ready); <-release }
	e := EventMsg{ID: 1, Message: "withdrawn", GenerationSet: true, PolicyGeneration: 0}
	go func() { defer close(done); s.PublishEvent(e) }()
	<-ready
	resetTestAudience(s)
	close(release)
	<-done
	if len(b.replayFrom(0, true)) != 0 {
		t.Fatal("obsolete event reentered replay ring")
	}
}

type blockedPolicyHistory struct{ ready, release chan struct{} }

func (f *blockedPolicyHistory) QueryEvents(context.Context, store.EventQuery) ([]store.StoredEvent, int, error) {
	close(f.ready)
	<-f.release
	return []store.StoredEvent{{ID: 1, Message: "withdrawn"}}, 1, nil
}
func (f *blockedPolicyHistory) SummarizeEventsForAudience(context.Context, string, time.Time, time.Time) (store.EventSummary, error) {
	close(f.ready)
	<-f.release
	return store.EventSummary{TotalEvents: 42}, nil
}
func (f *blockedPolicyHistory) CurrentConditions(context.Context, string, time.Time) (store.ConditionSnapshot, error) {
	close(f.ready)
	<-f.release
	return store.ConditionSnapshot{Cursor: 42, Events: []store.StoredEvent{{ID: 1, Message: "withdrawn"}}}, nil
}

// Every history-shaped route that spans a policy reset must fail closed —
// including the regression fix condition snapshot, which shares the delivery fence.
func TestHistoryAndSummarySpanningResetFailClosed(t *testing.T) {
	for _, path := range []string{"/gnss/api/events", "/gnss/api/events/summary", "/gnss/api/events/conditions"} {
		t.Run(path, func(t *testing.T) {
			history := &blockedPolicyHistory{make(chan struct{}), make(chan struct{})}
			s := newTestServer(nil, history)
			rr := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); s.http.Handler.ServeHTTP(rr, httptest.NewRequest("GET", path, nil)) }()
			<-history.ready
			resetTestAudience(s)
			close(history.release)
			<-done
			if rr.Code != http.StatusServiceUnavailable || strings.Contains(rr.Body.String(), "withdrawn") || strings.Contains(rr.Body.String(), "42") {
				t.Fatalf("obsolete history delivered: %d %s", rr.Code, rr.Body.String())
			}
			// The success headers were already set when the delivery fence
			// tripped; the 503 must not inherit a storable Cache-Control.
			if got := rr.Header().Get("Cache-Control"); got != "private, no-store" && got != "no-store" {
				t.Fatalf("policy-changed 503 Cache-Control = %q, want no-store", got)
			}
			if rr.Header().Get("Retry-After") != "1" {
				t.Fatalf("policy-changed 503 Retry-After = %q, want 1", rr.Header().Get("Retry-After"))
			}
		})
	}
}

// TestFeedNotReadyIsNotCacheable guards a public feed whose render was refused
// admission (a reset landed between render and admission, so nothing current
// exists to serve) answers 503 with Cache-Control: no-store and Retry-After,
// never the public max-age the success body would have carried: a shared cache
// must not turn that transient into a 30 s outage.
func TestFeedNotReadyIsNotCacheable(t *testing.T) {
	s := newTestServerForAudience(identity.Audience{Kind: identity.AudiencePublic}, nil)
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.beforeCacheAdmission = func() { once.Do(func() { close(ready) }); <-release }
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	}()
	<-ready
	resetTestAudience(s) // the in-flight render is now stale and will not be admitted
	close(release)
	<-done
	s.beforeCacheAdmission = nil
	if rr.Code != http.StatusServiceUnavailable || !strings.Contains(rr.Body.String(), "feed not ready") {
		t.Fatalf("cold feed after reset: %d %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("feed-not-ready 503 Cache-Control = %q, want no-store", got)
	}
	if rr.Header().Get("Retry-After") != "1" {
		t.Fatalf("feed-not-ready 503 Retry-After = %q, want 1", rr.Header().Get("Retry-After"))
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "X-GNSS-Audience") {
		t.Fatalf("error response dropped Vary: %q", got)
	}
}

func TestQueuedAndReplayedEventsAreGenerationTagged(t *testing.T) {
	s := testServer(nil)
	b := s.broker
	c, ok := b.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	defer b.unsubscribe(c)
	s.PublishEvent(EventMsg{ID: 1, Message: "withdrawn"})
	queued := <-c.events
	replayed := b.replayFrom(0, true)[0]
	resetTestAudience(s)
	if b.eventCurrent(c, queued) || b.eventCurrent(c, replayed) || b.clientCurrent(c) {
		t.Fatal("obsolete queued/replayed delivery admitted")
	}
}

type transportWriter struct {
	connection net.Conn
	ready      chan struct{}
	header     http.Header
}

func (w *transportWriter) Header() http.Header { return w.header }
func (w *transportWriter) WriteHeader(int)     {}
func (w *transportWriter) Write(body []byte) (int, error) {
	close(w.ready)
	return w.connection.Write(body)
}

func TestInvalidationInterruptsWriteWithoutWaitingForConsumer(t *testing.T) {
	s := testServer(nil)
	server, client := net.Pipe()
	defer client.Close()
	defer server.Close()
	req := httptest.NewRequest("GET", "/", nil).WithContext(connectionContext(context.Background(), server))
	delivery := s.beginDelivery(req, s.audience)
	defer delivery.finish()
	w := &transportWriter{server, make(chan struct{}), make(http.Header)}
	done := make(chan struct{})
	go func() { defer close(done); delivery.write(w, []byte("withdrawn")) }()
	<-w.ready // Write has started and the consumer is not reading.
	resetDone := make(chan struct{})
	go func() { defer close(resetDone); resetTestAudience(s) }()
	select {
	case <-resetDone:
	case <-time.After(time.Second):
		t.Fatal("ingest reset waited for network consumer")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("obsolete write survived reset")
	}
	body := make([]byte, 64)
	n, _ := client.Read(body)
	if n != 0 {
		t.Fatal("withdrawn body escaped after reset")
	}
}
