// Package config loads the navlistener TOML configuration.
//
// Config is TOML at /usr/local/etc/navlistener/navlistener.toml (never .env),
// passed via the -config flag. The struct anticipates every pipeline stage; a
// stage stays dormant until its section is populated, so the file grows with the
// build. This pass wires INGEST → DECODE → PROPAGATE, so it reads [logging],
// [metrics], [state], and the [[ingest]] connector list; the persist/serve/push
// sections arrive with those later passes (docs/DESIGN.md §5).
package config

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	toml "github.com/pelletier/go-toml/v2"
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
	Logging Logging  `toml:"logging"`
	Metrics Metrics  `toml:"metrics"`
	State   State    `toml:"state"`
	Store   Store    `toml:"store"`
	Serve   Serve    `toml:"serve"`
	Push    Push     `toml:"push"`
	Ingest  []Source `toml:"ingest"`

	// ShutdownTimeout bounds graceful shutdown; kept out of the wire format.
	ShutdownTimeout time.Duration `toml:"-"`
}

// Serve is the native v2 read API listener (docs/OUTPUT.md). It binds loopback and
// is fronted by a TLS reverse proxy; it is enabled only when an addr is given, so
// the daemon can run collector-only. The refresh cadences default per §5.
type Serve struct {
	Addr string `toml:"addr"` // e.g. 127.0.0.1:8080; empty = serve disabled

	RefreshFasts string        `toml:"refresh_interval"` // svs/global/observers/sbas, default "30s"
	RefreshFast  time.Duration `toml:"-"`
	RefreshSlows string        `toml:"almanac_refresh_interval"` // almanac, default "90s"
	RefreshSlow  time.Duration `toml:"-"`
	// SnapshotEvery is the cadence at which each served feed's current body is persisted
	// to the historian as a replay/backfill record (docs/OUTPUT.md §4). Only active when
	// both [serve].addr and [store].dsn are set. Default "5m"; "0s" disables.
	SnapshotEverys string        `toml:"snapshot_interval"`
	SnapshotEvery  time.Duration `toml:"-"`
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
	// forensic record; the long-term ephemeris history lives in continuous
	// aggregates (a later pass).
	RawRetention string `toml:"raw_retention"` // default "7 days"
	// CompressAfter is when a raw chunk is columnar-compressed (default "1 day").
	CompressAfter string `toml:"compress_after"`
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

// PushObserver is one enrolled edge feeder's credential and feed grant. TokenSHA256
// is the hex-encoded SHA-256 of the bearer token; Feeds is the allow-list of feed
// types this observer may push (the as-built Device.feed_types grant, DESIGN §3).
type PushObserver struct {
	Station     string   `toml:"station"`
	TokenSHA256 string   `toml:"token_sha256"`
	Feeds       []string `toml:"feeds"`

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
	// design mandates "transcribe, don't invent" and the broadcast UTC-parameter
	// decode is a stated future pass, but until that lands a leap second would
	// otherwise shift every wall-clock→GNSS conversion by 1s (~3.9 km) until a
	// rebuild. Set explicitly here to apply a new value without recompiling.
	LeapSeconds int `toml:"leap_seconds"`
}

// Source is one raw-frame ingest connector. Every navlistener ingest source is a
// dial-out connector to a receiver we control on the LAN (the authenticated fleet
// push endpoint is a separate mechanism — docs/DESIGN.md §1, a later pass). Type
// selects the wire parser; Addr is the host:port to dial.
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

	// Capabilities is this node's declared tudorgps fingerprint: the "gnss:sig" signals its
	// silicon can produce (docs/CONSTELLATIONS.md §7). The integrity layer compares it against
	// what the node actually delivers — a declared signal that goes silent, or an observed
	// signal the silicon can't produce, is a threat (docs/INTEGRITY.md §6). Parsed into CapDecl.
	Capabilities []string     `toml:"capabilities,omitempty"`
	CapDecl      []Capability `toml:"-"`

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
	return cfg, nil
}

func defaults() *Config {
	return &Config{
		Logging:         Logging{Level: "info", Format: "json"},
		Metrics:         Metrics{Addr: "127.0.0.1:9100"},
		State:           State{Shards: 16, SVTTLs: "2h", PropagateEverys: "1s"},
		Store:           Store{BatchSize: 1000, BatchEverys: "1s", RawRetention: "7 days", CompressAfter: "1 day"},
		ShutdownTimeout: 15 * time.Second,
	}
}

// finalize parses duration strings and validates required fields. Validation is
// strict and fatal: an unknown connector type or a duplicate source name is a
// configuration error, not a warning.
func (c *Config) finalize() error {
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
	if err := parseDur(c.State.SVTTLs, &c.State.SVTTL); err != nil {
		return fmt.Errorf("state.sv_ttl: %w", err)
	}
	if c.State.SVTTL <= 0 {
		c.State.SVTTL = 2 * time.Hour
	}
	if err := parseDur(c.State.PropagateEverys, &c.State.PropagateEvery); err != nil {
		return fmt.Errorf("state.propagate_interval: %w", err)
	}
	if c.State.PropagateEvery <= 0 {
		c.State.PropagateEvery = time.Second
	}
	if c.State.Shards < 1 {
		return fmt.Errorf("state.shards must be >= 1")
	}
	if c.State.LeapSeconds != 0 && (c.State.LeapSeconds < 10 || c.State.LeapSeconds > 30) {
		// Loose ICD-plausible band: ΔtLS has been 10-18s since GPS's 1980 epoch and
		// grows by at most 1s a year; this just catches a fat-fingered config value,
		// not a schedule.
		return fmt.Errorf("state.leap_seconds %d: outside the plausible 10-30 range", c.State.LeapSeconds)
	}

	if err := parseDur(c.Store.BatchEverys, &c.Store.BatchEvery); err != nil {
		return fmt.Errorf("store.batch_interval: %w", err)
	}
	if c.Store.BatchEvery <= 0 {
		c.Store.BatchEvery = time.Second
	}
	if c.Store.BatchSize < 1 {
		c.Store.BatchSize = 1000
	}
	if c.Store.RawRetention != "" && !IntervalRe.MatchString(c.Store.RawRetention) {
		return fmt.Errorf(`store.raw_retention %q: want a simple interval like "7 days"`, c.Store.RawRetention)
	}
	if c.Store.CompressAfter != "" && !IntervalRe.MatchString(c.Store.CompressAfter) {
		return fmt.Errorf(`store.compress_after %q: want a simple interval like "1 day"`, c.Store.CompressAfter)
	}

	if err := parseDur(c.Serve.RefreshFasts, &c.Serve.RefreshFast); err != nil {
		return fmt.Errorf("serve.refresh_interval: %w", err)
	}
	if c.Serve.RefreshFast <= 0 {
		c.Serve.RefreshFast = 30 * time.Second
	}
	if err := parseDur(c.Serve.RefreshSlows, &c.Serve.RefreshSlow); err != nil {
		return fmt.Errorf("serve.almanac_refresh_interval: %w", err)
	}
	if c.Serve.RefreshSlow <= 0 {
		c.Serve.RefreshSlow = 90 * time.Second
	}
	if err := parseDur(c.Serve.SnapshotEverys, &c.Serve.SnapshotEvery); err != nil {
		return fmt.Errorf("serve.snapshot_interval: %w", err)
	}
	if c.Serve.SnapshotEverys == "" { // unset → default; an explicit "0s" disables
		c.Serve.SnapshotEvery = 5 * time.Minute
	}

	if err := c.finalizePush(); err != nil {
		return err
	}

	seen := make(map[string]bool, len(c.Ingest))
	for i := range c.Ingest {
		s := &c.Ingest[i]
		if s.Name == "" {
			return fmt.Errorf("ingest[%d]: name is required", i)
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
		if (s.Type == "sbf" || s.Type == "rtcm" || s.Type == "ntrip") && !s.CaptureOnly {
			return fmt.Errorf("ingest %q: type %s is capture-only; set capture_only = true explicitly", s.Name, s.Type)
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
		caps, err := parseCapabilities(s.Capabilities)
		if err != nil {
			return fmt.Errorf("ingest %q: %w", s.Name, err)
		}
		s.CapDecl = caps
	}
	return nil
}

// parseCapabilities turns the declared "gnss:sig" strings into typed tuples, validating the
// numbering against the u-blox gnssId range (docs/CONSTELLATIONS.md §0). An empty list yields
// nil (no declared fingerprint — the observed-only detectors still run).
func parseCapabilities(raw []string) ([]Capability, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make([]Capability, 0, len(raw))
	seen := make(map[Capability]bool, len(raw))
	for _, s := range raw {
		g, sig, ok := strings.Cut(s, ":")
		if !ok {
			return nil, fmt.Errorf("capability %q: want \"gnss:sig\" (e.g. \"2:3\")", s)
		}
		gi, err := strconv.Atoi(strings.TrimSpace(g))
		if err != nil || gi < 0 || gi > 7 {
			return nil, fmt.Errorf("capability %q: gnssId must be 0..7", s)
		}
		si, err := strconv.Atoi(strings.TrimSpace(sig))
		if err != nil || si < 0 || si > 255 {
			return nil, fmt.Errorf("capability %q: sigId must be 0..255", s)
		}
		c := Capability{Gnss: gi, Sig: si}
		if seen[c] {
			return nil, fmt.Errorf("capability %q: duplicate", s)
		}
		seen[c] = true
		out = append(out, c)
	}
	return out, nil
}

// finalizePush validates and defaults the push endpoint. When disabled (no addr) it
// is a no-op; when enabled, TLS material is mandatory and every observer must carry
// a station, a well-formed token hash, and at least one known feed type.
func (c *Config) finalizePush() error {
	p := &c.Push
	if err := parseDur(p.AckIntervals, &p.AckInterval); err != nil {
		return fmt.Errorf("push.ack_interval: %w", err)
	}
	if p.AckInterval <= 0 {
		p.AckInterval = time.Second
	}
	if p.MaxConns <= 0 {
		p.MaxConns = 512
	}
	if p.Addr == "" {
		return nil // push disabled
	}
	if p.TLSCert == "" || p.TLSKey == "" {
		return fmt.Errorf("push.tls_cert and push.tls_key are required when push.addr is set")
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
	}
	return nil
}

// validateAddr checks host:port syntax  so a typo'd or malformed listener addr is
// a config-load error, not a silent listener-bind failure discovered only at startup.
func validateAddr(field, addr string) error {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		return fmt.Errorf("%s %q: %w", field, addr, err)
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
