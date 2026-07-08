package serve

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncRecorder is a concurrency-safe http.ResponseWriter+Flusher, so the test
// goroutine can observe the stream while the handler goroutine writes to it.
type syncRecorder struct {
	mu     sync.Mutex
	buf    bytes.Buffer
	header http.Header
}

func newSyncRecorder() *syncRecorder { return &syncRecorder{header: http.Header{}} }

func (s *syncRecorder) Header() http.Header { return s.header }
func (s *syncRecorder) WriteHeader(int)     {}
func (s *syncRecorder) Flush()              {}
func (s *syncRecorder) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}
func (s *syncRecorder) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestBrokerReplayFrom verifies the reconnect-replay windows: no cursor returns the
// recent tail; a Last-Event-ID cursor returns only later events.
func TestBrokerReplayFrom(t *testing.T) {
	b := newBroker()
	for i := int64(1); i <= 5; i++ {
		b.Publish(EventMsg{ID: i, Type: "orbit_disco"})
	}
	if got := b.replayFrom(0, false); len(got) != 5 {
		t.Errorf("no-cursor replay = %d events, want 5", len(got))
	}
	got := b.replayFrom(3, true)
	if len(got) != 2 || got[0].ID != 4 || got[1].ID != 5 {
		t.Errorf("cursor replay = %+v, want ids 4,5", got)
	}
}

// TestServeEventsStream verifies a client receives the replay tail, the connected
// status, and a live event pushed after it connected — as SSE frames with ids.
func TestServeEventsStream(t *testing.T) {
	b := newBroker()
	b.Publish(EventMsg{ID: 7, SV: "G05@0", Type: "health_change", Severity: 2})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/gnss/events", nil).WithContext(ctx)
	rr := newSyncRecorder()

	done := make(chan struct{})
	go func() { defer close(done); b.serveEvents(rr, req) }()

	// Let the handler replay + subscribe, then push a live event.
	waitFor(t, func() bool { return strings.Contains(rr.String(), "id: 7") })
	b.Publish(EventMsg{ID: 8, SV: "E14@1", Type: "orbit_disco", Severity: 1})
	waitFor(t, func() bool { return strings.Contains(rr.String(), "id: 8") })
	cancel()
	<-done

	body := rr.String()
	if rr.Header().Get("Content-Type") != "text/event-stream" {
		t.Errorf("content-type = %q", rr.Header().Get("Content-Type"))
	}
	if !strings.Contains(body, "event: gnss") {
		t.Error("missing gnss event frame")
	}
	if !strings.Contains(body, `"type":"health_change"`) {
		t.Error("replayed event body missing")
	}
	if !strings.Contains(body, "event: status") {
		t.Error("missing connected status frame")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}
