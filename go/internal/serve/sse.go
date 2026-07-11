package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// EventMsg is one integrity event pushed to SSE clients and mirrored in the query
// API (docs/OUTPUT.md §3). The daemon fills it from a confirmed detector transition
// (with the id the historian assigned, or a local counter when persistence is off).
type EventMsg struct {
	ID       int64          `json:"id"`
	Time     string         `json:"time"`
	SV       string         `json:"sv"`
	Type     string         `json:"type"`
	OldValue string         `json:"old_value,omitempty"`
	NewValue string         `json:"new_value,omitempty"`
	Severity int            `json:"severity"`
	Message  string         `json:"message,omitempty"`
	Params   map[string]any `json:"params,omitempty"`
}

// sseDefaults for the reconnect replay window and heartbeat cadence (docs/OUTPUT.md
// §3): replay at most N recent events, heartbeat every 60 s.
const (
	sseRecentCap    = 256
	sseReplayNoLast = 20
	sseHeartbeat    = 60 * time.Second
	sseClientBuffer = 64
)

// sseMaxClients bounds concurrent SSE streams : each connection costs a goroutine,
// a sseClientBuffer-slot channel, and a socket held open indefinitely, so an attacker or a
// buggy reconnect loop opening unbounded streams exhausts server resources. A var, not a
// const (like sseWriteTimeout above), so tests can shrink it instead of opening 1000 real
// connections.
var sseMaxClients = 1000

// sseWriteTimeout bounds every write+flush : a client whose TCP receive
// window is full (dead-but-not-reset) must not be able to park the handler
// goroutine (and its buffered EventMsgs) indefinitely. A var, not a const, so
// tests can shrink it rather than waiting out the production value.
var sseWriteTimeout = 10 * time.Second

// Broker fans confirmed integrity events out to connected SSE clients and keeps a
// bounded ring of recent events for Last-Event-ID reconnect replay. It is the
// server-driven push side of the events contract; the DB trigger's pg_notify serves
// external LISTENers separately.
type Broker struct {
	mu      sync.Mutex
	clients map[chan EventMsg]struct{}
	recent  []EventMsg // ring, oldest-first, capped at sseRecentCap
	log     *slog.Logger
}

// newBroker builds an empty Broker. log defaults to a discard logger (tests, and
// any construction that doesn't care) — New (serve.go) sets the real one.
func newBroker() *Broker {
	return &Broker{clients: map[chan EventMsg]struct{}{}, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// Publish records an event in the replay ring and delivers it to every connected
// client. A client whose buffer is full is skipped for this event (it will catch up
// via the ring on reconnect) rather than blocking the detector.
func (b *Broker) Publish(e EventMsg) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recent = append(b.recent, e)
	if len(b.recent) > sseRecentCap {
		b.recent = b.recent[len(b.recent)-sseRecentCap:]
	}
	for ch := range b.clients {
		select {
		case ch <- e:
		default: // slow client; it recovers from the ring on reconnect
		}
	}
}

// replayFrom returns the buffered events after lastID (all of them if lastID is 0
// and replayAll is false, the most recent sseReplayNoLast are returned instead).
func (b *Broker) replayFrom(lastID int64, hasLast bool) []EventMsg {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !hasLast {
		if len(b.recent) > sseReplayNoLast {
			return append([]EventMsg(nil), b.recent[len(b.recent)-sseReplayNoLast:]...)
		}
		return append([]EventMsg(nil), b.recent...)
	}
	var out []EventMsg
	for _, e := range b.recent {
		if e.ID > lastID {
			out = append(out, e)
		}
	}
	return out
}

// subscribe adds a new client, unless sseMaxClients concurrent streams are already
// connected, in which case it returns ok=false and adds nothing.
func (b *Broker) subscribe() (ch chan EventMsg, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.clients) >= sseMaxClients {
		return nil, false
	}
	ch = make(chan EventMsg, sseClientBuffer)
	b.clients[ch] = struct{}{}
	return ch, true
}

func (b *Broker) unsubscribe(ch chan EventMsg) {
	b.mu.Lock()
	delete(b.clients, ch)
	b.mu.Unlock()
}

// serveEvents is the GET /gnss/events SSE handler (docs/OUTPUT.md §3). It replays the
// reconnect window (Last-Event-ID, else the recent tail), then streams live events
// as `event: gnss` with the id set, and a `event: status` heartbeat on the cadence.
func (b *Broker) serveEvents(w http.ResponseWriter, r *http.Request) {
	if methodNotAllowedGetHead(w, r) { // RFC 9110 §15.5.6: 405 must name the allowed methods
		return
	}
	// HEAD answers with the stream's headers and completes immediately —
	// it must not subscribe, replay, or hold one of the capped stream slots for
	// the life of a connection that will never read a body.
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Subscribe *before* setting any SSE headers : a rejection past the cap must
	// still be a clean JSON error response, not a text/event-stream response that then
	// immediately errors.
	ch, ok := b.subscribe()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "too many active event streams")
		return
	}
	defer b.unsubscribe(ch)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat proxy buffering (docs/OUTPUT.md §3)

	rc := http.NewResponseController(w)
	writeAndFlush := func(f func() error) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout))
		if err := f(); err != nil {
			return false
		}
		flusher.Flush()
		return true
	}

	// subscribed (above) *before* replaying -- an event Published between an
	// after-replay subscribe and the replay finishing would reach neither path — past
	// the replay cut, and the client's channel didn't exist yet. Subscribing first means
	// any such event lands in ch; replayFrom's own snapshot may *also* include it (a
	// second race, opposite direction), so live events are deduplicated against the
	// highest id actually written by the replay loop — ids are monotonic within a
	// process, so a `<=` compare is exact either way.
	lastID, hasLast := parseLastEventID(r)
	var lastReplayedID int64
	for _, e := range b.replayFrom(lastID, hasLast) {
		if !writeAndFlush(func() error { return b.writeSSE(w, "gnss", e) }) {
			return
		}
		lastReplayedID = e.ID
	}
	if !writeAndFlush(func() error { return writeStatus(w, "connected") }) {
		return
	}

	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case e := <-ch:
			if e.ID <= lastReplayedID {
				continue // already delivered via the replay above
			}
			if !writeAndFlush(func() error { return b.writeSSE(w, "gnss", e) }) {
				return
			}
		case <-heartbeat.C:
			if !writeAndFlush(func() error { return writeStatus(w, "heartbeat") }) {
				return
			}
		case <-ctx.Done():
			return
		}
	}
}

// parseLastEventID reads the reconnect cursor from the Last-Event-ID header or the
// equivalent query parameter (EventSource sends the header; the query form helps
// manual testing).
func parseLastEventID(r *http.Request) (int64, bool) {
	v := r.Header.Get("Last-Event-ID")
	if v == "" {
		v = r.URL.Query().Get("lastEventId")
	}
	if v == "" {
		return 0, false
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return id, true
}

// writeSSE marshals and frames one event. a marshal failure (e.g. a
// non-finite float slipping past emitEvent's sanitization) is logged with the
// event's type/SV before being dropped -- silently swallowing it here meant the
// event vanished from both the live stream and the id sequence a client's
// Last-Event-ID replay depends on, with no signal anywhere that it happened.
func (b *Broker) writeSSE(w http.ResponseWriter, event string, e EventMsg) error {
	body, err := json.Marshal(e)
	if err != nil {
		b.log.Error("sse event marshal failed", "type", e.Type, "sv", e.SV, "id", e.ID, "error", err)
		return nil // malformed event: drop it, not a write failure
	}
	_, err = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, event, body)
	return err
}

func writeStatus(w http.ResponseWriter, state string) error {
	_, err := fmt.Fprintf(w, "event: status\ndata: {\"status\":%q}\n\n", state)
	return err
}
