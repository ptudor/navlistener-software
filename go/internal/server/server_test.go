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
