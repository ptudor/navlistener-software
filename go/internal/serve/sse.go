package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ptudor/navlistener/internal/metrics"
)

// EventMsg is one integrity event pushed to SSE clients and mirrored in the query
// API (docs/OUTPUT.md §3). The daemon fills it from a confirmed detector transition
// (with the id the historian assigned, or a local counter when persistence is off).
type EventMsg struct {
	PolicyGeneration uint64 `json:"-"`
	GenerationSet    bool   `json:"-"`
	brokerGeneration uint64
	ID               int64          `json:"id"`
	Time             string         `json:"time"`
	SV               string         `json:"sv"`
	Type             string         `json:"type"`
	OldValue         string         `json:"old_value,omitempty"`
	NewValue         string         `json:"new_value,omitempty"`
	Severity         int            `json:"severity"`
	Message          string         `json:"message,omitempty"`
	Params           map[string]any `json:"params,omitempty"`
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
var sseClients atomic.Int64

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
	clients map[*sseClient]struct{}
	recent  []EventMsg // ring, oldest-first, capped at sseRecentCap
	log     *slog.Logger
	// done is closed by Close() on server shutdown : http.Server.Shutdown never
	// cancels in-flight request contexts, so the SSE handler must have a daemon-scoped
	// signal to return on, or every restart with a live consumer (intsat holds a permanent
	// EventSource) burns the full ShutdownTimeout and logs "ungraceful".
	done             chan struct{}
	closeOnce        sync.Once
	generation       atomic.Uint64
	policyAdmission  func(uint64, func()) bool
	policyGeneration func() uint64
	beforePublish    func() // deterministic admission-race seam
}

// sseClient carries both the bounded live-event queue and an idempotent overflow
// signal. Closing kick terminates the stream so EventSource reconnects with its last
// delivered id instead of remaining connected across an invisible gap.
type sseClient struct {
	generation       uint64
	policyGeneration uint64
	events           chan EventMsg
	kick             chan struct{}
	kickOnce         sync.Once
}

func (c *sseClient) drop() { c.kickOnce.Do(func() { close(c.kick) }) }

// newBroker builds an empty Broker. log defaults to a discard logger (tests, and
// any construction that doesn't care) — New (serve.go) sets the real one.
func newBroker() *Broker {
	return &Broker{
		clients: map[*sseClient]struct{}{},
		log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		done:    make(chan struct{}),
	}
}

// Close signals every active SSE handler to return. Idempotent; registered via
// http.Server.RegisterOnShutdown so a graceful Shutdown completes promptly instead of
// polling until the timeout on a client holding a permanent EventSource.
func (b *Broker) Close() {
	b.closeOnce.Do(func() { close(b.done) })
}

// Reset crosses a policy epoch without permanently closing the broker. Existing
// streams reconnect and the replay ring is emptied, so no pre-withdrawal event
// can be replayed or delivered after the transition.
func (b *Broker) Reset() {
	b.mu.Lock()
	b.generation.Add(1)
	b.recent = nil
	for client := range b.clients {
		client.drop()
	}
	b.mu.Unlock()
}

// Publish records an event in the replay ring and delivers it to every connected
// client. A client whose buffer is full is kicked rather than silently skipped, forcing
// EventSource to reconnect and replay the gap. Replay can recover only events still in
// the 256-event ring; older history requires the query API. The publisher never blocks.
func (b *Broker) Publish(e EventMsg) {
	if b.beforePublish != nil {
		b.beforePublish()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.policyAdmission != nil {
		if !e.GenerationSet {
			e.PolicyGeneration = b.policyGeneration()
			e.GenerationSet = true
		}
		b.policyAdmission(e.PolicyGeneration, func() { b.publishLocked(e) })
		return
	}
	b.publishLocked(e)
}

func (b *Broker) publishLocked(e EventMsg) {
	e.brokerGeneration = b.generation.Load()
	b.recent = append(b.recent, e)
	if len(b.recent) > sseRecentCap {
		b.recent = b.recent[len(b.recent)-sseRecentCap:]
	}
	for client := range b.clients {
		select {
		case client.events <- e:
		default:
			// a chronically-slow consumer (a permanent EventSource being
			// overflow-kicked in a reconnect loop) is invisible without a counter.
			metrics.SSEEventsDroppedTotal.Inc()
			client.drop()
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
func (b *Broker) subscribe() (client *sseClient, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if total := sseClients.Add(1); total > int64(sseMaxClients) {
		sseClients.Add(-1)
		metrics.SSESubscribeRejectedTotal.Inc() // cap pressure/attack signal
		return nil, false
	}
	client = &sseClient{events: make(chan EventMsg, sseClientBuffer), kick: make(chan struct{}), generation: b.generation.Load()}
	if b.policyGeneration != nil {
		client.policyGeneration = b.policyGeneration()
	}
	b.clients[client] = struct{}{}
	// gauge set from the authoritative map size under the lock (never
	// inc/dec'd separately, so it cannot drift from reality).
	metrics.SSEClients.Set(float64(sseClients.Load()))
	return client, true
}

func (b *Broker) unsubscribe(client *sseClient) {
	b.mu.Lock()
	if _, exists := b.clients[client]; exists {
		delete(b.clients, client)
		sseClients.Add(-1)
	}
	metrics.SSEClients.Set(float64(sseClients.Load())) // regression fix
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
	client, ok := b.subscribe()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "too many active event streams")
		return
	}
	defer b.unsubscribe(client)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // defeat proxy buffering (docs/OUTPUT.md §3)

	rc := http.NewResponseController(w)
	writeAndFlush := func(f func() error) bool {
		if !b.clientCurrent(client) {
			return false
		}
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
		if !b.eventCurrent(client, e) {
			return
		}
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
		case e := <-client.events:
			if !b.eventCurrent(client, e) {
				return
			}
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
		case <-b.done:
			return // server is shutting down
		case <-client.kick:
			return // slow client: reconnect + Last-Event-ID replays the ring gap
		case <-ctx.Done():
			return
		}
	}
}

func (b *Broker) clientCurrent(c *sseClient) bool {
	if c.generation != b.generation.Load() {
		return false
	}
	if b.policyGeneration != nil && c.policyGeneration != b.policyGeneration() {
		return false
	}
	select {
	case <-c.kick:
		return false
	default:
		return true
	}
}

func (b *Broker) eventCurrent(c *sseClient, e EventMsg) bool {
	return b.clientCurrent(c) && e.brokerGeneration == c.generation && (!e.GenerationSet || e.PolicyGeneration == c.policyGeneration)
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
