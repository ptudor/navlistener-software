package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
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

// TestEmitEventRetriesAndQueuesOnTransientError guards a confirmed event whose
// durable write fails must not be silently dropped. emitEvent returns a *pendingEvent that
// the detect loop re-attempts on a later tick; when the DB recovers, the event is written
// once and published exactly once with its real id.
func TestEmitEventRetriesAndQueuesOnTransientError(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	saved := defaultEventRetry
	defaultEventRetry = eventRetry{attempts: 2, backoff: time.Millisecond, perCallTO: time.Second}
	defer func() { defaultEventRetry = saved }()

	// First tick: both attempts fail -> emitEvent returns a pending event, nothing published.
	w := &scriptedEventWriter{ids: []int64{0, 0}, errs: []error{errors.New("db down"), errors.New("db down")}}
	p := &capturePublisher{}
	ev := detect.Event{Time: time.Now(), SV: "G07@0", Type: "health_change", OldValue: "1", NewValue: "2", Severity: 2}
	pe := emitEvent(ctx, ev, w, p, log)
	if pe == nil {
		t.Fatal("failed durable write must return a pending event for re-attempt")
	}
	if len(p.events) != 0 {
		t.Fatalf("nothing should be published on a failed write, got %+v", p.events)
	}

	// Later tick: DB recovers -> reattemptPending writes once, publishes with the real id.
	w.ids, w.errs, w.n = []int64{900}, []error{nil}, 0
	pending := reattemptPending(ctx, []pendingEvent{*pe}, w, p, log)
	if len(pending) != 0 {
		t.Fatalf("recovered event must leave the queue, still pending: %d", len(pending))
	}
	if len(p.events) != 1 || p.events[0].ID != 900 {
		t.Fatalf("published = %+v, want exactly one event with durable id 900", p.events)
	}
}

// TestEnqueuePendingDropsOldestWhenFull guards bounded queue: at capacity the oldest
// event is dropped rather than growing the queue without limit.
func TestEnqueuePendingDropsOldestWhenFull(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := make([]pendingEvent, 0, eventPendingMax)
	for i := 0; i < eventPendingMax; i++ {
		q = enqueuePending(q, pendingEvent{ev: detect.Event{SV: "G01@0", Type: "orbit_disco"}}, log)
	}
	if len(q) != eventPendingMax {
		t.Fatalf("queue len = %d, want %d", len(q), eventPendingMax)
	}
	newest := pendingEvent{ev: detect.Event{SV: "GNEW@0", Type: "orbit_disco"}}
	q = enqueuePending(q, newest, log)
	if len(q) != eventPendingMax {
		t.Fatalf("queue over cap: len = %d, want %d", len(q), eventPendingMax)
	}
	if q[len(q)-1].ev.SV != "GNEW@0" {
		t.Fatal("newest event must be retained after dropping the oldest")
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
