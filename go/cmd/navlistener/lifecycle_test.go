package main

import (
	"context"
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/state"
)

func lifecycleCNAV(msg int, now time.Time, c identity.ObserverContext) *ingest.RawFrame {
	b := make([]byte, 40)
	set := func(start, n int, v uint64) {
		for i := 0; i < n; i++ {
			if v&(1<<uint(n-1-i)) != 0 {
				b[(start+i)/8] |= 1 << uint(7-(start+i)%8)
			}
		}
	}
	b[0] = 0x8b
	set(8, 6, 5)
	set(14, 6, uint64(msg))
	if msg == 10 {
		set(38, 13, 2288)
		set(70, 11, 400)
	} else {
		set(38, 11, 400)
	}
	set(276, 24, uint64(frame.CRC24QBits(b, 0, 276)))
	words := make([]uint32, 10)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(b[i*4:])
	}
	return &ingest.RawFrame{Recv: now, RecvLocal: now, Source: c.ObserverID, Observer: c, GnssID: gnss.GPS, SvID: 5, SigID: 3, Words: words}
}
func eventuallyLifecycle(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("lifecycle tick did not complete")
		}
		time.Sleep(time.Millisecond)
	}
}
func TestDynamicLifecycleAfterSchedulerStartsWithoutHistorian(t *testing.T) {
	t0 := time.Unix(1700000000, 0)
	registry := audience.NewRegistry(1, nil)
	public, operator := state.New(1), state.New(1)
	registry.Register(identity.Audience{Kind: identity.AudiencePublic}, public, nil)
	registry.Register(identity.Audience{Kind: identity.AudienceOperator, ID: "collector"}, operator, nil)
	prop, expire := make(chan time.Time), make(chan time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	cfg := config.State{SVTTL: 2 * time.Hour, PropagateEvery: time.Second}
	go func() { defer close(done); stateLoopTicks(ctx, cfg, registry, prop, expire, public, operator) }()
	prop <- t0 // scheduler already running when the private views materialize
	c := identity.NewPrivateContext("observer", identity.CredentialToken)
	c.OrganizationID = "org"
	c.CollectionIDs = []string{"fleet"}
	for _, msg := range []int{10, 11} {
		f := lifecycleCNAV(msg, t0, c)
		registry.ApplyPrivate(f)
		operator.Apply(f)
	}
	rf := &ingest.RawFrame{Recv: t0, RecvLocal: t0, Source: c.ObserverID, Observer: c, RF: &ingest.RawRF{Bands: []ingest.RFBand{{AGC: 100}}}}
	registry.ApplyPrivate(rf)
	operator.Apply(rf)
	views := []*state.Store{operator}
	for _, a := range []identity.Audience{{Kind: identity.AudienceOrganization, ID: "org"}, {Kind: identity.AudienceCollection, ID: "fleet"}} {
		s, _, ok := registry.Resolve(a)
		if !ok {
			t.Fatal("view missing")
		}
		views = append(views, s)
	}
	tick := t0.Add(time.Second)
	prop <- tick
	for _, s := range views {
		eventuallyLifecycle(t, func() bool { return s.FeedSVs(tick)["G05@3"].PosAtUnixNs == tick.UnixNano() })
		sv := s.FeedSVs(tick)["G05@3"]
		if sv.XM == nil || sv.YM == nil || sv.ZM == nil {
			t.Fatal("missing ECEF")
		}
		radius := math.Sqrt(*sv.XM**sv.XM + *sv.YM**sv.YM + *sv.ZM**sv.ZM)
		if radius < 26e6 || radius > 27.2e6 {
			t.Fatalf("ECEF radius %g", radius)
		}
	}
	// Fixed empty public must not replace the operator gauge; dynamic stores
	// and the operator all contain one SV, whose gauge must remain exactly one.
	if testutil.ToFloat64(metrics.LiveSVs.WithLabelValues("gps")) != 1 {
		t.Fatal("gauge lost operator meaning")
	}
	next := tick.Add(time.Second)
	prop <- next
	for _, s := range views {
		eventuallyLifecycle(t, func() bool { return s.FeedSVs(next)["G05@3"].PosAtUnixNs == next.UnixNano() })
	}
	expire <- t0.Add(cfg.SVTTL + time.Second)
	for _, s := range views {
		eventuallyLifecycle(t, func() bool { return len(s.Snapshot(t0).SVs) == 0 && len(s.FeedStationRF(t0)) == 0 })
	}
	// Querying at the original time above proves actual eviction, not feed-time filtering.
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not stop")
	}
}
func TestLifecycleDeduplicatesRegisteredFixedStores(t *testing.T) {
	registry := audience.NewRegistry(1, nil)
	public, operator := state.New(1), state.New(1)
	registry.Register(identity.Audience{Kind: identity.AudiencePublic}, public, nil)
	registry.Register(identity.Audience{Kind: identity.AudienceOperator, ID: "collector"}, operator, nil)
	got := lifecycleStores(registry, []*state.Store{public, public, operator})
	if len(got) != 2 || got[0] != public || got[1] != operator {
		t.Fatal("fixed stores duplicated or operator order lost")
	}
}
