package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/detect"
)

// TestEmitEventLocalFallbackStaysMonotonic guards without a historian (or
// on a write failure) emitEvent falls back to lastID+1 rather than an independent
// counter. If lastID were reset instead of threaded through, a local id could
// collide with or fall behind a previously-issued DB id, and serve.replayFrom's
// `e.ID > lastID` reconnect filter (docs/OUTPUT.md §3) would drop or duplicate
// events. This exercises both the "counting up from zero" case and — the actual
// regression — the "already seen a high id" case, by pre-seeding lastID the way a
// prior successful historian write would have left it.
func TestEmitEventLocalFallbackStaysMonotonic(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	ev := func() detect.Event {
		return detect.Event{Time: time.Now(), SV: "G01@0", Type: "orbit_disco", Severity: 1}
	}

	var lastID int64
	for i, want := range []int64{1, 2, 3} {
		emitEvent(ctx, ev(), nil, nil, &lastID, log)
		if lastID != want {
			t.Fatalf("call %d: lastID = %d, want %d", i, lastID, want)
		}
	}

	// Simulate a prior historian write that succeeded with DB id 500 (bigserial,
	// so far ahead of any local counter): a subsequent historian-less/failed call
	// must continue from 501, not restart near 1.
	lastID = 500
	emitEvent(ctx, ev(), nil, nil, &lastID, log)
	if lastID != 501 {
		t.Errorf("lastID after seeded call = %d, want 501 (must stay monotonic relative to DB ids)", lastID)
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
	var lastID int64
	emitEvent(ctx, ev, nil, nil, &lastID, log) // must not panic
	if lastID != 1 {
		t.Errorf("lastID = %d, want 1 (event must still be counted despite the NaN param)", lastID)
	}
}
