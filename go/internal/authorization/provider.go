// Package authorization resolves server-owned ingest and read grants from the
// control plane. Presented bearer tokens are hashed before lookup/cache keys;
// plaintext credentials are never retained by this package.
package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ptudor/navlistener/internal/identity"
)

const (
	observerAuthorizationView = "navlistener_observer_authorization_v2"
	readAuthorizationView     = "navlistener_read_authorization_v1"
	changeNotifyChannel       = "navlistener_authorization_changed"
	defaultCacheTTL           = 30 * time.Second
	// Per-method positive-entry budget. Denied results never enter these maps;
	// they live in their own separately bounded negative caches below, so a flood
	// of never-reused invalid credentials cannot evict useful positive authority.
	defaultCacheLimit    = 1024
	defaultLookupTimeout = 5 * time.Second
	// an authoritative denial is remembered briefly so one invalid
	// credential cannot force a control-plane query — and a connection-pool slot —
	// on every request. The TTL is deliberately far shorter than the positive one:
	// a negative entry only ever denies, so its staleness window is the delay
	// before a newly *granted* credential starts working, never a window in which
	// withdrawn authority is retained.
	defaultNegativeCacheTTL = 5 * time.Second
	// Negative-cache budget. Keys are fixed-size digests (see observerFoldedKey),
	// so the worst-case footprint is bounded no matter how long a hostile HELLO's
	// station/feed strings are.
	defaultNegativeCacheLimit = 4096
	// Ceiling on control-plane lookups in flight at once, across both APIs. The
	// negative cache and single-flight bound repeated and concurrent use of the
	// *same* credential; this bounds a stream of never-repeated ones, which misses
	// every cache by construction. Waiting for a slot is charged to the caller's
	// existing defaultLookupTimeout budget rather than added on top of it.
	defaultLookupConcurrency = 16
	// the invalidation listener's reconnect pacing. backoff used to
	// only ever grow, so once any early outage reached the cap, every later
	// transient disconnect for the life of the process waited the full interval —
	// widening the window in which a revocation depends solely on cache TTL.
	listenBackoffInitial = 250 * time.Millisecond
	listenBackoffMax     = 5 * time.Second
	// listenHealthyAfter is how long a LISTEN must hold before the connection
	// counts as proven even if the control plane never sent a notification.
	listenHealthyAfter = 30 * time.Second
)

type observerLookup func(context.Context, string, string, string) (identity.ObserverContext, bool, error)
type readLookup func(context.Context, string) (identity.ReadPrincipal, bool, error)

type observerCacheKey struct {
	tokenSHA256 string
	station     string
	feed        string
}

type observerCacheEntry struct {
	context identity.ObserverContext
	allowed bool
	expires time.Time
}

type readCacheEntry struct {
	principal identity.ReadPrincipal
	allowed   bool
	expires   time.Time
}

// lookupFlight is one in-flight control-plane query, shared by every caller that
// missed the caches for the same key while it was running. Its
// generation is captured at creation under p.mu, so the existing revocation
// barrier still applies: the shared result is trusted only when no
// revocation/policy change committed between the query starting and finishing.
type lookupFlight struct {
	done       chan struct{}
	generation uint64

	// Written by the leader before done is closed, read by followers after.
	observer identity.ObserverContext
	read     identity.ReadPrincipal
	allowed  bool
	trusted  bool
	err      error
}

// observerFoldedKey folds a token digest and the presented station/feed into one
// fixed-size key for the negative and single-flight maps. Station and feed are
// attacker-controlled up to the HELLO cap, so storing them verbatim would let a
// hostile feeder inflate negative-cache memory; folding also keeps those maps
// uniform with the read side. Lengths are prefixed so no pair of distinct
// (station, feed) inputs can produce the same pre-image.
func observerFoldedKey(key observerCacheKey) string {
	h := sha256.New()
	// tokenSHA256 is a fixed 64-character hex digest, so it needs no prefix.
	h.Write([]byte(key.tokenSHA256))
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(key.station)))
	h.Write(n[:])
	h.Write([]byte(key.station))
	binary.BigEndian.PutUint64(n[:], uint64(len(key.feed)))
	h.Write(n[:])
	h.Write([]byte(key.feed))
	return hex.EncodeToString(h.Sum(nil))
}

// Provider is a bounded ingest-authorization cache over a stable control-plane SQL
// views. A NOTIFY clears it immediately; TTL remains the fail-safe bound when a
// notification connection is interrupted.
type Provider struct {
	pool *pgxpool.Pool
	ttl  time.Duration
	now  func() time.Time
	log  *slog.Logger

	lookupObserver observerLookup
	lookupRead     readLookup

	// lookupSlots bounds control-plane queries in flight. It is fixed at
	// construction and never reassigned, so it needs no lock.
	lookupSlots chan struct{}

	mu        sync.Mutex
	observers map[observerCacheKey]observerCacheEntry
	readers   map[string]readCacheEntry
	// Authoritative denials, keyed by digest (readers) and folded digest
	// (observers), valued by expiry. Cleared by InvalidateAll with the positive
	// caches so a newly issued grant is never shadowed by a stale denial.
	deniedObservers map[string]time.Time
	deniedReaders   map[string]time.Time
	// In-flight lookups, so N concurrent misses on one key issue one query.
	flightObservers map[string]*lookupFlight
	flightReaders   map[string]*lookupFlight
	generation      uint64
	cacheLimit      int
	negativeLimit   int
	negativeTTL     time.Duration
	lookupTimeout   time.Duration
}

// NewDatabase connects to the read-only control-plane database and verifies it
// is reachable. The SQL view contract is documented in this package's README.
func NewDatabase(ctx context.Context, dsn string, ttl time.Duration, log *slog.Logger) (*Provider, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("authorization DSN is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("authorization pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("authorization database ping: %w", err)
	}
	p := newProvider(ttl, log, nil)
	p.pool = pool
	p.lookupObserver = p.lookupObserverDatabase
	p.lookupRead = p.lookupReadDatabase
	return p, nil
}

// VerifyContracts fails startup before listeners bind when an enabled surface's
// versioned SQL view or required columns are missing.
func (p *Provider) VerifyContracts(ctx context.Context, observer, read bool) error {
	if observer {
		const query = `SELECT token_sha256, observer_id, organization_id, enrollment_id,
	   collector_instance_id, collection_ids, feed_grants, declared_capabilities, credential_tier,
       credential_fingerprint, attestation_tier, aggregate_use, station_metadata,
       event_visibility, raw_export, federation_peers, publish_signals,
       policy_revision, enabled
  FROM ` + observerAuthorizationView + ` LIMIT 0`
		rows, err := p.pool.Query(ctx, query)
		if err != nil {
			return fmt.Errorf("observer authorization view contract: %w", err)
		}
		rows.Close()
	}
	if read {
		const query = `SELECT token_sha256, principal_id, audience_grants, revision, enabled
  FROM ` + readAuthorizationView + ` LIMIT 0`
		rows, err := p.pool.Query(ctx, query)
		if err != nil {
			return fmt.Errorf("read authorization view contract: %w", err)
		}
		rows.Close()
	}
	return nil
}

func newProvider(ttl time.Duration, log *slog.Logger, lookup observerLookup) *Provider {
	if ttl <= 0 {
		ttl = defaultCacheTTL
	}
	if log == nil {
		log = slog.Default()
	}
	// A negative entry must never outlive the positive TTL: the operator's
	// configured TTL is the contract for how quickly a control-plane change takes
	// effect, and that has to bound denials as well as grants.
	negativeTTL := min(ttl, defaultNegativeCacheTTL)
	return &Provider{
		ttl: ttl, now: time.Now, log: log, lookupObserver: lookup,
		lookupSlots:     make(chan struct{}, defaultLookupConcurrency),
		observers:       make(map[observerCacheKey]observerCacheEntry),
		readers:         make(map[string]readCacheEntry),
		deniedObservers: make(map[string]time.Time),
		deniedReaders:   make(map[string]time.Time),
		flightObservers: make(map[string]*lookupFlight),
		flightReaders:   make(map[string]*lookupFlight),
		cacheLimit:      defaultCacheLimit,
		negativeLimit:   defaultNegativeCacheLimit,
		negativeTTL:     negativeTTL,
		lookupTimeout:   defaultLookupTimeout,
	}
}

// AuthorizeRead resolves a bearer token to one principal and its explicit
// private audience grants. The cache key is only the token digest.
func (p *Provider) AuthorizeRead(ctx context.Context, token string) (identity.ReadPrincipal, bool) {
	if p.lookupRead == nil {
		return identity.ReadPrincipal{}, false
	}
	sum := sha256.Sum256([]byte(token))
	digest := hex.EncodeToString(sum[:])
	now := p.now()

	p.mu.Lock()
	if entry, ok := p.readers[digest]; ok {
		if now.Before(entry.expires) {
			p.mu.Unlock()
			return cloneReadPrincipal(entry.principal), entry.allowed
		}
		delete(p.readers, digest)
	}
	if until, ok := p.deniedReaders[digest]; ok {
		if now.Before(until) {
			p.mu.Unlock()
			return identity.ReadPrincipal{}, false
		}
		delete(p.deniedReaders, digest)
	}
	if inflight, ok := p.flightReaders[digest]; ok {
		p.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			// This caller gave up; the leader still finishes and caches for the rest.
			return identity.ReadPrincipal{}, false
		}
		if inflight.err != nil || !inflight.trusted {
			return identity.ReadPrincipal{}, false
		}
		return cloneReadPrincipal(inflight.read), inflight.allowed
	}
	flight := &lookupFlight{done: make(chan struct{}), generation: p.generation}
	p.flightReaders[digest] = flight
	p.mu.Unlock()

	principal, allowed, err := p.runReadLookup(ctx, digest)
	if err != nil {
		p.log.Warn("control-plane read authorization lookup failed", "error", err)
	} else if allowed {
		principal, err = identity.NormalizeReadPrincipal(principal)
		if err != nil {
			p.log.Error("control-plane read authorization row rejected", "error", err)
		}
	}
	if err != nil {
		// A transport error or a malformed row is not an authoritative denial:
		// publish nothing and cache nothing, so the next attempt retries.
		principal, allowed = identity.ReadPrincipal{}, false
	}

	p.mu.Lock()
	delete(p.flightReaders, digest)
	trusted := p.generation == flight.generation
	if err == nil && trusted {
		if allowed {
			p.storeReadLocked(digest, principal, now.Add(p.ttl))
		} else {
			p.storeDeniedLocked(p.deniedReaders, digest, now.Add(p.negativeTTL))
		}
	}
	flight.read, flight.allowed, flight.trusted, flight.err = cloneReadPrincipal(principal), allowed, trusted, err
	p.mu.Unlock()
	close(flight.done)

	if err != nil || !trusted {
		return identity.ReadPrincipal{}, false
	}
	return cloneReadPrincipal(principal), allowed
}

// Authenticate implements ingest.Authenticator structurally without importing
// the ingest package. It fails closed on malformed control-plane rows or DB
// errors and returns a defensive copy of cached slice fields.
func (p *Provider) Authenticate(ctx context.Context, token, station, feed string) (identity.ObserverContext, bool) {
	sum := sha256.Sum256([]byte(token))
	key := observerCacheKey{tokenSHA256: hex.EncodeToString(sum[:]), station: station, feed: feed}
	folded := observerFoldedKey(key)
	now := p.now()

	p.mu.Lock()
	if entry, ok := p.observers[key]; ok {
		if now.Before(entry.expires) {
			p.mu.Unlock()
			return cloneObserverContext(entry.context), entry.allowed
		}
		delete(p.observers, key)
	}
	if until, ok := p.deniedObservers[folded]; ok {
		if now.Before(until) {
			p.mu.Unlock()
			return identity.ObserverContext{}, false
		}
		delete(p.deniedObservers, folded)
	}
	if inflight, ok := p.flightObservers[folded]; ok {
		p.mu.Unlock()
		select {
		case <-inflight.done:
		case <-ctx.Done():
			// This handshake gave up; the leader still finishes for the rest.
			return identity.ObserverContext{}, false
		}
		if inflight.err != nil || !inflight.trusted {
			return identity.ObserverContext{}, false
		}
		return cloneObserverContext(inflight.observer), inflight.allowed
	}
	flight := &lookupFlight{done: make(chan struct{}), generation: p.generation}
	p.flightObservers[folded] = flight
	p.mu.Unlock()

	resolved, allowed, err := p.runObserverLookup(ctx, key.tokenSHA256, station, feed)
	if err != nil {
		p.log.Warn("control-plane observer authorization lookup failed", "station", station, "feed", feed, "error", err)
	} else if allowed {
		resolved, err = resolved.Normalize()
		if err == nil && resolved.ObserverID != station {
			err = errors.New("resolved observer does not match presented station")
		}
		if err != nil {
			p.log.Error("control-plane observer authorization row rejected", "station", station, "feed", feed, "error", err)
		}
	}
	if err != nil {
		// A transport error or a malformed/mismatched row is not an authoritative
		// denial: publish nothing and cache nothing, so the next attempt retries.
		resolved, allowed = identity.ObserverContext{}, false
	}

	p.mu.Lock()
	delete(p.flightObservers, folded)
	// A committed revocation/policy change may have raced this SELECT. Its result
	// could be from the pre-change snapshot, so deny this attempt instead of
	// putting stale authority back after NOTIFY cleared the cache.
	trusted := p.generation == flight.generation
	if err == nil && trusted {
		if allowed {
			p.storeObserverLocked(key, resolved, now.Add(p.ttl))
		} else {
			p.storeDeniedLocked(p.deniedObservers, folded, now.Add(p.negativeTTL))
		}
	}
	flight.observer, flight.allowed, flight.trusted, flight.err = cloneObserverContext(resolved), allowed, trusted, err
	p.mu.Unlock()
	close(flight.done)

	if err != nil || !trusted {
		return identity.ObserverContext{}, false
	}
	return cloneObserverContext(resolved), allowed
}

// runReadLookup and runObserverLookup bound one control-plane query to the
// caller's deadline or defaultLookupTimeout, whichever is earlier,
// and hold one of the fixed lookupSlots for its duration so a stream of
// never-repeated credentials cannot open unbounded concurrent queries.
func (p *Provider) runReadLookup(ctx context.Context, digest string) (identity.ReadPrincipal, bool, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, p.lookupTimeout)
	defer cancel()
	release, err := p.acquireLookupSlot(lookupCtx)
	if err != nil {
		return identity.ReadPrincipal{}, false, err
	}
	defer release()
	principal, allowed, err := p.lookupRead(lookupCtx, digest)
	if err == nil {
		err = lookupCtx.Err()
	}
	return principal, allowed, err
}

func (p *Provider) runObserverLookup(ctx context.Context, digest, station, feed string) (identity.ObserverContext, bool, error) {
	lookupCtx, cancel := context.WithTimeout(ctx, p.lookupTimeout)
	defer cancel()
	release, err := p.acquireLookupSlot(lookupCtx)
	if err != nil {
		return identity.ObserverContext{}, false, err
	}
	defer release()
	resolved, allowed, err := p.lookupObserver(lookupCtx, digest, station, feed)
	if err == nil {
		err = lookupCtx.Err()
	}
	return resolved, allowed, err
}

// acquireLookupSlot waits for a query slot within the caller's already-bounded
// lookup context. Exhaustion is reported as an error, never as a denial, so it
// is fail-closed for this attempt but never cached as authority.
func (p *Provider) acquireLookupSlot(ctx context.Context) (func(), error) {
	if p.lookupSlots == nil {
		return func() {}, nil
	}
	select {
	case p.lookupSlots <- struct{}{}:
		return func() { <-p.lookupSlots }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("authorization lookup concurrency limit: %w", ctx.Err())
	}
}

func (p *Provider) storeReadLocked(digest string, principal identity.ReadPrincipal, expires time.Time) {
	p.pruneExpiredLocked(p.now())
	if len(p.readers) >= p.cacheLimit {
		// Any eviction is safe: it only forces a fresh, fail-closed lookup.
		for key := range p.readers {
			delete(p.readers, key)
			break
		}
	}
	p.readers[digest] = readCacheEntry{principal: cloneReadPrincipal(principal), allowed: true, expires: expires}
}

func (p *Provider) storeObserverLocked(key observerCacheKey, resolved identity.ObserverContext, expires time.Time) {
	p.pruneExpiredLocked(p.now())
	if len(p.observers) >= p.cacheLimit {
		for existing := range p.observers {
			delete(p.observers, existing)
			break
		}
	}
	p.observers[key] = observerCacheEntry{context: cloneObserverContext(resolved), allowed: true, expires: expires}
}

// storeDeniedLocked records an authoritative "no such grant" for negativeTTL.
// Unlike the positive path it prunes only when the budget is actually reached:
// this is the attacker-reachable branch, and an O(n) sweep on every denied
// request would be its own amplifier. The periodic sweeper handles the rest.
func (p *Provider) storeDeniedLocked(cache map[string]time.Time, key string, expires time.Time) {
	if len(cache) >= p.negativeLimit {
		now := p.now()
		for existing, until := range cache {
			if !now.Before(until) {
				delete(cache, existing)
			}
		}
	}
	if len(cache) >= p.negativeLimit {
		// Evicting a denial only costs one extra lookup for that credential.
		for existing := range cache {
			delete(cache, existing)
			break
		}
	}
	cache[key] = expires
}

func (p *Provider) lookupObserverDatabase(ctx context.Context, tokenSHA256, station, feed string) (identity.ObserverContext, bool, error) {
	const query = `SELECT observer_id, organization_id, enrollment_id, collector_instance_id,
	   collection_ids, feed_grants, declared_capabilities,
	   credential_tier, credential_fingerprint, attestation_tier,
       aggregate_use, station_metadata, event_visibility, raw_export,
       federation_peers, publish_signals, policy_revision
  FROM ` + observerAuthorizationView + `
 WHERE token_sha256 = $1 AND observer_id = $2 AND $3 = ANY(feed_grants) AND enabled`
	rows, err := p.pool.Query(ctx, query, tokenSHA256, station, feed)
	if err != nil {
		return identity.ObserverContext{}, false, err
	}
	defer rows.Close()
	var (
		resolved             identity.ObserverContext
		declaredCapabilities []string
		publishSignals       []string
		count                int
	)
	for rows.Next() {
		count++
		if count > 1 {
			return identity.ObserverContext{}, false, errors.New("duplicate active observer authorization rows")
		}
		if err := rows.Scan(
			&resolved.ObserverID, &resolved.OrganizationID, &resolved.EnrollmentID, &resolved.CollectorInstanceID,
			&resolved.CollectionIDs, &resolved.FeedGrants, &declaredCapabilities,
			&resolved.CredentialTier, &resolved.CredentialFingerprint, &resolved.AttestationTier,
			&resolved.Publication.AggregateUse, &resolved.Publication.StationMetadata,
			&resolved.Publication.EventVisibility, &resolved.Publication.RawExport,
			&resolved.Publication.FederationPeers, &publishSignals, &resolved.Publication.Revision,
		); err != nil {
			return identity.ObserverContext{}, false, err
		}
	}
	if err := rows.Err(); err != nil {
		return identity.ObserverContext{}, false, err
	}
	if count == 0 {
		return identity.ObserverContext{}, false, nil
	}
	parsed, err := parseSignals(publishSignals)
	if err != nil {
		return identity.ObserverContext{}, false, err
	}
	resolved.Publication.Signals = parsed
	resolved.DeclaredCapabilities, err = parseSignals(declaredCapabilities)
	if err != nil {
		return identity.ObserverContext{}, false, err
	}
	return resolved, true, nil
}

// ReconcileObserver bypasses caches for retained contributors, even while the
// device is offline. Only digests from successful admissions reach this method.
func (p *Provider) ReconcileObserver(ctx context.Context, digest, station, feed string) (identity.ObserverContext, bool) {
	ctx, cancel := context.WithTimeout(ctx, p.lookupTimeout)
	defer cancel()
	p.mu.Lock()
	generation := p.generation
	p.mu.Unlock()
	current, allowed, err := p.lookupObserver(ctx, digest, station, feed)
	if err != nil || !allowed || ctx.Err() != nil {
		return identity.ObserverContext{}, false
	}
	current, err = current.Normalize()
	p.mu.Lock()
	unchanged := generation == p.generation
	p.mu.Unlock()
	if err != nil || !unchanged || current.ObserverID != station {
		return identity.ObserverContext{}, false
	}
	return current, true
}

func (p *Provider) lookupReadDatabase(ctx context.Context, tokenSHA256 string) (identity.ReadPrincipal, bool, error) {
	const query = `SELECT principal_id, audience_grants, revision
  FROM ` + readAuthorizationView + `
 WHERE token_sha256 = $1 AND enabled`
	rows, err := p.pool.Query(ctx, query, tokenSHA256)
	if err != nil {
		return identity.ReadPrincipal{}, false, err
	}
	defer rows.Close()
	var (
		principal identity.ReadPrincipal
		rawGrants []string
		count     int
	)
	for rows.Next() {
		count++
		if count > 1 {
			return identity.ReadPrincipal{}, false, errors.New("duplicate active read authorization rows")
		}
		if err := rows.Scan(&principal.ID, &rawGrants, &principal.Revision); err != nil {
			return identity.ReadPrincipal{}, false, err
		}
	}
	if err := rows.Err(); err != nil {
		return identity.ReadPrincipal{}, false, err
	}
	if count == 0 {
		return identity.ReadPrincipal{}, false, nil
	}
	for _, raw := range rawGrants {
		grant, err := identity.ParseAudience(raw)
		if err != nil {
			return identity.ReadPrincipal{}, false, err
		}
		principal.AudienceGrants = append(principal.AudienceGrants, grant)
	}
	return principal, true, nil
}

func parseSignals(raw []string) ([]identity.Signal, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]identity.Signal, 0, len(raw))
	for _, value := range raw {
		gnss, sig, ok := strings.Cut(value, ":")
		if !ok {
			return nil, fmt.Errorf("publication signal %q: want gnss:sig", value)
		}
		gnssID, err := strconv.Atoi(gnss)
		if err != nil {
			return nil, fmt.Errorf("publication signal %q: %w", value, err)
		}
		sigID, err := strconv.Atoi(sig)
		if err != nil {
			return nil, fmt.Errorf("publication signal %q: %w", value, err)
		}
		out = append(out, identity.Signal{GnssID: gnssID, SigID: sigID})
	}
	return out, nil
}

func cloneObserverContext(in identity.ObserverContext) identity.ObserverContext {
	out := in
	out.CollectionIDs = append([]string(nil), in.CollectionIDs...)
	out.FeedGrants = append([]string(nil), in.FeedGrants...)
	out.DeclaredCapabilities = append([]identity.Signal(nil), in.DeclaredCapabilities...)
	out.Publication.FederationPeers = append([]string(nil), in.Publication.FederationPeers...)
	out.Publication.Signals = append([]identity.Signal(nil), in.Publication.Signals...)
	return out
}

func cloneReadPrincipal(in identity.ReadPrincipal) identity.ReadPrincipal {
	out := in
	out.AudienceGrants = append([]identity.Audience(nil), in.AudienceGrants...)
	return out
}

// InvalidateAll clears positive and negative authorization caches.
func (p *Provider) InvalidateAll() {
	p.mu.Lock()
	clear(p.observers)
	clear(p.readers)
	// Denials are cleared too: a grant issued moments ago must not stay shadowed
	// by a negative entry recorded before it existed. In-flight lookups need no
	// handling here — the generation bump already makes their results untrusted.
	clear(p.deniedObservers)
	clear(p.deniedReaders)
	p.generation++
	p.mu.Unlock()
}

// RunInvalidation listens for control-plane changes. Reconnect always clears
// the cache because notifications may have been missed while disconnected.
// Connection loss is non-fatal: TTL continues to bound stale authority.
func (p *Provider) RunInvalidation(ctx context.Context) {
	// Expiry must not depend on NOTIFY traffic, database availability, or a
	// client reusing a particular token. Join the sweeper on normal shutdown.
	sweepCtx, stopSweep := context.WithCancel(ctx)
	swept := make(chan struct{})
	go func() { defer close(swept); p.runCacheExpiry(sweepCtx) }()
	defer func() { stopSweep(); <-swept }()
	if p.pool == nil {
		<-ctx.Done()
		return
	}
	backoff := listenBackoffInitial
	listenSQL := "LISTEN " + pgx.Identifier{changeNotifyChannel}.Sanitize()
	for ctx.Err() == nil {
		conn, err := p.pool.Acquire(ctx)
		healthy := false
		if err == nil {
			p.InvalidateAll()
			_, err = conn.Exec(ctx, listenSQL)
			listenedAt := p.now()
			for err == nil && ctx.Err() == nil {
				_, err = conn.Conn().WaitForNotification(ctx)
				if err == nil {
					// A delivered notification is proof the listener worked.
					healthy = true
					p.InvalidateAll()
				}
			}
			// a LISTEN that then held for listenHealthyAfter is
			// equally good proof — a quiet control plane delivers no notifications
			// for hours, and treating that as "never healthy" is what let one early
			// outage pin the backoff at its cap for the life of the process.
			if !healthy && err != nil && p.now().Sub(listenedAt) >= listenHealthyAfter {
				healthy = true
			}
			conn.Release()
		}
		if ctx.Err() != nil {
			return
		}
		if healthy {
			// Reset only after a demonstrated healthy connection, never merely
			// because Acquire returned: an accept-then-close database would
			// otherwise be retried forever at the initial interval.
			backoff = listenBackoffInitial
		}
		wait := p.jitter(backoff)
		p.log.Warn("authorization invalidation listener reconnecting", "error", err, "backoff", wait)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < listenBackoffMax {
			backoff *= 2
			if backoff > listenBackoffMax {
				backoff = listenBackoffMax
			}
		}
	}
}

// jitter spreads a fleet's reconnect attempts across up to ±25% of the interval,
// so every collector that lost the same control-plane database does not retry in
// lockstep. It never returns a non-positive duration.
func (p *Provider) jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Millisecond
	}
	spread := int64(d) / 2 // the full ±25% band
	if spread <= 0 {
		return d
	}
	offset := rand.Int64N(spread) - spread/2
	out := d + time.Duration(offset)
	if out <= 0 {
		return time.Millisecond
	}
	return out
}

func (p *Provider) pruneExpiredLocked(now time.Time) {
	for key, entry := range p.readers {
		if !now.Before(entry.expires) {
			delete(p.readers, key)
		}
	}
	for key, entry := range p.observers {
		if !now.Before(entry.expires) {
			delete(p.observers, key)
		}
	}
	for key, until := range p.deniedReaders {
		if !now.Before(until) {
			delete(p.deniedReaders, key)
		}
	}
	for key, until := range p.deniedObservers {
		if !now.Before(until) {
			delete(p.deniedObservers, key)
		}
	}
}

func (p *Provider) runCacheExpiry(ctx context.Context) {
	ticker := time.NewTicker(min(p.ttl, time.Second))
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.mu.Lock()
			p.pruneExpiredLocked(p.now())
			p.mu.Unlock()
		}
	}
}

// Close releases the control-plane connection pool.
func (p *Provider) Close() {
	if p.pool != nil {
		p.pool.Close()
	}
}
