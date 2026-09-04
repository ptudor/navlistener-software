package main

import (
	"io"
	"log/slog"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/state"
)

func TestReceiverMetricsIndependentOfAudienceGrants(t *testing.T) {
	snapshot := func() []float64 {
		return []float64{
			testutil.ToFloat64(metrics.DecodeTotal.WithLabelValues("0", "cnav")),
			testutil.ToFloat64(metrics.NavCRCFailTotal.WithLabelValues("0", "3", "metric-observer")),
			testutil.ToFloat64(metrics.DecodeErrorsTotal.WithLabelValues("7", "navic_deferred")),
			testutil.ToFloat64(metrics.CapturedOnlyTotal.WithLabelValues("metric-observer", "byte_frame")),
			testutil.ToFloat64(metrics.RawObsInvalidTotal.WithLabelValues("metric-observer", "state_validation")),
		}
	}
	run := func(collections []string, public bool) []float64 {
		live, pub, pubEvents := state.New(1), state.NewProjection(1), state.NewProjection(1)
		registry := audience.NewRegistry(1, nil)
		c := identity.NewPrivateContext("metric-observer", identity.CredentialToken)
		c.OrganizationID = "metrics-org"
		c.CollectionIDs = collections
		if public {
			c.Publication.AggregateUse = identity.AggregatePublicAttributed
			c.Publication.EventVisibility = identity.EventsPublic
			c.Publication.StationMetadata = identity.MetadataFull
		}
		now := time.Unix(1700000000, 0)
		good := lifecycleCNAV(10, now, c)
		bad := *good
		bad.Words = append([]uint32(nil), good.Words...)
		bad.Words[3] ^= 1
		unsupported := *good
		unsupported.GnssID = gnss.NavIC
		unsupported.SigID = 0
		captured := *good
		captured.Words = nil
		captured.Bytes = []byte{1, 2, 3}
		captured.MsgType = 1005
		invalidObs := *good
		invalidObs.Words = nil
		invalidObs.Obs = &ingest.RawObs{}
		frames := make(chan *ingest.RawFrame, 5)
		for _, f := range []*ingest.RawFrame{good, &bad, &unsupported, &captured, &invalidObs} {
			frames <- f
		}
		close(frames)
		before := snapshot()
		var last atomic.Int64
		decodeLoop(frames, live, pub, pubEvents, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), &last, registry, nil)
		after := snapshot()
		for i := range after {
			after[i] -= before[i]
		}
		if len(registry.Views()) != 1+len(collections) {
			t.Fatal("private projections not independently materialized")
		}
		for _, v := range registry.Views() {
			if len(v.Store.FeedCapabilityReports(now)) == 0 {
				t.Fatal("metrics suppression lost projected state")
			}
		}
		return after
	}
	one := run(nil, false)
	many := run([]string{"a", "b", "c", "d"}, true)
	if !reflect.DeepEqual(one, []float64{1, 1, 1, 1, 1}) || !reflect.DeepEqual(one, many) {
		t.Fatalf("physical metric deltas one=%v multiple=%v", one, many)
	}
}
