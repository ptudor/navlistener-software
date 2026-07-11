package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ptudor/navlistener/internal/store"
)

// Events query API bounds (docs/OUTPUT.md §2.1). Windowed reads over the historian, so the
// serve front can cache them (Cache-Control is set by the fronting proxy, §5).
const (
	eventsDefaultLimit  = 100
	eventsMaxLimit      = 500
	summaryDefaultHours = 24
	summaryMaxHours     = 720

	// an unbounded since/until window forces count(*) OVER() to materialize the
	// entire matching set (gnss_events has no retention policy); an unbounded offset adds
	// unnecessary pagination depth on top. eventsMaxWindow mirrors summaryMaxHours (the
	// existing precedent for "how far back is a legitimate query allowed to look").
	eventsMaxWindow    = summaryMaxHours * time.Hour
	eventsMaxOffset    = 1_000_000
	eventsQueryTimeout = 5 * time.Second
)

// serveEventsQuery is GET /gnss/api/events: a filtered, paginated window over the persisted
// integrity events (docs/OUTPUT.md §2.1/§3). Filters: since/until (RFC3339), sv, type,
// severity (minimum), limit (≤500), offset. Returns the standard envelope with the matched
// events (newest first) and the total before pagination.
func (s *Server) serveEventsQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events history unavailable (historian disabled)")
		return
	}
	q := r.URL.Query()
	now := s.now()
	since := parseTimeDefault(q.Get("since"), now.Add(-24*time.Hour))
	until := parseTimeDefault(q.Get("until"), now)
	if d := until.Sub(since); d > eventsMaxWindow {
		// clamp the window rather than reject it -- until (defaulting to now) is
		// the caller's anchor; since is pulled forward to eventsMaxWindow before it.
		since = until.Add(-eventsMaxWindow)
	}
	query := store.EventQuery{
		SV:          q.Get("sv"),
		Type:        q.Get("type"),
		MinSeverity: atoiDefault(q.Get("severity"), 0),
		Since:       since,
		Until:       until,
		Limit:       clampInt(atoiDefault(q.Get("limit"), eventsDefaultLimit), 1, eventsMaxLimit),
		Offset:      clampInt(atoiDefault(q.Get("offset"), 0), 0, eventsMaxOffset),
	}
	ctx, cancel := context.WithTimeout(r.Context(), eventsQueryTimeout)
	defer cancel()
	events, total, err := s.events.QueryEvents(ctx, query)
	if err != nil {
		s.log.Error("events query failed", "error", err)
		writeError(w, http.StatusInternalServerError, "events query failed")
		return
	}
	if events == nil {
		events = []store.StoredEvent{}
	}
	s.writeEnvelope(w, now, map[string]any{
		"schema": schemaVersion,
		"total":  total,
		"events": events,
	})
}

// serveEventsSummary is GET /gnss/api/events/summary: counts over a rolling window
// (?hours=, default 24, ≤720) — totals, active critical/warning counts, the last critical
// time, and breakdowns by event type and constellation (docs/OUTPUT.md §2.1).
func (s *Server) serveEventsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events history unavailable (historian disabled)")
		return
	}
	hours := clampInt(atoiDefault(r.URL.Query().Get("hours"), summaryDefaultHours), 1, summaryMaxHours)
	now := s.now()
	ctx, cancel := context.WithTimeout(r.Context(), eventsQueryTimeout)
	defer cancel()
	sum, err := s.events.SummarizeEvents(ctx, now.Add(-time.Duration(hours)*time.Hour), now)
	if err != nil {
		s.log.Error("events summary failed", "error", err)
		writeError(w, http.StatusInternalServerError, "events summary failed")
		return
	}

	var lastCritical, idleMessage any
	if sum.LastCritical != nil {
		lastCritical = sum.LastCritical.Format(time.RFC3339)
	}
	if sum.TotalEvents == 0 {
		idleMessage = fmt.Sprintf("No events in the last %d hours", hours)
	}
	s.writeEnvelope(w, now, map[string]any{
		"schema":           schemaVersion,
		"period_hours":     hours,
		"total_events":     sum.TotalEvents,
		"active_critical":  sum.ActiveCritical,
		"active_warnings":  sum.ActiveWarnings,
		"last_critical":    lastCritical,
		"by_type":          sum.ByType,
		"by_constellation": sum.ByConstellation,
		"idle_message":     idleMessage,
	})
}

// writeEnvelope marshals data inside the standard v2 response envelope (docs/OUTPUT.md §0).
func (s *Server) writeEnvelope(w http.ResponseWriter, now time.Time, data map[string]any) {
	body, err := json.Marshal(envelope{OK: true, Time: now.UTC().Format(time.RFC3339), Data: data})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(body)
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// parseTimeDefault parses an RFC3339 timestamp, falling back to def on any error.
func parseTimeDefault(s string, def time.Time) time.Time {
	if s == "" {
		return def
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return def
	}
	return t
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
