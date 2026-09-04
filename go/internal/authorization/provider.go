// Package authorization resolves server-owned ingest and read grants from the
// control plane. Presented bearer tokens are hashed before lookup/cache keys;
// plaintext credentials are never retained by this package.
package authorization

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
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
	// Per-method positive-entry budget; denied results are deliberately uncached.
	// Admission is bounded even if requests cycle through never-reused tokens.
	defaultCacheLimit = 1024
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

	mu         sync.Mutex
	observers  map[observerCacheKey]observerCacheEntry
	readers    map[string]readCacheEntry
	generation uint64
	cacheLimit int
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
	return &Provider{
		ttl: ttl, now: time.Now, log: log, lookupObserver: lookup,
		observers:  make(map[observerCacheKey]observerCacheEntry),
		readers:    make(map[string]readCacheEntry),
		cacheLimit: defaultCacheLimit,
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
	entry, ok := p.readers[digest]
	if ok && now.Before(entry.expires) {
		p.mu.Unlock()
		return cloneReadPrincipal(entry.principal), entry.allowed
	}
	if ok {
		delete(p.readers, digest)
	}
	lookupGeneration := p.generation
	p.mu.Unlock()

	principal, allowed, err := p.lookupRead(ctx, digest)
	if err != nil {
		p.log.Warn("control-plane read authorization lookup failed", "error", err)
		return identity.ReadPrincipal{}, false
	}
	if allowed {
		principal, err = identity.NormalizeReadPrincipal(principal)
		if err != nil {
			p.log.Error("control-plane read authorization row rejected", "error", err)
			return identity.ReadPrincipal{}, false
		}
	}
	entry = readCacheEntry{principal: cloneReadPrincipal(principal), allowed: allowed, expires: now.Add(p.ttl)}
	p.mu.Lock()
	if p.generation != lookupGeneration {
		p.mu.Unlock()
		return identity.ReadPrincipal{}, false
	}
	if allowed {
		p.pruneExpiredLocked(p.now())
		if len(p.readers) >= p.cacheLimit {
			// Any eviction is safe: it only forces a fresh, fail-closed lookup.
			for key := range p.readers {
				delete(p.readers, key)
				break
			}
		}
		p.readers[digest] = entry
	}
	p.mu.Unlock()
	return cloneReadPrincipal(principal), allowed
}

// Authenticate implements ingest.Authenticator structurally without importing
// the ingest package. It fails closed on malformed control-plane rows or DB
// errors and returns a defensive copy of cached slice fields.
func (p *Provider) Authenticate(ctx context.Context, token, station, feed string) (identity.ObserverContext, bool) {
	sum := sha256.Sum256([]byte(token))
	key := observerCacheKey{tokenSHA256: hex.EncodeToString(sum[:]), station: station, feed: feed}
	now := p.now()
	p.mu.Lock()
	entry, ok := p.observers[key]
	if ok && now.Before(entry.expires) {
		p.mu.Unlock()
		return cloneObserverContext(entry.context), entry.allowed
	}
	if ok {
		delete(p.observers, key)
	}
	lookupGeneration := p.generation
	p.mu.Unlock()

	resolved, allowed, err := p.lookupObserver(ctx, key.tokenSHA256, station, feed)
	if err != nil {
		p.log.Warn("control-plane observer authorization lookup failed", "station", station, "feed", feed, "error", err)
		return identity.ObserverContext{}, false
	}
	if allowed {
		resolved, err = resolved.Normalize()
		if err != nil || resolved.ObserverID != station {
			if err == nil {
				err = errors.New("resolved observer does not match presented station")
			}
			p.log.Error("control-plane observer authorization row rejected", "station", station, "feed", feed, "error", err)
			return identity.ObserverContext{}, false
		}
	}
	entry = observerCacheEntry{context: cloneObserverContext(resolved), allowed: allowed, expires: now.Add(p.ttl)}
	p.mu.Lock()
	if p.generation != lookupGeneration {
		// A committed revocation/policy change raced this SELECT. Its result may
		// be from the pre-change snapshot; deny this attempt instead of putting
		// stale authority back after NOTIFY cleared the cache.
		p.mu.Unlock()
		return identity.ObserverContext{}, false
	}
	if allowed {
		p.pruneExpiredLocked(p.now())
		if len(p.observers) >= p.cacheLimit {
			for key := range p.observers {
				delete(p.observers, key)
				break
			}
		}
		p.observers[key] = entry
	}
	p.mu.Unlock()
	return cloneObserverContext(resolved), allowed
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
	backoff := 250 * time.Millisecond
	listenSQL := "LISTEN " + pgx.Identifier{changeNotifyChannel}.Sanitize()
	for ctx.Err() == nil {
		conn, err := p.pool.Acquire(ctx)
		if err == nil {
			p.InvalidateAll()
			_, err = conn.Exec(ctx, listenSQL)
			for err == nil && ctx.Err() == nil {
				_, err = conn.Conn().WaitForNotification(ctx)
				if err == nil {
					p.InvalidateAll()
				}
			}
			conn.Release()
		}
		if ctx.Err() != nil {
			return
		}
		p.log.Warn("authorization invalidation listener reconnecting", "error", err, "backoff", backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if backoff < 5*time.Second {
			backoff *= 2
			if backoff > 5*time.Second {
				backoff = 5 * time.Second
			}
		}
	}
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
