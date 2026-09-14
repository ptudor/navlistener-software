// Package config loads the navlistener TOML configuration.
//
// Config is TOML at /usr/local/etc/navlistener/navlistener.toml (never .env),
// passed via the -config flag. It configures the complete collector pipeline:
// dial and push ingest, live state, observability, persistence, and the native
// read API. Optional stages stay dormant when their enabling address or DSN is
// empty (docs/DESIGN.md §5).
package config

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	toml "github.com/pelletier/go-toml/v2"
	"github.com/ptudor/navlistener/internal/federation"
	"github.com/ptudor/navlistener/internal/identity"
)

// IntervalRe is the simple-interval allowlist for store.Store's raw_retention and
// compress_after ("7 days", "1 hour", …). It is the sole definition (store.go
// references this one, regression fix) — it also doubles as the injection guard for the
// interval string's later interpolation into policy DDL, so it must never be
// relaxed.
var IntervalRe = regexp.MustCompile(`^[1-9][0-9]* (minute|hour|day|week)s?$`)

// ntripMountpointRe is the allowlist for an ntrip [[ingest]] source's Mountpoint
// : printable ASCII with no whitespace or control characters --
// NTRIP mountpoints are token-like (RTCM/NTRIP casters use short alphanumeric +
// punctuation names). Mountpoint is interpolated directly into the caster's
// HTTP request line (ntrip.go's `GET /%s HTTP/1.1`), so a space, \r, or \n in a
// fat-fingered or copy-pasted value would mangle the request line or inject
// headers toward the caster.
var ntripMountpointRe = regexp.MustCompile(`^[!-~]+$`)

// Sizing ceilings : loud range errors instead of a boot-time surprise
// — state.New eagerly allocates a map+mutex per shard, so a fat-fingered
// `shards = 1000000` allocated a million maps with no -check-config diagnostic;
// batch_size/max_conns were similarly unbounded upward. Values sit far above
// any sane deployment while still catching a pasted extra digit.
const (
	maxShards    = 4096
	maxBatchSize = 1_000_000
	maxPushConns = 65535
)

// DefaultPaths are searched in order when -config is not given.
var DefaultPaths = []string{
	"/usr/local/etc/navlistener/navlistener.toml",
	"/etc/navlistener/navlistener.toml",
	"./navlistener.toml",
}

// knownIngestTypes are the raw-frame message formats a feeder may push (the push
// feed grant, docs/CONSTELLATIONS.md §2). sbf is deliberately absent : GNF1's
// 1-byte frame_type field cannot carry an SBF block number (dial-mode sbf, decoded
// directly by the receiver's own connector, is unaffected -- see knownDialTypes
// below). Re-add it once a block-number carriage is defined.
var knownIngestTypes = map[string]bool{
	"ubx":  true, // u-blox UBX-RXM-SFRBX stream
	"rtcm": true, // RTCM3 ephemeris / SSR
}

// knownDialTypes are the connector types a dial [[ingest]] source may use: the raw-frame
// formats above plus ntrip, which is an RTCM3 transport (a caster spoken over HTTP), not a
// push feed grant — hence a separate set (a feeder can't push "ntrip").
var knownDialTypes = map[string]bool{
	"ubx":   true,
	"sbf":   true,
	"rtcm":  true,
	"ntrip": true, // RTCM3 over an NTRIP caster (host:port + mountpoint + basic auth)
}

// Config is the whole-daemon configuration.
type Config struct {
	Collector     Collector     `toml:"collector"`
	Logging       Logging       `toml:"logging"`
	Metrics       Metrics       `toml:"metrics"`
	State         State         `toml:"state"`
	Store         Store         `toml:"store"`
	Serve         Serve         `toml:"serve"`
	Push          Push          `toml:"push"`
	Authorization Authorization `toml:"authorization"`
	Federation    Federation    `toml:"federation"`
	Ingest        []Source      `toml:"ingest"`

	// ShutdownTimeout bounds graceful shutdown; kept out of the wire format.
	ShutdownTimeout time.Duration `toml:"-"`

	// Warnings are non-fatal config findings surfaced at startup and by
	// -check-config : a group/world-readable config holding
	// credentials, a non-loopback bind of an unauthenticated surface. Warnings,
	// not errors, because each has a legitimate deliberate mode (a secret-less
	// dev config; remote Prometheus scraping behind a firewall) — but never a
	// silent one. Populated by Load/finalize.
	Warnings []string `toml:"-"`
}

// Collector names this deployment's stable trust/authorization realm. It is
// deliberately process configuration, not an observer- or request-supplied id.
type Collector struct {
	InstanceID string `toml:"instance_id"`
}

// Serve is the native v2 read API listener (docs/OUTPUT.md). It binds loopback and
// is fronted by a TLS reverse proxy; it is enabled only when an addr is given, so
// the daemon can run collector-only. The refresh cadences default per §5.
type Serve struct {
	Addr string `toml:"addr"` // e.g. 127.0.0.1:8080; empty = serve disabled
	// Audience selects the one materialized view this unauthenticated listener
	// serves. "public" is the fail-closed default. "operator" exposes the local
	// all-source view and therefore requires an authenticated private front.
	// Organization/collection selection belongs to the read-auth layer and is
	// intentionally not accepted as a free-form config/query value here.
	Audience        string            `toml:"audience"`
	AudienceContext identity.Audience `toml:"-"`

	RefreshFasts string        `toml:"refresh_interval"` // svs/global/observers/sbas, default "30s"
	RefreshFast  time.Duration `toml:"-"`
	RefreshSlows string        `toml:"almanac_refresh_interval"` // almanac, default "90s"
	RefreshSlow  time.Duration `toml:"-"`
	// SnapshotEvery is the cadence at which each served feed's current body is persisted
	// to the historian as a replay/backfill record (docs/OUTPUT.md §4). Only active when
	// both [serve].addr and [store].dsn are set. Default "5m"; "0s" disables.
	SnapshotEverys string        `toml:"snapshot_interval"`
	SnapshotEvery  time.Duration `toml:"-"`

	// Principals are standalone/bootstrap read grants. Production uses the
	// versioned DB view instead; the two modes are mutually exclusive.
	Principals []ServePrincipal `toml:"principal"`
}

type ServePrincipal struct {
	ID          string                 `toml:"id"`
	TokenSHA256 string                 `toml:"token_sha256"`
	Audiences   []string               `toml:"audiences"`
	Revision    string                 `toml:"revision"`
	Principal   identity.ReadPrincipal `toml:"-"`
}

// Store is the TimescaleDB raw-nav-frame historian (docs/OUTPUT.md §4). It is
// enabled only when a DSN is given, so the daemon can run persist-less in dev; when
// enabled, TimescaleDB is required (a missing extension is fatal). Real credentials
// live only in the deployed, git-ignored config.
type Store struct {
	DSN string `toml:"dsn"` // pgx DSN; empty = persist disabled

	BatchSize   int           `toml:"batch_size"`     // rows per CopyFrom (default 1000)
	BatchEverys string        `toml:"batch_interval"` // flush cadence, e.g. "1s"
	BatchEvery  time.Duration `toml:"-"`

	// Retention (PostgreSQL INTERVAL literals). Raw frames are the short-window
	// forensic record. Confirmed events and feed snapshots live in separate
	// tables/policies; this setting does not retain a long-term decoded
	// ephemeris aggregate.
	RawRetention string `toml:"raw_retention"` // default "7 days"
	// CompressAfter is when a raw chunk is columnar-compressed (default "1 day").
	CompressAfter string `toml:"compress_after"`
}

// Authorization selects the shared, read-only ingest control-plane provider. When DSN
// is set, static push credential rows are not a fallback. CacheTTL bounds
// stale authority if NOTIFY is unavailable; RecheckEvery bounds active-feeder-session
// closure after cache invalidation/expiry.
type Authorization struct {
	DSN           string        `toml:"dsn"`
	CacheTTLs     string        `toml:"cache_ttl"`
	CacheTTL      time.Duration `toml:"-"`
	RecheckEverys string        `toml:"session_recheck_interval"`
	RecheckEvery  time.Duration `toml:"-"`
}

// Push is the authenticated GNF1 fleet push endpoint (docs/DESIGN.md §1/§2): the
// production ingest path where navfeeder edge feeders connect out to us over TLS. It
// is enabled only when addr is set; TLS is mandatory when it is. Observers
// authenticate with a bearer token whose SHA-256 is stored here (the token itself is
// shown once at enrollment and never committed).
type Push struct {
	Addr    string `toml:"addr"`     // e.g. 0.0.0.0:5580; empty = push disabled
	TLSCert string `toml:"tls_cert"` // server certificate (PEM)
	TLSKey  string `toml:"tls_key"`  // server private key (PEM)
	// ClientCA, when set, enables mTLS: connecting feeders must present a client
	// certificate signed by this CA (the software/ATECC cert tiers). The bearer
	// token stays the bootstrap tier and is always checked.
	ClientCA string `toml:"client_ca"`

	AckIntervals string        `toml:"ack_interval"` // ack cadence, default "1s"
	AckInterval  time.Duration `toml:"-"`

	// MaxConns bounds concurrent in-flight feeder connections : production
	// should gate admission at the TLS layer via ClientCA (mTLS), but when it isn't
	// set this is the only cap between the internet and unbounded goroutine/FD
	// growth. Default 512 — a generous multiple of any fleet size in this project.
	MaxConns int `toml:"max_conns"`

	Observers []PushObserver `toml:"observer"`
}

// Federation contains outbound authorization rows only. Defining a grant does
// not start a peer transport; it makes the safety gate deployable/testable
// before transport exists, as required by GROUPS-AND-FEDERATION stage 8.
type Federation struct {
	ExportGrants []FederationExportGrant `toml:"export_grant"`
}

// FederationExportGrant is the TOML form of federation.ExportGrant. Durations
// and timestamps stay strings until finalize so -check-config reports precise
// field errors and the runtime evaluator sees typed values only.
type FederationExportGrant struct {
	SourceCollector string   `toml:"source_collector"`
	DestinationPeer string   `toml:"destination_peer"`
	Organizations   []string `toml:"organizations,omitempty"`
	Collections     []string `toml:"collections,omitempty"`
	Observers       []string `toml:"observers,omitempty"`
	Signals         []string `toml:"signals,omitempty"`
	DataClasses     []string `toml:"data_classes"`
	Attribution     string   `toml:"attribution"`
	MaxRetentions   string   `toml:"max_retention"`
	Purposes        []string `toml:"purposes"`
	ValidFroms      string   `toml:"valid_from"`
	ValidUntils     string   `toml:"valid_until"`
	ApprovedBy      string   `toml:"approved_by"`
	Revision        string   `toml:"revision"`
	Enabled         bool     `toml:"enabled"`

	Grant federation.ExportGrant `toml:"-"`
}

// PushObserver is one enrolled edge feeder's credential and feed grant. TokenSHA256
// is the hex-encoded SHA-256 of the bearer token; Feeds is the allow-list of feed
// types this observer may push (the as-built Device.feed_types grant, DESIGN §3).
type PushObserver struct {
	Station     string   `toml:"station"`
	TokenSHA256 string   `toml:"token_sha256"`
	Feeds       []string `toml:"feeds"`

	// Config-backed bootstrap form of the server-resolved administrative and
	// publication context (docs/GROUPS-AND-FEDERATION.md §5.1). Omitted values
	// fail closed to local-unassigned/private. Production replaces this provider
	// with shared AAA rows; static config can never claim hardware attestation.
	OrganizationID      string                   `toml:"organization,omitempty"`
	EnrollmentID        string                   `toml:"enrollment,omitempty"`
	CollectorInstanceID string                   `toml:"collector_instance,omitempty"`
	CollectionIDs       []string                 `toml:"collections,omitempty"`
	AggregateUse        string                   `toml:"aggregate_use,omitempty"`
	StationMetadata     string                   `toml:"station_metadata,omitempty"`
	EventVisibility     string                   `toml:"event_visibility,omitempty"`
	RawExport           string                   `toml:"raw_export,omitempty"`
	FederationPeers     []string                 `toml:"federation_peers,omitempty"`
	PublishSignals      []string                 `toml:"publish_signals,omitempty"`
	PolicyRevision      string                   `toml:"policy_revision,omitempty"`
	ObserverContext     identity.ObserverContext `toml:"-"`

	// Capabilities is the declared tudorgps fingerprint ("gnss:sig" list), as on Source.
	Capabilities []string     `toml:"capabilities,omitempty"`
	CapDecl      []Capability `toml:"-"`
}

// Logging selects level and format for log/slog.
type Logging struct {
	Level  string `toml:"level"`  // debug | info | warn | error
	Format string `toml:"format"` // json | text
}

// Metrics is the Prometheus /metrics + /healthz listener (observability only),
// loopback-bound and entirely separate from any app-facing read path.
type Metrics struct {
	Addr string `toml:"addr"` // e.g. 127.0.0.1:9100
}

// State tunes the in-RAM per-SV live store (the sharded ephemeris store).
type State struct {
	Shards int `toml:"shards"` // number of SV-map shards; >= 1

	// SVTTLs is how long an SV is kept after any receiver last reported it,
	// before it is expired from live state. Parsed into SVTTL.
	SVTTLs string        `toml:"sv_ttl"`
	SVTTL  time.Duration `toml:"-"`

	// PropagateEverys is the cadence at which live SVs are re-propagated to
	// "now" for the debug snapshot / metrics. Parsed into PropagateEvery.
	PropagateEverys string        `toml:"propagate_interval"`
	PropagateEvery  time.Duration `toml:"-"`

	// LeapSeconds is ΔtLS (GPS−UTC), used to convert wall-clock time to GPS/BDT
	// time-of-week for propagation and served in the global feed's leap_seconds
	//. 0 (the default) means "use the compiled-in current value" — the
	// design mandates "transcribe, don't invent." BeiDou B-CNAV2 UTC parameters
	// are decoded and cross-checked, but the daemon does not derive a fleet-wide
	// leap-second consensus or automatically replace this process-wide value.
	// Set it explicitly after a leap event to avoid shifting every
	// wall-clock→GNSS conversion by 1s (~3.9 km) until a rebuild.
	LeapSeconds int `toml:"leap_seconds"`
}

// Source is one dial-out raw-frame ingest connector to a receiver or caster.
// Authenticated fleet push is configured separately under [push] because its
// observers connect inbound and have credentials/feed grants rather than dial
// addresses (docs/DESIGN.md §1). Type selects the wire parser; Addr is the
// host:port to dial.
type Source struct {
	Name        string `toml:"name"`
	Type        string `toml:"type"`                   // ubx | sbf | rtcm
	Addr        string `toml:"addr"`                   // host:port to dial
	Disabled    bool   `toml:"disabled,omitempty"`     // keep the entry but don't start it (pause)
	CaptureOnly bool   `toml:"capture_only,omitempty"` // required for byte sources until live decoders land

	// Remark is an operator-supplied free-form station note (docs/OUTPUT.md §1.3):
	// the ONLY thing that reaches the public observers feed's remark field
	//. Addr is the internal LAN dial target the collector connects
	// to -- publishing it would disclose network topology and the exact
	// host:port of an unauthenticated raw receiver TCP stream. Empty by default.
	Remark string `toml:"remark,omitempty"`

	// Dial sources do not authenticate through HELLO, but they still carry the
	// same immutable ownership/publication stamp as push observers. Defaults are
	// deliberately local-unassigned/private; public use must be explicit.
	OrganizationID      string                   `toml:"organization,omitempty"`
	EnrollmentID        string                   `toml:"enrollment,omitempty"`
	CollectorInstanceID string                   `toml:"collector_instance,omitempty"`
	CollectionIDs       []string                 `toml:"collections,omitempty"`
	AggregateUse        string                   `toml:"aggregate_use,omitempty"`
	StationMetadata     string                   `toml:"station_metadata,omitempty"`
	EventVisibility     string                   `toml:"event_visibility,omitempty"`
	RawExport           string                   `toml:"raw_export,omitempty"`
	FederationPeers     []string                 `toml:"federation_peers,omitempty"`
	PublishSignals      []string                 `toml:"publish_signals,omitempty"`
	PolicyRevision      string                   `toml:"policy_revision,omitempty"`
	ObserverContext     identity.ObserverContext `toml:"-"`

	// Capabilities is this node's declared tudorgps fingerprint: the "gnss:sig" signals its
	// silicon can produce (docs/CONSTELLATIONS.md §7). The integrity layer compares it against
	// what the node actually delivers — a declared signal that goes silent, or an observed
	// signal the silicon can't produce, is a threat (docs/INTEGRITY.md §6). Parsed into CapDecl.
	Capabilities []string     `toml:"capabilities,omitempty"`
	CapDecl      []Capability `toml:"-"`

	// MaxFrameSilences bounds how long a connected source may keep delivering bytes
	// without a single decodable frame before the connection is torn down and
	// re-dialed. The idle timeout covers byte silence (half-open peer);
	// this covers the chatter-but-no-frames variant — an F9 reset to factory-default
	// NMEA output, a mis-pointed TCP port, a caster streaming an HTML error page —
	// which otherwise reports SourceUp=1 with FramesTotal frozen, indefinitely.
	// Default 5m: the slowest legitimate cadence on any dial source is RTCM
	// ephemeris messages tens of seconds apart (the dialIdleTimeout rationale), so
	// 5 minutes is a ~10x margin, and a false trip costs one logged reconnect.
	// Parsed into MaxFrameSilence.
	MaxFrameSilences string        `toml:"max_frame_silence,omitempty"`
	MaxFrameSilence  time.Duration `toml:"-"`

	// NTRIP transport (type = "ntrip"): Addr is the caster host:port, Mountpoint is the stream
	// to subscribe, and Username/Password are the basic-auth credentials. Real credentials live
	// only in the deployed, git-ignored config — never in a committed example.
	Mountpoint                string `toml:"mountpoint,omitempty"`
	Username                  string `toml:"username,omitempty"`
	Password                  string `toml:"password,omitempty"`
	NTRIPCAFile               string `toml:"ca_file,omitempty"`
	NTRIPServerName           string `toml:"server_name,omitempty"`
	AllowInsecurePlaintext    bool   `toml:"allow_insecure_plaintext,omitempty"`
	AllowPlaintextCredentials bool   `toml:"allow_plaintext_credentials,omitempty"`
}

// Capability is one declared (gnssId, sigId) an observer's silicon can produce.
type Capability struct {
	Gnss int
	Sig  int
}

// Load reads the config from an explicit path, or the first existing default path.
func Load(path string) (*Config, error) {
	if path == "" {
		for _, p := range DefaultPaths {
			if _, err := os.Stat(p); err == nil {
				path = p
				break
			}
		}
		if path == "" {
			return nil, fmt.Errorf("no config given and none found in %v", DefaultPaths)
		}
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	cfg := defaults()
	decoder := toml.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	if err := cfg.finalize(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	cfg.addPermissionWarnings(path)
	return cfg, nil
}

// addPermissionWarnings warns when the config file itself is group/world-
// readable while holding credentials : store.dsn embeds the DB
// password, and an ntrip source may carry basic-auth credentials. A warning
// rather than an error — a credential-less file may be shared harmlessly, and
// existing deployments must not be bricked by a mode bit — but the docs' 0600
// discipline stops being convention-only. (push.tls_key, actual private-key
// material, IS a hard error — finalizePush.)
func (c *Config) addPermissionWarnings(path string) {
	if !c.holdsSecrets() {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return // Load already read the file; a racing stat failure is not a config finding
	}
	if m := fi.Mode().Perm(); m&0o077 != 0 {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"config %s holds credentials (store/authorization DSN or ntrip username/password) but is group/world-readable (mode %04o) — chmod 600 it", path, m))
	}
}

// holdsSecrets reports whether the config carries credential material in
// cleartext. Any non-empty DSN counts (conservative: most embed a password);
// push token hashes are SHA-256 digests, not secrets, and are excluded.
func (c *Config) holdsSecrets() bool {
	if c.Store.DSN != "" || c.Authorization.DSN != "" {
		return true
	}
	for _, s := range c.Ingest {
		if s.Username != "" || s.Password != "" {
			return true
		}
	}
	return false
}

func defaults() *Config {
	return &Config{
		Collector:       Collector{InstanceID: identity.LocalCollectorInstance},
		Logging:         Logging{Level: "info", Format: "json"},
		Metrics:         Metrics{Addr: "127.0.0.1:9100"},
		State:           State{Shards: 16, SVTTLs: "2h", PropagateEverys: "1s"},
		Store:           Store{BatchSize: 1000, BatchEverys: "1s", RawRetention: "7 days", CompressAfter: "1 day"},
		Authorization:   Authorization{CacheTTLs: "30s", RecheckEverys: "10s"},
		Serve:           Serve{Audience: "public"},
		Push:            Push{MaxConns: 512},
		ShutdownTimeout: 15 * time.Second,
	}
}

// finalize parses duration strings and validates required fields. Validation is
// strict and fatal: an unknown connector type or a duplicate source name is a
// configuration error, not a warning.
func (c *Config) finalize() error {
	if !identity.ValidScopeID(c.Collector.InstanceID) {
		return fmt.Errorf("collector.instance_id %q is invalid", c.Collector.InstanceID)
	}
	switch c.Logging.Format {
	case "", "json", "text":
	default:
		return fmt.Errorf("logging.format %q: want json or text", c.Logging.Format)
	}
	switch c.Logging.Level {
	case "", "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("logging.level %q: want debug, info, warn, or error", c.Logging.Level)
	}
	// a malformed addr must fail at -check-config / config-load time, not only
	// once the listener actually tries (and fails) to bind at startup.
	if err := validateAddr("metrics.addr", c.Metrics.Addr); err != nil {
		return err
	}
	if c.Serve.Addr != "" {
		if err := validateAddr("serve.addr", c.Serve.Addr); err != nil {
			return err
		}
	}
	// "binds loopback" was convention only. A public bind of these
	// surfaces exposes unauthenticated endpoints — metrics carries /debug/state
	// (full live state + source names) and serve carries the events query API
	// (per-request DB work) and the SSE stream. Warn rather than fail: remote
	// scraping through a firewall is a legitimate deliberate choice, but it must
	// be one the operator saw. (/debug/state is additionally gated to loopback
	// peers in main regardless of bind.)
	if !isLoopbackHost(c.Metrics.Addr) {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"metrics.addr %s binds a non-loopback interface: /metrics and /debug/state are unauthenticated — front with a proxy/firewall or bind 127.0.0.1", c.Metrics.Addr))
	}
	if c.Serve.Addr != "" && !isLoopbackHost(c.Serve.Addr) {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"serve.addr %s binds a non-loopback interface: the v2 feeds, events query API (DB-backed), and SSE stream have no auth layer — front with the TLS proxy or bind 127.0.0.1", c.Serve.Addr))
	}
	if err := parseDurPositive("state.sv_ttl", c.State.SVTTLs, &c.State.SVTTL, 2*time.Hour); err != nil {
		return err
	}
	if err := parseDurPositive("state.propagate_interval", c.State.PropagateEverys, &c.State.PropagateEvery, time.Second); err != nil {
		return err
	}
	if c.State.Shards < 1 || c.State.Shards > maxShards {
		return fmt.Errorf("state.shards %d: must be in 1..%d", c.State.Shards, maxShards)
	}
	if c.State.LeapSeconds != 0 && (c.State.LeapSeconds < 10 || c.State.LeapSeconds > 30) {
		// Loose ICD-plausible band: ΔtLS has been 10-18s since GPS's 1980 epoch and
		// grows by at most 1s a year; this just catches a fat-fingered config value,
		// not a schedule.
		return fmt.Errorf("state.leap_seconds %d: outside the plausible 10-30 range", c.State.LeapSeconds)
	}

	if err := parseDurPositive("store.batch_interval", c.Store.BatchEverys, &c.Store.BatchEvery, time.Second); err != nil {
		return err
	}
	// defaults() pre-populates the genuine unset value before TOML decode,
	// so a decoded zero is necessarily explicit and must not be silently defaulted.
	if c.Store.BatchSize <= 0 || c.Store.BatchSize > maxBatchSize {
		return fmt.Errorf("store.batch_size %d: must be in 1..%d", c.Store.BatchSize, maxBatchSize)
	}
	// every parse error is propagated. These were previously
	// discarded on the premise that the regex had already validated the string,
	// but the regex says nothing about magnitude — an overflowing count is
	// syntactically valid and used to reach both the ordering check (as a wrapped
	// duration) and the database (as its original text).
	var retention, compress time.Duration
	if c.Store.RawRetention != "" {
		d, err := ParseInterval(c.Store.RawRetention)
		if err != nil {
			return fmt.Errorf("store.raw_retention: %w", err)
		}
		retention = d
	}
	if c.Store.CompressAfter != "" {
		d, err := ParseInterval(c.Store.CompressAfter)
		if err != nil {
			return fmt.Errorf("store.compress_after: %w", err)
		}
		compress = d
	}
	// Both operands are now bounded by MaxInterval, so this comparison cannot be
	// reading wrapped values.
	if retention > 0 && compress > 0 && compress >= retention {
		return fmt.Errorf("store.compress_after (%q) must be shorter than store.raw_retention (%q), or chunks are dropped before compression runs", c.Store.CompressAfter, c.Store.RawRetention)
	}
	// parse the DSN at load so a malformed store.dsn fails -check-config, not at the
	// first pool connect.
	if c.Store.DSN != "" {
		if _, err := pgxpool.ParseConfig(c.Store.DSN); err != nil {
			return fmt.Errorf("store.dsn: %w", err)
		}
	}
	if err := parseDurPositive("authorization.cache_ttl", c.Authorization.CacheTTLs, &c.Authorization.CacheTTL, 30*time.Second); err != nil {
		return err
	}
	if err := parseDurPositive("authorization.session_recheck_interval", c.Authorization.RecheckEverys, &c.Authorization.RecheckEvery, 10*time.Second); err != nil {
		return err
	}
	if c.Authorization.CacheTTL > 5*time.Minute {
		return fmt.Errorf("authorization.cache_ttl %s: must not exceed 5m", c.Authorization.CacheTTL)
	}
	if c.Authorization.RecheckEvery > 5*time.Minute {
		return fmt.Errorf("authorization.session_recheck_interval %s: must not exceed 5m", c.Authorization.RecheckEvery)
	}
	if c.Authorization.DSN != "" {
		if _, err := pgxpool.ParseConfig(c.Authorization.DSN); err != nil {
			return fmt.Errorf("authorization.dsn: %w", err)
		}
		if len(c.Push.Observers) != 0 {
			return fmt.Errorf("authorization.dsn and [[push.observer]] cannot both be set: database authorization has no config fallback")
		}
		if len(c.Serve.Principals) != 0 {
			return fmt.Errorf("authorization.dsn and [[serve.principal]] cannot both be set: database authorization has no config fallback")
		}
	}

	if err := parseDurPositive("serve.refresh_interval", c.Serve.RefreshFasts, &c.Serve.RefreshFast, 30*time.Second); err != nil {
		return err
	}
	if err := parseDurPositive("serve.almanac_refresh_interval", c.Serve.RefreshSlows, &c.Serve.RefreshSlow, 90*time.Second); err != nil {
		return err
	}
	switch c.Serve.Audience {
	case "", "public":
		c.Serve.Audience = "public"
		c.Serve.AudienceContext = identity.Audience{Kind: identity.AudiencePublic}
	case "operator":
		c.Serve.AudienceContext = identity.Audience{Kind: identity.AudienceOperator, ID: c.Collector.InstanceID}
	default:
		return fmt.Errorf("serve.audience %q: want public or operator (organization/collection audiences require authenticated read grants)", c.Serve.Audience)
	}
	if err := parseDur(c.Serve.SnapshotEverys, &c.Serve.SnapshotEvery); err != nil {
		return fmt.Errorf("serve.snapshot_interval: %w", err)
	}
	if c.Serve.SnapshotEverys == "" { // unset → default; an explicit "0s" disables
		c.Serve.SnapshotEvery = 5 * time.Minute
	} else if c.Serve.SnapshotEvery < 0 {
		return fmt.Errorf("serve.snapshot_interval: must be non-negative (0 disables)")
	}
	if err := c.finalizeServePrincipals(); err != nil {
		return err
	}

	if err := c.finalizePush(); err != nil {
		return err
	}
	if err := c.finalizeFederation(); err != nil {
		return err
	}
	for i, raw := range c.Federation.ExportGrants {
		if raw.Grant.SourceCollectorInstanceID != c.Collector.InstanceID {
			return fmt.Errorf("federation.export_grant[%d].source_collector %q does not match collector.instance_id %q", i, raw.Grant.SourceCollectorInstanceID, c.Collector.InstanceID)
		}
	}

	seen := make(map[string]bool, len(c.Ingest))
	for i := range c.Ingest {
		s := &c.Ingest[i]
		if s.Name == "" {
			return fmt.Errorf("ingest[%d]: name is required", i)
		}
		// a dial source's name IS its observer identity — it keys
		// live state, RF telemetry, capability reports, events and the served
		// observers feed, and is now served verbatim rather than through a lossy
		// display sanitizer. Names stay opaque (the deliberate API contract), so
		// the only thing refused here is one that cannot round-trip at all.
		if !identity.ValidOpaqueObserverID(s.Name) {
			return fmt.Errorf("ingest[%d]: name %q is not valid UTF-8; a source name is the "+
				"station identity carried through feeds, events and station selection, "+
				"so it must survive byte-for-byte", i, s.Name)
		}
		if seen[s.Name] {
			return fmt.Errorf("ingest[%d]: duplicate source name %q", i, s.Name)
		}
		seen[s.Name] = true
		if !knownDialTypes[s.Type] {
			return fmt.Errorf("ingest %q: unknown type %q (want ubx, sbf, rtcm, or ntrip)", s.Name, s.Type)
		}
		if s.Addr == "" {
			return fmt.Errorf("ingest %q: addr is required (host:port to dial)", s.Name)
		}
		// an ntrip source's addr is interpolated into the caster's
		// HTTP Host header, so a malformed or control-character-bearing authority
		// must fail -check-config rather than reach the wire. The dial itself needs
		// host:port for every type, so the syntax check is not ntrip-specific.
		if err := validateAddr("ingest "+s.Name+" addr", s.Addr); err != nil {
			return err
		}
		if strings.ContainsFunc(s.Addr, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return fmt.Errorf("ingest %q: addr contains control characters", s.Name)
		}
		if (s.Type == "sbf" || s.Type == "rtcm" || s.Type == "ntrip") && !s.CaptureOnly {
			return fmt.Errorf("ingest %q: type %s is capture-only; set capture_only = true explicitly", s.Name, s.Type)
		}
		// capture_only is implied only for byte sources; on a ubx source (which
		// decodes into live state) it has no effect, so an explicitly-set value is an
		// operator misunderstanding — reject it rather than silently ignore it.
		if s.Type == "ubx" && s.CaptureOnly {
			return fmt.Errorf("ingest %q: capture_only is not valid on a ubx source (it decodes into live state; capture_only is implied only for byte sources sbf/rtcm/ntrip)", s.Name)
		}
		if s.Type != "ntrip" {
			var field string
			switch {
			case s.Mountpoint != "":
				field = "mountpoint"
			case s.Username != "":
				field = "username"
			case s.Password != "":
				field = "password"
			case s.NTRIPCAFile != "":
				field = "ca_file"
			case s.NTRIPServerName != "":
				field = "server_name"
			case s.AllowInsecurePlaintext:
				field = "allow_insecure_plaintext"
			case s.AllowPlaintextCredentials:
				field = "allow_plaintext_credentials"
			}
			if field != "" {
				return fmt.Errorf("ingest %q: %s is only valid on a ntrip source (type is %s)", s.Name, field, s.Type)
			}
		}
		if s.Type == "ntrip" && s.Mountpoint == "" {
			return fmt.Errorf("ingest %q: mountpoint is required for type ntrip", s.Name)
		}
		if s.Type == "ntrip" && !ntripMountpointRe.MatchString(s.Mountpoint) {
			return fmt.Errorf("ingest %q: mountpoint %q contains whitespace/control characters (not a valid NTRIP mountpoint)", s.Name, s.Mountpoint)
		}
		if s.Type == "ntrip" && s.AllowPlaintextCredentials && !s.AllowInsecurePlaintext {
			return fmt.Errorf("ingest %q: allow_plaintext_credentials requires allow_insecure_plaintext", s.Name)
		}
		if s.Type == "ntrip" && s.AllowInsecurePlaintext && (s.Username != "" || s.Password != "") && !s.AllowPlaintextCredentials {
			return fmt.Errorf("ingest %q: plaintext NTRIP credentials require allow_plaintext_credentials = true", s.Name)
		}
		if s.Type == "ntrip" && s.NTRIPCAFile != "" && s.AllowInsecurePlaintext {
			return fmt.Errorf("ingest %q: ca_file cannot be used with insecure plaintext", s.Name)
		}
		// validate the ntrip ca_file at load. A wrong path otherwise fails only at
		// dial time, presenting as a permanently-backoff-retried flaky receiver rather than
		// a clear config error.
		if s.Type == "ntrip" && s.NTRIPCAFile != "" {
			if err := validatePEMFile("ingest "+s.Name+" ca_file", s.NTRIPCAFile); err != nil {
				return err
			}
		}
		if err := parseDurPositive("ingest "+s.Name+" max_frame_silence",
			s.MaxFrameSilences, &s.MaxFrameSilence, 5*time.Minute); err != nil {
			return err
		}
		caps, err := parseCapabilities(s.Capabilities)
		if err != nil {
			return fmt.Errorf("ingest %q: %w", s.Name, err)
		}
		s.CapDecl = caps
		collector := s.CollectorInstanceID
		if collector == "" {
			collector = c.Collector.InstanceID
		} else if collector != c.Collector.InstanceID {
			return fmt.Errorf("ingest %q collector_instance %q does not match collector.instance_id %q", s.Name, collector, c.Collector.InstanceID)
		}
		s.ObserverContext, err = finalizeObserverContext(
			s.Name, s.OrganizationID, s.EnrollmentID, collector,
			s.CollectionIDs, s.AggregateUse, s.StationMetadata, s.EventVisibility,
			s.RawExport, s.FederationPeers, s.PublishSignals, s.PolicyRevision,
			identity.CredentialLocalDial,
		)
		if err != nil {
			return fmt.Errorf("ingest %q identity: %w", s.Name, err)
		}
		s.ObserverContext.FeedGrants = []string{s.Type}
		s.ObserverContext.DeclaredCapabilities = capabilitySignals(s.CapDecl)
		s.ObserverContext, err = s.ObserverContext.Normalize()
		if err != nil {
			return fmt.Errorf("ingest %q identity evidence: %w", s.Name, err)
		}
	}
	return nil
}

func (c *Config) finalizeServePrincipals() error {
	seenTokens := make(map[string]string, len(c.Serve.Principals))
	for i := range c.Serve.Principals {
		raw := &c.Serve.Principals[i]
		if len(raw.TokenSHA256) != 64 || !isHex(raw.TokenSHA256) {
			return fmt.Errorf("serve.principal[%d] token_sha256 must be 64 hex chars", i)
		}
		raw.TokenSHA256 = strings.ToLower(raw.TokenSHA256)
		if prior, ok := seenTokens[raw.TokenSHA256]; ok {
			return fmt.Errorf("serve.principal[%d] token duplicates principal %q", i, prior)
		}
		seenTokens[raw.TokenSHA256] = raw.ID
		principal := identity.ReadPrincipal{ID: raw.ID, Revision: raw.Revision}
		for _, value := range raw.Audiences {
			audience, err := identity.ParseAudience(value)
			if err != nil {
				return fmt.Errorf("serve.principal[%d]: %w", i, err)
			}
			principal.AudienceGrants = append(principal.AudienceGrants, audience)
		}
		var err error
		principal, err = identity.NormalizeReadPrincipal(principal)
		if err != nil {
			return fmt.Errorf("serve.principal[%d]: %w", i, err)
		}
		raw.Principal = principal
	}
	return nil
}

func (c *Config) finalizeFederation() error {
	seen := make(map[string]bool, len(c.Federation.ExportGrants))
	for i := range c.Federation.ExportGrants {
		raw := &c.Federation.ExportGrants[i]
		grant := federation.ExportGrant{
			SourceCollectorInstanceID: raw.SourceCollector,
			DestinationPeerID:         raw.DestinationPeer,
			OrganizationIDs:           append([]string(nil), raw.Organizations...),
			CollectionIDs:             append([]string(nil), raw.Collections...),
			ObserverIDs:               append([]string(nil), raw.Observers...),
			MaxAttribution:            federation.Attribution(raw.Attribution),
			Purposes:                  append([]string(nil), raw.Purposes...),
			ApprovedBy:                raw.ApprovedBy,
			Revision:                  raw.Revision,
			Enabled:                   raw.Enabled,
		}
		for _, class := range raw.DataClasses {
			grant.DataClasses = append(grant.DataClasses, federation.DataClass(class))
		}
		signals, err := parseSignals(raw.Signals, "federation signal")
		if err != nil {
			return fmt.Errorf("federation.export_grant[%d]: %w", i, err)
		}
		for _, signal := range signals {
			grant.Signals = append(grant.Signals, identity.Signal{GnssID: signal.Gnss, SigID: signal.Sig})
		}
		grant.MaxRetention, err = time.ParseDuration(raw.MaxRetentions)
		if err != nil {
			return fmt.Errorf("federation.export_grant[%d].max_retention: %w", i, err)
		}
		if raw.ValidFroms == "" || raw.ValidUntils == "" {
			return fmt.Errorf("federation.export_grant[%d]: valid_from and valid_until are required RFC3339 timestamps", i)
		}
		grant.ValidFrom, err = time.Parse(time.RFC3339, raw.ValidFroms)
		if err != nil {
			return fmt.Errorf("federation.export_grant[%d].valid_from: %w", i, err)
		}
		grant.ValidUntil, err = time.Parse(time.RFC3339, raw.ValidUntils)
		if err != nil {
			return fmt.Errorf("federation.export_grant[%d].valid_until: %w", i, err)
		}
		if err := federation.ValidateGrant(grant); err != nil {
			return fmt.Errorf("federation.export_grant[%d]: %w", i, err)
		}
		key := grant.SourceCollectorInstanceID + "\x00" + grant.DestinationPeerID + "\x00" + grant.Revision
		if seen[key] {
			return fmt.Errorf("federation.export_grant[%d]: duplicate source/destination/revision", i)
		}
		seen[key] = true
		raw.Grant = grant
	}
	return nil
}

// MaxInterval is the largest simple interval this configuration accepts: exactly
// time.Duration's range, about 292.47 years. PostgreSQL's interval type reaches
// far beyond that, but every Go-side consumer of a configured interval — the
// compress-before-retention ordering check, the store's replay-ledger prune
// horizon — is a time.Duration. A value the Go side cannot represent is one the
// daemon cannot honor, and rejecting it is what keeps the database policy and
// the in-memory horizon guaranteed to mean the same thing.
const MaxInterval = time.Duration(math.MaxInt64)

var intervalUnits = map[string]time.Duration{
	"minute": time.Minute,
	"hour":   time.Hour,
	"day":    24 * time.Hour,
	"week":   7 * 24 * time.Hour,
}

// ParseInterval converts one IntervalRe-shaped simple interval into a Duration.
// It is the single parser behind both `-check-config` validation and the store's
// replay-ledger retention horizon, so the two cannot disagree about what a
// configured string means. It re-applies IntervalRe itself, which also keeps it
// usable as the injection guard for the interval's later interpolation into
// policy DDL.
//
// the count is parsed at an explicit width and the multiplication
// is range-checked before it happens. IntervalRe allows an unbounded decimal
// count, so the previous `Atoi` plus unchecked `time.Duration(n) * per` could
// wrap a huge but syntactically valid interval to a small, negative, or zero
// duration — after which the ordering check compared wrapped values while the
// database still received the original, very large text.
func ParseInterval(s string) (time.Duration, error) {
	if !IntervalRe.MatchString(s) {
		return 0, fmt.Errorf(`invalid interval %q: want a simple interval like "7 days"`, s)
	}
	// IntervalRe guarantees exactly "<digits> <unit>", so the split cannot fail.
	count, unit, _ := strings.Cut(s, " ")
	per := intervalUnits[strings.TrimSuffix(unit, "s")]
	if per == 0 {
		return 0, fmt.Errorf("unknown interval unit %q", unit)
	}
	n, err := strconv.ParseInt(count, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("interval %q: count out of range", s)
	}
	if n <= 0 || n > int64(MaxInterval/per) {
		return 0, fmt.Errorf("interval %q exceeds the maximum supported interval (%d %ss)",
			s, int64(MaxInterval/per), strings.TrimSuffix(unit, "s"))
	}
	return time.Duration(n) * per, nil
}

// parseCapabilities turns the declared "gnss:sig" strings into typed tuples, validating the
// numbering against the u-blox gnssId range (docs/CONSTELLATIONS.md §0). An empty list yields
// nil (no declared fingerprint — the observed-only detectors still run).
func parseCapabilities(raw []string) ([]Capability, error) {
	return parseSignals(raw, "capability")
}

func parseSignals(raw []string, label string) ([]Capability, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]Capability, 0, len(raw))
	seen := make(map[Capability]bool, len(raw))
	for _, s := range raw {
		g, sig, ok := strings.Cut(s, ":")
		if !ok {
			return nil, fmt.Errorf("%s %q: want \"gnss:sig\" (e.g. \"2:3\")", label, s)
		}
		gi, err := strconv.Atoi(strings.TrimSpace(g))
		if err != nil || gi < 0 || gi > 7 {
			return nil, fmt.Errorf("%s %q: gnssId must be 0..7", label, s)
		}
		si, err := strconv.Atoi(strings.TrimSpace(sig))
		if err != nil || si < 0 || si > 255 {
			return nil, fmt.Errorf("%s %q: sigId must be 0..255", label, s)
		}
		c := Capability{Gnss: gi, Sig: si}
		if seen[c] {
			return nil, fmt.Errorf("%s %q: duplicate", label, s)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// finalizeObserverContext builds the config-backed bootstrap identity. Empty
// administrative fields are filled only with fail-closed local/private defaults;
// no config source can claim a manufacturer-attested tier.
func finalizeObserverContext(observer, organization, enrollment, collector string,
	collections []string, aggregate, metadata, events, rawExport string,
	federationPeers, publishSignals []string, revision string,
	credential identity.CredentialTier,
) (identity.ObserverContext, error) {
	c := identity.NewPrivateContext(observer, credential)
	if organization != "" {
		c.OrganizationID = organization
	}
	if enrollment != "" {
		c.EnrollmentID = enrollment
	}
	if collector != "" {
		c.CollectorInstanceID = collector
	}
	if len(collections) > 0 {
		c.CollectionIDs = append([]string(nil), collections...)
	}
	if aggregate != "" {
		c.Publication.AggregateUse = identity.AggregateUse(aggregate)
	}
	if metadata != "" {
		c.Publication.StationMetadata = identity.StationMetadata(metadata)
	}
	if events != "" {
		c.Publication.EventVisibility = identity.EventVisibility(events)
	}
	if rawExport != "" {
		c.Publication.RawExport = identity.RawExport(rawExport)
	}
	if len(federationPeers) > 0 {
		c.Publication.FederationPeers = append([]string(nil), federationPeers...)
	}
	signals, err := parseSignals(publishSignals, "publish signal")
	if err != nil {
		return c, err
	}
	for _, signal := range signals {
		c.Publication.Signals = append(c.Publication.Signals, identity.Signal{GnssID: signal.Gnss, SigID: signal.Sig})
	}
	if revision != "" {
		c.Publication.Revision = revision
	}
	return c.Normalize()
}

// finalizePush validates and defaults the push endpoint. When disabled (no addr) it
// is a no-op; when enabled, TLS material is mandatory and every observer must carry
// a station, a well-formed token hash, and at least one known feed type.
func (c *Config) finalizePush() error {
	p := &c.Push
	if err := parseDurPositive("push.ack_interval", p.AckIntervals, &p.AckInterval, time.Second); err != nil {
		return err
	}
	// defaults() pre-populates the genuine unset value before TOML decode;
	// an explicit zero from TOML therefore remains distinguishable and invalid.
	if p.MaxConns <= 0 || p.MaxConns > maxPushConns {
		return fmt.Errorf("push.max_conns %d: must be in 1..%d", p.MaxConns, maxPushConns)
	}
	if p.Addr == "" {
		return nil // push disabled
	}
	// fail -check-config for a malformed push listener addr, an unloadable TLS
	// keypair, or an unparsable client_ca — the same fail-fast parity regression fix gave metrics/
	// serve, so a config edit is caught pre-flight instead of in a 5 s supervisor restart loop.
	if err := validateAddr("push.addr", p.Addr); err != nil {
		return err
	}
	if p.TLSCert == "" || p.TLSKey == "" {
		return fmt.Errorf("push.tls_cert and push.tls_key are required when push.addr is set")
	}
	if _, err := tls.LoadX509KeyPair(p.TLSCert, p.TLSKey); err != nil {
		return fmt.Errorf("push.tls_cert/tls_key: %w", err)
	}
	// refuse a group/world-readable private key — the sshd/postgres
	// precedent. A readable push.tls_key lets any local user impersonate the
	// collector to the whole feeder fleet, so this is a hard error (unlike the
	// config-file warning: a key file has no credential-less mode). Stat after
	// the successful LoadX509KeyPair above, so the file is known to exist; a
	// racing stat failure is ignored rather than inventing a new failure mode.
	if fi, err := os.Stat(p.TLSKey); err == nil {
		if m := fi.Mode().Perm(); m&0o077 != 0 {
			return fmt.Errorf("push.tls_key %q is group/world-readable (mode %04o); private keys must be 0600", p.TLSKey, m)
		}
	}
	if p.ClientCA != "" {
		if err := validatePEMFile("push.client_ca", p.ClientCA); err != nil {
			return err
		}
	}
	stations := make(map[string]bool, len(p.Observers))
	tokens := make(map[string]string, len(p.Observers))
	for i := range p.Observers {
		o := &p.Observers[i]
		if o.Station == "" {
			return fmt.Errorf("push.observer[%d]: station is required", i)
		}
		if stations[o.Station] {
			return fmt.Errorf("push.observer[%d]: duplicate station %q", i, o.Station)
		}
		stations[o.Station] = true
		if p.ClientCA != "" && !ValidObserverID(o.Station) {
			return fmt.Errorf("push.observer %q: station must be a certificate-bindable name (ASCII letters/digits/./- only, at most 253 bytes) when push.client_ca is set", o.Station)
		}
		if len(o.TokenSHA256) != 64 || !isHex(o.TokenSHA256) {
			return fmt.Errorf("push.observer %q: token_sha256 must be 64 hex chars (a SHA-256)", o.Station)
		}
		normalized := strings.ToLower(o.TokenSHA256)
		if prior, exists := tokens[normalized]; exists {
			return fmt.Errorf("push.observer %q: token_sha256 duplicates observer %q", o.Station, prior)
		}
		tokens[normalized] = o.Station
		o.TokenSHA256 = normalized
		if len(o.Feeds) == 0 {
			return fmt.Errorf("push.observer %q: at least one feed type is required", o.Station)
		}
		for _, f := range o.Feeds {
			if !knownIngestTypes[f] {
				return fmt.Errorf("push.observer %q: unknown feed type %q", o.Station, f)
			}
		}
		caps, err := parseCapabilities(o.Capabilities)
		if err != nil {
			return fmt.Errorf("push.observer %q: %w", o.Station, err)
		}
		o.CapDecl = caps
		collector := o.CollectorInstanceID
		if collector == "" {
			collector = c.Collector.InstanceID
		} else if collector != c.Collector.InstanceID {
			return fmt.Errorf("push.observer %q collector_instance %q does not match collector.instance_id %q", o.Station, collector, c.Collector.InstanceID)
		}
		o.ObserverContext, err = finalizeObserverContext(
			o.Station, o.OrganizationID, o.EnrollmentID, collector,
			o.CollectionIDs, o.AggregateUse, o.StationMetadata, o.EventVisibility,
			o.RawExport, o.FederationPeers, o.PublishSignals, o.PolicyRevision,
			identity.CredentialToken,
		)
		if err != nil {
			return fmt.Errorf("push.observer %q identity: %w", o.Station, err)
		}
		o.ObserverContext.FeedGrants = append([]string(nil), o.Feeds...)
		o.ObserverContext.DeclaredCapabilities = capabilitySignals(o.CapDecl)
		o.ObserverContext, err = o.ObserverContext.Normalize()
		if err != nil {
			return fmt.Errorf("push.observer %q identity evidence: %w", o.Station, err)
		}
	}
	return nil
}

func capabilitySignals(capabilities []Capability) []identity.Signal {
	out := make([]identity.Signal, 0, len(capabilities))
	for _, capability := range capabilities {
		out = append(out, identity.Signal{GnssID: capability.Gnss, SigID: capability.Sig})
	}
	return out
}

// ValidObserverID reports whether station can serve as a canonical observer
// identity bindable to an mTLS certificate: the push handshake (ingest,
// matchPeerIdentity) compares the certificate's single DNS SAN byte-for-byte
// against this name, so it must be nonempty, at most 253 bytes (the DNS name
// bound), and only ASCII letters, digits, '.', '-' — no case-fold or Unicode
// aliases.
//
// this is now one delegation to identity.ValidObserverID, the
// single contract shared with the control-plane boundary (ObserverContext
// .Normalize). Two copies of the rule, applied at different boundaries with
// different strictness, is what let a database-authorized id reach the feed as
// something a display sanitizer had to clean.
func ValidObserverID(s string) bool { return identity.ValidObserverID(s) }

// isLoopbackHost reports whether addr's host part is provably loopback
// ("localhost", 127.0.0.0/8, ::1) — regression fix. An empty host (":9100") binds
// every interface and a non-"localhost" hostname cannot be proven loopback
// without resolving it, so both report false (conservative: they warn).
// Assumes addr already passed validateAddr.
func isLoopbackHost(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateAddr checks host:port syntax so a typo'd or malformed listener addr is
// a config-load error, not a silent listener-bind failure discovered only at startup.
func validateAddr(field, addr string) error {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q: %w", field, addr, err)
	}
	if port == "" {
		return fmt.Errorf("%s %q: port is required", field, addr)
	}
	resolved, err := net.LookupPort("tcp", port)
	if err != nil {
		return fmt.Errorf("%s %q: %w", field, addr, err)
	}
	if resolved == 0 {
		return fmt.Errorf("%s %q: port 0 would select an ephemeral listener", field, addr)
	}
	return nil
}

// validatePEMFile reads a PEM file and confirms it parses to at least one certificate
// , so a wrong client_ca / ntrip ca_file path fails config load rather than at
// dial/accept time (which for ntrip presents as an endlessly-retried flaky receiver).
func validatePEMFile(field, path string) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("%s %q: %w", field, path, err)
	}
	if !x509.NewCertPool().AppendCertsFromPEM(b) {
		return fmt.Errorf("%s %q: no valid PEM certificate found", field, path)
	}
	return nil
}

func isHex(s string) bool {
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

func parseDur(s string, out *time.Duration) error {
	if s == "" {
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*out = d
	return nil
}

// parseDurPositive applies def when s is empty (unset), but treats a non-empty s that parses
// to a non-positive duration as a hard config error : the previous "parse then coerce
// <= 0 to a default" pattern couldn't tell "unset" from "explicitly configured garbage", so
// sv_ttl = "0s" (an operator plausibly meaning "never expire") silently became 2 h and a
// negative batch_interval silently became 1 s. Fields where 0 is a documented disable
// (snapshot_interval) keep their own handling and do not use this.
func parseDurPositive(field, s string, out *time.Duration, def time.Duration) error {
	if s == "" {
		*out = def
		return nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("%s: %w", field, err)
	}
	if d <= 0 {
		return fmt.Errorf("%s %q: must be a positive duration", field, s)
	}
	*out = d
	return nil
}
