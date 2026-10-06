package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/store"
)

// evidenceSweepInterval is how often the collector captures evidence for newly
// confirmed station events. Capture waits for the post-roll anyway, so a sweep
// interval of the same order adds little latency.
const evidenceSweepInterval = 30 * time.Second

// evidencePolicy keeps ten minutes of inputs before a station event and one after,
// and looks back as far as raw retention still holds them, less a margin for the
// retention job's own schedule.
func evidencePolicy(rawRetention time.Duration) store.EvidencePolicy {
	horizon := rawRetention - time.Hour
	if horizon <= 2*time.Minute {
		horizon = rawRetention / 2
	}
	return store.EvidencePolicy{
		PreRoll: 10 * time.Minute, PostRoll: time.Minute, Horizon: horizon,
		Batch: 20, MaxSamples: 20000,
	}
}

type evidenceCapturer interface {
	CaptureEventEvidence(context.Context, string, time.Time, store.EvidencePolicy) (int, error)
}

// evidenceLoop captures station event evidence on a cadence until ctx ends. Capture
// state lives in the database, so a failed sweep, a restart or a crash simply
// leaves the events for the next sweep while their inputs remain.
func evidenceLoop(ctx context.Context, historian evidenceCapturer, collectorID string, p store.EvidencePolicy, log *slog.Logger) {
	tick := time.NewTicker(evidenceSweepInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			sweepEvidence(ctx, historian, collectorID, p, log)
		}
	}
}

// sweepEvidence drains the events due for capture, a batch at a time, within one
// sweep interval.
func sweepEvidence(ctx context.Context, historian evidenceCapturer, collectorID string, p store.EvidencePolicy, log *slog.Logger) {
	sweepCtx, cancel := context.WithTimeout(ctx, evidenceSweepInterval)
	defer cancel()
	for {
		n, err := historian.CaptureEventEvidence(sweepCtx, collectorID, time.Now(), p)
		metrics.EvidenceBundlesTotal.Add(float64(n))
		if err != nil {
			if ctx.Err() == nil {
				metrics.EvidenceCaptureErrorsTotal.Inc()
				log.Warn("event evidence capture failed; will retry", "error", err, "captured", n)
			}
			return
		}
		if n < p.Batch {
			return
		}
	}
}
