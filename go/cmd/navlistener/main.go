// Command navlistener is the project GNSS navigation-message collector: the
// central Go daemon that ingests raw broadcast nav frames from a fleet of
// receivers, decodes supported signals across the GNSS constellations,
// propagates orbits and clocks,
// cross-checks broadcast-vs-observed for integrity, stores the forensic record,
// and serves live feeds and integrity events behind the Integrity Constellation
// Map.
//
// See the repository docs/DESIGN.md for the architecture and design reports.
// The optional store, read API, and authenticated push listener are enabled by
// their respective non-empty configuration sections.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/authorization"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/identity"
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

// ingestStaleAfter is how long the collector may go without ingesting a single
// frame (across every dial source and push observer) before /healthz reports
// degraded. Mirrors the 5 min operating point of state's
// liveReceiverWindow / detect.ObserverOfflineThreshold ("an observer unseen
// this long is offline") — a healthy collector with any configured source sees
// frames every few seconds, so 5 min of total silence means the data plane is
// down even though every listener goroutine is alive.
const ingestStaleAfter = 5 * time.Minute

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
	// regression fix/non-fatal config findings (world-readable secrets file,
	// non-loopback bind of an unauthenticated surface) — loud at startup, once.
	for _, w := range cfg.Warnings {
		log.Warn("config warning", "warning", w)
	}
	metrics.Init()
	if len(cfg.Federation.ExportGrants) > 0 {
		log.Info("federation export grants loaded; peer transport remains disabled",
			"grants", len(cfg.Federation.ExportGrants))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var authProvider *authorization.Provider
	if cfg.Authorization.DSN != "" {
		connectCtx, connectCancel := context.WithTimeout(ctx, 10*time.Second)
		authProvider, err = authorization.NewDatabase(connectCtx, cfg.Authorization.DSN, cfg.Authorization.CacheTTL, log)
		if err == nil {
			err = authProvider.VerifyContracts(connectCtx, cfg.Push.Addr != "", cfg.Serve.Addr != "")
		}
		connectCancel()
		if err != nil {
			if authProvider != nil {
				authProvider.Close()
			}
			log.Error("control-plane authorization init failed", "error", err)
			return 1
		}
		// Cancel the LISTEN waiter before closing its pool; pgxpool.Close waits
		// for acquired connections to return.
		defer func() {
			cancel()
			authProvider.Close()
		}()
		go authProvider.RunInvalidation(ctx)
		log.Info("database authorization enabled", "cache_ttl", cfg.Authorization.CacheTTL,
			"session_recheck_interval", cfg.Authorization.RecheckEvery)
	}
	var readAuthorizer serve.ReadAuthorizer
	if authProvider != nil {
		readAuthorizer = authProvider
	} else if len(cfg.Serve.Principals) > 0 {
		grants := make([]authorization.StaticReadGrant, 0, len(cfg.Serve.Principals))
		for _, configured := range cfg.Serve.Principals {
			grants = append(grants, authorization.StaticReadGrant{
				TokenSHA256: configured.TokenSHA256,
				Principal:   configured.Principal,
			})
		}
		readAuthorizer, err = authorization.NewStaticReadAuthorizer(grants)
		if err != nil {
			log.Error("static read authorization init failed", "error", err)
			return 1
		}
	}

	// Pipeline: ingest → decode → live state (+ optional persist historian).
	state.SetLeapSeconds(cfg.State.LeapSeconds) // interim config override for ΔtLS
	live := state.New(cfg.State.Shards)
	publicLive := state.New(cfg.State.Shards)
	publicEventsLive := state.New(cfg.State.Shards)
	audienceRegistry := audience.NewRegistry(cfg.State.Shards, cfg.Ingest)
	audienceRegistry.Register(identity.Audience{Kind: identity.AudiencePublic}, publicLive, audience.PublicSources(cfg.Ingest))
	audienceRegistry.Register(identity.Audience{Kind: identity.AudienceOperator, ID: cfg.Collector.InstanceID}, live, cfg.Ingest)
	policyEpochs := audience.NewPolicyEpochs(time.Now())
	detector := detect.New(0)
	publicDetector := detect.New(0)
	scopeController := &scopeController{
		registry: audienceRegistry, publicEvents: publicEventsLive,
		operatorDetector: detector, publicDetector: publicDetector,
		epochs: policyEpochs, log: log,
	}
	if decl := declaredCapabilities(cfg); len(decl) > 0 {
		live.SetDeclaredCapabilities(decl)
		log.Info("declared capabilities loaded", "stations", len(decl))
	}
	if decl := declaredPublicCapabilities(cfg, false); len(decl) > 0 {
		publicLive.SetDeclaredCapabilities(decl)
	}
	if decl := declaredPublicCapabilities(cfg, true); len(decl) > 0 {
		publicEventsLive.SetDeclaredCapabilities(decl)
	}
	frames := make(chan *ingest.RawFrame, frameQueue)
	mgr := ingest.New(cfg.Ingest, frames, log)
	// lastFrameNano is the decode funnel's last-ingested stamp (every dial and
	// push frame passes decodeLoop), feeding the /healthz ingest probe.
	var lastFrameNano atomic.Int64

	// validate/construct the authenticated push endpoint (TLS cert/key/CA
	// load, addr) *before* any pipeline goroutine starts. Previously this lived
	// after the historian/decode/state goroutines were already running, so a bad
	// push.tls_cert failed via cancel()+storeCancel() waiting for nothing --
	// dying while the historian could be mid-CopyFrom instead of a clean
	// pre-pipeline exit. Only construction moves; Run() still starts below,
	// alongside the other pipeline goroutines.
	var pushSrv *ingest.PushServer
	if cfg.Push.Addr != "" {
		var auth ingest.Authenticator
		if authProvider != nil {
			auth = authProvider
		} else {
			auth = ingest.NewConfigAuthenticator(cfg.Push.Observers)
		}
		pushSrv, err = ingest.NewPushServer(cfg.Push, frames, auth, log)
		if err != nil {
			log.Error("push endpoint init failed", "error", err)
			return 1
		}
		pushSrv.SetReauthorizationInterval(cfg.Authorization.RecheckEvery)
		pushSrv.SetCollectorInstance(cfg.Collector.InstanceID)
	}

	// Bind every configured listener before starting the historian or any producer.
	// A startup address conflict therefore accepts zero frames and needs no drain.
	obs := server.New(cfg.Metrics.Addr, log, newDebugStateHandler(live, log))
	obsLn, err := obs.Listen()
	if err != nil {
		log.Error("metrics server", "error", err)
		return 1
	}
	defer obsLn.Close()
	// /healthz must reflect the data plane, not just listener liveness
	// — before these probes it stayed "ok" while every source was down or the
	// historian dropped every frame. The ingest probe is armed only when at
	// least one frame producer is configured: a deliberately source-less config
	// has no data plane to be stale.
	ingestConfigured := cfg.Push.Addr != ""
	for _, src := range cfg.Ingest {
		if !src.Disabled {
			ingestConfigured = true
			break
		}
	}
	if ingestConfigured {
		startAt := time.Now()
		obs.AddProbe("ingest", func() string {
			last := lastFrameNano.Load()
			if last == 0 {
				// Nothing ingested yet: allow the same window from process start
				// (sources dial with backoff; feeders reconnect on their own time).
				if age := time.Since(startAt); age > ingestStaleAfter {
					return fmt.Sprintf("no frames ingested since start %s ago", age.Round(time.Second))
				}
				return ""
			}
			if age := time.Since(time.Unix(0, last)); age > ingestStaleAfter {
				return fmt.Sprintf("no frames ingested for %s", age.Round(time.Second))
			}
			return ""
		})
	}
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
		// surface persistent flush failure (the historian silently
		// dropping the forensic record) as a degraded /healthz.
		obs.AddProbe("historian", historian.Degraded)
		// with a historian present, GNF1 ACK is the durability
		// watermark — the feeder prunes its spool only for frames the store has
		// durably resolved (committed / dedup-proven / quarantined-unfixable).
		// Without a historian the tracker stays nil and push acks on receipt:
		// the explicit live-only mode, documented in wire.go's Ack contract.
		if pushSrv != nil {
			tracker := ingest.NewDurableTracker()
			pushSrv.SetDurableTracker(tracker)
			historian.SetDurableNotify(tracker.Resolved)
		}
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
	go func() {
		defer wg.Done()
		decodeLoop(frames, live, publicLive, publicEventsLive, historian, log, &lastFrameNano, audienceRegistry, scopeController.Apply)
	}()

	wg.Add(1)
	// Run the public projection first and the operator state last so legacy
	// process-wide gauges retain their all-source/operator meaning.
	go func() { defer wg.Done(); stateLoop(ctx, cfg.State, publicEventsLive, publicLive, live) }()

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
		serveState := publicLive
		serveSources := audience.PublicSources(cfg.Ingest)
		if cfg.Serve.Audience == "operator" {
			serveState = live
			serveSources = cfg.Ingest
		}
		apiSrv = serve.NewForAudience(cfg.Serve.Addr, serveState, eventStore, serveSources,
			cfg.Serve.RefreshFast, cfg.Serve.RefreshSlow, log, cfg.Serve.AudienceContext)
		apiSrv.SetPolicyEpochs(policyEpochs)
		scopeController.AttachServer(apiSrv)
		if readAuthorizer != nil {
			apiSrv.EnableAudienceSelection(readAuthorizer, audienceRegistry, cfg.Authorization.RecheckEvery)
			log.Info("authenticated read audience selection enabled", "static_principals", len(cfg.Serve.Principals))
		}
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
		authSource := "config"
		if authProvider != nil {
			authSource = "database"
		}
		log.Info("push endpoint enabled", "addr", cfg.Push.Addr, "observers", len(cfg.Push.Observers), "authorization", authSource)
	}

	// Integrity DETECT: the debounced detector runs on a cadence over the same live
	// read model the feeds serve, persists confirmed events (firing pg_notify) and
	// pushes them to the SSE broker (docs/INTEGRITY.md, docs/OUTPUT.md §3).
	// publish the spoofing detector's coverage so its dormancy is a
	// fact on the operational surface, not an implication — with wired < quorum
	// (the v1 posture) spoofing_suspected cannot fire, and an operator must be
	// able to tell that from "no spoofing observed".
	metrics.SpoofGatesWired.Set(float64(detect.WiredSpoofGates))
	metrics.SpoofGateQuorumGauge.Set(float64(detect.SpoofGateQuorum))
	if detect.WiredSpoofGates < detect.SpoofGateQuorum {
		log.Warn("spoofing_suspected detector is dormant: fewer independent gates wired than the fusion quorum requires",
			"wired", detect.WiredSpoofGates, "quorum", detect.SpoofGateQuorum)
	}
	// detectLoop/emitEvent take the eventWriter/eventPublisher interfaces, but
	// historian/apiSrv are concrete pointers that are nil when their config section is
	// off. A nil concrete pointer boxed into an interface is a non-nil interface, so the
	// `== nil` guards in emitEvent would never fire and the first confirmed event would
	// dereference a nil receiver. Convert to true interface nils here, exactly once, the
	// same pattern used for serve.EventStore above.
	ew := asEventWriter(historian)
	ep := asEventPublisher(apiSrv)
	var operatorPublisher, publicPublisher eventPublisher
	if apiSrv != nil && apiSrv.Audience().Kind == identity.AudiencePublic {
		publicPublisher = ep
	} else {
		operatorPublisher = ep
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		detectLoop(ctx, live, detector, ew, operatorPublisher, policyEpochs, log,
			(identity.Audience{Kind: identity.AudienceOperator, ID: cfg.Collector.InstanceID}).Key())
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		detectLoop(ctx, publicEventsLive, publicDetector, ew, publicPublisher, policyEpochs, log, "public")
	}()
	if readAuthorizer != nil && historian != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			scopedDetectLoop(ctx, audienceRegistry, ew, apiSrv, policyEpochs, log)
		}()
	}

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
	if apiSrv != nil {
		if err := apiSrv.Shutdown(shutCtx); err != nil {
			log.Warn("v2 serve shutdown", "error", err)
		}
	}
	storeCancel() // historian drains its queue, flushes, closes the pool
	select {
	case <-storeDone:
	case <-shutCtx.Done():
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

type scopeController struct {
	registry         *audience.Registry
	publicEvents     *state.Store
	operatorDetector *detect.Detector
	publicDetector   *detect.Detector
	epochs           *audience.PolicyEpochs
	log              *slog.Logger

	mu     sync.RWMutex
	server *serve.Server
}

func (c *scopeController) AttachServer(server *serve.Server) {
	c.mu.Lock()
	c.server = server
	c.mu.Unlock()
}

// Apply is called from the ordered decode channel after every DATA record from
// the changed session. Whole-audience invalidation is conservative but sound:
// merged ephemeris/confidence/detector state cannot subtract one source exactly.
func (c *scopeController) Apply(revocation *ingest.ScopeRevocation) {
	if c == nil || revocation == nil || c.registry == nil {
		return
	}
	affected := c.registry.ResetContext(revocation.Previous)
	if len(affected) == 0 {
		return
	}
	for _, selected := range affected {
		switch selected.Kind {
		case identity.AudiencePublic:
			if c.publicEvents != nil {
				c.publicEvents.Reset()
			}
			if c.publicDetector != nil {
				c.publicDetector.Reset()
			}
		case identity.AudienceOperator:
			if c.operatorDetector != nil {
				c.operatorDetector.Reset()
			}
		}
	}
	var changed []string
	if c.epochs != nil {
		changed = c.epochs.Advance(affected, revocation.ChangedAt)
	}
	c.mu.RLock()
	server := c.server
	c.mu.RUnlock()
	if server != nil {
		server.InvalidateAudiences(affected)
	}
	if c.log != nil {
		c.log.Warn("authorization scope changed; rebuilt affected audiences from post-change receipts",
			"observer", revocation.Previous.ObserverID, "audiences", changed,
			"previous_policy_revision", revocation.Previous.Publication.Revision)
	}
}

// decodeLoop folds every ingested frame into live state and, when the historian
// is enabled, enqueues the raw frame for the forensic record — persistence is
// independent of decode success, so a decoder bug never loses evidence. Each
// frame is applied with a per-frame recover so a decoder edge case drops one
// frame rather than crashing the process. It has no shutdown logic of its own
// : it simply ranges frames until the channel is closed, which the owner
// (run, above) does only after every producer has confirmed it will never send
// again — so every already-enqueued frame is applied with no drain race.
func decodeLoop(frames <-chan *ingest.RawFrame, live, publicLive, publicEventsLive *state.Store, historian *store.Store, log *slog.Logger, lastFrame *atomic.Int64, scoped *audience.Registry, revoke func(*ingest.ScopeRevocation)) {
	lim := &panicLogLimiter{}
	apply := func(f *ingest.RawFrame) {
		if f != nil && f.ScopeRevocation != nil {
			if revoke != nil {
				revoke(f.ScopeRevocation)
			}
			return
		}
		// every dial and push frame funnels through here, so this one
		// stamp is the whole data plane's liveness signal for /healthz.
		lastFrame.Store(time.Now().UnixNano())
		defer recoverDecodePanic(f, log, lim)
		if historian != nil && f.Obs == nil && f.RF == nil { // telemetry (observables, RF) is not a nav-frame record
			observer := f.Observer
			if observer.ObserverID == "" {
				// Legacy/programmatic RawFrames have no trusted context. Persistence
				// still records an explicit private/unassigned decision rather than NULL
				// fields that a future reader might accidentally interpret as public.
				observer = identity.NewPrivateContext(f.Source, identity.CredentialLocalDial)
			}
			historian.Enqueue(&store.NavFrame{
				Ts:                    time.Now(),
				ReceivedAt:            f.Recv,
				SourceID:              f.Source,
				OrganizationID:        observer.OrganizationID,
				EnrollmentID:          observer.EnrollmentID,
				CollectorInstanceID:   observer.CollectorInstanceID,
				CollectionIDs:         append([]string(nil), observer.CollectionIDs...),
				FeedGrants:            append([]string(nil), observer.FeedGrants...),
				DeclaredCapabilities:  policySignalStrings(observer.DeclaredCapabilities),
				Provenance:            "local",
				CredentialTier:        string(observer.CredentialTier),
				CredentialFingerprint: observer.CredentialFingerprint,
				AttestationTier:       string(observer.AttestationTier),
				AggregateUse:          string(observer.Publication.AggregateUse),
				StationMetadata:       string(observer.Publication.StationMetadata),
				EventVisibility:       string(observer.Publication.EventVisibility),
				RawExport:             string(observer.Publication.RawExport),
				FederationPeers:       append([]string(nil), observer.Publication.FederationPeers...),
				PublishSignals:        policySignalStrings(observer.Publication.Signals),
				PolicyRevision:        observer.Publication.Revision,
				GnssID:                int(f.GnssID),
				SvID:                  f.SvID,
				SigID:                 f.SigID,
				FreqID:                f.FreqID,
				MsgType:               persistMsgType(f),
				Raw:                   f.RawBytes(),
				DecoderVer:            version.Version,
				SourceSeq:             f.Seq,
				HasSourceSeq:          f.HasSeq,
				Session:               f.Session, // dedup-key third component 
			})
		}
		if !f.Admission.Current() {
			return
		}
		live.Apply(f)
		if scoped != nil {
			scoped.ApplyPrivate(f)
		}
		if publicFrame, ok := audience.ProjectPublic(f); ok {
			publicLive.Apply(publicFrame)
		}
		if publicEventFrame, ok := audience.ProjectPublicEvents(f); ok {
			publicEventsLive.Apply(publicEventFrame)
		}
	}
	for f := range frames {
		apply(f)
	}
}

func policySignalStrings(signals []identity.Signal) []string {
	out := make([]string, len(signals))
	for i, signal := range signals {
		out[i] = fmt.Sprintf("%d:%d", signal.GnssID, signal.SigID)
	}
	return out
}

// panicLogEvery is the per-signal decode-panic log cadence : one full
// ERROR line per offending (gnssid, svid, sigid) per interval; recurrences
// within the interval increment DecodePanicsTotal silently.
const panicLogEvery = time.Minute

// panicLogLimiter rate-limits the decode-panic ERROR line per (gnssid, svid,
// sigid) : a decoder that panics on a specific bit pattern panics on
// every re-broadcast — the same SV every few seconds, for months — and per-frame
// ERROR logging both fills the logfile and buries the one line an operator
// needs. Only the LOG line is limited; the counter increments on every
// recurrence.
//
// the third key component is the SIGNAL id, not msg_type. sigid is what
// actually selects the panicking code path (decode dispatch is by gnssid+sigid,
// never by msg_type) and dispatch NARROWS it to ~10 values that reach any
// decoder — it is an unvalidated wire/receiver byte on arrival (not
// range-checked; the recover also wraps pre-dispatch work), whereas push-path
// msg_type is an unvalidated wire byte ranging over ~240 values. The honest bound
// is therefore 8 constellations × ≤256 SVs (svid is byte-wide — the ≤63 cap
// applies only to SBAS/QZSS/NavIC, see state.svIDInRange) × ~10 sigIds; entries
// are kept forever rather than evicted because that product is small and the map
// only ever grows on ACTUAL panics.
type panicLogLimiter struct {
	mu   sync.Mutex
	last map[[3]int]time.Time
}

func (l *panicLogLimiter) allow(gnssid, svid, sigid int, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.last == nil {
		l.last = map[[3]int]time.Time{}
	}
	k := [3]int{gnssid, svid, sigid}
	if t, ok := l.last[k]; ok && now.Sub(t) < panicLogEvery {
		return false
	}
	l.last[k] = now
	return true
}

// recoverDecodePanic is decodeLoop's per-frame recover; it must be
// deferred DIRECTLY (recover() is effective only in a directly-deferred
// function). The counter is the alertable signal — dashboards watching
// decode_errors_total/nav_crc_fail_total structurally never see panics — and
// the rate-limited log line carries the SV identity a debugger needs to
// reproduce the offending bit pattern from the historian's raw record.
func recoverDecodePanic(f *ingest.RawFrame, log *slog.Logger, lim *panicLogLimiter) {
	r := recover()
	if r == nil {
		return
	}
	metrics.DecodePanicsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID))).Inc()
	// keyed on sigid (the decode-dispatch key), not msg_type; the log line
	// still carries msg_type for the debugger.
	if lim.allow(int(f.GnssID), f.SvID, f.SigID, time.Now()) {
		log.Error("decode panic recovered; frame dropped",
			"gnssid", int(f.GnssID), "svid", f.SvID, "sigid", f.SigID,
			"source", f.Source, "msg_type", f.MsgType, "recover", fmt.Sprint(r))
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

// declaredPublicCapabilities projects the server-owned declarations into the
// same source keys used by public state. Anonymous contributors collapse into
// one bucket, so their declared signal sets are unioned; private sources vanish.
func declaredPublicCapabilities(cfg *config.Config, eventsOnly bool) map[string][]state.CapSignal {
	sets := map[string]map[state.CapSignal]bool{}
	add := func(c identity.ObserverContext, decl []config.Capability) {
		c, err := c.Normalize()
		if err != nil || !c.PublicEligible() || len(decl) == 0 {
			return
		}
		if eventsOnly && c.Publication.EventVisibility == identity.EventsPrivate {
			return
		}
		id := c.ObserverID
		if c.Publication.AggregateUse == identity.AggregatePublicAnonymous {
			id = audience.AnonymousPublicSource
		}
		if sets[id] == nil {
			sets[id] = map[state.CapSignal]bool{}
		}
		for _, capability := range decl {
			sets[id][state.CapSignal{Gnss: capability.Gnss, Sig: capability.Sig}] = true
		}
	}
	for _, s := range cfg.Ingest {
		add(s.ObserverContext, s.CapDecl)
	}
	for _, o := range cfg.Push.Observers {
		add(o.ObserverContext, o.CapDecl)
	}
	out := make(map[string][]state.CapSignal, len(sets))
	for id, set := range sets {
		for sig := range set {
			out[id] = append(out[id], sig)
		}
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
			for _, snapshot := range api.SnapshotAllFeeds() {
				// Per-call deadline : ctx is cancel-only, so a hung DB connection
				// must not block this loop indefinitely on OS TCP timeouts.
				callCtx, cancel := context.WithTimeout(ctx, snapshotWriteTimeout)
				err := historian.WriteSnapshot(callCtx, now, snapshot.Audience.Key(), snapshot.Feed, snapshot.Body)
				cancel()
				if err != nil {
					if ctx.Err() != nil {
						return // shutting down: the historian context is going away
					}
					log.Warn("feed snapshot write failed", "audience", snapshot.Audience.Key(), "feed", snapshot.Feed, "error", err)
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
func detectLoop(ctx context.Context, live *state.Store, det *detect.Detector, historian eventWriter, api eventPublisher, epochs *audience.PolicyEpochs, log *slog.Logger, audienceKey ...string) {
	tick := time.NewTicker(detectInterval)
	defer tick.Stop()
	// regression fix/detector sampling and durable writes have separate owners. The
	// detector only appends to the bounded FIFO; a slow historian therefore cannot
	// stall the 15-second sampling cadence or let a newer transition bypass an older
	// queued one.
	writer := newEventPipeline(historian, api, log, audienceKey...)
	writer.epochs = epochs
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		writer.run(ctx)
	}()
	for {
		select {
		case <-tick.C:
			detectTick(live, det, writer, log)
		case <-ctx.Done():
			// writer.run performs one bounded final flush with a fresh
			// context while run() deliberately keeps the historian alive.
			<-writerDone
			return
		}
	}
}

// detectTick runs one detector sample and emits its confirmed events. It is wrapped in
// a recover() : "the collector must never go down" — a panic in the detector or
// an event write must reconnect/skip this tick, not kill the detect goroutine (which has
// no supervisor within the process) and with it all integrity monitoring. Events already
// appended to the writer-owned FIFO remain visible if a later classification panics.
func detectTick(live *state.Store, det *detect.Detector, writer *eventPipeline, log *slog.Logger) {
	defer func() {
		if r := recover(); r != nil {
			metrics.EventWriteErrorsTotal.Inc()
			log.Error("detect tick panicked; skipping", "panic", r)
		}
	}()
	generation := writer.policyGeneration()
	for _, e := range detectEvents(live, det, writer.audience) {
		if pe := prepareEvent(e, writer.historian, log); pe != nil {
			pe.row.Audience = writer.audience
			pe.row.RedactionClass = writer.redactionClass
			pe.policyGeneration = generation
			writer.enqueue(*pe)
		}
	}
}

func detectEvents(live *state.Store, det *detect.Detector, audienceKey string) []detect.Event {
	now := time.Now()
	// the live-receiver count gates the per-SV silence classifier — from a
	// sub-constellation-footprint fleet, "unseen for an hour" is orbital mechanics,
	// not an outage.
	// the detector reads the UNFILTERED SBAS view (SBASDetect) so a dark
	// GEO stays classifiable after the served feed drops it — the sbas_lost event
	// has no input otherwise. The served /gnss feed keeps using FeedSBAS.
	events := det.Tick(now, live.FeedSVs(now), live.SBASDetect(now), live.LiveReceivers(now))
	// Station-scoped PNT-defense events (jamming/spoofing/RF, docs/DEFENSE-PNT.md)
	// share the debounce state machine and event pipeline.
	events = append(events, det.TickStations(now, live.FeedStationRF(now))...)
	// Station liveness (station_offline, regression fix/regression fix) reads the unfiltered
	// per-station age map — retained state, not the staleness-filtered RF view.
	stationLastSeen := live.StationLastSeen(now)
	capabilityReports := live.FeedCapabilityReports(now)
	if audienceKey == "public" {
		// Anonymous/redacted contributors may affect satellite-level public
		// detection, but the synthetic bucket must never become a station event.
		delete(stationLastSeen, audience.AnonymousPublicSource)
		delete(capabilityReports, audience.AnonymousPublicSource)
	}
	events = append(events, det.TickStationLiveness(now, stationLastSeen)...)
	// Capability plausibility: a demonstrated signal gone silent, or a signal the
	// node's silicon can't produce (docs/INTEGRITY.md §6, CONSTELLATIONS §7).
	events = append(events, det.TickCapabilities(now, capabilityReports)...)
	return events
}

func detectEventsSafely(live *state.Store, det *detect.Detector, audienceKey string, log *slog.Logger) (events []detect.Event) {
	defer func() {
		if recovered := recover(); recovered != nil {
			metrics.EventWriteErrorsTotal.Inc()
			log.Error("scoped detect tick panicked; skipping", "audience", audienceKey, "panic", recovered)
			events = nil
		}
	}()
	return detectEvents(live, det, audienceKey)
}

type scopedEventPublisher struct {
	server   *serve.Server
	audience identity.Audience
}

func (p scopedEventPublisher) PublishEvent(event serve.EventMsg) {
	p.server.PublishEventForAudience(p.audience, event)
}

// scopedDetectLoop gives each organization/collection an independent detector
// state machine and durable audience cursor. Writer goroutines are allocated
// lazily only after a view confirms its first event.
func scopedDetectLoop(ctx context.Context, registry *audience.Registry, historian eventWriter, api *serve.Server, epochs *audience.PolicyEpochs, log *slog.Logger) {
	tick := time.NewTicker(detectInterval)
	defer tick.Stop()
	type scopedState struct {
		detector   *detect.Detector
		writer     *eventPipeline
		done       chan struct{}
		audience   identity.Audience
		generation uint64
	}
	states := make(map[string]*scopedState)
	for {
		select {
		case <-tick.C:
			for _, view := range registry.Views() {
				if view.Audience.Kind != identity.AudienceOrganization && view.Audience.Kind != identity.AudienceCollection {
					continue
				}
				key := view.Audience.Key()
				scoped := states[key]
				if scoped == nil {
					scoped = &scopedState{detector: detect.New(0), audience: view.Audience, generation: view.Store.Generation()}
					states[key] = scoped
				}
				if generation := view.Store.Generation(); generation != scoped.generation {
					scoped.detector.Reset()
					scoped.generation = generation
				}
				policyGeneration, _ := epochs.Current(key)
				events := detectEventsSafely(view.Store, scoped.detector, key, log)
				if len(events) == 0 {
					continue
				}
				if scoped.writer == nil {
					var publisher eventPublisher
					if api != nil {
						publisher = scopedEventPublisher{server: api, audience: scoped.audience}
					}
					scoped.writer = newEventPipeline(historian, publisher, log, key)
					scoped.writer.epochs = epochs
					scoped.done = make(chan struct{})
					go func(state *scopedState) {
						defer close(state.done)
						state.writer.run(ctx)
					}(scoped)
				}
				for _, event := range events {
					if pending := prepareEvent(event, scoped.writer.historian, log); pending != nil {
						pending.row.Audience = key
						pending.row.RedactionClass = "private"
						pending.policyGeneration = policyGeneration
						scoped.writer.enqueue(*pending)
					}
				}
			}
		case <-ctx.Done():
			for _, scoped := range states {
				if scoped.done != nil {
					<-scoped.done
				}
			}
			return
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

// eventFinalFlushTimeout is one shared shutdown budget, deliberately shorter than the
// daemon's default 15-second ShutdownTimeout. It is a var so the hard-down test does not
// have to spend five seconds proving the bound.
var eventFinalFlushTimeout = 5 * time.Second

// eventPendingMax bounds the in-RAM re-attempt queue so a persistent DB outage cannot grow
// it without limit : when full, the oldest queued event is dropped (counted, logged
// loudly) rather than OOMing the process.
const eventPendingMax = 256

// pendingEvent is a confirmed event whose durable write has not yet succeeded, held for
// re-attempt on a later detector tick. row is the fully-built insert; ev carries the
// already-sanitized fields the SSE publish needs once the id is assigned.
type pendingEvent struct {
	row              store.EventRow
	ev               detect.Event
	policyGeneration uint64
}

// eventPipeline owns the ordered durable-write queue. The detector only holds mu long
// enough to append, so database latency never delays sampling. pending[0] stays in the
// slice while it is being written; enqueue therefore knows not to discard that in-flight
// item when applying the established oldest-drop policy at capacity.
type eventPipeline struct {
	mu             sync.Mutex
	pending        []pendingEvent
	writing        bool
	wake           chan struct{}
	historian      eventWriter
	publisher      eventPublisher
	log            *slog.Logger
	audience       string
	redactionClass string
	epochs         *audience.PolicyEpochs
}

func (p *eventPipeline) policyGeneration() uint64 {
	if p.epochs == nil {
		return 0
	}
	generation, _ := p.epochs.Current(p.audience)
	return generation
}

func (p *eventPipeline) generationCurrent(generation uint64) bool {
	return p.epochs == nil || p.policyGeneration() == generation
}

func newEventPipeline(historian eventWriter, publisher eventPublisher, log *slog.Logger, audienceKey ...string) *eventPipeline {
	audienceKeyValue := "operator:local"
	redactionClass := "private"
	if len(audienceKey) > 0 && audienceKey[0] != "" {
		audienceKeyValue = audienceKey[0]
	}
	if audienceKeyValue == "public" {
		redactionClass = "public_policy_filtered"
	}
	return &eventPipeline{
		pending:        make([]pendingEvent, 0, eventPendingMax),
		wake:           make(chan struct{}, 1),
		historian:      historian,
		publisher:      publisher,
		log:            log,
		audience:       audienceKeyValue,
		redactionClass: redactionClass,
	}
}

// enqueue preserves global detection order. At capacity it keeps an item currently in a
// database call and drops the oldest waiting item; otherwise it drops the oldest item,
// matching bounded-queue accounting without racing the writer.
func (p *eventPipeline) enqueue(pe pendingEvent) {
	if pe.row.Audience == "" {
		pe.row.Audience = p.audience
	}
	if pe.row.RedactionClass == "" {
		pe.row.RedactionClass = p.redactionClass
	}
	p.mu.Lock()
	if len(p.pending) >= eventPendingMax {
		dropAt := 0
		if p.writing {
			dropAt = 1
		}
		dropped := p.pending[dropAt]
		copy(p.pending[dropAt:], p.pending[dropAt+1:])
		p.pending = p.pending[:len(p.pending)-1]
		metrics.EventWriteErrorsTotal.Inc()
		p.log.Error("integrity event re-attempt queue full; dropping oldest confirmed event",
			"dropped_sv", dropped.ev.SV, "dropped_type", dropped.ev.Type, "queue_max", eventPendingMax)
	}
	p.pending = append(p.pending, pe)
	p.mu.Unlock()
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *eventPipeline) head() (pendingEvent, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.pending) == 0 {
		return pendingEvent{}, false
	}
	p.writing = true
	return p.pending[0], true
}

func (p *eventPipeline) finishHead(remove bool) {
	p.mu.Lock()
	if remove && len(p.pending) > 0 {
		copy(p.pending, p.pending[1:])
		p.pending = p.pending[:len(p.pending)-1]
	}
	p.writing = false
	p.mu.Unlock()
}

func (p *eventPipeline) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.pending)
}

// run writes one FIFO head at a time. A panic from an EventStore implementation or
// publisher is contained here; because the head is removed only after a normal
// successful return, the queue remains exactly ordered and contains no alias-created
// duplicates after recovery.
func (p *eventPipeline) run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			p.flushFinal()
			return
		}
		pe, ok := p.head()
		if !ok {
			select {
			case <-p.wake:
			case <-ctx.Done():
			}
			continue
		}
		ok = p.writeSafely(ctx, pe, defaultEventRetry, true)
		p.finishHead(ok)
		if ok {
			continue
		}
		select {
		case <-time.After(defaultEventRetry.backoff):
		case <-ctx.Done():
		}
	}
}

func (p *eventPipeline) writeSafely(ctx context.Context, pe pendingEvent, retry eventRetry, queued bool) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			metrics.EventWriteErrorsTotal.Inc()
			p.log.Error("integrity event writer panicked; event retained", "type", pe.ev.Type, "sv", pe.ev.SV, "panic", r)
			ok = false
		}
	}()
	guard := func() bool { return p.generationCurrent(pe.policyGeneration) }
	return writeAndPublishRetry(ctx, pe.row, pe.ev, p.historian, p.publisher, p.log, retry, queued, guard)
}

// flushFinal uses a fresh shared deadline because the detector's parent context is already
// cancelled. Every queued event gets at most one final call; failures are removed, counted
// by writeAndPublishRetry, and summarized loudly before the writer returns.
func (p *eventPipeline) flushFinal() {
	if p.len() == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), eventFinalFlushTimeout)
	defer cancel()
	retry := eventRetry{attempts: 1, perCallTO: eventFinalFlushTimeout}
	dropped := 0
	for {
		pe, ok := p.head()
		if !ok {
			break
		}
		if !p.writeSafely(ctx, pe, retry, false) {
			dropped++
		}
		// A final attempt is final whether it succeeds or fails.
		p.finishHead(true)
	}
	if dropped > 0 {
		p.log.Error("dropping confirmed integrity events after final shutdown flush", "count", dropped)
	}
}

// emitEvent counts, sanitizes and marshals one confirmed event (exactly once), then makes
// the first durable-write+publish attempt. It returns a *pendingEvent when the write failed
// after its bounded retry and the caller should re-attempt on a later tick; nil when the
// event was published, permanently non-durable (no historian), or otherwise complete.
func emitEvent(ctx context.Context, e detect.Event, historian eventWriter, api eventPublisher, log *slog.Logger) *pendingEvent {
	pe := prepareEvent(e, historian, log)
	if pe == nil {
		return nil
	}
	if writeAndPublish(ctx, pe.row, pe.ev, historian, api, log) {
		return nil
	}
	return pe
}

// prepareEvent performs the once-per-confirmation work before the event enters the FIFO.
// Retries only see the returned immutable row/event pair and never touch EventsTotal.
func prepareEvent(e detect.Event, historian eventWriter, log *slog.Logger) *pendingEvent {
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
		DedupeKey: newEventDedupeKey(),
	}
	return &pendingEvent{row: row, ev: e}
}

// newEventDedupeKey mints the regression fix idempotency identity for one confirmed
// transition. It is generated HERE — once, before the first write attempt — and
// carried unchanged in the immutable pendingEvent row, so every retry of the
// same confirmation presents the same key and the store's (time, dedupe_key)
// upsert returns the already-committed id instead of inserting a duplicate.
// Random rather than content-derived: a content hash (type/sv/values/time)
// would collide two legitimately identical transitions if a detector ever
// confirmed them at the same timestamp, and the review's requirement is only
// that the key be stable across retries of ONE confirmation. crypto/rand.Read
// never returns an error (its Go 1.24+ contract; it crashes on entropy
// failure, which is unreachable on supported platforms).
func newEventDedupeKey() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// writeAndPublish makes the durable write (bounded retry under a per-call timeout) and, on
// success, publishes to SSE with the assigned id — the durable-id contract (commit 93a21a9):
// SSE only ever carries a real DB id. Returns true on success, false when every attempt
// failed and the event must remain queued. It does NOT touch EventsTotal, so a retry never
// double-counts the event.
func writeAndPublish(ctx context.Context, row store.EventRow, e detect.Event, historian eventWriter, api eventPublisher, log *slog.Logger) bool {
	return writeAndPublishRetry(ctx, row, e, historian, api, log, defaultEventRetry, true, nil)
}

func writeAndPublishRetry(ctx context.Context, row store.EventRow, e detect.Event, historian eventWriter, api eventPublisher, log *slog.Logger, retry eventRetry, queued bool, authorized func() bool) bool {
	if authorized != nil && !authorized() {
		log.Warn("dropping integrity event invalidated by an audience policy transition",
			"audience", row.Audience, "type", e.Type, "sv", e.SV)
		return true
	}
	id, err := writeEventRetry(ctx, historian, row, retry, log)
	if err != nil {
		metrics.EventWriteErrorsTotal.Inc()
		msg := "persist integrity event failed during final shutdown flush"
		if queued {
			msg = "persist integrity event failed after retries; retained for re-attempt"
		}
		log.Error(msg, "type", e.Type, "sv", e.SV, "error", err)
		return false
	}
	if authorized != nil && !authorized() {
		// The write may have committed while the policy changed. Historical reads
		// are clamped to the new epoch; do not put the stale event back into SSE.
		log.Warn("withholding committed integrity event after an audience policy transition",
			"audience", row.Audience, "id", id, "type", e.Type, "sv", e.SV)
		return true
	}
	log.Info("integrity event", "id", id, "audience", row.Audience, "sv", e.SV, "type", e.Type,
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

// expireTickFor derives the SV-expiry sweep cadence from the configured TTL
// : min(30 s, ttl/2), floored at 1 s. The default 2 h TTL keeps the
// established 30 s sweep; a fast-expiry configuration (a short sv_ttl in a test
// deployment — parseDurPositive accepts any positive duration) previously
// quantized silently to the unrelated 30 s constant, holding SVs up to 30 s
// past their TTL. ttl/2 keeps the worst-case overshoot at half the TTL.
func expireTickFor(ttl time.Duration) time.Duration {
	tick := ttl / 2
	if tick > 30*time.Second {
		tick = 30 * time.Second
	}
	if tick < time.Second {
		tick = time.Second
	}
	return tick
}

// stateLoop re-propagates live SVs on the configured cadence and expires stale ones.
func stateLoop(ctx context.Context, cfg config.State, stores ...*state.Store) {
	prop := time.NewTicker(cfg.PropagateEvery)
	defer prop.Stop()
	expire := time.NewTicker(expireTickFor(cfg.SVTTL))
	defer expire.Stop()
	for {
		select {
		case <-prop.C:
			now := time.Now()
			for _, store := range stores {
				store.Propagate(now)
			}
		case <-expire.C:
			now := time.Now()
			for _, store := range stores {
				store.Expire(now, cfg.SVTTL)
				store.ExpireStations(now) // sbas/rf/almanac RAM eviction
			}
		case <-ctx.Done():
			return
		}
	}
}

// newDebugStateHandler builds the /debug/state handler: the regression fix loopback-peer
// gate plus the regression fix marshal-before-status body. It is a package-level
// constructor rather than a closure inside run() so the security gate — the sole
// control keeping a full live-state + source-name dump off a deliberately
// non-loopback metrics bind — is directly testable.
func newDebugStateHandler(live *state.Store, log *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// /debug/state dumps full live state + source names. Even when
		// [metrics].addr is deliberately bound non-loopback (remote Prometheus),
		// this endpoint stays loopback-only — a reverse proxy on this host still
		// reaches it (its upstream connection originates from loopback). The gate
		// reads ONLY the transport peer: X-Forwarded-For and friends are attacker-
		// settable headers and are deliberately never consulted.
		if !isLoopbackPeer(r.RemoteAddr) {
			http.Error(w, "forbidden: /debug/state is loopback-only", http.StatusForbidden)
			return
		}
		// marshal to a buffer BEFORE committing a status, so an encode
		// failure is a clean 500 + log instead of a silently truncated 200.
		// Defense-in-depth: every float in SVEntry is finite-gated (snapshot.go),
		// so this branch is unreachable today — kept so a future field can never
		// reintroduce the truncation mode.
		b, err := json.Marshal(live.Snapshot(time.Now()))
		if err != nil {
			log.Error("debug/state marshal failed", "error", err)
			http.Error(w, "encode failed", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b) // a mid-write client disconnect is the client's problem
	}
}

// isLoopbackPeer reports whether an http.Request.RemoteAddr is a loopback
// address. RemoteAddr is always host:port from net/http; anything
// unparsable is treated as non-loopback (deny).
func isLoopbackPeer(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func printConfigSummary(cfg *config.Config) {
	fmt.Println("Configuration valid.")
	fmt.Printf("  collector realm: %s\n", cfg.Collector.InstanceID)
	fmt.Printf("  metrics addr:   %s\n", cfg.Metrics.Addr)
	serveAddr := cfg.Serve.Addr
	if serveAddr == "" {
		serveAddr = "(disabled)"
	}
	fmt.Printf("  serve addr:     %s\n", serveAddr)
	fmt.Printf("  serve audience: %s\n", cfg.Serve.Audience)
	pushAddr := cfg.Push.Addr
	if pushAddr == "" {
		pushAddr = "(disabled)"
	}
	fmt.Printf("  push addr:      %s (%d observers)\n", pushAddr, len(cfg.Push.Observers))
	authSource := "config bootstrap"
	if cfg.Authorization.DSN != "" {
		authSource = fmt.Sprintf("database (TTL %s, recheck %s)", cfg.Authorization.CacheTTL, cfg.Authorization.RecheckEvery)
	}
	fmt.Printf("  authorization:  %s\n", authSource)
	fmt.Printf("  log:            %s / %s\n", cfg.Logging.Level, cfg.Logging.Format)
	fmt.Printf("  state shards:   %d\n", cfg.State.Shards)
	fmt.Printf("  sv ttl:         %s\n", cfg.State.SVTTL)
	fmt.Printf("  ingest sources: %d\n", len(cfg.Ingest))
	fmt.Printf("  export grants:  %d (transport disabled)\n", len(cfg.Federation.ExportGrants))
	for _, s := range cfg.Ingest {
		status := "enabled"
		if s.Disabled {
			status = "disabled"
		}
		fmt.Printf("    - %-16s %-4s %-22s %s\n", s.Name, s.Type, s.Addr, status)
	}
	// regression fix/-check-config surfaces the same non-fatal findings the
	// daemon logs at startup, so the rc.d preflight  shows them too.
	for _, w := range cfg.Warnings {
		fmt.Printf("  WARNING: %s\n", w)
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
