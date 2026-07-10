package main

import (
	"context"
	"io"
	"log/slog"
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
