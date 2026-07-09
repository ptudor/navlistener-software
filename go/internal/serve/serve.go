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
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
)

// EventStore is the read side of the integrity-event historian the query API serves
// (docs/OUTPUT.md §2.1/§3). The TimescaleDB store satisfies it; it is nil when the
// historian is disabled, in which case the events query endpoints report unavailable.
type EventStore interface {
	QueryEvents(ctx context.Context, q store.EventQuery) ([]store.StoredEvent, int, error)
	SummarizeEvents(ctx context.Context, since, until time.Time) (store.EventSummary, error)
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
	http    *http.Server
	store   *state.Store
	events  EventStore
	sources []config.Source
	log     *slog.Logger
	now     func() time.Time

	fast time.Duration
	slow time.Duration

	broker *Broker

	mu    sync.RWMutex
	cache map[string][]byte
}

// New builds the v2 API server bound to addr. sources describes the configured
// ingest connectors, published (dial mode) as the observer list until authenticated
// push observers replace it. fast/slow are the refresh cadences (§5); zero uses the
// defaults (30 s / 90 s).
func New(addr string, st *state.Store, events EventStore, sources []config.Source, fast, slow time.Duration, log *slog.Logger) *Server {
	if fast <= 0 {
		fast = 30 * time.Second
	}
	if slow <= 0 {
		slow = 90 * time.Second
	}
	s := &Server{
		store:   st,
		events:  events,
		sources: sources,
		log:     log,
		now:     time.Now,
		fast:    fast,
		slow:    slow,
		broker:  newBroker(),
		cache:   map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/gnss/api/v2/svs", s.serveFeed("svs"))
	mux.HandleFunc("/gnss/api/v2/global", s.serveFeed("global"))
	mux.HandleFunc("/gnss/api/v2/observers", s.serveFeed("observers"))
	mux.HandleFunc("/gnss/api/v2/almanac", s.serveFeed("almanac"))
	mux.HandleFunc("/gnss/api/v2/sbas", s.serveFeed("sbas"))
	mux.HandleFunc("/gnss/api/events/summary", s.serveEventsSummary)
	mux.HandleFunc("/gnss/api/events", s.serveEventsQuery)
	mux.HandleFunc("/gnss/events", s.broker.serveEvents)
	s.http = &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Start refreshes every feed once so the API is warm on the first request, then
// serves until the listener closes. Run it in a goroutine; run Refresh separately.
func (s *Server) Start() error {
	s.refreshAll()
	s.log.Info("v2 serve listening", "addr", s.http.Addr)
	err := s.http.ListenAndServe()
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
	s.broker.Publish(e)
}

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
	data := map[string]any{"schema": schemaVersion}
	switch feed {
	case "svs":
		data["svs"] = s.store.FeedSVs(now)
	case "global":
		g := s.store.FeedGlobal(now)
		for k, v := range g.Counts {
			data[k] = v
		}
		data["last_seen"] = g.LastSeen
		data["leap_seconds"] = g.LeapSeconds
		data["total_live_svs"] = g.TotalLiveSVs
		data["total_live_signals"] = g.TotalLiveSignals
		data["total_live_receivers"] = g.TotalLiveReceivers
	case "observers":
		data["observers"] = s.observers(now)
	case "almanac":
		data["almanac"] = s.store.FeedAlmanac(now)
	case "sbas":
		data["sbas"] = s.store.FeedSBAS(now)
	default:
		return
	}
	body, err := json.Marshal(envelope{OK: true, Time: now.UTC().Format(time.RFC3339), Data: data})
	if err != nil {
		s.log.Error("serve feed marshal failed", "feed", feed, "error", err)
		return
	}
	s.mu.Lock()
	s.cache[feed] = body
	s.mu.Unlock()
}

// serveFeed returns the handler for one cached feed. GET/HEAD only; other methods
// get 405. A not-yet-warmed feed is built on demand.
func (s *Server) serveFeed(feed string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			writeError(w, http.StatusMethodNotAllowed, "method not allowed")
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
		if body == nil {
			writeError(w, http.StatusServiceUnavailable, "feed not ready")
			return
		}
		_, _ = w.Write(body)
	}
}

// envelope is the standard v2 response wrapper (docs/OUTPUT.md §0).
type envelope struct {
	OK   bool           `json:"ok"`
	Time string         `json:"time"`
	Data map[string]any `json:"data"`
}

// observer is one station record (docs/OUTPUT.md §1.3). In dial mode each configured
// ingest connector is published as a receiver we control; the richer per-SV reception
// (perrecv, position, azimuth/elevation) arrives with the authenticated push +
// measurement path. RF carries the PNT-defense per-station RF-environment metrics when
// the receiver reports MON-RF/NAV-SAT telemetry (docs/DEFENSE-PNT.md §6).
type observer struct {
	ID       string           `json:"id"`
	Vendor   string           `json:"vendor"`
	Remark   string           `json:"remark"`
	Disabled bool             `json:"disabled"`
	RF       *state.StationRF `json:"rf,omitempty"`
}

func (s *Server) observers(now time.Time) []observer {
	rf := s.store.FeedStationRF(now)
	out := make([]observer, 0, len(s.sources))
	for _, src := range s.sources {
		o := observer{
			ID:       sanitize(src.Name),
			Vendor:   sanitize(src.Type),
			Remark:   sanitize(src.Addr),
			Disabled: src.Disabled,
		}
		if r, ok := rf[src.Name]; ok {
			o.RF = &r
		}
		out = append(out, o)
	}
	return out
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": msg, "code": code})
}
