package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/serve"
	"github.com/ptudor/navlistener/internal/store"
)

type scriptedEventWriter struct {
	ids  []int64
	errs []error
	n    int
}

func (w *scriptedEventWriter) WriteEvent(context.Context, store.EventRow) (int64, error) {
	i := w.n
	w.n++
	return w.ids[i], w.errs[i]
}

type capturePublisher struct{ events []serve.EventMsg }

func (p *capturePublisher) PublishEvent(e serve.EventMsg) { p.events = append(p.events, e) }

func TestEmitEventPublishesOnlyDurableDatabaseIDs(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	// Pin single-attempt semantics for this contract test (regression fix adds bounded retry by
	// default; the durable-id contract itself is what's under test here).
	saved := defaultEventRetry
	defaultEventRetry = eventRetry{attempts: 1, backoff: 0, perCallTO: time.Second}
	defer func() { defaultEventRetry = saved }()

	ev := func() detect.Event {
		return detect.Event{Time: time.Now(), SV: "G01@0", Type: "orbit_disco", Severity: 1}
	}
	w := &scriptedEventWriter{ids: []int64{500, 0, 501}, errs: []error{nil, errors.New("db down"), nil}}
	p := &capturePublisher{}
	for i := 0; i < 3; i++ {
		emitEvent(ctx, ev(), w, p, log)
	}
	if len(p.events) != 2 || p.events[0].ID != 500 || p.events[1].ID != 501 {
		t.Fatalf("published IDs = %+v, want durable [500, 501] only", p.events)
	}
	emitEvent(ctx, ev(), nil, p, log)
	if len(p.events) != 2 {
		t.Fatal("historian-disabled event was published")
	}
}

// TestEventPipelineRetriesTransientError guards regression fix/a confirmed event whose
// durable write fails remains at the FIFO head; once the DB recovers, it and the later
// event are published exactly once in detection order with durable ids.
func TestEventPipelineRetriesTransientErrorInFIFOOrder(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	saved := defaultEventRetry
	defaultEventRetry = eventRetry{attempts: 1, backoff: time.Millisecond, perCallTO: time.Second}
	defer func() { defaultEventRetry = saved }()

	w := &switchEventWriter{}
	p := &lockedPublisher{}
	pipeline := newEventPipeline(w, p, log)
	a := detect.Event{Time: time.Now(), SV: "G07@0", Type: "health_change", OldValue: "1", NewValue: "2", Severity: 2}
	b := detect.Event{Time: time.Now(), SV: "G07@0", Type: "health_change", OldValue: "2", NewValue: "1", Severity: 0}
	pipeline.enqueue(*prepareEvent(a, w, log))
	pipeline.enqueue(*prepareEvent(b, w, log))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pipeline.run(ctx) }()
	w.waitCalls(t, 1)
	w.setAvailable(true)
	w.waitCalls(t, 3) // failed A, successful A, successful B
	waitEventCount(t, p, 2)
	cancel()
	<-done
	got := p.snapshot()
	if got[0].OldValue != "1" || got[1].OldValue != "2" {
		t.Fatalf("publish order = %+v, want A then B", got)
	}
}

// TestEnqueuePendingDropsOldestWhenFull guards bounded queue: at capacity the oldest
// event is dropped rather than growing the queue without limit.
func TestEnqueuePendingDropsOldestWhenFull(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	pipeline := newEventPipeline(&switchEventWriter{}, nil, log)
	for i := 0; i < eventPendingMax; i++ {
		pipeline.enqueue(pendingEvent{ev: detect.Event{SV: "G01@0", Type: "orbit_disco"}})
	}
	if pipeline.len() != eventPendingMax {
		t.Fatalf("queue len = %d, want %d", pipeline.len(), eventPendingMax)
	}
	newest := pendingEvent{ev: detect.Event{SV: "GNEW@0", Type: "orbit_disco"}}
	pipeline.enqueue(newest)
	if pipeline.len() != eventPendingMax {
		t.Fatalf("queue over cap: len = %d, want %d", pipeline.len(), eventPendingMax)
	}
	if pipeline.pending[len(pipeline.pending)-1].ev.SV != "GNEW@0" {
		t.Fatal("newest event must be retained after dropping the oldest")
	}
}

type switchEventWriter struct {
	mu        sync.Mutex
	available bool
	block     bool
	calls     []store.EventRow
}

func (w *switchEventWriter) WriteEvent(ctx context.Context, row store.EventRow) (int64, error) {
	w.mu.Lock()
	w.calls = append(w.calls, row)
	available, block, id := w.available, w.block, int64(len(w.calls))
	w.mu.Unlock()
	if block {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	if !available {
		return 0, errors.New("db down")
	}
	return id, nil
}

func (w *switchEventWriter) setAvailable(v bool) {
	w.mu.Lock()
	w.available = v
	w.mu.Unlock()
}

func (w *switchEventWriter) waitCalls(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		w.mu.Lock()
		n := len(w.calls)
		w.mu.Unlock()
		if n >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("writer did not reach %d calls", want)
}

type lockedPublisher struct {
	mu     sync.Mutex
	events []serve.EventMsg
}

func (p *lockedPublisher) PublishEvent(e serve.EventMsg) {
	p.mu.Lock()
	p.events = append(p.events, e)
	p.mu.Unlock()
}

func (p *lockedPublisher) snapshot() []serve.EventMsg {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]serve.EventMsg(nil), p.events...)
}

func waitEventCount(t *testing.T, p *lockedPublisher, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(p.snapshot()) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("publisher did not reach %d events", want)
}

// A cancelled detector context must not poison the final write context.
func TestEventPipelineFinalFlushUsesFreshContext(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := &switchEventWriter{available: true}
	p := &lockedPublisher{}
	pipeline := newEventPipeline(w, p, log)
	ev := detect.Event{Time: time.Now(), SV: "G01@0", Type: "orbit_disco", Severity: 2}
	pipeline.enqueue(*prepareEvent(ev, w, log))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pipeline.run(ctx)
	if got := p.snapshot(); len(got) != 1 {
		t.Fatalf("final flush published %d events, want 1", len(got))
	}
	if pipeline.len() != 0 {
		t.Fatalf("final flush left %d queued events", pipeline.len())
	}
}

// The final flush has one shared bound and reports its dropped count.
func TestEventPipelineFinalFlushHardDownIsBounded(t *testing.T) {
	var logBuf strings.Builder
	log := slog.New(slog.NewTextHandler(&logBuf, nil))
	w := &switchEventWriter{block: true}
	pipeline := newEventPipeline(w, nil, log)
	pipeline.enqueue(*prepareEvent(detect.Event{Time: time.Now(), SV: "G01@0", Type: "orbit_disco"}, w, log))
	saved := eventFinalFlushTimeout
	eventFinalFlushTimeout = 25 * time.Millisecond
	defer func() { eventFinalFlushTimeout = saved }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	pipeline.run(ctx)
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Fatalf("hard-down final flush took %v, want bounded return", elapsed)
	}
	if !strings.Contains(logBuf.String(), "dropping confirmed integrity events") {
		t.Fatalf("missing final dropped-count log: %s", logBuf.String())
	}
}

// Enqueue remains fast while the writer is stalled on a database call.
func TestEventPipelineDatabaseStallDoesNotBlockEnqueue(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := &switchEventWriter{block: true}
	pipeline := newEventPipeline(w, nil, log)
	savedFlush := eventFinalFlushTimeout
	eventFinalFlushTimeout = 25 * time.Millisecond
	defer func() { eventFinalFlushTimeout = savedFlush }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pipeline.run(ctx) }()
	pipeline.enqueue(pendingEvent{row: store.EventRow{Type: "orbit_disco"}, ev: detect.Event{Type: "orbit_disco"}})
	w.waitCalls(t, 1)
	start := time.Now()
	for i := 0; i < 5; i++ {
		pipeline.enqueue(pendingEvent{row: store.EventRow{Type: "orbit_disco"}, ev: detect.Event{Type: "orbit_disco"}})
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("enqueue coupled to stalled writer: %v", elapsed)
	}
	cancel()
	<-done
}

type panicOnceWriter struct {
	mu      sync.Mutex
	paniced bool
	calls   []string
}

func (w *panicOnceWriter) WriteEvent(_ context.Context, row store.EventRow) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.calls = append(w.calls, row.NewValue)
	if row.NewValue == "B" && !w.paniced {
		w.paniced = true
		panic("scripted writer panic")
	}
	return int64(len(w.calls)), nil
}

// A mid-write panic retains the current head and untouched tail without aliasing or
// duplicating already-completed items.
func TestEventPipelinePanicPreservesExactQueue(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := &panicOnceWriter{}
	p := &lockedPublisher{}
	pipeline := newEventPipeline(w, p, log)
	for _, v := range []string{"A", "B", "C"} {
		ev := detect.Event{Time: time.Now(), SV: "G01@0", Type: "health_change", NewValue: v}
		pipeline.enqueue(*prepareEvent(ev, w, log))
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); pipeline.run(ctx) }()
	waitEventCount(t, p, 3)
	cancel()
	<-done
	got := p.snapshot()
	if got[0].NewValue != "A" || got[1].NewValue != "B" || got[2].NewValue != "C" {
		t.Fatalf("published = %+v, want A,B,C exactly once", got)
	}
	w.mu.Lock()
	calls := append([]string(nil), w.calls...)
	w.mu.Unlock()
	want := []string{"A", "B", "B", "C"}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Fatalf("write attempts = %v, want %v", calls, want)
	}
}

// TestEmitEventBoundaryConvertsTypedNil guards a nil concrete *store.Store /
// *serve.Server must be converted to a true interface nil before reaching emitEvent, or
// the `== nil` guard is defeated (a typed nil boxed into an interface is non-nil) and the
// first confirmed event dereferences a nil receiver, crashing the daemon in every
// persist-less/serve-less config. The asEventWriter/asEventPublisher helpers do that
// conversion; here we feed them typed nils and confirm the downstream emitEvent takes the
// warn-and-skip path instead of panicking.
func TestEmitEventBoundaryConvertsTypedNil(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()

	var typedStore *store.Store   // nil concrete
	var typedServer *serve.Server // nil concrete

	ew := asEventWriter(typedStore)
	ep := asEventPublisher(typedServer)
	if ew != nil {
		t.Fatal("asEventWriter(nil) must return a true interface nil")
	}
	if ep != nil {
		t.Fatal("asEventPublisher(nil) must return a true interface nil")
	}

	ev := detect.Event{Time: time.Now(), SV: "G01@0", Type: "health_change", Severity: 2}
	emitEvent(ctx, ev, ew, ep, log) // must not panic; takes the not-publishable warn path

	// And with store present but serve off, the publish must be skipped without panic.
	w := &scriptedEventWriter{ids: []int64{700}, errs: []error{nil}}
	emitEvent(ctx, ev, w, asEventPublisher(typedServer), log)
}

// TestSanitizeEventParamsHandlesNonFinite guards Params is a map of
// ephemeris-derived float64s, and encoding/json fails the whole document on a
// non-finite value -- sanitizeEventParams must replace NaN/+Inf/-Inf with their
// string form so the sanitized map always survives json.Marshal, while leaving
// every other value (finite floats, ints, strings) untouched.
func TestSanitizeEventParamsHandlesNonFinite(t *testing.T) {
	in := map[string]any{
		"sv":            "G05@0",
		"orbit_disco_m": math.NaN(),
		"time_disco_ns": math.Inf(1),
		"neg_inf":       math.Inf(-1),
		"finite":        1.5,
		"count":         3,
	}
	out := sanitizeEventParams(in)

	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("sanitized params still fail to marshal: %v", err)
	}
	if out["sv"] != "G05@0" || out["finite"] != 1.5 || out["count"] != 3 {
		t.Errorf("finite/non-float values must survive unchanged: %+v", out)
	}
	if out["orbit_disco_m"] != "NaN" {
		t.Errorf("orbit_disco_m = %v, want stringified \"NaN\"", out["orbit_disco_m"])
	}
	if out["time_disco_ns"] != "+Inf" {
		t.Errorf("time_disco_ns = %v, want stringified \"+Inf\"", out["time_disco_ns"])
	}
	if out["neg_inf"] != "-Inf" {
		t.Errorf("neg_inf = %v, want stringified \"-Inf\"", out["neg_inf"])
	}

	// A params map with nothing non-finite is returned as-is (no copy needed).
	clean := map[string]any{"a": 1.0, "b": "x"}
	if got := sanitizeEventParams(clean); len(got) != 2 {
		t.Errorf("clean params mutated unexpectedly: %+v", got)
	}
}

// TestEmitEventSanitizesNonFiniteParamsBeforeMarshal guards regression fix end-to-end
// through emitEvent: a NaN param must not prevent the event from being counted
// and logged (in particular, it must not panic or silently vanish before
// reaching the historian/SSE stage).
func TestEmitEventSanitizesNonFiniteParamsBeforeMarshal(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	ev := detect.Event{
		Time: time.Now(), SV: "G01@0", Type: "orbit_disco", Severity: 1,
		Params: map[string]any{"orbit_disco_m": math.NaN()},
	}
	emitEvent(ctx, ev, nil, nil, log) // must not panic
}

// TestPrepareEventDedupeKey guards client half: every confirmed
// transition gets a dedupe key minted exactly once, BEFORE the first write
// attempt, distinct across confirmations, and carried unchanged through the
// pendingEvent a retry re-presents — the stability the store's
// (time, dedupe_key) upsert depends on to return the committed id instead of
// inserting a duplicate row.
func TestPrepareEventDedupeKey(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := &scriptedEventWriter{ids: []int64{1, 2}, errs: []error{nil, nil}}

	a := prepareEvent(detect.Event{Time: time.Now(), SV: "G01@0", Type: "orbit_disco"}, w, log)
	b := prepareEvent(detect.Event{Time: time.Now(), SV: "G02@0", Type: "orbit_disco"}, w, log)
	if a == nil || b == nil {
		t.Fatal("prepareEvent returned nil with a live historian")
	}
	if a.row.DedupeKey == "" || len(a.row.DedupeKey) != 32 {
		t.Errorf("dedupe key = %q, want 32 hex chars", a.row.DedupeKey)
	}
	if a.row.DedupeKey == b.row.DedupeKey {
		t.Error("two confirmations shared one dedupe key")
	}

	// The queued copy a retry re-presents must carry the identical key: the row
	// is immutable once prepared (the whole point of prepareEvent).
	pipeline := newEventPipeline(nil, nil, log)
	pipeline.enqueue(*a)
	head, ok := pipeline.head()
	if !ok || head.row.DedupeKey != a.row.DedupeKey {
		t.Errorf("queued key %q, want the prepared key %q", head.row.DedupeKey, a.row.DedupeKey)
	}
}

func TestPublicEventPipelineStampsAudienceAndRedaction(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	w := &switchEventWriter{available: true}
	pipeline := newEventPipeline(w, nil, log, "public")
	pe := prepareEvent(detect.Event{Time: time.Now(), SV: "G01@0", Type: "orbit_disco"}, w, log)
	pipeline.enqueue(*pe)
	head, ok := pipeline.head()
	if !ok {
		t.Fatal("public event was not queued")
	}
	if head.row.Audience != "public" || head.row.RedactionClass != "public_policy_filtered" {
		t.Fatalf("public event scope = %+v", head.row)
	}
}
