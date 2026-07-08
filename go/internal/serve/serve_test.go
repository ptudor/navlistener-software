package serve

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/state"
)

func testServer(sources []config.Source) *Server {
	return newTestServer(sources, nil)
}

func newTestServer(sources []config.Source, events EventStore) *Server {
	return New("127.0.0.1:0", state.New(4), events, sources, time.Minute, time.Minute,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
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
	if env.Data.Observers[1].ID != "badname" { // NUL stripped by sanitize
		t.Errorf("unsanitized observer id %q", env.Data.Observers[1].ID)
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
