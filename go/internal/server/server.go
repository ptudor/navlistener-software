// Package server runs the observability HTTP listener: Prometheus /metrics, a
// /healthz endpoint, and an optional /debug/state live-state snapshot. It is
// loopback-only and entirely separate from any app-facing read path (the native
// v2 serve contract, a later pass) and the ingest path.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/ptudor/navlistener/internal/version"
)

// Server is the metrics/health HTTP server.
type Server struct {
	http *http.Server
	log  *slog.Logger
}

// New builds the server bound to addr (e.g. 127.0.0.1:9100). If debugState is
// non-nil it is served at /debug/state — the loopback view of live per-SV state
// used to verify the pipeline before the native v2 feeds exist.
func New(addr string, log *slog.Logger, debugState http.HandlerFunc) *Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"status":  "ok",
			"version": version.Version,
			"build":   version.BuildTime,
		})
	})
	if debugState != nil {
		mux.HandleFunc("/debug/state", debugState)
	}
	return &Server{
		http: &http.Server{
			Addr:              addr,
			Handler:           mux,
			ReadHeaderTimeout: 5 * time.Second,
		},
		log: log,
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
