package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
)

// Coverage is served from a per-audience cache with a serve-time TTL: inside
// the TTL the admitted body is reused even though its witnesses have aged
// (ageing is the client's job, docs/MONITORING-MAP.md); past the TTL the next
// request rebuilds it at the current time; a reset invalidates it at once.
func TestMonitoringFeedUsesViewAndCurrentTime(t *testing.T) {
	s := testServer(nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	// A witness 45 s old: fresh at the first render, aged out (> 60 s) 20 s later.
	s.store.Apply(&ingest.RawFrame{Source: "station", GnssID: gnss.GPS, SvID: 5, SigID: 0, Recv: now.Add(-45 * time.Second), Words: gpsLNAVWords()})
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
	witnesses := func(body envelope) int {
		records := body.Data["observations"].([]any)
		if len(records) != 1 {
			t.Fatalf("observations = %v, want the one witnessed SV", records)
		}
		return len(records[0].(map[string]any)["witness_times"].([]any))
	}
	first := read()
	if witnesses(first) != 1 {
		t.Fatalf("nav-only evidence missing: %v", first.Data["observations"])
	}
	// 20 s later the witness is 65 s old, but the cached body is 20 s old:
	// witness age alone does not rebuild it.
	now = now.Add(20 * time.Second)
	second := read()
	if second.Time != first.Time || witnesses(second) != 1 {
		t.Fatalf("coverage re-rendered inside its TTL: time %s -> %s, witnesses %d", first.Time, second.Time, witnesses(second))
	}
	// 31 s after the render the entry is past coverageTTL: rebuilt now, and the
	// rebuilt body no longer carries the aged witness.
	now = now.Add(11 * time.Second)
	third := read()
	if third.Time == first.Time {
		t.Fatal("coverage served past its TTL")
	}
	if witnesses(third) != 0 {
		t.Fatal("rebuilt coverage kept an aged witness")
	}
	s.store.Reset()
	if len(read().Data["observations"].([]any)) != 0 {
		t.Fatal("withdrawn observations survived reset")
	}
}

// TestCoverageRendersOncePerTTLUnderConcurrentPublicRequests guards the public
// coverage path is single-flight and TTL-cached: a burst of concurrent
// anonymous requests walks the state shards and propagates the reference
// catalogue once, and again only once the TTL has elapsed.
func TestCoverageRendersOncePerTTLUnderConcurrentPublicRequests(t *testing.T) {
	s := newTestServerForAudience(identity.Audience{Kind: identity.AudiencePublic}, nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	s.store.Apply(&ingest.RawFrame{Source: "station", GnssID: gnss.GPS, SvID: 5, SigID: 0, Recv: now, Words: gpsLNAVWords()})
	var renders atomic.Int64
	s.onBuildFeed = func(feed string, _ identity.Audience) {
		if feed == "coverage" {
			renders.Add(1)
		}
	}
	burst := func() {
		var wg sync.WaitGroup
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				rr := httptest.NewRecorder()
				s.http.Handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/gnss/api/v2/coverage", nil))
				if rr.Code != http.StatusOK {
					t.Errorf("coverage status %d: %s", rr.Code, rr.Body.String())
				}
				if got := rr.Header().Get("Cache-Control"); got != "public, max-age=30" {
					t.Errorf("public coverage Cache-Control = %q", got)
				}
			}()
		}
		wg.Wait()
	}
	burst()
	if got := renders.Load(); got != 1 {
		t.Fatalf("concurrent cold requests rendered coverage %d times, want 1", got)
	}
	now = now.Add(coverageTTL / 2)
	burst()
	if got := renders.Load(); got != 1 {
		t.Fatalf("requests inside the TTL rendered coverage again (%d renders)", got)
	}
	now = now.Add(coverageTTL)
	burst()
	if got := renders.Load(); got != 2 {
		t.Fatalf("requests past the TTL rendered coverage %d times in total, want 2", got)
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
