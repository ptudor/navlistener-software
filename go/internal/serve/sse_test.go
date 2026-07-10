package serve

import (
	"bytes"
	"context"
	"net"
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

// TestServeEventsNoDuplicateAcrossReplayAndLive guards subscribe() now
// happens before replayFrom(), which closes the original "gap" race but opens the
// opposite one — an event already delivered by the replay loop can also arrive on
// the live channel (Publish doesn't know a client's replay already covered it).
// The id-based dedup must catch that regardless of which race actually produced
// the duplicate, so this forces the duplicate deterministically by re-publishing
// an id the client already received via replay.
func TestServeEventsNoDuplicateAcrossReplayAndLive(t *testing.T) {
	b := newBroker()
	b.Publish(EventMsg{ID: 1, SV: "G01@0", Type: "orbit_disco"})

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/gnss/events", nil).WithContext(ctx)
	rr := newSyncRecorder()

	done := make(chan struct{})
	go func() { defer close(done); b.serveEvents(rr, req) }()

	waitFor(t, func() bool { return strings.Contains(rr.String(), "id: 1") })
	b.Publish(EventMsg{ID: 1, SV: "G01@0", Type: "orbit_disco"}) // the forced duplicate
	b.Publish(EventMsg{ID: 2, SV: "G02@0", Type: "orbit_disco"})
	waitFor(t, func() bool { return strings.Contains(rr.String(), "id: 2") })
	cancel()
	<-done

	if n := strings.Count(rr.String(), "id: 1\n"); n != 1 {
		t.Errorf("\"id: 1\" appears %d times in the stream, want exactly 1 (replay+live duplicate must be deduplicated)", n)
	}
}

// pipeWriter adapts a net.Conn to http.ResponseWriter (+Flusher). Embedding
// net.Conn gives it Write and, crucially, SetWriteDeadline for free — the exact
// method http.ResponseController looks for via type assertion — so a net.Pipe
// pair can stand in for "a client that never reads."
type pipeWriter struct {
	net.Conn
	header http.Header
}

func (p *pipeWriter) Header() http.Header { return p.header }
func (p *pipeWriter) WriteHeader(int)     {}
func (p *pipeWriter) Flush()              {}

// TestServeEventsWriteDeadline guards a client whose reads never drain the
// connection (net.Pipe's Write blocks until something Reads the other end) must
// not park the handler goroutine forever — the write deadline must expire and the
// handler must return.
func TestServeEventsWriteDeadline(t *testing.T) {
	old := sseWriteTimeout
	sseWriteTimeout = 100 * time.Millisecond
	defer func() { sseWriteTimeout = old }()

	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	w := &pipeWriter{Conn: serverConn, header: http.Header{}}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/gnss/events", nil).WithContext(ctx)

	b := newBroker()
	done := make(chan struct{})
	go func() { defer close(done); b.serveEvents(w, req) }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("serveEvents did not return after the write deadline — a stuck client can park the handler forever")
	}
}
