package serve

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
)

type fixedReadAuthorizer map[string]identity.ReadPrincipal

func (a fixedReadAuthorizer) AuthorizeRead(_ context.Context, token string) (identity.ReadPrincipal, bool) {
	principal, ok := a[token]
	return principal, ok
}

type toggleReadAuthorizer struct {
	enabled   atomic.Bool
	principal identity.ReadPrincipal
}

func (a *toggleReadAuthorizer) AuthorizeRead(_ context.Context, _ string) (identity.ReadPrincipal, bool) {
	return a.principal, a.enabled.Load()
}

func testServer(sources []config.Source) *Server {
	return newTestServer(sources, nil)
}

func newTestServer(sources []config.Source, events EventStore) *Server {
	return New("127.0.0.1:0", state.New(4), events, sources, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// newTestServerForAudience builds a listener whose default view is selected,
// with no read authorizer and no opt-in: a public default serves credential-
// free, a private one answers 503 until ServeUnauthenticated is called.
func newTestServerForAudience(selected identity.Audience, events EventStore) *Server {
	return NewForAudience("127.0.0.1:0", state.New(4), events, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), selected)
}

// TestServerIdleTimeoutSet guards an idle keep-alive connection between requests
// must be bounded, distinct from the SSE per-write deadline and unset WriteTimeout
// (SSE streams are exempt from that by design).
func TestServerIdleTimeoutSet(t *testing.T) {
	s := testServer(nil)
	if s.http.IdleTimeout <= 0 {
		t.Error("http.Server.IdleTimeout is unset, want a bounded value")
	}
	if s.http.WriteTimeout != 0 {
		t.Errorf("http.Server.WriteTimeout = %v, want unset (SSE streams need long-lived writes)", s.http.WriteTimeout)
	}
}

func TestPublicAudienceHeadersAndAnonymousObserverFiltering(t *testing.T) {
	s := NewForAudience("127.0.0.1:0", state.New(1), nil, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Audience{Kind: identity.AudiencePublic})
	now := time.Now()
	s.store.Apply(&ingest.RawFrame{Source: audience.AnonymousPublicSource, Recv: now, RF: &ingest.RawRF{
		Bands: []ingest.RFBand{{Block: 0, AGC: 3000}},
	}})
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	if got := rr.Header().Get("Cache-Control"); !strings.HasPrefix(got, "public") {
		t.Fatalf("public cache header = %q", got)
	}
	// A shared cache keys variants by the stored response's Vary: without it the
	// cached public body would answer a later credentialed, audience-selected
	// request on the same URL.
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "X-GNSS-Audience") {
		t.Fatalf("public Vary = %q, want Authorization and X-GNSS-Audience", got)
	}
	if strings.Contains(rr.Body.String(), audience.AnonymousPublicSource) {
		t.Fatalf("anonymous source leaked into observer feed: %s", rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"audience":"public"`) {
		t.Fatalf("audience key missing from envelope data: %s", rr.Body.String())
	}

	events := httptest.NewRecorder()
	s.serveEventsQuery(events, httptest.NewRequest(http.MethodGet, "/gnss/api/events", nil))
	if events.Code != http.StatusServiceUnavailable {
		t.Fatalf("unscoped public events status = %d, want 503", events.Code)
	}
}

func TestOperatorAudienceResponsesArePrivate(t *testing.T) {
	s := NewForAudience("127.0.0.1:0", state.New(4), nil, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		identity.Audience{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance})
	s.ServeUnauthenticated() // the explicit credential-free embedding opt-in
	s.refresh("global")
	rr := httptest.NewRecorder()
	s.serveFeed("global")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/global", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("opted-in operator view status = %d: %s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("operator cache header = %q", got)
	}
	if got := rr.Header().Get("Vary"); !strings.Contains(got, "Authorization") {
		t.Fatalf("operator Vary = %q", got)
	}
}

// TestPrivateDefaultAudienceRequiresReadAuthorization guards a non-public
// default audience must never be served credential-free by omission: with no
// ReadAuthorizer and no explicit ServeUnauthenticated opt-in, every feed and
// the event stream answer 503, and the public selection still works once a
// resolver can materialize it. The public default audience is unaffected.
func TestPrivateDefaultAudienceRequiresReadAuthorization(t *testing.T) {
	operator := identity.Audience{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}
	s := NewForAudience("127.0.0.1:0", state.New(4), nil, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), operator)
	s.refreshAll()
	for _, path := range []string{"/gnss/api/v2/observers", "/gnss/api/v2/coverage", "/gnss/api/events", "/gnss/events"} {
		rr := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without read authorization: status %d, want 503: %s", path, rr.Code, rr.Body.String())
		}
		if strings.Contains(rr.Body.String(), `"ok":true`) {
			t.Errorf("%s served the private operator view credential-free", path)
		}
	}

	// A public view materialized by a resolver remains selectable and credential-free.
	registry := audience.NewRegistry(1, nil)
	registry.Register(identity.Audience{Kind: identity.AudiencePublic}, state.New(1), nil)
	s.resolver = registry
	req := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil)
	req.Header.Set("X-GNSS-Audience", "public")
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"audience":"public"`) {
		t.Fatalf("explicit public selection: status %d body %s", rr.Code, rr.Body.String())
	}

	// The opt-in restores the convenience behaviour New provides.
	s.ServeUnauthenticated()
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"audience":"operator:local"`) {
		t.Fatalf("opted-in operator view: status %d body %s", rr.Code, rr.Body.String())
	}
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/gnss/events", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("opted-in event stream HEAD: status %d", rr.Code)
	}

	// A public default audience never needed the opt-in.
	public := NewForAudience("127.0.0.1:0", state.New(4), nil, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Audience{Kind: identity.AudiencePublic})
	public.refreshAll()
	rr = httptest.NewRecorder()
	public.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("public default audience: status %d body %s", rr.Code, rr.Body.String())
	}
}

// TestPublicSelectionUnderPrivateDefaultIsCachedPerAudience guards an
// anonymous `X-GNSS-Audience: public` request on a private-default listener is
// admitted to a per-audience public cache (one render per cadence, reused by
// later requests, invalidated by the audience's reset) instead of re-rendering
// every feed per request, while the default audience's warmed body stays
// separate.
func TestPublicSelectionUnderPrivateDefaultIsCachedPerAudience(t *testing.T) {
	s := testServer(nil) // operator default, opted in
	public := identity.Audience{Kind: identity.AudiencePublic}
	registry := audience.NewRegistry(1, nil)
	registry.Register(public, state.New(1), nil)
	s.resolver = registry
	now := time.Now()
	s.now = func() time.Time { return now }
	var renders atomic.Int64
	s.onBuildFeed = func(feed string, selected identity.Audience) {
		if feed == "svs" && selected == public {
			renders.Add(1)
		}
	}
	get := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/svs", nil)
		req.Header.Set("X-GNSS-Audience", "public")
		rr := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"audience":"public"`) {
			t.Fatalf("public selection: status %d body %s", rr.Code, rr.Body.String())
		}
		if got := rr.Header().Get("Cache-Control"); got != "public, max-age=30" {
			t.Fatalf("public selection Cache-Control = %q", got)
		}
		return rr
	}
	first, second := get(), get()
	if renders.Load() != 1 || first.Body.String() != second.Body.String() {
		t.Fatalf("public selection rendered %d times for two requests; bodies equal=%v", renders.Load(), first.Body.String() == second.Body.String())
	}
	s.InvalidateAudiences([]identity.Audience{public})
	get()
	if renders.Load() != 2 {
		t.Fatalf("public reset did not invalidate the public cache (%d renders)", renders.Load())
	}
	now = now.Add(s.fast + time.Second)
	get()
	if renders.Load() != 3 {
		t.Fatalf("public entry past its cadence was not re-rendered (%d renders)", renders.Load())
	}
	// The default (operator) audience's warmed body is a separate entry and is
	// never the public body.
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/svs", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), `"audience":"operator:local"`) {
		t.Fatalf("default audience body: status %d body %s", rr.Code, rr.Body.String())
	}
	if renders.Load() != 3 {
		t.Fatalf("serving the default audience rendered the public view (%d renders)", renders.Load())
	}
}

func TestPublicEventHistoryUsesPublicAudience(t *testing.T) {
	events := &fakeEvents{}
	s := NewForAudience("127.0.0.1:0", state.New(1), events, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Audience{Kind: identity.AudiencePublic})
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("public scoped event query status = %d: %s", rr.Code, rr.Body.String())
	}
	if events.lastQuery.Audience != "public" {
		t.Fatalf("event query audience = %q, want public", events.lastQuery.Audience)
	}
}

func TestAuthenticatedAudienceSelectionNeverServesAnOperatorSuperset(t *testing.T) {
	ctxA := identity.NewPrivateContext("observer-a", identity.CredentialToken)
	ctxA.OrganizationID = "customer-a"
	ctxA.CollectionIDs = []string{"fleet-a"}
	ctxB := identity.NewPrivateContext("observer-b", identity.CredentialToken)
	ctxB.OrganizationID = "customer-b"
	registry := audience.NewRegistry(1, []config.Source{
		{Name: "observer-a", Type: "ubx", ObserverContext: ctxA},
		{Name: "observer-b", Type: "ubx", ObserverContext: ctxB},
	})
	publicState := state.New(1)
	registry.Register(identity.Audience{Kind: identity.AudiencePublic}, publicState, nil)
	now := time.Now()
	registry.ApplyPrivate(&ingest.RawFrame{Recv: now, Source: "observer-a", Observer: ctxA, RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 100}}}})
	registry.ApplyPrivate(&ingest.RawFrame{Recv: now, Source: "observer-b", Observer: ctxB, RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 200}}}})
	principalA := identity.ReadPrincipal{ID: "viewer-a", Revision: "grant-v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOrganization, ID: "customer-a"}, {Kind: identity.AudienceCollection, ID: "fleet-a"}}}
	principalB := identity.ReadPrincipal{ID: "viewer-b", Revision: "grant-v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOrganization, ID: "customer-b"}}}
	auth := fixedReadAuthorizer{"token-a": principalA, "token-b": principalB}
	events := &fakeEvents{}
	s := NewForAudience("127.0.0.1:0", publicState, events, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Audience{Kind: identity.AudiencePublic})
	s.EnableAudienceSelection(auth, registry, 10*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil)
	req.Header.Set("Authorization", "Bearer token-a")
	req.Header.Set("X-GNSS-Audience", "organization:customer-a")
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "observer-a") || strings.Contains(rr.Body.String(), "observer-b") {
		t.Fatalf("organization view leaked or omitted a station: status=%d body=%s", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("private audience cache header = %q", got)
	}

	for name, tc := range map[string]struct {
		token string
		want  int
	}{
		"missing credential": {"", http.StatusUnauthorized},
		"wrong principal":    {"token-b", http.StatusForbidden},
	} {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/global", nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			req.Header.Set("X-GNSS-Audience", "organization:customer-a")
			rr := httptest.NewRecorder()
			s.http.Handler.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status=%d body=%s", rr.Code, rr.Body.String())
			}
		})
	}

	eventReq := httptest.NewRequest(http.MethodGet, "/gnss/api/events", nil)
	eventReq.Header.Set("Authorization", "Bearer token-a")
	eventReq.Header.Set("X-GNSS-Audience", "organization:customer-a")
	eventRR := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(eventRR, eventReq)
	if eventRR.Code != http.StatusOK || events.lastQuery.Audience != "organization:customer-a" {
		t.Fatalf("scoped event query = status %d audience %q", eventRR.Code, events.lastQuery.Audience)
	}

	discovery := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/audiences", nil)
	discovery.Header.Set("Authorization", "Bearer token-a")
	discoveryRR := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(discoveryRR, discovery)
	body := discoveryRR.Body.String()
	if discoveryRR.Code != http.StatusOK || !strings.Contains(body, "organization:customer-a") ||
		!strings.Contains(body, "collection:fleet-a") || !strings.Contains(body, `"revision":"`) ||
		strings.Contains(body, "customer-b") {
		t.Fatalf("audience discovery = status %d body %s", discoveryRR.Code, body)
	}
	var discovered struct {
		Data struct {
			Revision string `json:"revision"`
		} `json:"data"`
	}
	if err := json.Unmarshal(discoveryRR.Body.Bytes(), &discovered); err != nil || len(discovered.Data.Revision) != 64 {
		t.Fatalf("audience discovery revision is not opaque SHA-256: %v body=%s", err, body)
	}
	s.policyEpochs.Advance([]identity.Audience{{Kind: identity.AudienceOrganization, ID: "customer-a"}}, now.Add(time.Second))
	revisedDiscovery := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/audiences", nil)
	revisedDiscovery.Header.Set("Authorization", "Bearer token-a")
	revisedRR := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(revisedRR, revisedDiscovery)
	var revised struct {
		Data struct {
			Revision string `json:"revision"`
		} `json:"data"`
	}
	if err := json.Unmarshal(revisedRR.Body.Bytes(), &revised); err != nil || revised.Data.Revision == discovered.Data.Revision {
		t.Fatalf("policy epoch did not advance discovery revision: %v before=%q after=%q", err, discovered.Data.Revision, revised.Data.Revision)
	}
	publicDiscovery := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(publicDiscovery, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/audiences", nil))
	if publicDiscovery.Code != http.StatusOK || !strings.Contains(publicDiscovery.Body.String(), `"audiences":["public"]`) ||
		strings.Contains(publicDiscovery.Body.String(), `"principal"`) || !strings.Contains(publicDiscovery.Body.String(), `"revision":"`) {
		t.Fatalf("anonymous audience discovery leaked auth metadata: status %d body %s", publicDiscovery.Code, publicDiscovery.Body.String())
	}
	for name, rr := range map[string]*httptest.ResponseRecorder{"anonymous": publicDiscovery, "authenticated": discoveryRR} {
		if got := rr.Header().Get("Vary"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "X-GNSS-Audience") {
			t.Fatalf("%s audience discovery Vary = %q, want Authorization and X-GNSS-Audience", name, got)
		}
	}

	// The organization's epoch now starts a second in the future, so a scoped
	// query's whole window lies before it: answered empty and marked limited,
	// without reaching the historian.
	calls := events.calls
	limitedReq := httptest.NewRequest(http.MethodGet, "/gnss/api/events", nil)
	limitedReq.Header.Set("Authorization", "Bearer token-a")
	limitedReq.Header.Set("X-GNSS-Audience", "organization:customer-a")
	limitedRR := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(limitedRR, limitedReq)
	if limited := decodeEnvelope(t, limitedRR); limited["history_limited"] != true || limited["total"] != float64(0) || events.calls != calls {
		t.Fatalf("scoped query across the advanced epoch = %s (store calls %d -> %d)", limitedRR.Body.String(), calls, events.calls)
	}
}

// TestPrivateEventStreamIsPrivateNoStore guards the one authenticated response
// that violated the contract: a private audience's SSE stream (GET and HEAD)
// must send Cache-Control: private, no-store and Vary, while the public stream
// keeps no-cache.
func TestPrivateEventStreamIsPrivateNoStore(t *testing.T) {
	s := testServer(nil) // private operator default, opted in
	head := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/gnss/events", nil))
	if head.Code != http.StatusOK || head.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("private HEAD: status %d Cache-Control %q", head.Code, head.Header().Get("Cache-Control"))
	}
	if got := head.Header().Get("Vary"); !strings.Contains(got, "Authorization") || !strings.Contains(got, "X-GNSS-Audience") {
		t.Fatalf("private HEAD Vary = %q", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	rr := newSyncRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/events", nil).WithContext(ctx))
	}()
	waitFor(t, func() bool { return strings.Contains(rr.String(), "event: status") })
	cancel()
	<-done
	if got := rr.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("private GET stream Cache-Control = %q, want private, no-store", got)
	}
	if got := rr.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("private GET stream Content-Type = %q", got)
	}

	public := newTestServerForAudience(identity.Audience{Kind: identity.AudiencePublic}, nil)
	head = httptest.NewRecorder()
	public.http.Handler.ServeHTTP(head, httptest.NewRequest(http.MethodHead, "/gnss/events", nil))
	if head.Code != http.StatusOK || head.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("public HEAD: status %d Cache-Control %q, want no-cache", head.Code, head.Header().Get("Cache-Control"))
	}
}

// TestEventStreamChecksMethodBeforeAuthorization guards the routed stream
// handler behaves like every other endpoint: a POST without a credential on a
// private-default listener is 405 + Allow, not 401, and a HEAD never registers
// a revocable delivery it would tear down at once.
func TestEventStreamChecksMethodBeforeAuthorization(t *testing.T) {
	s := testServer(nil)
	s.EnableAudienceSelection(fixedReadAuthorizer{"token": identity.ReadPrincipal{ID: "viewer", AudienceGrants: []identity.Audience{s.audience}}}, nil, time.Second)
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/gnss/events", nil))
	if rr.Code != http.StatusMethodNotAllowed || rr.Header().Get("Allow") != "GET, HEAD" {
		t.Fatalf("POST without credential: status %d Allow %q, want 405 GET, HEAD", rr.Code, rr.Header().Get("Allow"))
	}
	// The credential check still guards HEAD and GET.
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodHead, "/gnss/events", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("HEAD without credential: status %d, want 401", rr.Code)
	}
	req := httptest.NewRequest(http.MethodHead, "/gnss/events", nil)
	req.Header.Set("Authorization", "Bearer token")
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("authenticated HEAD: status %d", rr.Code)
	}
	s.deliveryMu.Lock()
	registered := len(s.deliveries)
	s.deliveryMu.Unlock()
	if registered != 0 {
		t.Fatalf("HEAD left %d deliveries registered, want 0", registered)
	}
}

// TestSSEPublicShareLeavesPrivateStreamsAdmitted guards the stream cap is
// partitioned per audience: an anonymous client filling the public share is
// refused at that share, a private audience's stream is still admitted, and
// the global ceiling remains the outer bound across audiences.
func TestSSEPublicShareLeavesPrivateStreamsAdmitted(t *testing.T) {
	old := sseMaxClients
	sseMaxClients = 4
	defer func() { sseMaxClients = old }()
	s := testServer(nil) // operator default; brokers are bound per audience
	public := identity.Audience{Kind: identity.AudiencePublic}
	publicBroker := s.brokerFor(public)
	type held struct {
		broker *Broker
		client *sseClient
	}
	var admitted []held
	defer func() {
		for _, h := range admitted {
			h.broker.unsubscribe(h.client)
		}
	}()
	for i := 0; i < sseMaxClients; i++ {
		if c, ok := publicBroker.subscribe(); ok {
			admitted = append(admitted, held{publicBroker, c})
		}
	}
	if want := sseAudienceShare(public); len(admitted) != want {
		t.Fatalf("public streams admitted = %d, want the public share %d of %d", len(admitted), want, sseMaxClients)
	}
	c, ok := s.broker.subscribe() // the private operator audience
	if !ok {
		t.Fatal("private stream refused while only the public share was full")
	}
	admitted = append(admitted, held{s.broker, c})
	orgA := s.brokerFor(identity.Audience{Kind: identity.AudienceOrganization, ID: "customer-a"})
	c, ok = orgA.subscribe()
	if !ok {
		t.Fatal("second private audience refused under the global ceiling")
	}
	admitted = append(admitted, held{orgA, c})
	// Public 2 + operator 1 + customer-a 1 = the global cap: the next audience's
	// own share is free, but the outer bound holds.
	orgB := s.brokerFor(identity.Audience{Kind: identity.AudienceOrganization, ID: "customer-b"})
	if c, ok := orgB.subscribe(); ok {
		orgB.unsubscribe(c)
		t.Fatal("global stream ceiling exceeded through per-audience shares")
	}
}

// TestHistorySlotsArePartitionedPerPrincipal guards the authenticated
// history/evidence bound is shared fairly: a principal at its share is
// refused while another principal is still admitted, releases return both
// the share and the global slot, and the global pool stays the outer bound.
func TestHistorySlotsArePartitionedPerPrincipal(t *testing.T) {
	s := testServer(nil)
	var releases []func()
	for i := 0; i < historyPerPrincipal; i++ {
		release, ok := s.acquireHistorySlot("tenant-a")
		if !ok {
			t.Fatalf("tenant-a slot %d refused under its share of %d", i, historyPerPrincipal)
		}
		releases = append(releases, release)
	}
	if _, ok := s.acquireHistorySlot("tenant-a"); ok {
		t.Fatal("tenant-a was granted more than its per-principal share")
	}
	release, ok := s.acquireHistorySlot("tenant-b")
	if !ok {
		t.Fatal("tenant-b refused while tenant-a held only its own share")
	}
	releases = append(releases, release)
	if len(s.historySlots) != historyPerPrincipal+1 {
		t.Fatalf("global slots in use = %d, want %d", len(s.historySlots), historyPerPrincipal+1)
	}
	for _, r := range releases {
		r()
	}
	if len(s.historySlots) != 0 || len(s.historyInUse) != 0 {
		t.Fatalf("slots not returned: global %d, per-principal %v", len(s.historySlots), s.historyInUse)
	}
	for i := 0; i < cap(s.historySlots); i++ {
		s.historySlots <- struct{}{}
	}
	if _, ok := s.acquireHistorySlot("tenant-c"); ok {
		t.Fatal("a slot was granted with the global pool exhausted")
	}
	for i := 0; i < cap(s.historySlots); i++ {
		<-s.historySlots
	}
}

// TestBearerSchemeIsCaseInsensitive guards the auth-scheme token is
// case-insensitive (RFC 9110 §11.1): a client or proxy that lower-cases
// "bearer" must still authenticate, while the single-header, no-whitespace
// and length rules keep rejecting malformed credentials.
func TestBearerSchemeIsCaseInsensitive(t *testing.T) {
	s := testServer(nil)
	s.EnableAudienceSelection(fixedReadAuthorizer{"secret": identity.ReadPrincipal{ID: "viewer", AudienceGrants: []identity.Audience{s.audience}}}, nil, time.Second)
	s.refresh("global")
	for header, want := range map[string]int{
		"Bearer secret":  http.StatusOK,
		"bearer secret":  http.StatusOK,
		"BEARER secret":  http.StatusOK,
		"Bearer  secret": http.StatusOK, // surrounding whitespace is trimmed, as before
		"Basic secret":   http.StatusUnauthorized,
		"Bearer":         http.StatusUnauthorized,
		"Bearer\tsecret": http.StatusUnauthorized,
		"Bearer se cret": http.StatusUnauthorized,
		"Bearer " + strings.Repeat("x", 4097): http.StatusUnauthorized,
	} {
		req := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/global", nil)
		req.Header.Set("Authorization", header)
		rr := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("Authorization %q: status %d, want %d: %s", header, rr.Code, want, rr.Body.String())
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/gnss/api/v2/global", nil)
	req.Header.Add("Authorization", "Bearer secret")
	req.Header.Add("Authorization", "Bearer secret")
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("two Authorization headers: status %d, want 401", rr.Code)
	}
}

func TestActiveReadSessionReauthorizationCancelsAfterRevocation(t *testing.T) {
	principal := identity.ReadPrincipal{ID: "viewer-a", Revision: "grant-v1", AudienceGrants: []identity.Audience{{Kind: identity.AudienceOrganization, ID: "customer-a"}}}
	auth := &toggleReadAuthorizer{principal: principal}
	auth.enabled.Store(true)
	s := testServer(nil)
	s.readAuth = auth
	s.reauthorizeEvery = 5 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	view := requestView{audience: principal.AudienceGrants[0], principal: principal, token: "secret"}
	done := make(chan struct{})
	go func() {
		s.watchReadAuthorization(ctx, cancel, view)
		close(done)
	}()
	auth.enabled.Store(false)
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("revoked read session was not canceled")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("read reauthorization watcher did not exit")
	}
}

// TestPrivateObservationCannotChangePublicFeedBytes is the stage-5 privacy
// parity invariant: a private-only source is rejected before aggregation, so
// every public feed remains byte-for-byte identical at a fixed refresh instant.
func TestPrivateObservationCannotChangePublicFeedBytes(t *testing.T) {
	publicState := state.New(1)
	s := NewForAudience("127.0.0.1:0", publicState, nil, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Audience{Kind: identity.AudiencePublic})
	fixed := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return fixed }
	s.refreshAll()
	before := s.SnapshotFeeds()

	private := &ingest.RawFrame{
		Source: "customer-a-secret-roof", Recv: fixed,
		Observer: identity.NewPrivateContext("customer-a-secret-roof", identity.CredentialHardwareMTLS),
		RF:       &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 1, JamState: 3}}},
	}
	if projected, ok := audience.ProjectPublic(private); ok {
		publicState.Apply(projected)
	}
	s.refreshAll()
	after := s.SnapshotFeeds()
	if len(before) != len(after) {
		t.Fatalf("feed count changed: %d -> %d", len(before), len(after))
	}
	for feed, want := range before {
		if got := after[feed]; !bytes.Equal(got, want) {
			t.Errorf("private frame changed public %s bytes\nbefore=%s\nafter=%s", feed, want, got)
		}
	}
}

// TestEnvelopeAndSchema verifies every v2 feed returns the standard envelope with
// ok=true, an RFC3339 time, and the schema version in data (docs/OUTPUT.md §0).
func TestEnvelopeAndSchema(t *testing.T) {
	s := testServer(nil)
	s.refreshAll()
	for _, feed := range []string{"svs", "global", "observers", "almanac", "sbas"} {
		rr := httptest.NewRecorder()
		s.serveFeed(feed)(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/"+feed, nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200", feed, rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: content-type %q", feed, ct)
		}
		var env struct {
			OK   bool                       `json:"ok"`
			Time string                     `json:"time"`
			Data map[string]json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%s: unmarshal: %v", feed, err)
		}
		if !env.OK {
			t.Errorf("%s: ok=false", feed)
		}
		if _, err := time.Parse(time.RFC3339, env.Time); err != nil {
			t.Errorf("%s: time %q not RFC3339: %v", feed, env.Time, err)
		}
		var schema string
		if err := json.Unmarshal(env.Data["schema"], &schema); err != nil || schema != schemaVersion {
			t.Errorf("%s: schema %q, want %q", feed, schema, schemaVersion)
		}
	}
}

// TestSnapshotFeeds verifies the historian backfill accessor returns a copy of each warmed
// feed's current body, omits un-warmed feeds, and does not alias the live cache.
func TestSnapshotFeeds(t *testing.T) {
	s := testServer(nil)
	if len(s.SnapshotFeeds()) != 0 {
		t.Fatalf("cold server should snapshot no feeds, got %d", len(s.SnapshotFeeds()))
	}
	s.refreshAll()
	snap := s.SnapshotFeeds()
	for _, feed := range []string{"svs", "global", "observers", "almanac", "sbas"} {
		body, ok := snap[feed]
		if !ok || len(body) == 0 {
			t.Fatalf("feed %q missing from snapshot", feed)
		}
		var env struct {
			OK   bool `json:"ok"`
			Data struct {
				Schema string `json:"schema"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &env); err != nil || !env.OK || env.Data.Schema != schemaVersion {
			t.Errorf("feed %q snapshot not a valid envelope: %v", feed, err)
		}
	}
	// The returned slice is a copy: mutating it must not corrupt the served cache.
	snap["svs"][0] = 'X'
	s.mu.RLock()
	cached := s.cache[cacheKey{audience: s.audience.Key(), feed: "svs"}].body[0]
	s.mu.RUnlock()
	if cached == 'X' {
		t.Error("SnapshotFeeds aliased the live cache")
	}
}

func TestSnapshotAllFeedsKeepsPrivateAudiencesSeparate(t *testing.T) {
	ctxA := identity.NewPrivateContext("observer-a", identity.CredentialToken)
	ctxA.OrganizationID = "customer-a"
	ctxB := identity.NewPrivateContext("observer-b", identity.CredentialToken)
	ctxB.OrganizationID = "customer-b"
	registry := audience.NewRegistry(1, nil)
	publicState := state.New(1)
	registry.Register(identity.Audience{Kind: identity.AudiencePublic}, publicState, nil)
	now := time.Now()
	registry.ApplyPrivate(&ingest.RawFrame{Recv: now, Source: "observer-a", Observer: ctxA, RF: &ingest.RawRF{Bands: []ingest.RFBand{{AGC: 100}}}})
	registry.ApplyPrivate(&ingest.RawFrame{Recv: now, Source: "observer-b", Observer: ctxB, RF: &ingest.RawRF{Bands: []ingest.RFBand{{AGC: 200}}}})
	s := NewForAudience("127.0.0.1:0", publicState, nil, nil, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)), identity.Audience{Kind: identity.AudiencePublic})
	s.resolver = registry
	s.refreshAll()
	var bodyA string
	for _, snapshot := range s.SnapshotAllFeeds() {
		if snapshot.Audience.Key() == "organization:customer-a" && snapshot.Feed == "observers" {
			bodyA = string(snapshot.Body)
		}
	}
	if bodyA == "" || !strings.Contains(bodyA, "observer-a") || strings.Contains(bodyA, "observer-b") {
		t.Fatalf("customer-a snapshot crossed audience state: %s", bodyA)
	}
}

// TestGlobalCounters verifies the global feed carries the flat leap-second and
// live-total scalars (docs/OUTPUT.md §1.2).
func TestGlobalCounters(t *testing.T) {
	s := testServer(nil)
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("global")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/global", nil))
	var env struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if _, ok := env.Data["leap_seconds"]; !ok {
		t.Error("global feed missing leap_seconds")
	}
	if _, ok := env.Data["total_live_svs"]; !ok {
		t.Error("global feed missing total_live_svs")
	}
}

// TestObservers verifies configured ingest sources are published as observers with
// their strings sanitized (docs/OUTPUT.md §1.3, §INTEGRITY.md §9).
func TestObservers(t *testing.T) {
	s := testServer([]config.Source{
		{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"},
		{Name: "bad\x00name", Type: "sbf", Addr: "x"},
	})
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 2 {
		t.Fatalf("got %d observers, want 2", len(env.Data.Observers))
	}
	if env.Data.Observers[0].ID != "observer16" {
		t.Errorf("observer id %q", env.Data.Observers[0].ID)
	}
	// an id is an identity, not display text. It is served
	// verbatim (JSON escapes the NUL losslessly) so it still equals the identity
	// used by state, events and station selection. Stripping the NUL is what let
	// this station collide with a genuine "badname".
	if env.Data.Observers[1].ID != "bad\x00name" {
		t.Errorf("observer id %q was altered; identities must round-trip", env.Data.Observers[1].ID)
	}
}

// TestObserversRemarkNeverLeaksAddr guards the internal dial address
// (LAN topology + the exact port of an unauthenticated raw receiver TCP
// stream) must never reach the public observers feed's remark field -- only
// an operator-supplied Source.Remark may.
func TestObserversRemarkNeverLeaksAddr(t *testing.T) {
	s := testServer([]config.Source{
		{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"},
		{Name: "observer17", Type: "ubx", Addr: "192.168.1.5:2948", Remark: "roof antenna"},
	})
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	body := rr.Body.String()
	if strings.Contains(body, "10.0.0.2") || strings.Contains(body, "192.168.1.5") {
		t.Fatalf("observers body leaks a configured dial address: %s", body)
	}
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Observers[0].Remark != "" {
		t.Errorf("observer16 remark = %q, want empty (no operator remark configured)", env.Data.Observers[0].Remark)
	}
	if env.Data.Observers[1].Remark != "roof antenna" {
		t.Errorf("observer17 remark = %q, want the operator-supplied remark", env.Data.Observers[1].Remark)
	}
}

// TestObserversUnionsPushStations guards FeedStationRF/FeedCapabilityReports
// key stations by dial source OR authenticated push station id, and the detector
// already sees both -- but the observers feed must union in a push-fleet station
// that has no [[ingest]] entry at all, not just dial sources.
func TestObserversUnionsPushStations(t *testing.T) {
	s := testServer([]config.Source{{Name: "dial1", Type: "ubx", Addr: "10.0.0.2:2947"}})
	now := time.Now()
	// "pushstation1" never appears in cfg.Ingest -- only via authenticated RF telemetry.
	s.store.Apply(&ingest.RawFrame{Source: "pushstation1", Recv: now, RF: &ingest.RawRF{
		Bands: []ingest.RFBand{{Block: 0, AGC: 3000}},
	}})
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 2 {
		t.Fatalf("got %d observers, want 2 (dial1 + pushstation1): %+v", len(env.Data.Observers), env.Data.Observers)
	}
	var push *observer
	for i := range env.Data.Observers {
		if env.Data.Observers[i].ID == "pushstation1" {
			push = &env.Data.Observers[i]
		}
	}
	if push == nil {
		t.Fatal("pushstation1 missing from observers feed despite reporting RF telemetry")
	}
	if push.RF == nil {
		t.Error("pushstation1's RF data missing from its observer entry")
	}
}

// gpsLNAVWords/beidouD1Words/galileoINAVWords build the minimal Words payload
// each decoder needs to succeed (since recordCapability now fires only
// after a real decode, capability tests must route through frames the
// decoders actually accept, not an undersized placeholder). None of the
// decoded field values matter here -- only that decode succeeds.
func gpsLNAVWords() []uint32 {
	w := make([]uint32, 10)
	w[0] = 0x8B << 22 // TLM preamble
	w[1] = 1 << 8     // HOW subframe id = 1 (id must be 1..5 to decode)
	return w
}
func beidouD1Words() []uint32 {
	w := make([]uint32, 10)
	w[0] = 1 << 12 // FraID = 1 (FraID must be 1..5 to decode)
	frame.StampBeiDouD1BCH(w)
	return w
}
func galileoINAVWords() []uint32 {
	w := make([]uint32, 8)
	w[4] = 0x80000000 // odd-part Even/Odd flag (page bit 128) must be 1
	frame.StampGalileoINAVCRC(w)
	return w
}

// TestObserversCarryCapabilities confirms a station's demonstrated (gnss, sig) fingerprint
// attaches to its observer record once it has produced nav frames (docs/OUTPUT.md §1.3,
// CONSTELLATIONS §7).
func TestObserversCarryCapabilities(t *testing.T) {
	s := testServer([]config.Source{{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"}})
	now := time.Now()
	// A GPS L1 nav frame and a Galileo E1-B I/NAV frame from this station.
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.GPS, SigID: 0, Recv: now, Words: gpsLNAVWords()})
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.Galileo, SigID: 0, Recv: now, Words: galileoINAVWords()})
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 1 {
		t.Fatalf("got %d observers, want 1", len(env.Data.Observers))
	}
	caps := env.Data.Observers[0].Capabilities
	if len(caps) != 2 {
		t.Fatalf("capabilities = %+v, want 2 signals", caps)
	}
	if caps[0].Gnss != int(gnss.GPS) || caps[1].Gnss != int(gnss.Galileo) || caps[1].Sig != 0 {
		t.Errorf("capabilities not the expected sorted set: %+v", caps)
	}
}

// TestObserversCapabilityMismatch confirms a node's declared-vs-observed mismatch surfaces in
// the feed: an observed signal outside the declared set is flagged unexpected, a declared
// signal never observed is flagged missing (docs/INTEGRITY.md §6).
func TestObserversCapabilityMismatch(t *testing.T) {
	s := testServer([]config.Source{{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"}})
	// Declared: GPS L1 (0:0) and Galileo I/NAV (2:0).
	s.store.SetDeclaredCapabilities(map[string][]state.CapSignal{
		"observer16": {{Gnss: 0, Sig: 0}, {Gnss: 2, Sig: 0}},
	})
	now := time.Now()
	// Observed: GPS L1 (declared, fine) + BeiDou D1 (3:0, not declared → unexpected).
	// Galileo I/NAV is declared but never seen → missing.
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.GPS, SigID: 0, Recv: now, Words: gpsLNAVWords()})
	s.store.Apply(&ingest.RawFrame{Source: "observer16", GnssID: gnss.BeiDou, SigID: 0, Recv: now, Words: beidouD1Words()})
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	o := env.Data.Observers[0]
	if len(o.Declared) != 2 {
		t.Errorf("declared = %+v, want 2", o.Declared)
	}
	if len(o.Unexpected) != 1 || o.Unexpected[0] != (state.CapSignal{Gnss: 3, Sig: 0}) {
		t.Errorf("unexpected = %+v, want [3:0]", o.Unexpected)
	}
	if len(o.Missing) != 1 || o.Missing[0] != (state.CapSignal{Gnss: 2, Sig: 0}) {
		t.Errorf("missing = %+v, want [2:0]", o.Missing)
	}
}

// TestMethodNotAllowed verifies non-GET/HEAD requests are rejected with 405 and an
// error envelope (docs/OUTPUT.md §0).
func TestMethodNotAllowed(t *testing.T) {
	s := testServer(nil)
	s.refreshAll()
	rr := httptest.NewRecorder()
	s.serveFeed("svs")(rr, httptest.NewRequest(http.MethodPost, "/gnss/api/v2/svs", nil))
	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status %d, want 405", rr.Code)
	}
	var env struct {
		OK   bool `json:"ok"`
		Code int  `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Code != http.StatusMethodNotAllowed {
		t.Errorf("error envelope = %+v", env)
	}
}

// TestRejectedRequestBodyDrainIsBounded guards a POST that declares a body it
// never sends gets its 405 with Connection: close and the server closes the
// connection within the body read deadline, instead of parking the connection
// goroutine in net/http's post-handler body drain for as long as the client
// likes. Runs against a real listener because the drain lives in net/http's
// connection loop, not in the handler.
func TestRejectedRequestBodyDrainIsBounded(t *testing.T) {
	s := testServer(nil)
	ln, err := s.Listen()
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Start(ln) }()
	defer func() { _ = s.Close() }()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := fmt.Fprintf(conn, "POST /gnss/api/v2/svs HTTP/1.1\r\nHost: navlistener.invalid\r\nContent-Length: 200000\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("no response to a body-less POST: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed || resp.Header.Get("Allow") != "GET, HEAD" {
		t.Fatalf("status %d Allow %q, want 405 GET, HEAD", resp.StatusCode, resp.Header.Get("Allow"))
	}
	if !resp.Close {
		t.Errorf("405 did not ask to close the connection (Connection header %q)", resp.Header.Get("Connection"))
	}
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("reading the 405 body: %v", err)
	}
	// The body was never sent; the server must give up draining it and close.
	if n, err := conn.Read(make([]byte, 1)); err == nil || n != 0 {
		t.Fatalf("connection still open %v after the 405 (read %d bytes, err %v)", time.Since(start), n, err)
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("server kept the connection open past the client deadline: the body drain was not bounded")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("connection closed only after %v, want within the %v body read deadline", elapsed, bodyReadDeadline)
	}
}

// TestObserversCarryRF confirms a station's PNT-defense RF metrics attach to its observer
// record when the receiver has reported RF telemetry (docs/OUTPUT.md §1.3, DEFENSE-PNT §6).
func TestObserversCarryRF(t *testing.T) {
	s := testServer([]config.Source{{Name: "observer16", Type: "ubx", Addr: "10.0.0.2:2947"}})
	// Feed a clear-sky RF sample so the station's RF read model exists.
	for i := 0; i < 10; i++ {
		s.store.Apply(&ingest.RawFrame{
			Source: "observer16", Recv: time.Now(),
			RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 4000, AntStatus: 2}}},
		})
	}
	s.refresh("observers")
	rr := httptest.NewRecorder()
	s.serveFeed("observers")(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/observers", nil))
	var env struct {
		Data struct {
			Observers []observer `json:"observers"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Observers) != 1 || env.Data.Observers[0].RF == nil {
		t.Fatalf("observer RF not attached: %+v", env.Data.Observers)
	}
	if got := env.Data.Observers[0].RF.RFTrust; got != 1.0 {
		t.Errorf("rf_trust = %v, want 1.0 (clear sky)", got)
	}
}
