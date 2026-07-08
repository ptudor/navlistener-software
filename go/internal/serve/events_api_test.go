package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/store"
)

// fakeEvents is an in-memory EventStore for the query-API tests: it records the last query
// it received and returns canned results, so the handler's param parsing and envelope
// shaping are tested without a database.
type fakeEvents struct {
	lastQuery store.EventQuery
	events    []store.StoredEvent
	total     int
	summary   store.EventSummary
	err       error
}

func (f *fakeEvents) QueryEvents(_ context.Context, q store.EventQuery) ([]store.StoredEvent, int, error) {
	f.lastQuery = q
	return f.events, f.total, f.err
}

func (f *fakeEvents) SummarizeEvents(_ context.Context, _, _ time.Time) (store.EventSummary, error) {
	return f.summary, f.err
}

func decodeEnvelope(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if rr.Code != http.StatusOK {
		t.Fatalf("status %d, want 200: %s", rr.Code, rr.Body.String())
	}
	var env struct {
		OK   bool           `json:"ok"`
		Time string         `json:"time"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if !env.OK || env.Data["schema"] != schemaVersion {
		t.Fatalf("envelope = %+v, want ok+schema %s", env, schemaVersion)
	}
	return env.Data
}

// TestEventsQueryDisabled confirms the endpoints report unavailable when no historian is
// wired (persist-less dev), rather than panicking on a nil store.
func TestEventsQueryDisabled(t *testing.T) {
	s := testServer(nil) // events == nil
	for _, path := range []string{"/gnss/api/events", "/gnss/api/events/summary"} {
		rr := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusServiceUnavailable {
			t.Errorf("%s: status %d, want 503", path, rr.Code)
		}
	}
}

// TestEventsQueryParamsAndShape drives the query handler: params map onto the store query,
// and the response carries the envelope with total + the typed events.
func TestEventsQueryParamsAndShape(t *testing.T) {
	fe := &fakeEvents{
		total: 3,
		events: []store.StoredEvent{
			{ID: 42, Time: time.Unix(1000, 0).UTC(), SV: "E14@1", Type: "orbit_disco", Severity: 1, Message: "disco"},
		},
	}
	s := newTestServer(nil, fe)

	req := httptest.NewRequest(http.MethodGet,
		"/gnss/api/events?sv=E14@1&type=orbit_disco&severity=1&limit=9999&offset=5", nil)
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, req)
	data := decodeEnvelope(t, rr)

	// Params reached the store, with limit clamped to the max and the filters passed through.
	if fe.lastQuery.SV != "E14@1" || fe.lastQuery.Type != "orbit_disco" || fe.lastQuery.MinSeverity != 1 {
		t.Errorf("filters not passed: %+v", fe.lastQuery)
	}
	if fe.lastQuery.Limit != eventsMaxLimit {
		t.Errorf("limit = %d, want clamp to %d", fe.lastQuery.Limit, eventsMaxLimit)
	}
	if fe.lastQuery.Offset != 5 {
		t.Errorf("offset = %d, want 5", fe.lastQuery.Offset)
	}
	if total, _ := data["total"].(float64); int(total) != 3 {
		t.Errorf("total = %v, want 3", data["total"])
	}
	evs, _ := data["events"].([]any)
	if len(evs) != 1 {
		t.Fatalf("events len = %d, want 1", len(evs))
	}
	ev0, _ := evs[0].(map[string]any)
	if ev0["sv"] != "E14@1" || ev0["type"] != "orbit_disco" {
		t.Errorf("event0 = %+v", ev0)
	}
}

// TestEventsQueryEmpty confirms an empty result serializes as [] (not null), so consumers
// can iterate without a nil guard.
func TestEventsQueryEmpty(t *testing.T) {
	s := newTestServer(nil, &fakeEvents{total: 0, events: nil})
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events", nil))
	data := decodeEnvelope(t, rr)
	evs, ok := data["events"].([]any)
	if !ok || len(evs) != 0 {
		t.Errorf("events = %#v, want empty array", data["events"])
	}
}

// TestEventsSummaryShape drives the summary handler: hours clamps, and the aggregates and
// idle message land in the envelope.
func TestEventsSummaryShape(t *testing.T) {
	last := time.Unix(2000, 0).UTC()
	fe := &fakeEvents{summary: store.EventSummary{
		TotalEvents: 5, ActiveCritical: 2, ActiveWarnings: 3,
		LastCritical:    &last,
		ByType:          map[string]int{"orbit_disco": 4, "clock_jump": 1},
		ByConstellation: map[string]int{"galileo": 5},
	}}
	s := newTestServer(nil, fe)

	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events/summary?hours=99999", nil))
	data := decodeEnvelope(t, rr)

	if ph, _ := data["period_hours"].(float64); int(ph) != summaryMaxHours {
		t.Errorf("period_hours = %v, want clamp to %d", data["period_hours"], summaryMaxHours)
	}
	if tot, _ := data["total_events"].(float64); int(tot) != 5 {
		t.Errorf("total_events = %v, want 5", data["total_events"])
	}
	if data["last_critical"] != last.Format(time.RFC3339) {
		t.Errorf("last_critical = %v, want %s", data["last_critical"], last.Format(time.RFC3339))
	}
	if data["idle_message"] != nil {
		t.Errorf("idle_message = %v, want null when events exist", data["idle_message"])
	}
	byType, _ := data["by_type"].(map[string]any)
	if v, _ := byType["orbit_disco"].(float64); int(v) != 4 {
		t.Errorf("by_type[orbit_disco] = %v, want 4", byType["orbit_disco"])
	}
}

// TestEventsSummaryIdle confirms an empty window reports a null last_critical and an idle
// message naming the actual window (not a hardcoded 24h).
func TestEventsSummaryIdle(t *testing.T) {
	s := newTestServer(nil, &fakeEvents{summary: store.EventSummary{
		ByType: map[string]int{}, ByConstellation: map[string]int{},
	}})
	rr := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/events/summary?hours=6", nil))
	data := decodeEnvelope(t, rr)
	if data["last_critical"] != nil {
		t.Errorf("last_critical = %v, want null", data["last_critical"])
	}
	msg, _ := data["idle_message"].(string)
	if msg != "No events in the last 6 hours" {
		t.Errorf("idle_message = %q, want the 6-hour message", msg)
	}
}
