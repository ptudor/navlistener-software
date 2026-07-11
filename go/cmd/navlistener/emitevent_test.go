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
