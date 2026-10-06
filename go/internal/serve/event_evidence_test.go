package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/store"
)

type eventEvidenceFunc func(context.Context, store.EvidenceQuery) (store.EventEvidence, error)

func (f eventEvidenceFunc) QueryEventEvidence(ctx context.Context, q store.EvidenceQuery) (store.EventEvidence, error) {
	return f(ctx, q)
}

func evidenceTestServer(scope identity.Audience, f eventEvidenceFunc) *Server {
	s := testServer(nil)
	s.audience = scope
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	s.SetPolicyEpochs(audience.NewPolicyEpochs(now.Add(-24 * time.Hour)))
	s.SetEventEvidence(f)
	s.EnableAudienceSelection(fixedReadAuthorizer{"token": {
		ID: "reader-a", Revision: "rev-1", AudienceGrants: []identity.Audience{scope},
	}}, nil, time.Second)
	return s
}

func evidenceRequest(s *Server, params, token, scope string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/gnss/api/v2/event-evidence?"+params, nil)
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

func TestEventEvidenceServedToItsAudience(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOrganization, ID: "org-a"}
	var query store.EvidenceQuery
	eventTime := time.Date(2026, 10, 5, 11, 0, 0, 0, time.UTC)
	s := evidenceTestServer(scope, func(ctx context.Context, q store.EvidenceQuery) (store.EventEvidence, error) {
		query = q
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > 5*time.Second {
			t.Fatal("missing query deadline")
		}
		seq := "7"
		return store.EventEvidence{
			Event:       store.StoredEvent{ID: q.Seq, Time: eventTime, SV: "board-0001-aa", Type: "spoofing_suspected"},
			WindowStart: eventTime.Add(-10 * time.Minute), WindowEnd: eventTime.Add(time.Minute), RFSamples: 1,
			Samples: []store.EvidenceSample{{Origin: "rf", Kind: "solution", ReceivedAt: eventTime, Sequence: &seq,
				HardwareTrust: "trusted", Raw: []byte{1, 7}, Data: json.RawMessage(`{"pvt":{}}`)}},
			HasMore: true,
		}, nil
	})
	w := evidenceRequest(s, "id=42&limit=1", "token", scope.Key())
	data := decodeEnvelope(t, w)
	if query.Audience != scope || query.Seq != 42 || query.Limit != 1 || query.Offset != 0 {
		t.Fatalf("query = %+v", query)
	}
	evidence := data["evidence"].(map[string]any)
	sample := evidence["samples"].([]any)[0].(map[string]any)
	if data["next_offset"] != float64(1) || evidence["event"].(map[string]any)["type"] != "spoofing_suspected" ||
		sample["raw"] != "AQc=" || sample["sequence"] != "7" {
		t.Fatalf("envelope: %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "private, no-store" || !strings.Contains(w.Header().Get("Vary"), "Authorization") {
		t.Fatal("cache scope missing")
	}
}

func TestEventEvidenceRejects(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOperator, ID: "collector-a"}
	called := false
	s := evidenceTestServer(scope, func(context.Context, store.EvidenceQuery) (store.EventEvidence, error) {
		called = true
		return store.EventEvidence{}, store.ErrNoEvidence
	})
	for name, tc := range map[string]struct {
		params, token string
		code          int
	}{
		"anonymous":     {"id=1", "", 401},
		"missing id":    {"limit=1", "token", 400},
		"bad id":        {"id=x", "token", 400},
		"unknown param": {"id=1&station=x", "token", 400},
		"limit":         {"id=1&limit=501", "token", 400},
	} {
		called = false
		if w := evidenceRequest(s, tc.params, tc.token, scope.Key()); w.Code != tc.code || called {
			t.Errorf("%s: %d (store called %v), want %d", name, w.Code, called, tc.code)
		}
	}
	if w := evidenceRequest(s, "id=9", "token", scope.Key()); w.Code != 404 || !called {
		t.Fatalf("missing evidence: %d", w.Code)
	}
}

// TestEventEvidenceHidesEventsBeforePolicyEpoch: an event older than the audience's
// current policy epoch is as invisible here as in the events API.
func TestEventEvidenceHidesEventsBeforePolicyEpoch(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOperator, ID: "collector-a"}
	s := evidenceTestServer(scope, func(_ context.Context, q store.EvidenceQuery) (store.EventEvidence, error) {
		return store.EventEvidence{Event: store.StoredEvent{ID: q.Seq, Time: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}}, nil
	})
	if w := evidenceRequest(s, "id=1", "token", scope.Key()); w.Code != 404 {
		t.Fatalf("pre-epoch event served: %d %s", w.Code, w.Body.String())
	}
}

func TestEventEvidenceUnavailableWithoutHistorian(t *testing.T) {
	scope := identity.Audience{Kind: identity.AudienceOperator, ID: "collector-a"}
	s := evidenceTestServer(scope, nil)
	s.eventEvidence = nil
	if w := evidenceRequest(s, "id=1", "token", scope.Key()); w.Code != 503 {
		t.Fatalf("no historian: %d", w.Code)
	}
}
