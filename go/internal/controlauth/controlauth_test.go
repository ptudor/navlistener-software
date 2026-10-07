package controlauth

import (
	"bytes"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func request(remote string) *http.Request {
	r := httptest.NewRequest("POST", "/gnss/api/v2/updates", nil)
	r.RemoteAddr = remote
	return r
}

// Every refusal counts; the log names a remote address once per interval and
// per endpoint, and names it again once the interval has passed.
func TestEveryRefusalCountsAndTheLogIsBoundedPerRemote(t *testing.T) {
	var logged bytes.Buffer
	l := New(slog.New(slog.NewTextHandler(&logged, nil)))
	now := time.Unix(1_800_000_000, 0)
	l.now = func() time.Time { return now }
	before := testutil.ToFloat64(FailuresTotal.WithLabelValues("updates"))
	for port := 40000; port < 40005; port++ {
		l.Failed("updates", request(fmt.Sprintf("198.51.100.7:%d", port)))
	}
	if got := testutil.ToFloat64(FailuresTotal.WithLabelValues("updates")); got != before+5 {
		t.Fatalf("failures = %v, want %v", got, before+5)
	}
	if lines := strings.Count(logged.String(), "control credential refused"); lines != 1 {
		t.Fatalf("one remote logged %d lines within the interval:\n%s", lines, logged.String())
	}
	if !strings.Contains(logged.String(), "remote=198.51.100.7") || strings.Contains(logged.String(), ":4000") {
		t.Fatalf("log must name the host without its port:\n%s", logged.String())
	}
	// Another remote, and the same remote on another endpoint, each get a line.
	l.Failed("updates", request("198.51.100.8:1"))
	l.Failed("station-snapshot", request("198.51.100.7:1"))
	if lines := strings.Count(logged.String(), "control credential refused"); lines != 3 {
		t.Fatalf("distinct remote and endpoint logged %d lines, want 3", lines)
	}
	now = now.Add(WarnEvery)
	l.Failed("updates", request("198.51.100.7:1"))
	if lines := strings.Count(logged.String(), "control credential refused"); lines != 4 {
		t.Fatalf("the interval passing did not allow a new line: %d", lines)
	}
}

// The remembered addresses are bounded; past the bound a new address is still
// logged rather than silently dropped.
func TestRememberedRemotesAreBounded(t *testing.T) {
	var logged bytes.Buffer
	l := New(slog.New(slog.NewTextHandler(&logged, nil)))
	now := time.Unix(1_800_000_000, 0)
	l.now = func() time.Time { return now }
	for i := 0; i < maxRemotes+10; i++ {
		l.Failed("updates", request(fmt.Sprintf("[2001:db8::%x]:9", i+1)))
	}
	if len(l.last) > maxRemotes {
		t.Fatalf("%d remotes remembered, bound is %d", len(l.last), maxRemotes)
	}
	if lines := strings.Count(logged.String(), "control credential refused"); lines != maxRemotes+10 {
		t.Fatalf("a new remote past the bound was not logged: %d lines", lines)
	}
}
