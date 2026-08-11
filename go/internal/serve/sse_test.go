package serve

import (
	"bytes"
	"context"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// gatedRecorder can pause a live SSE write after the connected frame has completed,
// modelling a slow-but-draining client without relying on socket-buffer timing.
type gatedRecorder struct {
	*syncRecorder
	muGate  sync.Mutex
	gate    chan struct{}
	started chan struct{}
	once    sync.Once
}

func newGatedRecorder() *gatedRecorder {
	return &gatedRecorder{syncRecorder: newSyncRecorder()}
}

func (g *gatedRecorder) blockWrites() (<-chan struct{}, func()) {
	g.muGate.Lock()
	g.gate = make(chan struct{})
	g.started = make(chan struct{})
	g.once = sync.Once{}
	started, gate := g.started, g.gate
	g.muGate.Unlock()
	return started, func() { close(gate) }
}

func (g *gatedRecorder) Write(p []byte) (int, error) {
	g.muGate.Lock()
	gate, started := g.gate, g.started
	g.muGate.Unlock()
	if gate != nil {
		g.once.Do(func() { close(started) })
		<-gate
		g.muGate.Lock()
		if g.gate == gate {
			g.gate = nil
		}
		g.muGate.Unlock()
	}
	return g.syncRecorder.Write(p)
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

func TestBrokerPolicyResetClearsReplayAndKicksClients(t *testing.T) {
	b := newBroker()
	b.Publish(EventMsg{ID: 1})
	client, ok := b.subscribe()
	if !ok {
		t.Fatal("subscribe rejected")
	}
	defer b.unsubscribe(client)
	b.Reset()
	if replay := b.replayFrom(0, false); len(replay) != 0 {
		t.Fatalf("pre-policy replay survived reset: %+v", replay)
	}
	select {
	case <-client.kick:
	default:
		t.Fatal("active SSE client was not forced across the policy boundary")
	}
}

// TestWriteSSELogsMarshalFailure guards a non-finite float in an event's
// params makes json.Marshal fail; writeSSE must log the failure (with the event's
// type/sv) rather than silently dropping the event with no signal anywhere. The
// event is still dropped from the stream (a malformed event, not a write failure),
// but now with a log record proving it happened.
func TestWriteSSELogsMarshalFailure(t *testing.T) {
	var logBuf bytes.Buffer
	b := newBroker()
	b.log = slog.New(slog.NewTextHandler(&logBuf, nil))

	rr := newSyncRecorder()
	e := EventMsg{ID: 9, SV: "G05@0", Type: "orbit_disco", Params: map[string]any{"orbit_disco_m": math.NaN()}}
	if err := b.writeSSE(rr, "gnss", e); err != nil {
		t.Fatalf("writeSSE returned an error, want nil (malformed event dropped, not a write failure): %v", err)
	}
	if rr.String() != "" {
		t.Errorf("a marshal-failed event must not write any bytes to the stream: %q", rr.String())
	}
	logged := logBuf.String()
	if !strings.Contains(logged, "sse event marshal failed") {
		t.Errorf("marshal failure not logged: %s", logged)
	}
	if !strings.Contains(logged, "orbit_disco") || !strings.Contains(logged, "G05@0") {
		t.Errorf("logged record missing type/sv: %s", logged)
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

// TestServeEventsClosedOnShutdown guards Broker.Close() (registered on server
// shutdown) must release an in-flight SSE handler promptly, even though the request context
// is never cancelled — otherwise a client holding a permanent EventSource forces every
// restart to burn the full ShutdownTimeout.
func TestServeEventsClosedOnShutdown(t *testing.T) {
	b := newBroker()
	// A background request context that is NOT cancelled — the shutdown must come via Close().
	req := httptest.NewRequest("GET", "/gnss/events", nil).WithContext(context.Background())
	rr := newSyncRecorder()

	done := make(chan struct{})
	go func() { defer close(done); b.serveEvents(rr, req) }()
	waitFor(t, func() bool { return strings.Contains(rr.String(), "event: status") }) // connected

	b.Close()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SSE handler did not return after Broker.Close() ")
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

// TestServeEventsOverflowKicksAndReplays guards filling one client's live queue
// must never block Publish or leave a silently-gapped stream connected. The kicked
// handler returns, and a Last-Event-ID reconnect replays through the newest ring event.
func TestServeEventsOverflowKicksAndReplays(t *testing.T) {
	b := newBroker()
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodGet, "/gnss/events", nil).WithContext(ctx)
	rr := newGatedRecorder()
	done := make(chan struct{})
	go func() { defer close(done); b.serveEvents(rr, req) }()
	waitFor(t, func() bool { return strings.Contains(rr.String(), "event: status") })

	started, release := rr.blockWrites()
	b.Publish(EventMsg{ID: 1, Type: "orbit_disco"})
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("handler did not enter the gated live write")
	}
	start := time.Now()
	latest := int64(sseClientBuffer + 12)
	for id := int64(2); id <= latest; id++ {
		b.Publish(EventMsg{ID: id, Type: "orbit_disco"})
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("Publish blocked on slow client for %v", elapsed)
	}
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("overflow did not terminate the SSE handler")
	}
	cancel()

	lastID := highestSSEID(rr.String())
	if lastID < 1 || lastID >= latest {
		t.Fatalf("first stream last id = %d, want a partial stream before %d", lastID, latest)
	}
	replayCtx, replayCancel := context.WithCancel(context.Background())
	replayReq := httptest.NewRequest(http.MethodGet, "/gnss/events", nil).WithContext(replayCtx)
	replayReq.Header.Set("Last-Event-ID", strconv.FormatInt(lastID, 10))
	replay := newSyncRecorder()
	replayDone := make(chan struct{})
	go func() { defer close(replayDone); b.serveEvents(replay, replayReq) }()
	waitFor(t, func() bool { return strings.Contains(replay.String(), "id: "+strconv.FormatInt(latest, 10)+"\n") })
	replayCancel()
	<-replayDone
}

func highestSSEID(body string) int64 {
	var highest int64
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "id: ") {
			continue
		}
		id, err := strconv.ParseInt(strings.TrimPrefix(line, "id: "), 10, 64)
		if err == nil && id > highest {
			highest = id
		}
	}
	return highest
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

// TestServeEventsMethodNotAllowed guards part of every other v2/events endpoint
// 405s a non-GET/HEAD method, and /gnss/events must too (previously it accepted POST).
func TestServeEventsMethodNotAllowed(t *testing.T) {
	b := newBroker()
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/gnss/events", nil)
	b.serveEvents(rr, req)
	if rr.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rr.Code)
	}
	if allow := rr.Header().Get("Allow"); allow != "GET, HEAD" {
		t.Errorf("Allow = %q, want %q (RFC 9110 §15.5.6)", allow, "GET, HEAD")
	}
}

// TestServeEventsHeadCompletesWithoutSubscribing guards HEAD gets the
// stream's headers and returns immediately — it must not subscribe (consuming a
// capped slot until disconnect), must carry no SSE body, and repeated HEADs
// must not reduce the GET streams that can still subscribe.
func TestServeEventsHeadCompletesWithoutSubscribing(t *testing.T) {
	old := sseMaxClients
	sseMaxClients = 1
	defer func() { sseMaxClients = old }()

	b := newBroker()
	for i := 0; i < 5; i++ {
		rr := httptest.NewRecorder()
		b.serveEvents(rr, httptest.NewRequest(http.MethodHead, "/gnss/events", nil))
		if rr.Code != http.StatusOK {
			t.Fatalf("HEAD status = %d, want 200", rr.Code)
		}
		if ct := rr.Header().Get("Content-Type"); ct != "text/event-stream" {
			t.Errorf("HEAD content-type = %q, want text/event-stream", ct)
		}
		if rr.Body.Len() != 0 {
			t.Errorf("HEAD wrote %d body bytes, want none", rr.Body.Len())
		}
	}
	b.mu.Lock()
	n := len(b.clients)
	b.mu.Unlock()
	if n != 0 {
		t.Fatalf("HEAD requests left %d subscribed clients, want 0", n)
	}

	// The only capped slot is still available for a real GET stream.
	if ch, ok := b.subscribe(); !ok {
		t.Fatal("GET stream slot consumed by HEAD requests")
	} else {
		b.unsubscribe(ch)
	}
}

// TestServeEventsClientCapEnforced guards client cap: past sseMaxClients
// concurrent streams, a new connection gets 503 with a clean JSON error (not a
// text/event-stream response), and a freed slot lets the next connection through.
func TestServeEventsClientCapEnforced(t *testing.T) {
	old := sseMaxClients
	sseMaxClients = 2
	defer func() { sseMaxClients = old }()

	b := newBroker()
	var chans []*sseClient
	for i := 0; i < sseMaxClients; i++ {
		ch, ok := b.subscribe()
		if !ok {
			t.Fatalf("subscribe %d unexpectedly rejected before reaching the cap", i)
		}
		chans = append(chans, ch)
	}

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/gnss/events", nil)
	b.serveEvents(rr, req)
	if rr.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 past the cap", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json (not an SSE stream) on the 503", ct)
	}

	// Freeing a slot must let a new connection through.
	b.unsubscribe(chans[0])
	ctx, cancel := context.WithCancel(context.Background())
	req2 := httptest.NewRequest(http.MethodGet, "/gnss/events", nil).WithContext(ctx)
	rr2 := newSyncRecorder()
	done := make(chan struct{})
	go func() { defer close(done); b.serveEvents(rr2, req2) }()
	waitFor(t, func() bool { return strings.Contains(rr2.String(), "event: status") })
	cancel()
	<-done

	for _, ch := range chans[1:] {
		b.unsubscribe(ch)
	}
}
