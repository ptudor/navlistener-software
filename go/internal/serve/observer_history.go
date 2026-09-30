package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/store"
)

type ObserverHistoryStore interface {
	QueryObserverSamples(context.Context, store.ObserverSampleQuery) (store.ObserverSamplePage, error)
}

// SetObserverHistory wires the private historian before the server starts. A
// native ReadAuthorizer is also required, even on a fixed operator listener.
func (s *Server) SetObserverHistory(history ObserverHistoryStore, collectorID string) {
	s.observerHistory = history
	s.historyCollector = collectorID
}

func observerHistoryParams(r *http.Request, now time.Time, view requestView, collectorID string) (store.ObserverSampleQuery, string, error) {
	q := store.ObserverSampleQuery{Audience: view.audience, CollectorID: collectorID}
	if len(r.URL.RawQuery) > 16*1024 {
		return q, "", fmt.Errorf("query too long")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, "", fmt.Errorf("invalid query encoding")
	}
	for key, vals := range values {
		switch key {
		case "observer", "kind", "since", "until", "limit", "offset", "revision":
		default:
			return q, "", fmt.Errorf("unknown query parameter")
		}
		if len(vals) != 1 || vals[0] == "" {
			return q, "", fmt.Errorf("query parameters must have one nonempty value")
		}
	}
	q.Observer = values.Get("observer")
	q.Kind = values.Get("kind")
	if q.Kind == "" {
		q.Kind = "environment"
	}
	q.Until, err = parseTimeParam(values.Get("until"), now)
	if err != nil {
		return q, "", fmt.Errorf("until must be RFC3339")
	}
	if q.Until.After(now) {
		q.Until = now
	}
	q.Since, err = parseTimeParam(values.Get("since"), q.Until.Add(-time.Hour))
	if err != nil {
		return q, "", fmt.Errorf("since must be RFC3339")
	}
	q.Limit, err = atoiParam(values.Get("limit"), 100)
	if err != nil {
		return q, "", fmt.Errorf("limit must be an integer")
	}
	q.Offset, err = atoiParam(values.Get("offset"), 0)
	if err != nil {
		return q, "", fmt.Errorf("offset must be an integer")
	}
	return q, values.Get("revision"), q.Validate()
}

func (s *Server) serveObserverSamples(w http.ResponseWriter, r *http.Request) {
	// Errors, including anonymous denials, must never become shared cache entries.
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization, X-GNSS-Audience")
	if methodNotAllowedGetHead(w, r) {
		return
	}
	if s.readAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "sensor history requires native read authorization")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	view, ok := s.resolveRequestView(w, r)
	if !ok {
		return
	}
	if view.audience.Kind == identity.AudiencePublic || view.principal.ID == "" {
		writeError(w, http.StatusForbidden, "sensor history requires a private audience")
		return
	}
	if s.observerHistory == nil || !identity.ValidScopeID(s.historyCollector) {
		writeError(w, http.StatusServiceUnavailable, "sensor history unavailable (historian disabled)")
		return
	}
	delivery := s.beginDelivery(r, view.audience)
	defer delivery.finish()
	now := s.now().UTC()
	q, requestedRevision, err := observerHistoryParams(r, now, view, s.historyCollector)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	revision := s.discoveryRevision(view.principal.Revision, []string{view.audience.Key()})
	if requestedRevision != "" && requestedRevision != revision {
		writeError(w, http.StatusConflict, "history authorization revision changed; restart pagination")
		return
	}
	_, visibleSince := s.policyEpochs.Current(view.audience.Key())
	requestedSince := q.Since
	if q.Since.Before(visibleSince) {
		q.Since = visibleSince
	}
	select {
	case s.historySlots <- struct{}{}:
		defer func() { <-s.historySlots }()
	default:
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "sensor history busy; retry request")
		return
	}
	page := store.ObserverSamplePage{Samples: []store.StoredObserverSample{}}
	if !q.Since.After(q.Until) {
		page, err = s.observerHistory.QueryObserverSamples(ctx, q)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
				writeError(w, http.StatusGatewayTimeout, "sensor history query timed out")
			} else {
				s.log.Error("sensor history query failed", "error", err)
				writeError(w, http.StatusInternalServerError, "sensor history query failed")
			}
			return
		}
	}
	if page.Samples == nil {
		page.Samples = []store.StoredObserverSample{}
	}
	var nextOffset *int
	paginationLimited := false
	if page.HasMore {
		next := q.Offset + len(page.Samples)
		if next <= store.ObserverHistoryMaxOffset {
			nextOffset = &next
		} else {
			paginationLimited = true
		}
	}
	body, err := json.Marshal(envelope{OK: true, Time: now.Format(time.RFC3339), Data: map[string]any{
		"schema": schemaVersion, "audience": view.audience.Key(), "observer": q.Observer, "kind": q.Kind,
		"since": requestedSince, "until": q.Until, "effective_since": q.Since, "visible_since": visibleSince,
		"history_limited": q.Since.After(requestedSince), "revision": revision,
		"limit": q.Limit, "offset": q.Offset, "next_offset": nextOffset,
		"has_more": page.HasMore, "pagination_limited": paginationLimited, "samples": page.Samples,
	}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode failed")
		return
	}
	// Recheck after the database read and encoding: grants can be revoked while
	// either operation is in flight. Audience resets additionally guard delivery.
	principal, authorized := s.readAuth.AuthorizeRead(ctx, view.token)
	if ctx.Err() != nil {
		writeError(w, http.StatusGatewayTimeout, "sensor history query timed out")
		return
	}
	if !authorized || !principal.AuthorizationEqual(view.principal) || !principal.Allows(view.audience) {
		writeError(w, http.StatusForbidden, "read authorization changed; retry with current credentials")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = controller.SetWriteDeadline(time.Time{}) }()
	if r.Method == http.MethodHead {
		body = nil
	}
	delivery.write(w, body)
}
