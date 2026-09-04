package main

import (
	"context"
	"time"

	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/state"
)

// lifecycleStores includes views materialized since the previous tick. Fixed
// stores retain their caller order (operator last, for process-wide gauges),
// and pointers already registered under audience keys are processed only once.
func lifecycleStores(registry *audience.Registry, fixed []*state.Store) []*state.Store {
	seen := make(map[*state.Store]bool, len(fixed))
	for _, s := range fixed {
		seen[s] = true
	}
	var stores []*state.Store
	if registry != nil {
		for _, s := range registry.Stores() {
			if s != nil && !seen[s] {
				seen[s] = true
				stores = append(stores, s)
			}
		}
	}
	seen = make(map[*state.Store]bool, len(fixed))
	for _, s := range fixed {
		if s != nil && !seen[s] {
			seen[s] = true
			stores = append(stores, s)
		}
	}
	return stores
}

// One shutdown-joined worker handles the registry's bounded materialized set.
// Tickers coalesce overload; no per-audience goroutines or queued tick backlog.
func stateLoop(ctx context.Context, cfg config.State, registry *audience.Registry, stores ...*state.Store) {
	prop := time.NewTicker(cfg.PropagateEvery)
	defer prop.Stop()
	expire := time.NewTicker(expireTickFor(cfg.SVTTL))
	defer expire.Stop()
	stateLoopTicks(ctx, cfg, registry, prop.C, expire.C, stores...)
}

func stateLoopTicks(ctx context.Context, cfg config.State, registry *audience.Registry, prop, expire <-chan time.Time, stores ...*state.Store) {
	for {
		var now time.Time
		propagate := false
		select {
		case now = <-prop:
			propagate = true
		case now = <-expire:
		case <-ctx.Done():
			return
		}
		for _, s := range lifecycleStores(registry, stores) {
			if ctx.Err() != nil {
				return
			}
			if propagate {
				s.Propagate(now)
			} else {
				s.Expire(now, cfg.SVTTL)
				s.ExpireStations(now)
			}
		}
	}
}
