package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

func TestMonitoringFeedUsesViewAndCurrentTime(t *testing.T) {
	s := testServer(nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.store.Apply(&ingest.RawFrame{Source: "station", GnssID: gnss.GPS, SvID: 5, SigID: 0, Recv: now, Words: gpsLNAVWords()})
	read := func() envelope {
		rr := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rr, httptest.NewRequest("GET", "/gnss/api/v2/coverage", nil))
		if rr.Code != 200 {
			t.Fatal(rr.Body.String())
		}
		if rr.Header().Get("Cache-Control") != "private, no-store" {
			t.Fatal("private response is cacheable")
		}
		var body envelope
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	body := read()
	records := body.Data["observations"].([]any)
	if len(records) != 1 || len(records[0].(map[string]any)["witness_times"].([]any)) != 1 {
		t.Fatalf("nav-only evidence missing: %v", records)
	}
	now = now.Add(61 * time.Second)
	body = read()
	records = body.Data["observations"].([]any)
	if len(records[0].(map[string]any)["witness_times"].([]any)) != 0 {
		t.Fatal("coverage reused cached fresh evidence")
	}
	s.store.Reset()
	if len(read().Data["observations"].([]any)) != 0 {
		t.Fatal("withdrawn observations survived reset")
	}
}

func TestMonitoringFeedCannotSelectUnauthorizedAudience(t *testing.T) {
	s := testServer(nil)
	s.EnableAudienceSelection(fixedReadAuthorizer{"token": identity.ReadPrincipal{ID: "viewer"}}, nil, time.Second)
	for _, token := range []string{"", "token"} {
		r := httptest.NewRequest("GET", "/gnss/api/v2/coverage", nil)
		r.Header.Set("X-GNSS-Audience", "organization:other")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		rr := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rr, r)
		if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
			t.Fatalf("unauthorized coverage: %d", rr.Code)
		}
	}
}
