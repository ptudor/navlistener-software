package main

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ptudor/navlistener/internal/state"
)

// TestDebugStatePeerGate guards security half, previously untested
// : /debug/state serves a full live-state + source-name dump, and the
// loopback-peer gate is the ONLY control keeping it off a deliberately
// non-loopback [metrics].addr. The risk is future regression — a refactor that
// trusts a forwarding header, mishandles a bracketed IPv6 RemoteAddr, or reorders
// the gate after the body write — so each of those is pinned here.
func TestDebugStatePeerGate(t *testing.T) {
	h := newDebugStateHandler(state.New(2), slog.New(slog.NewTextHandler(io.Discard, nil)))

	cases := []struct {
		name       string
		remoteAddr string
		headers    map[string]string
		want       int
	}{
		{"ipv4 loopback", "127.0.0.1:51234", nil, http.StatusOK},
		{"ipv4 loopback range", "127.2.3.4:51234", nil, http.StatusOK},
		{"ipv6 loopback bracketed", "[::1]:51234", nil, http.StatusOK},
		{"public peer", "203.0.113.9:51234", nil, http.StatusForbidden},
		{"public ipv6 peer", "[2001:db8::1]:51234", nil, http.StatusForbidden},
		// A proxy header must never launder a remote peer into "loopback": XFF is
		// attacker-settable and the gate reads the transport peer only.
		{"forwarded-for spoof", "203.0.113.9:51234", map[string]string{
			"X-Forwarded-For": "127.0.0.1", "X-Real-IP": "127.0.0.1"}, http.StatusForbidden},
		// Anything net.SplitHostPort cannot parse is denied (fail closed).
		{"unparsable remote addr", "not-an-address", nil, http.StatusForbidden},
		{"host without port", "127.0.0.1", nil, http.StatusForbidden},
		{"empty remote addr", "", nil, http.StatusForbidden},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/debug/state", nil)
			r.RemoteAddr = tc.remoteAddr
			for k, v := range tc.headers {
				r.Header.Set(k, v)
			}
			w := httptest.NewRecorder()
			h(w, r)
			if w.Code != tc.want {
				t.Fatalf("RemoteAddr %q → %d, want %d", tc.remoteAddr, w.Code, tc.want)
			}
			if tc.want == http.StatusForbidden {
				// The gate must run BEFORE the snapshot is written: a 403 body may
				// not contain any live-state JSON.
				if ct := w.Header().Get("Content-Type"); ct == "application/json" {
					t.Errorf("403 response carried a JSON state body (Content-Type %q)", ct)
				}
			}
		})
	}
}
