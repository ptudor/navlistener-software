package main

import (
	"context"
	"log/slog"

	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
)

func startManufacturers(ctx context.Context, configs config.ManufacturerAuthorities, log *slog.Logger) (commissioning.Authorities, error) {
	verifiers := commissioning.Authorities{}
	for _, cfg := range configs {
		if !cfg.Enabled() {
			continue
		}
		v, err := startHardwareTrust(ctx, cfg, log)
		if err != nil {
			return nil, err
		}
		verifiers[cfg.ManufacturerAuthorityID] = v
	}
	if len(verifiers) == 0 {
		return nil, nil
	}
	return verifiers, nil
}

// startHardwareTrust builds the push endpoint's evidence verifier
// (docs/COMMISSIONING.md). It returns nil when no manufacturer keys are pinned:
// the push endpoint then reads evidence and answers "unconfigured". A configured
// registry is loaded before this returns, so the collector never accepts a
// session while able to honour a board the registry has withdrawn, and is then
// reloaded on change until ctx ends. With registry_state the newest sequence
// adopted is recorded, so a restart cannot be handed an older registry.
func startHardwareTrust(ctx context.Context, cfg config.HardwareTrust, log *slog.Logger) (*commissioning.Verifier, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	log = log.With("manufacturer_authority_id", cfg.ManufacturerAuthorityID)
	verifier, err := cfg.NewVerifier()
	if err != nil {
		return nil, err
	}
	if cfg.Registry == "" {
		log.Info("hardware evidence verification enabled without a registry; no board can be withdrawn",
			"manufacturer_keys", len(cfg.ManufacturerKeys))
		return verifier, nil
	}
	if cfg.RegistryState != "" {
		if err := verifier.UseRegistryState(cfg.RegistryState); err != nil {
			return nil, err
		}
	}
	observe := func(reg *commissioning.RegistryIndex, err error) {
		observeRegistry(cfg.ManufacturerAuthorityID, reg, err)
	}
	if err := verifier.WatchRegistry(ctx, cfg.Registry, cfg.RegistryReload, log, observe); err != nil {
		return nil, err
	}
	registry := verifier.Registry()
	log.Info("hardware evidence verification enabled", "manufacturer_keys", len(cfg.ManufacturerKeys),
		"registry", cfg.Registry, "registry_sequence", registry.Sequence, "registry_boards", registry.Len(),
		"registry_issued_at", registry.IssuedAt, "registry_state", cfg.RegistryState,
		"require_registry_entry", cfg.RequireRegistryEntry)
	return verifier, nil
}

// observeRegistry publishes the registry in force after every load attempt. A
// refused load leaves the earlier registry in force, so the gauges keep
// describing it and only the failure counter moves. The counter also moves for
// a registry that was adopted but whose sequence could not be recorded.
func observeRegistry(authorityID string, inForce *commissioning.RegistryIndex, err error) {
	if err != nil {
		metrics.HardwareRegistryReloadFailuresTotal.WithLabelValues(authorityID).Inc()
	}
	if inForce == nil {
		return
	}
	metrics.HardwareRegistrySequence.WithLabelValues(authorityID).Set(float64(inForce.Sequence))
	metrics.HardwareRegistryIssuedTimestampSeconds.WithLabelValues(authorityID).Set(float64(inForce.IssuedAt.Unix()))
	metrics.HardwareRegistryBoards.WithLabelValues(authorityID).Set(float64(inForce.Len()))
}
