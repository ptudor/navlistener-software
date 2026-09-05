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
	var since time.Time
	if s.policyEpochs != nil {
		_, since = s.policyEpochs.Current(view.audience.Key())
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
	})
}
