package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/store"
)

// EventEvidenceStore reads the durable evidence captured for station events.
type EventEvidenceStore interface {
	QueryEventEvidence(context.Context, store.EvidenceQuery) (store.EventEvidence, error)
}

// SetEventEvidence wires the historian's event evidence before the server starts.
func (s *Server) SetEventEvidence(evidence EventEvidenceStore) { s.eventEvidence = evidence }

func eventEvidenceParams(r *http.Request, view requestView) (store.EvidenceQuery, error) {
	q := store.EvidenceQuery{Audience: view.audience}
	if len(r.URL.RawQuery) > 4096 {
		return q, fmt.Errorf("query too long")
	}
	values, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return q, fmt.Errorf("invalid query encoding")
	}
	for key, vals := range values {
		switch key {
		case "id", "limit", "offset":
		default:
			return q, fmt.Errorf("unknown query parameter")
		}
		if len(vals) != 1 || vals[0] == "" {
			return q, fmt.Errorf("query parameters must have one nonempty value")
		}
	}
	q.Seq, err = strconv.ParseInt(values.Get("id"), 10, 64)
	if err != nil {
		return q, fmt.Errorf("id must be the event's integer id")
	}
	if q.Limit, err = atoiParam(values.Get("limit"), 100); err != nil {
		return q, fmt.Errorf("limit must be an integer")
	}
	if q.Offset, err = atoiParam(values.Get("offset"), 0); err != nil {
		return q, fmt.Errorf("offset must be an integer")
	}
	return q, q.Validate()
}

// serveEventEvidence returns one station event with the inputs captured behind it
// (docs/OUTPUT.md §3): the event, its evidence window, and a page of the stored RF,
// receiver-solution and board samples, exact bodies included. It is private: the
// samples hold the receiver's coordinates. The event id is the audience's own event
// id, and the evidence was captured scoped to that audience.
func (s *Server) serveEventEvidence(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization, X-GNSS-Audience")
	if methodNotAllowedGetHead(w, r) {
		return
	}
	if s.readAuth == nil {
		writeError(w, http.StatusServiceUnavailable, "event evidence requires native read authorization")
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
		writeError(w, http.StatusForbidden, "event evidence requires a private audience")
		return
	}
	if s.eventEvidence == nil {
		writeError(w, http.StatusServiceUnavailable, "event evidence unavailable (historian disabled)")
		return
	}
	delivery := s.beginDelivery(r, view.audience)
	defer delivery.finish()
	q, err := eventEvidenceParams(r, view)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	release, ok := s.acquireHistorySlot(view.principal.ID)
	if !ok {
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "event evidence busy; retry request")
		return
	}
	defer release()
	evidence, err := s.eventEvidence.QueryEventEvidence(ctx, q)
	_, visibleSince := s.policyEpochs.Current(view.audience.Key())
	switch {
	case errors.Is(err, store.ErrNoEvidence) || (err == nil && evidence.Event.Time.Before(visibleSince)):
		// An event before the audience's current policy epoch is as invisible
		// here as it is in the events API.
		writeError(w, http.StatusNotFound, "no evidence for this event")
		return
	case err != nil:
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) || ctx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, "event evidence query timed out")
		} else {
			s.log.Error("event evidence query failed", "error", err)
			writeError(w, http.StatusInternalServerError, "event evidence query failed")
		}
		return
	}
	var nextOffset *int
	if evidence.HasMore {
		next := q.Offset + len(evidence.Samples)
		if next <= store.ObserverHistoryMaxOffset {
			nextOffset = &next
		}
	}
	now := s.now().UTC()
	body, err := json.Marshal(envelope{OK: true, Time: now.Format(time.RFC3339), Data: map[string]any{
		"schema": schemaVersion, "audience": view.audience.Key(),
		"limit": q.Limit, "offset": q.Offset, "next_offset": nextOffset, "evidence": evidence,
	}})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode failed")
		return
	}
	// Recheck after the database read and encoding: grants can be revoked while
	// either operation is in flight.
	principal, authorized := s.readAuth.AuthorizeRead(ctx, view.token)
	if ctx.Err() != nil {
		writeError(w, http.StatusGatewayTimeout, "event evidence query timed out")
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
