// Package server runs the observability HTTP listener: Prometheus /metrics, a
// /healthz endpoint, and an optional /debug/state live-state snapshot. It is
// entirely separate from the native v2 read API and the ingest path. Production
// configuration normally binds it to loopback; non-loopback binds are allowed
// with a startup warning for remote Prometheus deployments, while main guards
// /debug/state by the request peer address.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ptudor/navlistener/internal/version"
)

// Server is the metrics/health HTTP server.
type Server struct {
	http    *http.Server
	log     *slog.Logger
	mu      sync.RWMutex
	failure string
	probes  []healthProbe
}

// healthProbe is one data-plane liveness check. fn returns "" while
// healthy, or a short reason while degraded. Probes make /healthz reflect the
// pipeline, not just listener termination: before them, the endpoint stayed
// "ok" while every ingest source was down or the historian dropped every frame
// — a green health endpoint over a dead data plane, the exact silent-wrong
// output this daemon exists to prevent.
type healthProbe struct {
	name string
	fn   func() string
}

// AddProbe registers a data-plane liveness probe under name. A degraded probe
// yields status "degraded" with HTTP 200 — deliberately NOT 503, so a transient
// ingest gap or DB blip cannot flap rc.d/daemon(8) restarts; 503 stays reserved
// for Fail() (a required component terminated). Call before Start.
func (s *Server) AddProbe(name string, fn func() string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.probes = append(s.probes, healthProbe{name: name, fn: fn})
}

// New builds the server bound to addr (e.g. 127.0.0.1:9100). If debugState is
// non-nil it is served at /debug/state as a compact diagnostic view of live
// per-SV state. The caller owns any access-control policy for that handler.
func New(addr string, log *slog.Logger, debugState http.HandlerFunc) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		status, failure, degraded := "ok", "", map[string]string(nil)
		// The handler closes over s through health below, initialized after the mux.
		if h := healthStateFromContext(r.Context()); h != nil {
			status, failure, degraded = h.status()
		}
		// Only "failed" (a required component terminated) is 503; "degraded"
		// (data-plane liveness probes, regression fix) stays 200 so external probes see
		// the warning without the supervisor flapping the process.
		if status == "failed" {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		body := map[string]any{
			"status":  status,
			"version": version.Version,
			"build":   version.BuildTime,
		}
		if failure != "" {
			body["failure"] = failure
		}
		if len(degraded) > 0 {
			body["degraded"] = degraded
		}
		_ = json.NewEncoder(w).Encode(body)
	})
	if debugState != nil {
		mux.HandleFunc("/debug/state", debugState)
	}
	s := &Server{
		http: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
			// a stalled scraper connection must not hold a goroutine
			// forever — /debug/state is a full live-snapshot encode with no
			// other write deadline, and idle keep-alives had no bound. Safe
			// HERE because no SSE lives on this listener; do NOT copy
			// WriteTimeout to the v2 serve server (see serve.go's IdleTimeout
			// comment — its SSE streams must never carry a whole-response
			// write deadline).
			WriteTimeout: 10 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
		log: log,
	}
	// Make the server health state available to the handler without package globals.
	base := s.http.Handler
	s.http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), healthContextKey{}, s)))
	})
	return s
}

type healthContextKey struct{}

func healthStateFromContext(ctx context.Context) *Server {
	s, _ := ctx.Value(healthContextKey{}).(*Server)
	return s
}

// status resolves the health tri-state: "failed" (a required component
// terminated — Fail) beats "degraded" (a data-plane probe reports a reason,
// regression fix) beats "ok". The probe functions run outside the lock: they only read
// their own atomics, and one must never be able to deadlock health reporting.
func (s *Server) status() (string, string, map[string]string) {
	s.mu.RLock()
	failure := s.failure
	probes := s.probes
	s.mu.RUnlock()
	if failure != "" {
		return "failed", failure, nil
	}
	var degraded map[string]string
	for _, p := range probes {
		if reason := p.fn(); reason != "" {
			if degraded == nil {
				degraded = make(map[string]string, len(probes))
			}
			degraded[p.name] = reason
		}
	}
	if len(degraded) > 0 {
		return "degraded", "", degraded
	}
	return "ok", "", nil
}

// Fail transitions health to non-OK before controlled process shutdown.
func (s *Server) Fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failure == "" && err != nil {
		s.failure = err.Error()
	}
}

// Listen binds the metrics listener synchronously : call this at startup, before
// the daemon logs "ready", so a malformed or already-bound [metrics].addr fails the
// process immediately instead of leaving it running with no /metrics or /healthz and
// rc.d reporting it healthy.
func (s *Server) Listen() (net.Listener, error) {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return nil, fmt.Errorf("metrics listen %s: %w", s.http.Addr, err)
	}
	return ln, nil
}

// Start blocks serving ln until it closes. Call Listen synchronously first; run Start in
// a goroutine.
func (s *Server) Start(ln net.Listener) error {
	s.log.Info("metrics server listening", "addr", s.http.Addr)
	err := s.http.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}
