package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

// memoryAGCStore keeps checkpoints by station and, like the historian, hands a
// collector back only the rows it saved.
type memoryAGCStore struct {
	rows map[string]store.AGCBaselineCheckpoint
}

func (m *memoryAGCStore) LoadAGCBaselines(_ context.Context, collectorID string) ([]store.AGCBaselineCheckpoint, error) {
	var out []store.AGCBaselineCheckpoint
	for _, c := range m.rows {
		if c.CollectorInstanceID == collectorID {
			out = append(out, c)
		}
	}
	return out, nil
}

func (m *memoryAGCStore) SaveAGCBaseline(_ context.Context, c store.AGCBaselineCheckpoint) error {
	m.rows[c.SourceID] = c
	return nil
}

func learnAGC(s *state.Store, station string, from time.Time, d time.Duration, agc int) time.Time {
	at := from
	for ; at.Before(from.Add(d)); at = at.Add(time.Second) {
		s.Apply(&ingest.RawFrame{Source: station, Recv: at, RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: agc, AntStatus: 2}}}})
	}
	return at
}

func TestAGCBaselineCheckpointRoundTrip(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	now := time.Now().Add(-time.Hour).Truncate(time.Second)
	live := state.New(1)
	at := learnAGC(live, "obs-a", now, 11*time.Minute, 4000)
	learnAGC(live, "obs-b", now, 11*time.Minute, 3000)
	mem := &memoryAGCStore{rows: map[string]store.AGCBaselineCheckpoint{}}
	epochs := map[string]string{"obs-a": "antenna-1", "obs-b": "antenna-1"}
	const collector = "collector-a"
	if err := saveAGCBaselines(ctx, mem, collector, live, epochs); err != nil || len(mem.rows) != 2 || mem.rows["obs-a"].Epoch != "antenna-1" {
		t.Fatalf("saved %+v %v", mem.rows, err)
	}
	if mem.rows["obs-a"].CollectorInstanceID != collector || mem.rows["obs-b"].CollectorInstanceID != collector {
		t.Fatalf("checkpoints not stamped with the collector: %+v", mem.rows)
	}
	// obs-b's antenna changed; an unreadable checkpoint is skipped, not fatal;
	// and another collector's checkpoint for obs-d in a shared database is not
	// this collector's to restore.
	mem.rows["obs-c"] = store.AGCBaselineCheckpoint{CollectorInstanceID: collector, SourceID: "obs-c", Data: json.RawMessage(`{"not":"bands"}`)}
	mem.rows["obs-d"] = store.AGCBaselineCheckpoint{CollectorInstanceID: "collector-b", SourceID: "obs-d", Epoch: "antenna-1", Data: mem.rows["obs-a"].Data}
	restarted := state.New(1)
	if err := restoreAGCBaselines(ctx, mem, collector, restarted, map[string]string{"obs-a": "antenna-1", "obs-b": "antenna-2", "obs-d": "antenna-1"}, at, log); err != nil {
		t.Fatal(err)
	}
	for station, want := range map[string]bool{"obs-a": true, "obs-b": false, "obs-d": false} {
		restarted.Apply(&ingest.RawFrame{Source: station, Recv: at, RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 2000, AntStatus: 2}}}})
		got := restarted.FeedStationRF(at)[station].Bands[0].AGCDeparture != nil
		if got != want {
			t.Errorf("%s: baseline served after restart = %v, want %v", station, got, want)
		}
	}
}
