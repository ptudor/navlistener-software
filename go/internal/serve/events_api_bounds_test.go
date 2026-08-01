package serve

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestEventsQueryFilterLengthBounds guards the client-supplied sv/type
// filters must be length-bounded before they become DB bind values — a multi-MB
// param was pure nuisance amplification (compared per row under count(*) OVER()).
func TestEventsQueryFilterLengthBounds(t *testing.T) {
	fe := &fakeEvents{}
	s := newTestServer(nil, fe)

	for _, tc := range []struct{ name, query string }{
		{"sv too long", "sv=" + strings.Repeat("G", eventsMaxSVParam+1)},
		{"type too long", "type=" + strings.Repeat("t", eventsMaxTypeParam+1)},
	} {
		rr := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events?"+tc.query, nil))
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", tc.name, rr.Code)
		}
	}

	// Maximum-length values still pass (bounds are limits, not off-by-one traps).
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/gnss/api/events?sv="+strings.Repeat("G", eventsMaxSVParam)+"&type="+strings.Repeat("t", eventsMaxTypeParam), nil))
	if rr.Code != http.StatusOK {
		t.Errorf("at-limit filters: status %d, want 200: %s", rr.Code, rr.Body.String())
	}

	// for station-scoped events (jamming/spoofing/rf/antenna/offline)
	// the sv column holds a station id, which under the documented fleet naming
	// is a DNS FQDN — the exact triage filter the old 32-byte cap rejected.
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/gnss/api/events?sv=rx-observer16.example.invalid", nil))
	if rr.Code != http.StatusOK {
		t.Errorf("station-id sv filter: status %d, want 200: %s", rr.Code, rr.Body.String())
	}
}

// TestEventsQueryFutureSinceWithOmittedUntil guards since=<future> with
// no until previously skipped the inversion guard (it required BOTH params),
// producing an inverted window and a confidently-wrong empty ok:true result —
// the exact class regression fix/regression fix were written to prevent.
func TestEventsQueryFutureSinceWithOmittedUntil(t *testing.T) {
	fe := &fakeEvents{}
	s := newTestServer(nil, fe)

	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/gnss/api/events?since=3000-01-01T00:00:00Z", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("future since, omitted until: status %d, want 400 (got body %s)", rr.Code, rr.Body.String())
	}

	// The both-present inverted case still 400s (unchanged behavior).
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/gnss/api/events?since=2026-01-02T00:00:00Z&until=2026-01-01T00:00:00Z", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("inverted since/until: status %d, want 400", rr.Code)
	}

	// A past since with an omitted until remains a valid window.
	pastSince := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rr = httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet,
		"/gnss/api/events?since="+pastSince, nil))
	if rr.Code != http.StatusOK {
		t.Errorf("past since, omitted until: status %d, want 200: %s", rr.Code, rr.Body.String())
	}
}
