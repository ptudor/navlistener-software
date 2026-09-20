package commissioning

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRegistryStateRoundTripAndHardening(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.state")
	if got, err := ReadRegistryState(path, testManufacturerAuthority); err != nil || got != 0 {
		t.Fatalf("absent state = %d, %v; want 0 and no error", got, err)
	}
	if err := writeRegistryState(path, testManufacturerAuthority, 42); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRegistryState(path, testManufacturerAuthority); err != nil || got != 42 {
		t.Fatalf("state = %d, %v", got, err)
	}
	if _, err := ReadRegistryState(path, "other-manufacturer"); err == nil {
		t.Fatal("registry sequence floor reused across manufacturer authorities")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode = %v, %v; want 0600", info.Mode().Perm(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("atomic write left %d entries behind", len(entries))
	}

	for name, write := range map[string]func(string) error{
		"writable by others": func(p string) error {
			if err := os.WriteFile(p, []byte(`{"registry_sequence":1}`), 0o600); err != nil {
				return err
			}
			return os.Chmod(p, 0o666)
		},
		"not JSON":    func(p string) error { return os.WriteFile(p, []byte("42"+"x"), 0o600) },
		"no sequence": func(p string) error { return os.WriteFile(p, []byte(`{}`), 0o600) },
		"oversized":   func(p string) error { return os.WriteFile(p, make([]byte, registryStateMaxBytes+1), 0o600) },
		"a directory": func(p string) error { return os.Mkdir(p, 0o700) },
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "registry.state")
			if err := write(p); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadRegistryState(p, testManufacturerAuthority); err == nil {
				t.Fatal("unusable state file accepted")
			}
		})
	}
}

// TestRegistryFloorSurvivesARestart: a new process that has recorded sequence 5
// refuses a validly signed sequence 4 even as its very first registry, which the
// in-memory check alone cannot do.
func TestRegistryFloorSurvivesARestart(t *testing.T) {
	mfg, mfgKeys := testSigner(t)
	ops, opsKeys := testSigner(t)
	record, _ := Sign(trustedStatement(t), mfg)
	dir := t.TempDir()
	registry, state := filepath.Join(dir, "registry.json"), filepath.Join(dir, "registry.state")
	newer, _ := SignRegistry(registryFor(t, 5, StatusRevoked, record), ops)
	older, _ := SignRegistry(registryFor(t, 4, StatusActive, record), ops)

	process := func(t *testing.T) *Verifier {
		t.Helper()
		v, err := NewVerifier(testManufacturerAuthority, mfgKeys)
		if err != nil {
			t.Fatal(err)
		}
		if err := v.UseRegistry(opsKeys, false); err != nil {
			t.Fatal(err)
		}
		if err := v.UseRegistryState(state); err != nil {
			t.Fatal(err)
		}
		return v
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := os.WriteFile(registry, newer, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := process(t).WatchRegistry(ctx, registry, time.Hour, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadRegistryState(state, testManufacturerAuthority); err != nil || got != 5 {
		t.Fatalf("recorded sequence = %d, %v", got, err)
	}

	// The registry file is rolled back while the collector is down.
	if err := os.WriteFile(registry, older, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := process(t)
	if err := restarted.WatchRegistry(ctx, registry, time.Hour, nil, nil); err == nil {
		t.Fatal("restarted collector adopted a registry older than the one it had recorded")
	}
	if restarted.Registry() != nil {
		t.Fatal("refused registry is in force")
	}
	// A configuration check predicts the same refusal without writing anything.
	check, _ := NewVerifier(testManufacturerAuthority, mfgKeys)
	if err := check.UseRegistry(opsKeys, false); err != nil {
		t.Fatal(err)
	}
	floor, err := ReadRegistryState(state, testManufacturerAuthority)
	if err != nil {
		t.Fatal(err)
	}
	check.SetRegistryFloor(floor)
	if _, err := check.LoadRegistry(older); err == nil {
		t.Fatal("configuration check accepted what the daemon refuses")
	}
	if _, err := check.LoadRegistry(newer); err != nil {
		t.Fatalf("configuration check refused the recorded registry: %v", err)
	}
	check.SetRegistryFloor(1) // a floor never moves down
	if _, err := check.LoadRegistry(older); err == nil {
		t.Fatal("floor was lowered")
	}
}

func TestRegistryStateThatCannotBeWritten(t *testing.T) {
	mfg, mfgKeys := testSigner(t)
	ops, opsKeys := testSigner(t)
	record, _ := Sign(trustedStatement(t), mfg)
	dir := t.TempDir()
	registry := filepath.Join(dir, "registry.json")
	first, _ := SignRegistry(registryFor(t, 1, StatusActive, record), ops)
	if err := os.WriteFile(registry, first, 0o600); err != nil {
		t.Fatal(err)
	}
	v, _ := NewVerifier(testManufacturerAuthority, mfgKeys)
	if err := v.UseRegistry(opsKeys, false); err != nil {
		t.Fatal(err)
	}
	// The state directory does not exist: recording is impossible.
	if err := v.UseRegistryState(filepath.Join(dir, "absent", "registry.state")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := v.WatchRegistry(ctx, registry, time.Hour, nil, nil); err == nil {
		t.Fatal("collector started although it cannot record the registry sequence it was told to keep")
	}
}

// TestRegistryReloadIsAdoptedEvenWhenItCannotBeRecorded: a reload may carry a
// withdrawal. Refusing it because the state file cannot be written would leave
// the withdrawn board trusted, so it is adopted and the failure is reported.
func TestRegistryReloadIsAdoptedEvenWhenItCannotBeRecorded(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a privileged process can write into a read-only directory")
	}
	mfg, mfgKeys := testSigner(t)
	ops, opsKeys := testSigner(t)
	record, _ := Sign(trustedStatement(t), mfg)
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(stateDir, 0o700) })
	registry, state := filepath.Join(dir, "registry.json"), filepath.Join(stateDir, "registry.state")
	first, _ := SignRegistry(registryFor(t, 1, StatusActive, record), ops)
	if err := os.WriteFile(registry, first, 0o600); err != nil {
		t.Fatal(err)
	}
	v, _ := NewVerifier(testManufacturerAuthority, mfgKeys)
	if err := v.UseRegistry(opsKeys, false); err != nil {
		t.Fatal(err)
	}
	if err := v.UseRegistryState(state); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reports := make(chan error, 16)
	if err := v.WatchRegistry(ctx, registry, 5*time.Millisecond, nil, func(_ *RegistryIndex, err error) { reports <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-reports; err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stateDir, 0o500); err != nil {
		t.Fatal(err)
	}
	withdrawal, _ := SignRegistry(registryFor(t, 2, StatusRevoked, record), ops)
	if err := os.WriteFile(registry, withdrawal, 0o600); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	for reported := false; !reported; {
		select {
		case err := <-reports:
			reported = err != nil
		case <-deadline:
			t.Fatal("the unrecorded sequence was never reported")
		}
	}
	if v.Registry().Sequence != 2 {
		t.Fatalf("registry in force = %d, want the withdrawal to have been adopted", v.Registry().Sequence)
	}
	if got, err := ReadRegistryState(state, testManufacturerAuthority); err != nil || got != 1 {
		t.Fatalf("recorded sequence = %d, %v; want the last one that could be written", got, err)
	}
}
