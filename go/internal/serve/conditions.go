package serve

import (
	"context"
	"github.com/ptudor/navlistener/internal/store"
	"net/http"
	"time"
)

type conditionStore interface {
	CurrentConditions(context.Context, string, time.Time) (store.ConditionSnapshot, error)
}

func (s *Server) serveCurrentConditions(w http.ResponseWriter, r *http.Request) {
	if methodNotAllowedGetHead(w, r) {
		return
	}
	view, ok := s.resolveRequestView(w, r)
	if !ok {
		return
	}
	backend, ok := s.events.(conditionStore)
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "current conditions unavailable")
		return
	}
	delivery := s.beginDelivery(r, view.audience)
	defer delivery.finish()
	// The snapshot starts at the audience's current policy boundary, which is
	// the process start for an audience that has had no policy transition: a
	// condition confirmed by an earlier process is not re-served until a
	// detector re-confirms it (docs/OUTPUT.md, "Current station conditions").
	// The response says so — visible_since and history_limited, as the
	// sensor-history endpoint reports them — so a consumer never reads an
	// empty snapshot as an all-clear for the time before the boundary.
	var since time.Time
	if s.policyEpochs != nil {
		_, since = s.policyEpochs.Current(view.audience.Key())
	}
	select {
	case s.querySlots <- struct{}{}:
		defer func() { <-s.querySlots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "current conditions busy; retry request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), eventsQueryTimeout)
	defer cancel()
	snapshot, err := backend.CurrentConditions(ctx, view.audience.Key(), since)
	if err != nil {
		s.log.Error("current conditions failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "current conditions unavailable")
		return
	}
	s.writeEnvelope(w, s.now(), view.audience, delivery, map[string]any{
		"schema": schemaVersion, "audience": view.audience.Key(), "complete": true,
		"epoch":  since.UTC().Format(time.RFC3339Nano),
		"cursor": snapshot.Cursor, "events": snapshot.Events,
		"visible_since": since, "history_limited": !since.IsZero(),
	})
}
