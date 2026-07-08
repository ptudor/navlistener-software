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
	"fmt"
	"os"
	"time"

	toml "github.com/pelletier/go-toml/v2"
)

// DefaultPaths are searched in order when -config is not given.
var DefaultPaths = []string{
	"/usr/local/etc/navlistener/navlistener.toml",
	"/etc/navlistener/navlistener.toml",
	"./navlistener.toml",
}

// knownIngestTypes are the raw-frame connector types this build implements
// (docs/CONSTELLATIONS.md §2). Each dials a receiver and forwards raw frames.
var knownIngestTypes = map[string]bool{
	"ubx":  true, // u-blox UBX-RXM-SFRBX stream
	"sbf":  true, // Septentrio SBF raw-nav blocks
	"rtcm": true, // RTCM3 ephemeris / SSR
}

// Config is the whole-daemon configuration.
type Config struct {
	Logging Logging  `toml:"logging"`
	Metrics Metrics  `toml:"metrics"`
	State   State    `toml:"state"`
	Ingest  []Source `toml:"ingest"`

	// ShutdownTimeout bounds graceful shutdown; kept out of the wire format.
	ShutdownTimeout time.Duration `toml:"-"`
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
}

// Source is one raw-frame ingest connector. Every navlistener ingest source is a
// dial-out connector to a receiver we control on the LAN (the authenticated fleet
// push endpoint is a separate mechanism — docs/DESIGN.md §1, a later pass). Type
// selects the wire parser; Addr is the host:port to dial.
type Source struct {
	Name     string `toml:"name"`
	Type     string `toml:"type"`               // ubx | sbf | rtcm
	Addr     string `toml:"addr"`               // host:port to dial
	Disabled bool   `toml:"disabled,omitempty"` // keep the entry but don't start it (pause)
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
	if err := toml.Unmarshal(raw, cfg); err != nil {
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
		if !knownIngestTypes[s.Type] {
			return fmt.Errorf("ingest %q: unknown type %q (want ubx, sbf, or rtcm)", s.Name, s.Type)
		}
		if s.Addr == "" {
			return fmt.Errorf("ingest %q: addr is required (host:port to dial)", s.Name)
		}
	}
	return nil
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
