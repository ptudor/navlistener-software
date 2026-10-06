package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

// agcBaselineCheckpointInterval is how often learned AGC baselines are saved, like the
// received-power models.
const agcBaselineCheckpointInterval = 5 * time.Minute

// agcEpochs maps each station to its antenna epoch. An AGC baseline belongs to one
// antenna, cable and receiver path, which power_model_epoch already names for the
// received-power model; a station without one has the empty epoch.
func agcEpochs(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Reception.Stations))
	for _, site := range cfg.Reception.Stations {
		out[site.Observer] = site.PowerModelEpoch
	}
	return out
}

type agcBaselineStore interface {
	LoadAGCBaselines(context.Context) ([]store.AGCBaselineCheckpoint, error)
	SaveAGCBaseline(context.Context, store.AGCBaselineCheckpoint) error
}

// restoreAGCBaselines queues the stored baselines whose epoch is the station's current
// one into live state. Checkpoints that no longer apply stay stored for forensics.
func restoreAGCBaselines(ctx context.Context, historian agcBaselineStore, live *state.Store, epochs map[string]string, now time.Time, log *slog.Logger) error {
	saved, err := historian.LoadAGCBaselines(ctx)
	if err != nil {
		return err
	}
	for _, c := range saved {
		if c.Epoch != epochs[c.SourceID] {
			log.Info("stored AGC baseline is from another antenna epoch; relearning", "observer", c.SourceID)
			continue
		}
		var bands []state.AGCBaseline
		if err := json.Unmarshal(c.Data, &bands); err != nil {
			log.Warn("stored AGC baseline is unreadable; relearning", "observer", c.SourceID, "error", err)
			continue
		}
		if n := live.RestoreAGCBaselines(c.SourceID, bands, now); n < len(bands) {
			log.Info("some stored AGC baselines are too old or invalid; relearning those", "observer", c.SourceID,
				"restored", n, "stored", len(bands))
		}
	}
	return nil
}

// saveAGCBaselines checkpoints every established baseline.
func saveAGCBaselines(ctx context.Context, historian agcBaselineStore, live *state.Store, epochs map[string]string) error {
	var failures []error
	for id, bands := range live.AGCBaselines() {
		data, err := json.Marshal(bands)
		if err != nil {
			failures = append(failures, errors.New(id+": "+err.Error()))
			continue
		}
		if err := historian.SaveAGCBaseline(ctx, store.AGCBaselineCheckpoint{SourceID: id, UpdatedAt: time.Now(), Epoch: epochs[id], Data: data}); err != nil {
			failures = append(failures, errors.New(id+": "+err.Error()))
		}
	}
	return errors.Join(failures...)
}

func agcBaselineLoop(ctx context.Context, historian agcBaselineStore, live *state.Store, epochs map[string]string, log *slog.Logger) {
	ticker := time.NewTicker(agcBaselineCheckpointInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			saveCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			err := saveAGCBaselines(saveCtx, historian, live, epochs)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Error("AGC baseline checkpoint failed", "error", err)
			}
		}
	}
}
