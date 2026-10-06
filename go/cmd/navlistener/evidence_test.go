package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/store"
)

func TestEvidencePolicyWithinRetention(t *testing.T) {
	for _, retention := range []time.Duration{7 * 24 * time.Hour, 90 * time.Minute, time.Hour, 10 * time.Minute} {
		p := evidencePolicy(retention)
		if err := p.Validate(); err != nil {
			t.Fatalf("retention %s: %v", retention, err)
		}
		if p.Horizon >= retention {
			t.Fatalf("retention %s: horizon %s reaches expired inputs", retention, p.Horizon)
		}
	}
	if p := evidencePolicy(7 * 24 * time.Hour); p.Horizon != 7*24*time.Hour-time.Hour {
		t.Fatalf("horizon = %s", p.Horizon)
	}
}

type fakeCapturer struct {
	results []int
	err     error
	calls   int
}

func (f *fakeCapturer) CaptureEventEvidence(context.Context, string, time.Time, store.EvidencePolicy) (int, error) {
	f.calls++
	if f.calls > len(f.results) {
		return 0, f.err
	}
	return f.results[f.calls-1], nil
}

func TestSweepEvidenceDrainsFullBatches(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	p := evidencePolicy(7 * 24 * time.Hour)
	before := testutil.ToFloat64(metrics.EvidenceBundlesTotal)
	f := &fakeCapturer{results: []int{p.Batch, p.Batch, 3}}
	sweepEvidence(context.Background(), f, "c1", p, log)
	if f.calls != 3 {
		t.Fatalf("sweep made %d calls, want 3 (two full batches, then a partial one)", f.calls)
	}
	if got := testutil.ToFloat64(metrics.EvidenceBundlesTotal) - before; got != float64(2*p.Batch+3) {
		t.Fatalf("bundles metric advanced %g", got)
	}
	errsBefore := testutil.ToFloat64(metrics.EvidenceCaptureErrorsTotal)
	f = &fakeCapturer{results: []int{p.Batch}, err: errors.New("database unavailable")}
	sweepEvidence(context.Background(), f, "c1", p, log)
	if f.calls != 2 || testutil.ToFloat64(metrics.EvidenceCaptureErrorsTotal)-errsBefore != 1 {
		t.Fatalf("failed sweep: calls %d", f.calls)
	}
}
