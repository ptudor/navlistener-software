package server

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthFailsAfterRequiredComponentFailure(t *testing.T) {
	s := New("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	check := func(wantCode int, wantStatus string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		if w.Code != wantCode || !strings.Contains(w.Body.String(), `"status":"`+wantStatus+`"`) {
			t.Fatalf("health = %d %s, want %d status=%s", w.Code, w.Body.String(), wantCode, wantStatus)
		}
	}
	check(http.StatusOK, "ok")
	s.Fail(errors.New("push endpoint: terminal accept failure"))
	check(http.StatusServiceUnavailable, "failed")
}

// TestHealthzDegradedFromDataPlaneProbes guards /healthz must reflect
// data-plane liveness probes, not just listener termination — and a degraded
// probe must answer 200 (visible warning, no supervisor flap), while Fail()
// (a terminated required component) still trumps everything with a 503.
func TestHealthzDegradedFromDataPlaneProbes(t *testing.T) {
	s := New("127.0.0.1:0", slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	reason := ""
	s.AddProbe("ingest", func() string { return reason })

	get := func() (int, string) {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		w := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}

	if code, body := get(); code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("healthy probe: got %d %s, want 200 ok", code, body)
	}

	reason = "no frames ingested for 7m30s"
	code, body := get()
	if code != http.StatusOK {
		t.Errorf("degraded probe: got %d, want 200 (degraded must not flap the supervisor)", code)
	}
	if !strings.Contains(body, `"status":"degraded"`) || !strings.Contains(body, "no frames ingested for 7m30s") {
		t.Errorf("degraded body = %s, want status=degraded with the probe's reason", body)
	}

	// Recovery clears it without restart.
	reason = ""
	if code, body := get(); code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Errorf("recovered probe: got %d %s, want 200 ok", code, body)
	}

	// A terminated required component still beats degraded, with a 503.
	reason = "historian: down"
	s.Fail(errors.New("v2 serve: listener terminated"))
	if code, body := get(); code != http.StatusServiceUnavailable || !strings.Contains(body, `"status":"failed"`) {
		t.Errorf("failed+degraded: got %d %s, want 503 failed (failure trumps degraded)", code, body)
	}
}
