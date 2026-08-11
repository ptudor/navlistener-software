// Package serve is the SERVE stage: the native, versioned read API
// (docs/OUTPUT.md). It publishes the live per-SV state as the /gnss/api/v2/*
// feeds, each wrapped in the standard response envelope, from an in-RAM snapshot
// refreshed on a slow cadence (satellites move slowly; §5). It binds loopback and
// is physically separate from the ingest write path and the metrics listener — a
// TLS front (a reverse proxy) terminates and fronts it as intsat.space.
package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

// EventStore is the read side of the integrity-event historian the query API serves
// (docs/OUTPUT.md §2.1/§3). The TimescaleDB store satisfies it; it is nil when the
// historian is disabled, in which case the events query endpoints report unavailable.
type EventStore interface {
	QueryEvents(ctx context.Context, q store.EventQuery) ([]store.StoredEvent, int, error)
	SummarizeEventsForAudience(ctx context.Context, audience string, since, until time.Time) (store.EventSummary, error)
}

type ReadAuthorizer interface {
	AuthorizeRead(context.Context, string) (identity.ReadPrincipal, bool)
}

type ViewResolver interface {
	Resolve(identity.Audience) (*state.Store, []config.Source, bool)
}

type requestView struct {
	audience  identity.Audience
	store     *state.Store
	sources   []config.Source
	principal identity.ReadPrincipal
	token     string
}

// schemaVersion is the OUTPUT contract version carried in every feed's data object.
const schemaVersion = "2.0"

// feedGroup is the set of feeds refreshed on the fast cadence; almanac refreshes on
// its own slower cadence (docs/OUTPUT.md §5).
var fastFeeds = []string{"svs", "global", "observers", "sbas"}

// Server is the v2 read API. It caches each feed's marshalled envelope and swaps it
// under an RWMutex on refresh, so a request never blocks on state-lock contention or
// JSON encoding — it copies a ready []byte.
type Server struct {
	http     *http.Server
	store    *state.Store
	events   EventStore
	sources  []config.Source
	log      *slog.Logger
	now      func() time.Time
	audience identity.Audience

	fast time.Duration
	slow time.Duration

	broker   *Broker
	brokerMu sync.Mutex
	brokers  map[string]*Broker

	readAuth         ReadAuthorizer
	resolver         ViewResolver
	reauthorizeEvery time.Duration

	mu    sync.RWMutex
	cache map[string][]byte
}

// New builds the v2 API server bound to addr. sources supplies the configured
// dial observers; state-derived RF/capability reports add authenticated push
// stations as they are seen, so the observer feed is the union of configured
// dial sources and active push identities. fast/slow are the refresh cadences
// (§5); zero uses the defaults (30 s / 90 s).
func New(addr string, st *state.Store, events EventStore, sources []config.Source, fast, slow time.Duration, log *slog.Logger) *Server {
	return NewForAudience(addr, st, events, sources, fast, slow, log,
		identity.Audience{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance})
}

// NewForAudience builds one physically separated audience view. Callers must
// pass a state store that was populated only with contributions authorized for
// this audience; Server never attempts to redact a global aggregate after the
// fact. Organization/collection audiences require a separately authenticated
// read front before they may be constructed.
func NewForAudience(addr string, st *state.Store, events EventStore, sources []config.Source, fast, slow time.Duration, log *slog.Logger, selected identity.Audience) *Server {
	if fast <= 0 {
		fast = 30 * time.Second
	}
	if slow <= 0 {
		slow = 90 * time.Second
	}
	s := &Server{
		store:    st,
		events:   events,
		sources:  sources,
		log:      log,
		now:      time.Now,
		audience: selected,
		fast:     fast,
		slow:     slow,
		broker:   newBroker(),
		cache:    map[string][]byte{},
		brokers:  map[string]*Broker{},
	}
	s.broker.log = log // SSE marshal failures log through the server's real logger
	s.brokers[selected.Key()] = s.broker
	mux := http.NewServeMux()
	mux.HandleFunc("/gnss/api/v2/svs", s.serveFeed("svs"))
	mux.HandleFunc("/gnss/api/v2/global", s.serveFeed("global"))
	mux.HandleFunc("/gnss/api/v2/observers", s.serveFeed("observers"))
	mux.HandleFunc("/gnss/api/v2/almanac", s.serveFeed("almanac"))
	mux.HandleFunc("/gnss/api/v2/sbas", s.serveFeed("sbas"))
	mux.HandleFunc("/gnss/api/v2/audiences", s.serveAudiences)
	mux.HandleFunc("/gnss/api/events/summary", s.serveEventsSummary)
	mux.HandleFunc("/gnss/api/events", s.serveEventsQuery)
	mux.HandleFunc("/gnss/events", s.serveEventStream)
	s.http = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// bounds an idle keep-alive connection between requests. Does not affect
		// an active SSE stream (net/http only counts a connection idle while no handler
		// is running on it) -- WriteTimeout stays unset, SSE's per-write deadline is regression fix.
		IdleTimeout: 120 * time.Second,
	}
	// Shutdown() doesn't cancel in-flight SSE request contexts, so signal the broker
	// to release its handlers when a graceful shutdown begins.
	s.http.RegisterOnShutdown(s.closeBrokers)
	return s
}

// EnableAudienceSelection installs authenticated organization/collection view
// selection. It must be called before Listen/Start. Public remains credential-
// free; every private request is authorized server-side on its canonical key.
func (s *Server) EnableAudienceSelection(auth ReadAuthorizer, resolver ViewResolver, reauthorizeEvery time.Duration) {
	s.readAuth = auth
	s.resolver = resolver
	if reauthorizeEvery <= 0 {
		reauthorizeEvery = 10 * time.Second
	}
	s.reauthorizeEvery = reauthorizeEvery
}

// Listen binds the v2 API listener synchronously : call this at startup, before
// the daemon logs "ready", so a malformed or already-bound [serve].addr fails the process
// immediately instead of leaving it running with no API and rc.d reporting it healthy.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return nil, fmt.Errorf("v2 serve listen %s: %w", s.http.Addr, err)
	}
	return ln, nil
}

// Start refreshes every feed once so the API is warm on the first request, then serves ln
// until it closes. Call Listen synchronously first; run Start in a goroutine.
func (s *Server) Start(ln net.Listener) error {
	s.refreshAll()
	s.log.Info("v2 serve listening", "addr", s.http.Addr)
	err := s.http.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Run drives the refresh tickers until ctx is cancelled. The fast tick rebuilds the
// per-SV/global/observer/sbas feeds; the slow tick rebuilds the almanac.
func (s *Server) Run(ctx context.Context) {
	fast := time.NewTicker(s.fast)
	defer fast.Stop()
	slow := time.NewTicker(s.slow)
	defer slow.Stop()
	for {
		select {
		case <-fast.C:
			for _, f := range fastFeeds {
				s.refresh(f)
			}
		case <-slow.C:
			s.refresh("almanac")
		case <-ctx.Done():
			return
		}
	}
}

// Shutdown gracefully stops the HTTP server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

// PublishEvent fans a confirmed integrity event out to the SSE clients and records
// it in the reconnect-replay ring (docs/OUTPUT.md §3).
func (s *Server) PublishEvent(e EventMsg) {
	s.PublishEventForAudience(s.audience, e)
}

func (s *Server) PublishEventForAudience(a identity.Audience, e EventMsg) {
	s.brokerFor(a).Publish(e)
}

func (s *Server) brokerFor(a identity.Audience) *Broker {
	key := a.Key()
	s.brokerMu.Lock()
	defer s.brokerMu.Unlock()
	if broker := s.brokers[key]; broker != nil {
		return broker
	}
	broker := newBroker()
	broker.log = s.log
	s.brokers[key] = broker
	return broker
}

func (s *Server) closeBrokers() {
	s.brokerMu.Lock()
	brokers := make([]*Broker, 0, len(s.brokers))
	for _, broker := range s.brokers {
		brokers = append(brokers, broker)
	}
	s.brokerMu.Unlock()
	for _, broker := range brokers {
		broker.Close()
	}
}

// Audience returns the immutable view key served and snapshotted by this server.
func (s *Server) Audience() identity.Audience { return s.audience }

// SnapshotFeeds returns a copy of every warmed feed's current marshalled envelope, keyed
// by feed name, for the historian's replay/backfill record (docs/OUTPUT.md §4). Feeds not
// yet built are omitted; the returned byte slices are copies, so the caller may retain them
// without racing the next refresh's cache swap.
func (s *Server) SnapshotFeeds() map[string][]byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[string][]byte, len(s.cache))
	for f, b := range s.cache {
		if len(b) == 0 {
			continue
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		out[f] = cp
	}
	return out
}

func (s *Server) refreshAll() {
	for _, f := range fastFeeds {
		s.refresh(f)
	}
	s.refresh("almanac")
}

// refresh rebuilds one feed's cached envelope bytes. A marshalling failure is logged
// and leaves the previous (stale) bytes in place rather than serving a broken body.
func (s *Server) refresh(feed string) {
	now := s.now()
	body, err := s.buildFeed(feed, s.audience, s.store, s.sources, now)
	if err != nil {
		metrics.ServeFeedMarshalErrorsTotal.WithLabelValues(feed).Inc()
		s.log.Error("serve feed marshal failed", "feed", feed, "audience", s.audience.Key(), "error", err)
		return
	}
	s.mu.Lock()
	s.cache[feed] = body
	s.mu.Unlock()
	metrics.ServeFeedRefreshTimestamp.WithLabelValues(feed).SetToCurrentTime()
}

func (s *Server) buildFeed(feed string, selected identity.Audience, st *state.Store, sources []config.Source, now time.Time) ([]byte, error) {
	if st == nil {
		return nil, fmt.Errorf("audience state is unavailable")
	}
	data := map[string]any{"schema": schemaVersion, "audience": selected.Key()}
	switch feed {
	case "svs":
		data["svs"] = st.FeedSVs(now)
	case "global":
		g := st.FeedGlobal(now)
		for k, v := range g.Counts {
			data[k] = v
		}
		data["last_seen"] = g.LastSeen
		data["leap_seconds"] = g.LeapSeconds
		data["total_live_svs"] = g.TotalLiveSVs
		data["total_live_signals"] = g.TotalLiveSignals
		data["total_live_receivers"] = g.TotalLiveReceivers
	case "observers":
		data["observers"] = s.observers(now, st, sources, selected)
	case "almanac":
		data["almanac"] = st.FeedAlmanac(now)
	case "sbas":
		data["sbas"] = st.FeedSBAS(now)
	default:
		return nil, fmt.Errorf("unknown feed %q", feed)
	}
	body, err := json.Marshal(envelope{OK: true, Time: now.UTC().Format(time.RFC3339), Data: data})
	if err != nil {
		return nil, err
	}
	return body, nil
}

// serveFeed returns the handler for one cached feed. GET/HEAD only; other methods
// get 405. A not-yet-warmed feed is built on demand.
func (s *Server) serveFeed(feed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if methodNotAllowedGetHead(w, r) {
			return
		}
		view, ok := s.resolveRequestView(w, r)
		if !ok {
			return
		}
		// Only the fixed/default view uses the shared warmed cache. Authenticated
		// private responses are rendered per request so cache entries never cross
		// principals or audiences.
		if view.principal.ID != "" || view.audience != s.audience {
			body, err := s.buildFeed(feed, view.audience, view.store, view.sources, s.now())
			if err != nil {
				s.log.Error("serve scoped feed marshal failed", "feed", feed, "audience", view.audience.Key(), "error", err)
				writeError(w, http.StatusInternalServerError, "feed encode failed")
				return
			}
			w.Header().Set("Content-Type", "application/json")
			s.setAudienceCacheHeaders(w, view.audience)
			if r.Method != http.MethodHead {
				_, _ = w.Write(body)
			}
			return
		}
		s.mu.RLock()
		body := s.cache[feed]
		s.mu.RUnlock()
		if body == nil {
			s.refresh(feed)
			s.mu.RLock()
			body = s.cache[feed]
			s.mu.RUnlock()
		}
		w.Header().Set("Content-Type", "application/json")
		s.setAudienceCacheHeaders(w, view.audience)
		if body == nil {
			writeError(w, http.StatusServiceUnavailable, "feed not ready")
			return
		}
		if r.Method != http.MethodHead {
			_, _ = w.Write(body)
		}
	}
}

// envelope is the standard v2 response wrapper (docs/OUTPUT.md §0).
type envelope struct {
	OK   bool           `json:"ok"`
	Time string         `json:"time"`
	Data map[string]any `json:"data"`
}

// observer is one station record (docs/OUTPUT.md §1.3). Configured dial connectors
// and push stations discovered through live RF/capability state share this shape.
// Per-SV reception details live in the svs feed's perrecv map. RF carries the
// PNT-defense per-station RF-environment metrics when
// the receiver reports MON-RF/NAV-SAT telemetry (docs/DEFENSE-PNT.md §6). Capabilities is the
// node's demonstrated (gnssId, sigId) fingerprint — what signals it actually produces, so a
// consumer (and the integrity layer) knows what it should be reporting (docs/CONSTELLATIONS.md
// §7, docs/INTEGRITY.md §6).
type observer struct {
	ID           string                    `json:"id"`
	Vendor       string                    `json:"vendor"`
	Remark       string                    `json:"remark"`
	Disabled     bool                      `json:"disabled"`
	RF           *state.StationRF          `json:"rf,omitempty"`
	Capabilities []state.StationCapability `json:"capabilities,omitempty"`
	// Declared/Unexpected/Missing surface the tudorgps capability mismatch directly in the
	// feed (docs/INTEGRITY.md §6), so an operator sees a signal the silicon shouldn't produce
	// (unexpected) or one it should but hasn't (missing) without waiting for a detector event.
	// All omitted when the node has no declared fingerprint.
	Declared   []state.CapSignal `json:"declared_capabilities,omitempty"`
	Unexpected []state.CapSignal `json:"unexpected_capabilities,omitempty"`
	Missing    []state.CapSignal `json:"missing_capabilities,omitempty"`
}

func (s *Server) observers(now time.Time, st *state.Store, sources []config.Source, selected identity.Audience) []observer {
	rf := st.FeedStationRF(now)
	reps := st.FeedCapabilityReports(now)
	seen := make(map[string]bool, len(sources))
	out := make([]observer, 0, len(sources))
	for _, src := range sources {
		seen[src.Name] = true
		o := observer{
			ID: sanitize(src.Name),
			// Remark is the operator-supplied station note; the internal
			// dial address (LAN topology + the exact port of an unauthenticated
			// raw receiver TCP stream) must never reach this public feed.
			Vendor:   sanitize(src.Type),
			Remark:   sanitize(src.Remark),
			Disabled: src.Disabled,
		}
		if r, ok := rf[src.Name]; ok {
			o.RF = &r
		}
		if rep, ok := reps[src.Name]; ok {
			o.Capabilities = rep.Observed
			o.Declared = rep.Declared
			o.Unexpected, o.Missing = state.CapabilityDiff(rep.Observed, rep.Declared)
		}
		out = append(out, o)
	}
	// FeedStationRF/FeedCapabilityReports key stations by dial source OR
	// authenticated push station id -- the detector already sees both -- but until
	// now this loop joined them only against s.sources (cfg.Ingest), so a
	// push-fleet station's RF and capability data was collected, classified, and
	// evented, yet unreachable in this feed. Union in any station id present in
	// either read model that isn't already a dial source, with dial-only metadata
	// (vendor/remark/disabled) simply absent. Sorted for a stable feed.
	extra := make([]string, 0)
	for id := range rf {
		if !seen[id] && !(selected.Kind == identity.AudiencePublic && audience.IsAnonymousPublicSource(id)) {
			seen[id] = true
			extra = append(extra, id)
		}
	}
	for id := range reps {
		if !seen[id] && !(selected.Kind == identity.AudiencePublic && audience.IsAnonymousPublicSource(id)) {
			seen[id] = true
			extra = append(extra, id)
		}
	}
	sort.Strings(extra)
	for _, id := range extra {
		o := observer{ID: sanitize(id)}
		if r, ok := rf[id]; ok {
			o.RF = &r
		}
		if rep, ok := reps[id]; ok {
			o.Capabilities = rep.Observed
			o.Declared = rep.Declared
			o.Unexpected, o.Missing = state.CapabilityDiff(rep.Observed, rep.Declared)
		}
		out = append(out, o)
	}
	return out
}

func (s *Server) setAudienceCacheHeaders(w http.ResponseWriter, selected identity.Audience) {
	if selected.Kind == identity.AudiencePublic {
		w.Header().Set("Cache-Control", "public, max-age=30")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Vary", "Authorization, X-GNSS-Audience")
}

func (s *Server) serveEventStream(w http.ResponseWriter, r *http.Request) {
	view, ok := s.resolveRequestView(w, r)
	if !ok {
		return
	}
	if view.audience.Kind != identity.AudiencePublic {
		w.Header().Set("Vary", "Authorization, X-GNSS-Audience")
	}
	if view.principal.ID == "" {
		s.brokerFor(view.audience).serveEvents(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go s.watchReadAuthorization(ctx, cancel, view)
	s.brokerFor(view.audience).serveEvents(w, r.WithContext(ctx))
}

func (s *Server) resolveRequestView(w http.ResponseWriter, r *http.Request) (requestView, bool) {
	selected := s.audience
	if values := r.Header.Values("X-GNSS-Audience"); len(values) > 1 {
		writeError(w, http.StatusBadRequest, "multiple audience headers")
		return requestView{}, false
	} else if len(values) == 1 && strings.TrimSpace(values[0]) != "" {
		var err error
		selected, err = identity.ParseAudience(strings.TrimSpace(values[0]))
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return requestView{}, false
		}
	}
	resolve := func() (*state.Store, []config.Source, bool) {
		if s.resolver != nil {
			if st, sources, ok := s.resolver.Resolve(selected); ok {
				return st, sources, true
			}
		}
		if selected == s.audience {
			return s.store, append([]config.Source(nil), s.sources...), s.store != nil
		}
		return nil, nil, false
	}
	if selected.Kind == identity.AudiencePublic {
		st, sources, ok := resolve()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "public audience unavailable")
			return requestView{}, false
		}
		return requestView{audience: selected, store: st, sources: sources}, true
	}
	if s.readAuth == nil {
		if selected != s.audience {
			writeError(w, http.StatusForbidden, "audience selection requires read authorization")
			return requestView{}, false
		}
		st, sources, ok := resolve()
		if !ok {
			writeError(w, http.StatusServiceUnavailable, "audience state unavailable")
			return requestView{}, false
		}
		return requestView{audience: selected, store: st, sources: sources}, true
	}
	token, err := readBearerToken(r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer realm="navlistener"`)
		writeError(w, http.StatusUnauthorized, err.Error())
		return requestView{}, false
	}
	principal, ok := s.readAuth.AuthorizeRead(r.Context(), token)
	if !ok {
		w.Header().Set("WWW-Authenticate", `Bearer realm="navlistener"`)
		writeError(w, http.StatusUnauthorized, "invalid read credential")
		return requestView{}, false
	}
	if !principal.Allows(selected) {
		writeError(w, http.StatusForbidden, "audience is not granted")
		return requestView{}, false
	}
	st, sources, ok := resolve()
	if !ok {
		writeError(w, http.StatusNotFound, "audience has no materialized state")
		return requestView{}, false
	}
	return requestView{audience: selected, store: st, sources: sources, principal: principal, token: token}, true
}

func readBearerToken(r *http.Request) (string, error) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", fmt.Errorf("one bearer authorization header is required")
	}
	const prefix = "Bearer "
	if !strings.HasPrefix(values[0], prefix) {
		return "", fmt.Errorf("bearer authorization is required")
	}
	token := strings.TrimSpace(strings.TrimPrefix(values[0], prefix))
	if token == "" || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("bearer credential is malformed")
	}
	return token, nil
}

func (s *Server) serveAudiences(w http.ResponseWriter, r *http.Request) {
	if methodNotAllowedGetHead(w, r) {
		return
	}
	audiences := []string{"public"}
	principalID := ""
	if len(r.Header.Values("Authorization")) > 0 {
		if s.readAuth == nil {
			writeError(w, http.StatusUnauthorized, "read authorization is not configured")
			return
		}
		token, err := readBearerToken(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="navlistener"`)
			writeError(w, http.StatusUnauthorized, err.Error())
			return
		}
		principal, ok := s.readAuth.AuthorizeRead(r.Context(), token)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="navlistener"`)
			writeError(w, http.StatusUnauthorized, "invalid read credential")
			return
		}
		principalID = principal.ID
		for _, grant := range principal.AudienceGrants {
			if s.resolver == nil {
				if grant == s.audience {
					audiences = append(audiences, grant.Key())
				}
				continue
			}
			if _, _, exists := s.resolver.Resolve(grant); exists {
				audiences = append(audiences, grant.Key())
			}
		}
	}
	now := s.now()
	data := map[string]any{"schema": schemaVersion, "audiences": audiences}
	if principalID != "" {
		data["principal"] = principalID
	}
	body, err := json.Marshal(envelope{OK: true, Time: now.UTC().Format(time.RFC3339), Data: data})
	if err != nil {
		writeError(w, http.StatusInternalServerError, "encode failed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if principalID == "" {
		s.setAudienceCacheHeaders(w, identity.Audience{Kind: identity.AudiencePublic})
	} else {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Vary", "Authorization")
	}
	if r.Method != http.MethodHead {
		_, _ = w.Write(body)
	}
}

func (s *Server) watchReadAuthorization(ctx context.Context, cancel context.CancelFunc, view requestView) {
	ticker := time.NewTicker(s.reauthorizeEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
			principal, ok := s.readAuth.AuthorizeRead(checkCtx, view.token)
			checkCancel()
			if !ok || !view.principal.AuthorizationEqual(principal) || !principal.Allows(view.audience) {
				cancel()
				return
			}
		}
	}
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg, "code": code})
}

// methodNotAllowedGetHead writes a 405 with the RFC 9110 §15.5.6 Allow header when the
// request method is not GET or HEAD, returning true (the caller should then return). // shared by the feed, events-query/summary, and SSE handlers so the Allow header — which the
// SSE handler already set — is applied consistently on every read endpoint's 405.
func methodNotAllowedGetHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return true
	}
	return false
}
