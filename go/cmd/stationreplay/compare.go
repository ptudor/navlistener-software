package main

import (
	"time"

	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/store"
)

// comparison pairs replayed station events with the stored ones.
type comparison struct {
	Matched        int
	MissingInStore []detect.Event             // replayed, not stored
	MissingReplay  []store.StoredStationEvent // stored, not replayed
}

// compareEvents matches each stored event to an unmatched replayed event of the same
// station, type and new value confirmed within tolerance of it. Events before
// warmEnd are skipped on both sides: the replay starts cold, while the live machines
// carried state from before the window. Old values are not compared, because a cold
// replay reports "unknown" where the live machine knew its prior state.
func compareEvents(replayed []detect.Event, stored []store.StoredStationEvent, station string, warmEnd time.Time, tolerance time.Duration) comparison {
	var c comparison
	used := make([]bool, len(replayed))
	for _, s := range stored {
		if s.Time.Before(warmEnd) {
			continue
		}
		found := false
		for i, e := range replayed {
			if used[i] || e.SV != station || e.Type != s.Type || e.NewValue != s.NewValue {
				continue
			}
			if d := e.Time.Sub(s.Time); d < -tolerance || d > tolerance {
				continue
			}
			used[i], found = true, true
			c.Matched++
			break
		}
		if !found {
			c.MissingReplay = append(c.MissingReplay, s)
		}
	}
	for i, e := range replayed {
		if !used[i] && e.SV == station && !e.Time.Before(warmEnd) {
			c.MissingInStore = append(c.MissingInStore, e)
		}
	}
	return c
}

// dedupeStored keeps one stored event per (time, type, new value): the same
// transition is written once per audience that sees the station.
func dedupeStored(events []store.StoredStationEvent) []store.StoredStationEvent {
	type key struct {
		t         time.Time
		typ, next string
	}
	seen := map[key]bool{}
	out := events[:0:0]
	for _, e := range events {
		k := key{e.Time, e.Type, e.NewValue}
		if !seen[k] {
			seen[k] = true
			out = append(out, e)
		}
	}
	return out
}
