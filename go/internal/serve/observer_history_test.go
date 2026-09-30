package serve

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/store"
)

type observerHistoryFunc func(context.Context, store.ObserverSampleQuery) (store.ObserverSamplePage, error)

func (f observerHistoryFunc) QueryObserverSamples(ctx context.Context, q store.ObserverSampleQuery) (store.ObserverSamplePage, error) {
	return f(ctx, q)
}

func sensorTestServer(scope identity.Audience, f observerHistoryFunc) *Server {
	s := testServer(nil)
	s.audience = scope
	now := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.SetPolicyEpochs(audience.NewPolicyEpochs(now.Add(-24 * time.Hour)))
	s.SetObserverHistory(f, "collector-a")
	s.EnableAudienceSelection(fixedReadAuthorizer{"token": {
		ID: "reader-a", Revision: "rev-1", AudienceGrants: []identity.Audience{scope},
	}}, nil, time.Second)
	return s
}

func sensorRequest(s *Server, method, params, token, scope string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/gnss/api/v2/observer-samples?"+params, nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if scope != "" {
		r.Header.Set("X-GNSS-Audience", scope)
	}
	w := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(w, r)
	return w
}

func TestObserverSamplesScopesAndEnvelope(t *testing.T) {
	for _, scope := range []identity.Audience{
		{Kind: identity.AudienceOperator, ID: "collector-a"},
		{Kind: identity.AudienceOrganization, ID: "org-a"},
		{Kind: identity.AudienceCollection, ID: "group-a"},
	} {
		t.Run(scope.Key(), func(t *testing.T) {
			observer := "receiver: 001/東京"
			var query store.ObserverSampleQuery
			s := sensorTestServer(scope, func(ctx context.Context, q store.ObserverSampleQuery) (store.ObserverSamplePage, error) {
				query = q
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
					t.Fatal("missing query deadline")
				}
				seq := "18446744073709551615"
				return store.ObserverSamplePage{Samples: []store.StoredObserverSample{{ReceivedAt: q.Since, Sequence: &seq, HardwareTrust: "trusted", Details: json.RawMessage(`{"environment":{"mcp9808_c":null}}`)}}, HasMore: true}, nil
			})
			params := "observer=" + url.QueryEscape(observer) + "&kind=timing&limit=1"
			w := sensorRequest(s, "GET", params, "token", scope.Key())
			data := decodeEnvelope(t, w)
			if query.Observer != observer || query.Audience != scope || query.CollectorID != "collector-a" || query.Kind != "timing" || query.Limit != 1 || !query.Since.Equal(s.now().Add(-time.Hour)) {
				t.Fatalf("query changed: %+v", query)
			}
			if data["observer"] != observer || data["audience"] != scope.Key() || data["next_offset"] != float64(1) || data["has_more"] != true || data["history_limited"] != false {
				t.Fatalf("bad envelope: %s", w.Body.String())
			}
			sample := data["samples"].([]any)[0].(map[string]any)
			if sample["sample_time"] != nil || sample["session"] != nil || sample["sequence"] != "18446744073709551615" {
				t.Fatalf("lost null/sequence: %+v", sample)
			}
			if w.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(w.Header().Get("Vary"), "Authorization") {
				t.Fatal("cache scope missing")
			}
			w = sensorRequest(s, "GET", params+"&offset=1&revision="+data["revision"].(string), "token", scope.Key())
			if w.Code != 200 || query.Offset != 1 {
				t.Fatalf("pagination failed: %s", w.Body.String())
			}
			w = sensorRequest(s, "HEAD", params, "token", scope.Key())
			if w.Code != 200 || w.Body.Len() != 0 {
				t.Fatalf("HEAD returned body: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestObserverSamplesRejectRequestsBeforeDatabase(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"}
	for _, tc := range []struct {
		name, method, params, token, scope string
		status                             int
		alter                              func(*Server)
	}{
		{"missing token", "GET", "observer=x", "", "", 401, nil},
		{"invalid token", "GET", "observer=x", "wrong", "", 401, nil},
		{"other owner", "GET", "observer=x", "token", "organization:org-b", 403, nil},
		{"operator escalation", "GET", "observer=x", "token", "operator:collector-a", 403, nil},
		{"public", "GET", "observer=x", "token", "", 403, func(s *Server) { s.audience = identity.Audience{Kind: identity.AudiencePublic} }},
		{"no authorizer", "GET", "observer=x", "token", "", 503, func(s *Server) { s.readAuth = nil }},
		{"no historian", "GET", "observer=x", "token", "", 503, func(s *Server) { s.observerHistory = nil }},
		{"method", "POST", "observer=x", "token", "", 405, nil},
		{"no observer", "GET", "", "token", "", 400, nil},
		{"duplicate observer", "GET", "observer=x&observer=y", "token", "", 400, nil},
		{"unknown scope filter", "GET", "observer=x&organization_id=org-b", "token", "", 400, nil},
		{"malformed URL", "GET", "observer=%ZZ", "token", "", 400, nil},
		{"malformed UTF8", "GET", "observer=%FF", "token", "", 400, nil},
		{"NUL", "GET", "observer=a%00b", "token", "", 400, nil},
		{"oversized query", "GET", "observer=" + strings.Repeat("a", 17*1024), "token", "", 400, nil},
		{"oversized observer", "GET", "observer=" + strings.Repeat("a", 4097), "token", "", 400, nil},
		{"invalid kind", "GET", "observer=x&kind=raw", "token", "", 400, nil},
		{"blank kind", "GET", "observer=x&kind=", "token", "", 400, nil},
		{"invalid since", "GET", "observer=x&since=yesterday", "token", "", 400, nil},
		{"inverted window", "GET", "observer=x&since=2026-09-21T00:00:00Z", "token", "", 400, nil},
		{"large window", "GET", "observer=x&since=2026-01-01T00:00:00Z", "token", "", 400, nil},
		{"zero limit", "GET", "observer=x&limit=0", "token", "", 400, nil},
		{"large limit", "GET", "observer=x&limit=501", "token", "", 400, nil},
		{"bad limit", "GET", "observer=x&limit=many", "token", "", 400, nil},
		{"negative offset", "GET", "observer=x&offset=-1", "token", "", 400, nil},
		{"large offset", "GET", "observer=x&offset=100001", "token", "", 400, nil},
		{"stale revision", "GET", "observer=x&revision=old", "token", "", 409, nil},
		{"busy", "GET", "observer=x", "token", "", 503, func(s *Server) {
			for i := 0; i < cap(s.historySlots); i++ {
				s.historySlots <- struct{}{}
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := sensorTestServer(scope, func(context.Context, store.ObserverSampleQuery) (store.ObserverSamplePage, error) {
				t.Fatal("denied request reached database")
				return store.ObserverSamplePage{}, nil
			})
			if tc.alter != nil {
				tc.alter(s)
			}
			w := sensorRequest(s, tc.method, tc.params, tc.token, tc.scope)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.status, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatal("cacheable error")
			}
		})
	}
}

func TestObserverSamplesPolicyWindow(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"}
	var query store.ObserverSampleQuery
	calls := 0
	s := sensorTestServer(scope, func(_ context.Context, q store.ObserverSampleQuery) (store.ObserverSamplePage, error) {
		calls++
		query = q
		return store.ObserverSamplePage{}, nil
	})
	transition := s.now().Add(-30 * time.Minute)
	s.policyEpochs.Advance([]identity.Audience{scope}, transition)
	w := sensorRequest(s, "GET", "observer=offline", "token", "")
	data := decodeEnvelope(t, w)
	if !query.Since.Equal(transition) || data["history_limited"] != true || data["effective_since"] != transition.Format(time.RFC3339) || len(data["samples"].([]any)) != 0 {
		t.Fatalf("boundary missing: %s", w.Body.String())
	}
	w = sensorRequest(s, "GET", "observer=offline&until=2026-09-19T00:00:00Z", "token", "")
	data = decodeEnvelope(t, w)
	if calls != 1 || data["history_limited"] != true || len(data["samples"].([]any)) != 0 {
		t.Fatal("old process history was queried")
	}
	w = sensorRequest(s, "GET", "observer=offline&until=2099-01-01T00:00:00Z", "token", "")
	decodeEnvelope(t, w)
	if !query.Until.Equal(s.now()) {
		t.Fatal("future upper bound not clamped")
	}
}

func TestObserverSamplesDropsRevokedResults(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"}
	for _, reason := range []string{"revoke", "revision", "grant", "policy"} {
		t.Run(reason, func(t *testing.T) {
			var s *Server
			s = sensorTestServer(scope, func(context.Context, store.ObserverSampleQuery) (store.ObserverSamplePage, error) {
				auth := s.readAuth.(fixedReadAuthorizer)
				p := auth["token"]
				switch reason {
				case "revoke":
					delete(auth, "token")
				case "revision":
					p.Revision = "rev-2"
					auth["token"] = p
				case "grant":
					p.AudienceGrants = nil
					auth["token"] = p
				case "policy":
					s.policyEpochs.Advance([]identity.Audience{scope}, s.now())
					s.InvalidateAudiences([]identity.Audience{scope})
				}
				return store.ObserverSamplePage{Samples: []store.StoredObserverSample{{Details: json.RawMessage(`{"secret":"withheld"}`)}}}, nil
			})
			w := sensorRequest(s, "GET", "observer=x", "token", "")
			want := 403
			if reason == "policy" {
				want = 503
			}
			if w.Code != want || strings.Contains(w.Body.String(), "withheld") {
				t.Fatalf("revoked results escaped: %d %s", w.Code, w.Body.String())
			}
			if len(s.historySlots) != 0 || len(s.deliveries) != 0 {
				t.Fatal("request resources leaked")
			}
		})
	}
}

func TestObserverSamplesDatabaseErrors(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{errors.New("private database connection details"), 500}, {context.DeadlineExceeded, 504}} {
		s := sensorTestServer(identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"}, func(context.Context, store.ObserverSampleQuery) (store.ObserverSamplePage, error) {
			return store.ObserverSamplePage{}, tc.err
		})
		w := sensorRequest(s, "GET", "observer=x", "token", "")
		if w.Code != tc.status || strings.Contains(w.Body.String(), "private database") {
			t.Fatalf("error escaped: %d %s", w.Code, w.Body.String())
		}
	}
}
