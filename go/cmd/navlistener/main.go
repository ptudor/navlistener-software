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
	"math"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/detect"
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
	state.SetLeapSeconds(cfg.State.LeapSeconds) // interim config override for ΔtLS
	live := state.New(cfg.State.Shards)
	if decl := declaredCapabilities(cfg); len(decl) > 0 {
		live.SetDeclaredCapabilities(decl)
		log.Info("declared capabilities loaded", "stations", len(decl))
	}
	frames := make(chan *ingest.RawFrame, frameQueue)
	mgr := ingest.New(cfg.Ingest, frames, log)

	// validate/construct the authenticated push endpoint (TLS cert/key/CA
	// load, addr) *before* any pipeline goroutine starts. Previously this lived
	// after the historian/decode/state goroutines were already running, so a bad
	// push.tls_cert failed via cancel()+storeCancel() waiting for nothing --
	// dying while the historian could be mid-CopyFrom instead of a clean
	// pre-pipeline exit. Only construction moves; Run() still starts below,
	// alongside the other pipeline goroutines.
	var pushSrv *ingest.PushServer
	if cfg.Push.Addr != "" {
		auth := ingest.NewConfigAuthenticator(cfg.Push.Observers)
		pushSrv, err = ingest.NewPushServer(cfg.Push, frames, auth, log)
		if err != nil {
			log.Error("push endpoint init failed", "error", err)
			return 1
		}
	}

	// Bind every configured listener before starting the historian or any producer.
	// A startup address conflict therefore accepts zero frames and needs no drain.
	debugState := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(live.Snapshot(time.Now()))
	}
	obs := server.New(cfg.Metrics.Addr, log, debugState)
	obsLn, err := obs.Listen()
	if err != nil {
		log.Error("metrics server", "error", err)
		return 1
	}
	defer obsLn.Close()
	var apiLn net.Listener
	if cfg.Serve.Addr != "" {
		apiLn, err = net.Listen("tcp", cfg.Serve.Addr)
		if err != nil {
			log.Error("v2 serve", "error", fmt.Errorf("v2 serve listen %s: %w", cfg.Serve.Addr, err))
			return 1
		}
		defer apiLn.Close()
	}
	var pushLn net.Listener
	if pushSrv != nil {
		pushLn, err = pushSrv.Listen()
		if err != nil {
			log.Error("push endpoint", "error", err)
			return 1
		}
		defer pushLn.Close()
	}

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

	// ingestWG tracks only the frame producers (dial connectors + the authenticated
	// push server) so shutdown can wait for "nothing will ever send to frames again"
	// before closing it. decodeLoop's own completion (tracked in wg below)
	// depends on that closure, so producers must be waited on separately, and first.
	var ingestWG sync.WaitGroup
	ingestWG.Add(1)
	go func() { defer ingestWG.Done(); mgr.Run(ctx) }()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); decodeLoop(frames, live, historian, log) }()

	wg.Add(1)
	go func() { defer wg.Done(); stateLoop(ctx, cfg.State, live) }()

	// Any required listener that terminates after readiness fails health and drives
	// the same ordered shutdown path as a signal.
	fatalCh := make(chan error, 1)
	reportFatal := func(component string, err error) {
		if err == nil || ctx.Err() != nil {
			return
		}
		wrapped := fmt.Errorf("%s: %w", component, err)
		obs.Fail(wrapped)
		select {
		case fatalCh <- wrapped:
		default:
		}
	}
	go func() {
		if err := obs.Start(obsLn); err != nil {
			log.Error("metrics server", "error", err)
			reportFatal("metrics server", err)
		}
	}()

	// The native v2 read API (docs/OUTPUT.md) is optional (enabled by [serve].addr).
	// It serves the live feeds from RAM on a loopback listener behind a TLS front,
	// separate from the ingest write path and the metrics listener.
	var apiSrv *serve.Server
	if cfg.Serve.Addr != "" {
		// The events query API reads the historian; a true nil interface (not a typed nil
		// *store.Store) keeps the endpoints reporting "unavailable" when persistence is off.
		var eventStore serve.EventStore
		if historian != nil {
			eventStore = historian
		}
		apiSrv = serve.New(cfg.Serve.Addr, live, eventStore, cfg.Ingest, cfg.Serve.RefreshFast, cfg.Serve.RefreshSlow, log)
		wg.Add(1)
		go func() { defer wg.Done(); apiSrv.Run(ctx) }()
		go func() {
			if err := apiSrv.Start(apiLn); err != nil {
				log.Error("v2 serve", "error", err)
				reportFatal("v2 serve", err)
			}
		}()
		// Persist each served feed to the historian on a slow cadence — the replay/backfill
		// record (docs/OUTPUT.md §4). Only meaningful when the historian is on and the
		// cadence is non-zero ([serve].snapshot_interval; "0s" disables).
		if historian != nil && cfg.Serve.SnapshotEvery > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				snapshotLoop(ctx, apiSrv, historian, cfg.Serve.SnapshotEvery, log)
			}()
			log.Info("feed snapshots enabled", "interval", cfg.Serve.SnapshotEvery.String())
		}
	}

	// The authenticated GNF1 push endpoint (docs/DESIGN.md §1/§2) is the production
	// fleet ingest path: navfeeder edge feeders connect out to us over TLS and their
	// frames join the same decode stage as the dial connectors. Constructed/validated
	// above; only the run loop starts here.
	if pushSrv != nil {
		ingestWG.Add(1)
		go func() {
			defer ingestWG.Done()
			if err := pushSrv.Serve(ctx, pushLn); err != nil {
				log.Error("push endpoint", "error", err)
				reportFatal("push endpoint", err)
			}
		}()
		log.Info("push endpoint enabled", "addr", cfg.Push.Addr, "observers", len(cfg.Push.Observers))
	}

	// Integrity DETECT: the debounced detector runs on a cadence over the same live
	// read model the feeds serve, persists confirmed events (firing pg_notify) and
	// pushes them to the SSE broker (docs/INTEGRITY.md, docs/OUTPUT.md §3).
	detector := detect.New(0)
	// detectLoop/emitEvent take the eventWriter/eventPublisher interfaces, but
	// historian/apiSrv are concrete pointers that are nil when their config section is
	// off. A nil concrete pointer boxed into an interface is a non-nil interface, so the
	// `== nil` guards in emitEvent would never fire and the first confirmed event would
	// dereference a nil receiver. Convert to true interface nils here, exactly once, the
	// same pattern used for serve.EventStore above.
	ew := asEventWriter(historian)
	ep := asEventPublisher(apiSrv)
	wg.Add(1)
	go func() { defer wg.Done(); detectLoop(ctx, live, detector, ew, ep, log) }()

	log.Info("ready", "ingest_sources", len(cfg.Ingest), "metrics_addr", cfg.Metrics.Addr, "serve_addr", cfg.Serve.Addr, "shards", cfg.State.Shards)
	if len(cfg.Ingest) == 0 {
		log.Warn("no ingest sources configured")
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	// Go's default SIGHUP action terminates the process. Ignore it so a mis-aimed
	// newsyslog HUP (or a HUP at the child pidfile instead of the daemon(8) supervisor) can
	// never kill the collector mid-drain. Log rotation reopens the logfile via daemon(8)'s
	// -H on the supervisor; the collector needs no reload signal of its own.
	signal.Ignore(syscall.SIGHUP)
	exitCode := 0
	select {
	case sig := <-sigCh:
		log.Info("shutdown signal", "signal", sig.String())
	case fatal := <-fatalCh:
		exitCode = 1
		log.Error("required component failed; shutting down", "error", fatal)
	}

	// Ordered shutdown : cancel() stops every producer's accept/dial loop, but
	// decodeLoop must not race them to give up on a momentarily-empty frames channel —
	// a producer still mid-teardown (an in-flight connection handler, an in-flight
	// dial reconnect) can still send after that false-empty read, and those frames
	// would reach neither state nor the historian with no drop metric or log. Instead:
	// wait for ingestWG (every producer's own Run already waits out its in-flight
	// handlers before returning, so ingestWG.Wait() is a reliable "nothing can send to
	// frames again" signal), THEN close frames, THEN let decodeLoop range it to
	// closure — draining every already-enqueued frame with no race. Only close frames
	// once producers are confirmed done: closing while one might still send would
	// panic.
	cancel()
	shutCtx, shutCancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
	defer shutCancel()

	ingestDone := make(chan struct{})
	go func() { ingestWG.Wait(); close(ingestDone) }()
	select {
	case <-ingestDone:
		close(frames)
	case <-shutCtx.Done():
		log.Warn("shutdown timeout waiting for ingest producers; exiting without closing the frame queue")
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
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
	return exitCode
}

// persistMsgType returns the msg_type to persist for f : a dial connector
// (ubx.go's scanUBX) never populates MsgType (leaves it 0), while the push path's
// feeder sets it via its own frame_type() mirror of NavType() -- so the identical
// GPS LNAV subframe historian-persists as msg_type=0 via one ingest mode and
// msg_type=0x10 via the other, splitting one frame type into two populations for
// queries/replay tooling. Backfilling from NavType() here (word-oriented frames
// only -- f.Words != nil -- so SBF/RTCM's own Bytes-frame message numbers are
// never overwritten) makes both paths agree without touching the wire.
func persistMsgType(f *ingest.RawFrame) int {
	if f.MsgType == 0 && f.Words != nil {
		return f.NavType()
	}
	return f.MsgType
}

// decodeLoop folds every ingested frame into live state and, when the historian
// is enabled, enqueues the raw frame for the forensic record — persistence is
// independent of decode success, so a decoder bug never loses evidence. Each
// frame is applied with a per-frame recover so a decoder edge case drops one
// frame rather than crashing the process. It has no shutdown logic of its own
// : it simply ranges frames until the channel is closed, which the owner
// (run, above) does only after every producer has confirmed it will never send
// again — so every already-enqueued frame is applied with no drain race.
func decodeLoop(frames <-chan *ingest.RawFrame, live *state.Store, historian *store.Store, log *slog.Logger) {
	apply := func(f *ingest.RawFrame) {
		defer func() {
			if r := recover(); r != nil {
				log.Error("decode panic recovered; frame dropped", "recover", fmt.Sprint(r))
			}
		}()
		if historian != nil && f.Obs == nil && f.RF == nil { // telemetry (observables, RF) is not a nav-frame record
			historian.Enqueue(&store.NavFrame{
				Ts:           time.Now(),
				ReceivedAt:   f.Recv,
				SourceID:     f.Source,
				GnssID:       int(f.GnssID),
				SvID:         f.SvID,
				SigID:        f.SigID,
				MsgType:      persistMsgType(f),
				Raw:          f.RawBytes(),
				DecoderVer:   version.Version,
				SourceSeq:    f.Seq,
				HasSourceSeq: f.HasSeq,
			})
		}
		live.Apply(f)
	}
	for f := range frames {
		apply(f)
	}
}

// declaredCapabilities collects each station's declared tudorgps fingerprint (the signals its
// silicon can produce) from the dial sources and push observers, keyed by station id, for the
// capability-plausibility detector (docs/INTEGRITY.md §6). A station may be configured in only
// one place; a push observer's declaration overrides a dial source of the same name.
func declaredCapabilities(cfg *config.Config) map[string][]state.CapSignal {
	out := map[string][]state.CapSignal{}
	add := func(id string, decl []config.Capability) {
		if len(decl) == 0 {
			return
		}
		sigs := make([]state.CapSignal, len(decl))
		for i, c := range decl {
			sigs[i] = state.CapSignal{Gnss: c.Gnss, Sig: c.Sig}
		}
		out[id] = sigs
	}
	for _, s := range cfg.Ingest {
		add(s.Name, s.CapDecl)
	}
	for _, o := range cfg.Push.Observers {
		add(o.Station, o.CapDecl)
	}
	return out
}

// snapshotLoop persists each served v2 feed's current body to the historian on the
// configured cadence — the light replay/backfill record (docs/OUTPUT.md §4), complementary
// to the raw-nav-frame hypertable. A write failure is logged and the loop continues: a
// missed snapshot never disrupts serving or ingest. It stops when ctx is cancelled, before
// the historian is closed in the ordered shutdown.
func snapshotLoop(ctx context.Context, api *serve.Server, historian *store.Store, every time.Duration, log *slog.Logger) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			now := time.Now()
			for feed, body := range api.SnapshotFeeds() {
				// Per-call deadline : ctx is cancel-only, so a hung DB connection
				// must not block this loop indefinitely on OS TCP timeouts.
				callCtx, cancel := context.WithTimeout(ctx, snapshotWriteTimeout)
				err := historian.WriteSnapshot(callCtx, now, feed, body)
				cancel()
				if err != nil {
					if ctx.Err() != nil {
						return // shutting down: the historian context is going away
					}
					log.Warn("feed snapshot write failed", "feed", feed, "error", err)
				}
			}
		case <-ctx.Done():
			return
		}
	}
}

// snapshotWriteTimeout bounds each feed-snapshot write.
const snapshotWriteTimeout = 10 * time.Second

// detectInterval is the cadence at which the integrity detector samples live state.
// It is well under the 60 s debounce window, so a confirmed transition is caught
// promptly without the detector itself defining the confirmation delay.
const detectInterval = 15 * time.Second

// detectLoop samples the live read model on a cadence, folds it through the
// debounced detector, and routes each confirmed event to the historian (which
// assigns the id and fires pg_notify) and the SSE broker. Only durable events are
// published, so the database id is the sole replay cursor domain.
func detectLoop(ctx context.Context, live *state.Store, det *detect.Detector, historian eventWriter, api eventPublisher, log *slog.Logger) {
	tick := time.NewTicker(detectInterval)
	defer tick.Stop()
	// pending holds confirmed events whose durable write has not yet succeeded.
	// It survives across ticks so a transient DB error cannot silently drop the namesake
	// output: each tick re-attempts the queue before processing new detections.
	var pending []pendingEvent
	for {
		select {
		case <-tick.C:
			detectTick(ctx, live, det, historian, api, log, &pending)
		case <-ctx.Done():
			return
		}
	}
}

// detectTick runs one detector sample and emits its confirmed events. It is wrapped in
// a recover() : "the collector must never go down" — a panic in the detector or
// an event write must reconnect/skip this tick, not kill the detect goroutine (which has
// no supervisor within the process) and with it all integrity monitoring. pending is a
// pointer so a mid-tick panic still preserves whatever queue mutations completed.
func detectTick(ctx context.Context, live *state.Store, det *detect.Detector, historian eventWriter, api eventPublisher, log *slog.Logger, pending *[]pendingEvent) {
	defer func() {
		if r := recover(); r != nil {
			metrics.EventWriteErrorsTotal.Inc()
			log.Error("detect tick panicked; skipping", "panic", r)
		}
	}()
	// Re-attempt any events queued by a prior tick's failed write before taking new samples
	// : a DB blip must not drop a confirmed transition permanently.
	*pending = reattemptPending(ctx, *pending, historian, api, log)
	now := time.Now()
	events := det.Tick(now, live.FeedSVs(now), live.FeedSBAS(now))
	// Station-scoped PNT-defense events (jamming/spoofing/RF, docs/DEFENSE-PNT.md)
	// share the debounce state machine and event pipeline.
	events = append(events, det.TickStations(now, live.FeedStationRF(now))...)
	// Capability plausibility: a demonstrated signal gone silent, or a signal the
	// node's silicon can't produce (docs/INTEGRITY.md §6, CONSTELLATIONS §7).
	events = append(events, det.TickCapabilities(now, live.FeedCapabilityReports(now))...)
	for _, e := range events {
		if pe := emitEvent(ctx, e, historian, api, log); pe != nil {
			*pending = enqueuePending(*pending, *pe, log)
		}
	}
}

// emitEvent persists one integrity event (if the historian is enabled) and pushes it
// to the SSE broker, tagging metrics. The historian-assigned id is the only SSE
// cursor: a non-durable event is logged/metriced but deliberately unavailable to
// SSE rather than receiving an id PostgreSQL may later allocate.
type eventWriter interface {
	WriteEvent(context.Context, store.EventRow) (int64, error)
}
type eventPublisher interface{ PublishEvent(serve.EventMsg) }

// asEventWriter / asEventPublisher convert the concrete historian/serve pointers to
// their interface types at the run() boundary, returning a true interface nil when the
// pointer is nil. Without this explicit conversion, a nil *store.Store boxed
// into an eventWriter would be a non-nil interface and defeat emitEvent's nil guard,
// crashing the daemon on the first confirmed event in any persist-less/serve-less config.
func asEventWriter(s *store.Store) eventWriter {
	if s == nil {
		return nil
	}
	return s
}

func asEventPublisher(s *serve.Server) eventPublisher {
	if s == nil {
		return nil
	}
	return s
}

// eventRetry bounds the per-call WriteEvent retry (mirrors store.flushRetry): confirmed
// integrity events are the low-rate, individually-meaningful namesake artifact, so a
// transient DB error gets a few bounded attempts under a per-call timeout rather than the
// single un-timeout'd shot that could silently drop a confirmed transition forever
//. A var so tests can shrink the timing.
type eventRetry struct {
	attempts  int
	backoff   time.Duration
	perCallTO time.Duration
}

var defaultEventRetry = eventRetry{attempts: 3, backoff: 250 * time.Millisecond, perCallTO: 10 * time.Second}

// eventPendingMax bounds the in-RAM re-attempt queue so a persistent DB outage cannot grow
// it without limit : when full, the oldest queued event is dropped (counted, logged
// loudly) rather than OOMing the process.
const eventPendingMax = 256

// pendingEvent is a confirmed event whose durable write has not yet succeeded, held for
// re-attempt on a later detector tick. row is the fully-built insert; ev carries the
// already-sanitized fields the SSE publish needs once the id is assigned.
type pendingEvent struct {
	row store.EventRow
	ev  detect.Event
}

// emitEvent counts, sanitizes and marshals one confirmed event (exactly once), then makes
// the first durable-write+publish attempt. It returns a *pendingEvent when the write failed
// after its bounded retry and the caller should re-attempt on a later tick; nil when the
// event was published, permanently non-durable (no historian), or otherwise complete.
func emitEvent(ctx context.Context, e detect.Event, historian eventWriter, api eventPublisher, log *slog.Logger) *pendingEvent {
	metrics.EventsTotal.WithLabelValues(e.Type, fmt.Sprint(e.Severity)).Inc()

	// Params is a map of ephemeris-derived float64s, and encoding/json fails
	// the whole document on NaN/±Inf -- sanitize once here, the single production
	// EventMsg/EventRow construction point, so neither the historian write nor the
	// SSE publish below can silently drop the event over one bad value.
	e.Params = sanitizeEventParams(e.Params)

	var rawJSON []byte
	if len(e.Params) > 0 {
		b, err := json.Marshal(e.Params)
		if err != nil {
			log.Error("event params marshal failed", "type", e.Type, "sv", e.SV, "error", err)
		} else {
			rawJSON = b
		}
	}

	if historian == nil {
		log.Warn("integrity event is not publishable without durable historian", "sv", e.SV, "type", e.Type)
		return nil
	}
	row := store.EventRow{
		Time: e.Time, SV: e.SV, Type: e.Type, OldValue: e.OldValue,
		NewValue: e.NewValue, Severity: e.Severity, Message: e.Message, Raw: rawJSON,
	}
	if writeAndPublish(ctx, row, e, historian, api, log) {
		return nil
	}
	return &pendingEvent{row: row, ev: e}
}

// writeAndPublish makes the durable write (bounded retry under a per-call timeout) and, on
// success, publishes to SSE with the assigned id — the durable-id contract (commit 93a21a9):
// SSE only ever carries a real DB id. Returns true on success, false when every attempt
// failed and the event must be re-attempted later. Shared by the first attempt (emitEvent)
// and the re-attempt path (reattemptPending); it does NOT touch EventsTotal, so a re-attempt
// never double-counts the event.
func writeAndPublish(ctx context.Context, row store.EventRow, e detect.Event, historian eventWriter, api eventPublisher, log *slog.Logger) bool {
	id, err := writeEventRetry(ctx, historian, row, defaultEventRetry, log)
	if err != nil {
		metrics.EventWriteErrorsTotal.Inc()
		log.Error("persist integrity event failed after retries; queued for re-attempt", "type", e.Type, "sv", e.SV, "error", err)
		return false
	}
	log.Info("integrity event", "id", id, "sv", e.SV, "type", e.Type,
		"severity", e.Severity, "old", e.OldValue, "new", e.NewValue)

	if api != nil {
		api.PublishEvent(serve.EventMsg{
			ID: id, Time: e.Time.UTC().Format(time.RFC3339), SV: e.SV, Type: e.Type,
			OldValue: e.OldValue, NewValue: e.NewValue, Severity: e.Severity,
			Message: e.Message, Params: e.Params,
		})
	}
	return true
}

// writeEventRetry does a bounded WriteEvent retry, each attempt under its own timeout.
// main's ctx is cancel-only (no statement timeout), so a silently-hung DB connection would
// otherwise block the whole detect loop for minutes on OS TCP timeouts; the per-call
// deadline bounds it. Returns immediately if ctx is cancelled (shutdown).
func writeEventRetry(ctx context.Context, historian eventWriter, row store.EventRow, r eventRetry, log *slog.Logger) (int64, error) {
	var lastErr error
	for attempt := 0; attempt < r.attempts; attempt++ {
		if attempt > 0 {
			select {
			case <-time.After(r.backoff):
			case <-ctx.Done():
				return 0, ctx.Err()
			}
		}
		callCtx, cancel := context.WithTimeout(ctx, r.perCallTO)
		id, err := historian.WriteEvent(callCtx, row)
		cancel()
		if err == nil {
			return id, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		log.Warn("write integrity event attempt failed", "attempt", attempt+1, "type", row.Type, "sv", row.SV, "error", err)
	}
	return 0, lastErr
}

// reattemptPending re-tries every queued event (oldest first), returning the queue of those
// that still failed. It filters in place; a re-published event is dropped from the queue.
func reattemptPending(ctx context.Context, pending []pendingEvent, historian eventWriter, api eventPublisher, log *slog.Logger) []pendingEvent {
	if len(pending) == 0 {
		return pending
	}
	kept := pending[:0]
	for _, pe := range pending {
		if !writeAndPublish(ctx, pe.row, pe.ev, historian, api, log) {
			kept = append(kept, pe)
		}
	}
	return kept
}

// enqueuePending appends a failed event to the re-attempt queue, dropping the oldest
// (counted, logged) if the queue is at its cap so a persistent outage can't grow it forever.
func enqueuePending(pending []pendingEvent, pe pendingEvent, log *slog.Logger) []pendingEvent {
	if len(pending) >= eventPendingMax {
		dropped := pending[0]
		metrics.EventWriteErrorsTotal.Inc()
		log.Error("integrity event re-attempt queue full; dropping oldest confirmed event",
			"dropped_sv", dropped.ev.SV, "dropped_type", dropped.ev.Type, "queue_max", eventPendingMax)
		pending = pending[1:]
	}
	return append(pending, pe)
}

// sanitizeEventParams replaces any non-finite float64 (NaN/±Inf) in params with its
// string representation ("NaN", "+Inf", "-Inf") so json.Marshal can never fail on it
//. encoding/json fails the whole document on a non-finite float; without
// this, one such value in an event's params silently drops the event from both the
// SSE stream (whose id is still consumed, so Last-Event-ID replay can't recover it)
// and the persisted historian row (no params at all, no log, no metric). Returns
// params unchanged (same map) when nothing needs sanitizing, to avoid an allocation
// on the common path.
func sanitizeEventParams(params map[string]any) map[string]any {
	var dirty bool
	for _, v := range params {
		if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			dirty = true
			break
		}
	}
	if !dirty {
		return params
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		if f, ok := v.(float64); ok && (math.IsNaN(f) || math.IsInf(f, 0)) {
			out[k] = fmt.Sprint(f) // "NaN", "+Inf", "-Inf"
			continue
		}
		out[k] = v
	}
	return out
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
	pushAddr := cfg.Push.Addr
	if pushAddr == "" {
		pushAddr = "(disabled)"
	}
	fmt.Printf("  push addr:      %s (%d observers)\n", pushAddr, len(cfg.Push.Observers))
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
