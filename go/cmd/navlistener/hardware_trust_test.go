package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/metrics"
)

func pinnedKey(t *testing.T, dir, name string) (string, commissioning.Signer) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	signer, err := commissioning.NewKeySigner(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	return path, signer
}

func writeRegistry(t *testing.T, path string, sequence uint64, signer commissioning.Signer) {
	t.Helper()
	data, err := commissioning.SignRegistry(commissioning.Registry{
		Sequence: sequence, IssuedAt: time.Unix(1789650000+int64(sequence), 0).UTC(), LedgerHead: strings.Repeat("ab", 32),
	}, signer)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStartHardwareTrust(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	manufacturer, _ := pinnedKey(t, dir, "manufacturer.pem")
	operations, signer := pinnedKey(t, dir, "registry.pem")
	registry := filepath.Join(dir, "registry.json")

	if v, err := startHardwareTrust(ctx, config.HardwareTrust{}, log); v != nil || err != nil {
		t.Fatalf("disabled hardware trust = %v, %v; want no verifier", v, err)
	}
	v, err := startHardwareTrust(ctx, config.HardwareTrust{ManufacturerKeys: []string{manufacturer}}, log)
	if err != nil || v == nil || v.Registry() != nil {
		t.Fatalf("keys only = %v, %v", v, err)
	}

	cfg := config.HardwareTrust{ManufacturerKeys: []string{manufacturer}, Registry: registry,
		RegistryKeys: []string{operations}, RegistryReload: 5 * time.Millisecond}
	// A configured registry that is absent stops startup: a collector must not
	// serve while unable to honour a withdrawal it was told to enforce.
	failures := testutil.ToFloat64(metrics.HardwareRegistryReloadFailuresTotal)
	if _, err := startHardwareTrust(ctx, cfg, log); err == nil {
		t.Fatal("started without the configured registry")
	}
	if got := testutil.ToFloat64(metrics.HardwareRegistryReloadFailuresTotal); got != failures+1 {
		t.Errorf("reload failures = %v, want %v", got, failures+1)
	}

	writeRegistry(t, registry, 7, signer)
	v, err = startHardwareTrust(ctx, cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	if v.Registry() == nil || testutil.ToFloat64(metrics.HardwareRegistrySequence) != 7 ||
		testutil.ToFloat64(metrics.HardwareRegistryIssuedTimestampSeconds) != 1789650007 ||
		testutil.ToFloat64(metrics.HardwareRegistryBoards) != 0 {
		t.Fatalf("registry gauges = sequence %v issued %v boards %v", testutil.ToFloat64(metrics.HardwareRegistrySequence),
			testutil.ToFloat64(metrics.HardwareRegistryIssuedTimestampSeconds), testutil.ToFloat64(metrics.HardwareRegistryBoards))
	}
	// A published update is adopted without a restart.
	writeRegistry(t, registry, 8, signer)
	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(metrics.HardwareRegistrySequence) != 8 {
		if time.Now().After(deadline) {
			t.Fatal("registry update was not adopted")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestStartHardwareTrustRecordsTheRegistrySequence: with registry_state a
// restarted daemon refuses a registry older than the one it last adopted.
func TestStartHardwareTrustRecordsTheRegistrySequence(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	dir := t.TempDir()
	manufacturer, _ := pinnedKey(t, dir, "manufacturer.pem")
	operations, signer := pinnedKey(t, dir, "registry.pem")
	registry, state := filepath.Join(dir, "registry.json"), filepath.Join(dir, "registry.state")
	cfg := config.HardwareTrust{ManufacturerKeys: []string{manufacturer}, Registry: registry,
		RegistryKeys: []string{operations}, RegistryReload: time.Hour, RegistryState: state}

	writeRegistry(t, registry, 9, signer)
	running, stop := context.WithCancel(context.Background())
	if _, err := startHardwareTrust(running, cfg, log); err != nil {
		t.Fatal(err)
	}
	stop()
	if recorded, err := commissioning.ReadRegistryState(state); err != nil || recorded != 9 {
		t.Fatalf("recorded sequence = %d, %v", recorded, err)
	}

	writeRegistry(t, registry, 8, signer)
	restarted, stopAgain := context.WithCancel(context.Background())
	defer stopAgain()
	if _, err := startHardwareTrust(restarted, cfg, log); err == nil || !strings.Contains(err.Error(), "already adopted") {
		t.Fatalf("restart with a rolled-back registry: err = %v, want a refusal", err)
	}
}
