package serve

import (
	"encoding/json"
	"fmt"
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

// Broker fans confirmed integrity events out to connected SSE clients and keeps a
// bounded ring of recent events for Last-Event-ID reconnect replay. It is the
// server-driven push side of the events contract; the DB trigger's pg_notify serves
// external LISTENers separately.
type Broker struct {
	mu      sync.Mutex
	clients map[chan EventMsg]struct{}
	recent  []EventMsg // ring, oldest-first, capped at sseRecentCap
}

// newBroker builds an empty Broker.
func newBroker() *Broker {
	return &Broker{clients: map[chan EventMsg]struct{}{}}
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

func (b *Broker) subscribe() chan EventMsg {
	ch := make(chan EventMsg, sseClientBuffer)
	b.mu.Lock()
	b.clients[ch] = struct{}{}
	b.mu.Unlock()
	return ch
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
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat proxy buffering (docs/OUTPUT.md §3)

	lastID, hasLast := parseLastEventID(r)
	for _, e := range b.replayFrom(lastID, hasLast) {
		writeSSE(w, "gnss", e)
	}
	writeStatus(w, "connected")
	flusher.Flush()

	ch := b.subscribe()
	defer b.unsubscribe(ch)
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	ctx := r.Context()
	for {
		select {
		case e := <-ch:
			writeSSE(w, "gnss", e)
			flusher.Flush()
		case <-heartbeat.C:
			writeStatus(w, "heartbeat")
			flusher.Flush()
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

func writeSSE(w http.ResponseWriter, event string, e EventMsg) {
	body, err := json.Marshal(e)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, event, body)
}

func writeStatus(w http.ResponseWriter, state string) {
	fmt.Fprintf(w, "event: status\ndata: {\"status\":%q}\n\n", state)
}
