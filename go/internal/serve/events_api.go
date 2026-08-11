package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
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

	// client-supplied filter strings, unlike receiver-originated ones
	// (bounded by sanitize/maxStringField=256), reached the DB with no length
	// bound — a multi-MB sv= became a large bind value compared per row.
	// the sv column's value space is NOT just name@sigid keys (≤ ~8
	// chars) — for station-scoped events (jamming_detected, spoofing_suspected,
	// station_rf_degraded, antenna_fault, station_offline) it holds the STATION
	// id (store/events.go), which for push observers may be a DNS FQDN up to
	// 253 bytes (config.ValidObserverID; the mTLS class requires the id to
	// equal a DNS SAN) and for dial sources is the configured source name. 256
	// matches the receiver-string precedent maxStringField (sanitize.go) while
	// still meeting the original DB-nuisance goal.
	eventsMaxSVParam   = 256
	eventsMaxTypeParam = 64
)

// serveEventsQuery is GET /gnss/api/events: a filtered, paginated window over the persisted
// integrity events (docs/OUTPUT.md §2.1/§3). Filters: since/until (RFC3339), sv, type,
// severity (minimum), limit (≤500), offset. Returns the standard envelope with the matched
// events (newest first) and the total before pagination.
func (s *Server) serveEventsQuery(w http.ResponseWriter, r *http.Request) {
	if methodNotAllowedGetHead(w, r) {
		return
	}
	view, ok := s.resolveRequestView(w, r)
	if !ok {
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events history unavailable (historian disabled)")
		return
	}
	q := r.URL.Query()
	now := s.now()
	// an unparsable since/until/severity/limit/offset must not silently
	// coerce to a default with ok:true -- a typo (since=2026-7-1, severity=high,
	// limit=abc) becomes a confidently wrong window/filter with no signal to the
	// caller. Absent (empty) params still default, unchanged.
	untilRaw := q.Get("until")
	sinceRaw := q.Get("since")
	until, err := parseTimeParam(untilRaw, now)
	if err != nil {
		writeError(w, http.StatusBadRequest, "until: "+err.Error())
		return
	}
	// events are historical, so clamp until to now. A client sending a "no upper
	// bound" idiom (until=3000-01-01) would otherwise anchor the window-size clamp below in
	// the far future and silently drag since with it, returning zero rows with ok:true — the
	// confidently-wrong empty output regression fix exists to prevent.
	if until.After(now) {
		until = now
	}
	// an omitted since describes the 24 hours ending at the caller's
	// explicit until, not the 24 hours ending now. Parse/clamp until first so a
	// future upper bound retains now-anchored behavior.
	sinceDefault := now.Add(-24 * time.Hour)
	if untilRaw != "" {
		sinceDefault = until.Add(-24 * time.Hour)
	}
	since, err := parseTimeParam(sinceRaw, sinceDefault)
	if err != nil {
		writeError(w, http.StatusBadRequest, "since: "+err.Error())
		return
	}
	// run the inversion guard whenever the caller supplied since — not
	// only when BOTH bounds were supplied. until is already clamped to now
	//  above, so a future since with an omitted until previously skipped
	// this guard, ran the query with since > until, and returned zero rows with
	// ok:true — the confidently-wrong empty output regression fix/regression fix exist to prevent.
	// A defaulted since (empty sinceRaw) cannot invert by construction.
	if sinceRaw != "" && since.After(until) {
		writeError(w, http.StatusBadRequest, "since after until")
		return
	}
	// bound the filter strings (see the consts above).
	if len(q.Get("sv")) > eventsMaxSVParam {
		writeError(w, http.StatusBadRequest, "sv: too long")
		return
	}
	if len(q.Get("type")) > eventsMaxTypeParam {
		writeError(w, http.StatusBadRequest, "type: too long")
		return
	}
	severity, err := atoiParam(q.Get("severity"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "severity: "+err.Error())
		return
	}
	limit, err := atoiParam(q.Get("limit"), eventsDefaultLimit)
	if err != nil {
		writeError(w, http.StatusBadRequest, "limit: "+err.Error())
		return
	}
	offset, err := atoiParam(q.Get("offset"), 0)
	if err != nil {
		writeError(w, http.StatusBadRequest, "offset: "+err.Error())
		return
	}
	if d := until.Sub(since); d > eventsMaxWindow {
		// clamp the window rather than reject it -- until (defaulting to now) is
		// the caller's anchor; since is pulled forward to eventsMaxWindow before it.
		since = until.Add(-eventsMaxWindow)
	}
	query := store.EventQuery{
		Audience:    view.audience.Key(),
		SV:          q.Get("sv"),
		Type:        q.Get("type"),
		MinSeverity: severity,
		Since:       since,
		Until:       until,
		Limit:       clampInt(limit, 1, eventsMaxLimit),
		Offset:      clampInt(offset, 0, eventsMaxOffset),
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
	s.writeEnvelope(w, now, view.audience, map[string]any{
		"schema": schemaVersion,
		"total":  total,
		"events": events,
	})
}

// serveEventsSummary is GET /gnss/api/events/summary: counts over a rolling window
// (?hours=, default 24, ≤720) — totals, active critical/warning counts, the last critical
// time, and breakdowns by event type and constellation (docs/OUTPUT.md §2.1).
func (s *Server) serveEventsSummary(w http.ResponseWriter, r *http.Request) {
	if methodNotAllowedGetHead(w, r) {
		return
	}
	view, ok := s.resolveRequestView(w, r)
	if !ok {
		return
	}
	if s.events == nil {
		writeError(w, http.StatusServiceUnavailable, "events history unavailable (historian disabled)")
		return
	}
	hoursRaw, err := atoiParam(r.URL.Query().Get("hours"), summaryDefaultHours)
	if err != nil {
		writeError(w, http.StatusBadRequest, "hours: "+err.Error())
		return
	}
	hours := clampInt(hoursRaw, 1, summaryMaxHours)
	now := s.now()
	ctx, cancel := context.WithTimeout(r.Context(), eventsQueryTimeout)
	defer cancel()
	sum, err := s.events.SummarizeEventsForAudience(ctx, view.audience.Key(), now.Add(-time.Duration(hours)*time.Hour), now)
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
	s.writeEnvelope(w, now, view.audience, map[string]any{
		"schema":           schemaVersion,
		"period_hours":     hours,
		"total_events":     sum.TotalEvents,
		"critical_events":  sum.CriticalEvents,
		"warning_events":   sum.WarningEvents,
		"last_critical":    lastCritical,
		"by_type":          sum.ByType,
		"by_constellation": sum.ByConstellation,
		"idle_message":     idleMessage,
	})
}

// writeEnvelope marshals data inside the standard v2 response envelope (docs/OUTPUT.md §0).
func (s *Server) writeEnvelope(w http.ResponseWriter, now time.Time, selected identity.Audience, data map[string]any) {
	body, err := json.Marshal(envelope{OK: true, Time: now.UTC().Format(time.RFC3339), Data: data})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	s.setAudienceCacheHeaders(w, selected)
	_, _ = w.Write(body)
}

// atoiParam parses an integer query param. An absent (empty) value returns def, nil --
// the pre-regression fix default-on-absence behavior, unchanged. A malformed non-empty value
// returns an error instead of silently falling back to def: a typo like limit=abc must
// not become a confidently-wrong default with ok:true.
func atoiParam(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", s)
	}
	return n, nil
}

// parseTimeParam parses an RFC3339 timestamp query param, with the same
// absent-defaults/malformed-errors split as atoiParam.
func parseTimeParam(s string, def time.Time) (time.Time, error) {
	if s == "" {
		return def, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid RFC3339 timestamp %q", s)
	}
	return t, nil
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
