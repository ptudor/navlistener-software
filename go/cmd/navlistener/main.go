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
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/serve"
	"github.com/ptudor/navlistener/internal/server"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/store"
	"github.com/ptudor/navlistener/internal/version"
)

// frameQueue bounds the ingest→decode channel; a full queue backpressures ingest.
const frameQueue = 8192

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

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Pipeline: ingest → decode → live state (+ optional persist historian).
	live := state.New(cfg.State.Shards)
	frames := make(chan *ingest.RawFrame, frameQueue)
	mgr := ingest.New(cfg.Ingest, frames, log)

	// The TimescaleDB historian is optional (enabled by [store].dsn). It runs under
	// its own context, cancelled only after the decode loop has drained — so no frame
	// is lost at the decode→persist hop at shutdown.
	var historian *store.Store
	storeCtx, storeCancel := context.WithCancel(context.Background())
	storeDone := make(chan struct{})
	if cfg.Store.DSN != "" {
		historian, err = store.New(ctx, cfg.Store, log)
		if err != nil {
			log.Error("historian init failed — TimescaleDB is required when store.dsn is set", "error", err)
			storeCancel()
			return 1
		}
		go func() { defer close(storeDone); historian.Run(storeCtx) }()
		log.Info("historian enabled")
	} else {
		close(storeDone)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); mgr.Run(ctx) }()

	wg.Add(1)
	go func() { defer wg.Done(); decodeLoop(ctx, frames, live, historian, log) }()

	wg.Add(1)
	go func() { defer wg.Done(); stateLoop(ctx, cfg.State, live) }()

	// Observability server exposes /metrics + /healthz + the live-state snapshot.
	debugState := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(live.Snapshot(time.Now()))
	}
	obs := server.New(cfg.Metrics.Addr, log, debugState)
	go func() {
		if err := obs.Start(); err != nil {
			log.Error("metrics server", "error", err)
		}
	}()

	// The native v2 read API (docs/OUTPUT.md) is optional (enabled by [serve].addr).
	// It serves the live feeds from RAM on a loopback listener behind a TLS front,
	// separate from the ingest write path and the metrics listener.
	var apiSrv *serve.Server
	if cfg.Serve.Addr != "" {
		apiSrv = serve.New(cfg.Serve.Addr, live, cfg.Ingest, cfg.Serve.RefreshFast, cfg.Serve.RefreshSlow, log)
		wg.Add(1)
		go func() { defer wg.Done(); apiSrv.Run(ctx) }()
		go func() {
			if err := apiSrv.Start(); err != nil {
				log.Error("v2 serve", "error", err)
			}
		}()
	}

	log.Info("ready", "ingest_sources", len(cfg.Ingest), "metrics_addr", cfg.Metrics.Addr, "serve_addr", cfg.Serve.Addr, "shards", cfg.State.Shards)
	if len(cfg.Ingest) == 0 {
		log.Warn("no ingest sources configured")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Info("shutdown signal", "signal", sig.String())

	// Ordered shutdown: stop ingest + decode + tick (the decode loop drains its
	// buffered frames into the historian), THEN stop the historian so it flushes the
	// last batch, THEN the obs server — all bounded by ShutdownTimeout.
	cancel()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutCancel()
	select {
	case <-done:
		log.Info("pipeline drained")
	case <-shutCtx.Done():
		log.Warn("shutdown timeout exceeded; exiting")
	}
	storeCancel() // historian drains its queue, flushes, closes the pool
	select {
	case <-storeDone:
	case <-shutCtx.Done():
	}
	if apiSrv != nil {
		if err := apiSrv.Shutdown(shutCtx); err != nil {
			log.Warn("v2 serve shutdown", "error", err)
		}
	}
	if err := obs.Shutdown(shutCtx); err != nil {
		log.Warn("metrics server shutdown", "error", err)
	}
	log.Info("graceful shutdown complete")
	return 0
}

// decodeLoop folds every ingested frame into live state and, when the historian
// is enabled, enqueues the raw frame for the forensic record — persistence is
// independent of decode success, so a decoder bug never loses evidence. Each
// frame is applied with a per-frame recover so a decoder edge case drops one
// frame rather than crashing the process. On shutdown it drains the buffered
// frames before returning.
func decodeLoop(ctx context.Context, frames <-chan *ingest.RawFrame, live *state.Store, historian *store.Store, log *slog.Logger) {
	apply := func(f *ingest.RawFrame) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("decode panic recovered; frame dropped", "recover", fmt.Sprint(r))
			}
		}()
		if historian != nil && f.Obs == nil { // observables are telemetry, not the nav-frame record
			historian.Enqueue(&store.NavFrame{
				Ts:         time.Now(),
				ReceivedAt: f.Recv,
				SourceID:   f.Source,
				GnssID:     int(f.GnssID),
				SvID:       f.SvID,
				SigID:      f.SigID,
				MsgType:    f.MsgType,
				Raw:        f.RawBytes(),
				DecoderVer: version.Version,
			})
		}
		live.Apply(f)
	}
	for {
		select {
		case f := <-frames:
			apply(f)
		case <-ctx.Done():
			for {
				select {
				case f := <-frames:
					apply(f)
				default:
					return
				}
			}
		}
	}
}

// stateLoop re-propagates live SVs on the configured cadence and expires stale ones.
func stateLoop(ctx context.Context, cfg config.State, store *state.Store) {
	prop := time.NewTicker(cfg.PropagateEvery)
	defer prop.Stop()
	expire := time.NewTicker(30 * time.Second)
	defer expire.Stop()
	for {
		select {
		case <-prop.C:
			store.Propagate(time.Now())
		case <-expire.C:
			store.Expire(time.Now(), cfg.SVTTL)
		case <-ctx.Done():
			return
		}
	}
}

func printConfigSummary(cfg *config.Config) {
	fmt.Println("Configuration valid.")
	fmt.Printf("  metrics addr:   %s\n", cfg.Metrics.Addr)
	serveAddr := cfg.Serve.Addr
	if serveAddr == "" {
		serveAddr = "(disabled)"
	}
	fmt.Printf("  serve addr:     %s\n", serveAddr)
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
