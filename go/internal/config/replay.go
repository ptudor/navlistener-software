package config

import (
	"bytes"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
	"github.com/ptudor/navlistener/internal/identity"
)

// LoadForReplay reads an explicit collector config with strict TOML parsing and
// resolves only its reception and integrity installations. Replay uses the
// collector id and historian DSN, but needs no listener credentials, authority
// files or startup permission checks. Use Load for daemon startup and preflight.
func LoadForReplay(path string) (*Config, error) {
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
	if !identity.ValidScopeID(cfg.Collector.InstanceID) {
		return nil, fmt.Errorf("invalid config %s: collector.instance_id %q is invalid", path, cfg.Collector.InstanceID)
	}
	if err := cfg.Reception.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	for _, site := range cfg.Reception.Stations {
		if !ValidObserverID(site.Observer) {
			return nil, fmt.Errorf("invalid config %s: reception: invalid observer %q", path, site.Observer)
		}
	}
	if err := cfg.finalizeIntegrity(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return cfg, nil
}
