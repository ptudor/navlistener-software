// Command navlistener is the project GNSS navigation-message collector: the
// central Go daemon that ingests raw broadcast nav frames from a fleet of
// receivers, decodes every constellation, propagates orbits and clocks,
// cross-checks broadcast-vs-observed for integrity, and (in later passes) stores
// and serves the result behind the Integrity Constellation Map.
//
// See the repository docs/DESIGN.md for the architecture and design reports.
// This build wires the first three pipeline stages: INGEST → DECODE → PROPAGATE.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/server"
	"github.com/ptudor/navlistener/internal/version"
)

func main() {
	os.Exit(run())
}

func run() int {
	configPath := flag.String("config", "", "path to the TOML config file")
	showVersion := flag.Bool("version", false, "print version and exit")
	checkConfig := flag.Bool("check-config", false, "validate config, print a summary, and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return 0
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		return 1
	}

	if *checkConfig {
		printConfigSummary(cfg)
		return 0
	}

	log := setupLogger(cfg.Logging)
	slog.SetDefault(log)
	log.Info("starting", "version", version.Version, "build", version.BuildTime)

	metrics.Init()

	// The pipeline (ingest → decode → state) will register a live-state snapshot
	// handler here; until that stage lands there is no snapshot to expose.
	var debugState http.HandlerFunc

	// Observability server (Prometheus /metrics + /healthz + /debug/state),
	// loopback only, separate from any future app-facing read path.
	obs := server.New(cfg.Metrics.Addr, log, debugState)
	go func() {
		if err := obs.Start(); err != nil {
			log.Error("metrics server", "error", err)
		}
	}()

	log.Info("ready", "ingest_sources", len(cfg.Ingest), "metrics_addr", cfg.Metrics.Addr)
	if len(cfg.Ingest) == 0 {
		log.Warn("no ingest sources configured")
	}

	// Wait for a shutdown signal.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info("shutdown signal", "signal", sig.String())

	shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutCancel()
	if err := obs.Shutdown(shutCtx); err != nil {
		log.Warn("metrics server shutdown", "error", err)
	}
	log.Info("graceful shutdown complete")
	return 0
}

func printConfigSummary(cfg *config.Config) {
	fmt.Println("Configuration valid.")
	fmt.Printf("  metrics addr:   %s\n", cfg.Metrics.Addr)
	fmt.Printf("  log:            %s / %s\n", cfg.Logging.Level, cfg.Logging.Format)
	fmt.Printf("  state shards:   %d\n", cfg.State.Shards)
	fmt.Printf("  sv ttl:         %s\n", cfg.State.SVTTL)
	fmt.Printf("  ingest sources: %d\n", len(cfg.Ingest))
	for _, s := range cfg.Ingest {
		status := "enabled"
		if s.Disabled {
			status = "disabled"
		}
		fmt.Printf("    - %-16s %-4s %-22s %s\n", s.Name, s.Type, s.Addr, status)
	}
}

func setupLogger(c config.Logging) *slog.Logger {
	var level slog.Level
	switch c.Level {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if c.Format == "text" {
		return slog.New(slog.NewTextHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, opts))
}
