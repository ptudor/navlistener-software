package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptudor/navlistener/internal/updates"
)

// updatesConfig is a valid [updates] section with its state file at state, on
// top of the push and serve endpoints the section requires.
func updatesConfig(t *testing.T, state string) *Config {
	t.Helper()
	cert, key := testKeypair(t)
	c := pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key,
		Observers: []PushObserver{{Station: "observer16", TokenSHA256: goodHash, Feeds: []string{"ubx"}}}})
	c.Serve.Addr = "127.0.0.1:8080"
	c.Updates = updates.Config{
		StateFile:  state,
		Repository: filepath.Join(t.TempDir(), "repository"),
		Principals: []updates.Principal{{ID: "operator-1", TokenSHA256: goodHash,
			Devices: []updates.Device{{Observer: "observer16", Enrollment: "enrollment-1", Organization: "org-1", Collector: "local"}}}},
	}
	return c
}

// unwritableDir returns a directory the test user cannot create files in, or
// "" when the process is root (root bypasses the permission bits, so no such
// directory can be made; the rc.d preflight runs as the daemon user for the
// same reason).
func unwritableDir(t *testing.T) string {
	t.Helper()
	if os.Geteuid() == 0 {
		return ""
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	return dir
}

// TestUpdatesStateFilePreflight: the daemon creates the state directory, takes an
// instance lock beside the file and rewrites the file on every transition, so a
// configuration check must prove the directory writable and an existing file
// loadable — without creating anything or taking the lock, which a running
// daemon holds. Each of these used to pass -check-config and then exit the
// daemon after every listener was bound.
func TestUpdatesStateFilePreflight(t *testing.T) {
	base := t.TempDir()

	// A directory that does not exist yet is fine when its nearest existing
	// ancestor is writable (the daemon MkdirAlls it); the check creates nothing.
	missing := filepath.Join(base, "state", "deeper", "updates.json")
	if err := updatesConfig(t, missing).finalize(); err != nil {
		t.Fatalf("creatable state directory refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "state")); !os.IsNotExist(err) {
		t.Fatalf("configuration check created the state directory: %v", err)
	}
	if _, err := os.Stat(missing + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("configuration check created the lock file: %v", err)
	}

	if ro := unwritableDir(t); ro != "" {
		for _, state := range []string{
			filepath.Join(ro, "updates.json"),          // directory exists, not writable
			filepath.Join(ro, "state", "updates.json"), // ancestor not writable, so MkdirAll would fail
		} {
			err := updatesConfig(t, state).finalize()
			if err == nil || !strings.Contains(err.Error(), "updates.state_file") {
				t.Errorf("state_file %s under an unwritable directory: err = %v, want an updates.state_file error", state, err)
			}
		}
	}

	// An ancestor that is a file, not a directory.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updatesConfig(t, filepath.Join(file, "updates.json")).finalize(); err == nil || !strings.Contains(err.Error(), "updates.state_file") {
		t.Errorf("state_file under a regular file: err = %v, want an updates.state_file error", err)
	}

	// An existing state file is checked as the daemon would load it.
	state := filepath.Join(base, "updates.json")
	if err := os.WriteFile(state, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updatesConfig(t, state).finalize(); err != nil {
		t.Fatalf("valid empty state refused: %v", err)
	}
	if err := os.Chmod(state, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := updatesConfig(t, state).finalize(); err == nil || !strings.Contains(err.Error(), "updates.state_file") {
		t.Errorf("group-readable state file: err = %v, want an updates.state_file error", err)
	}
	if err := os.WriteFile(state, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := updatesConfig(t, state).finalize(); err == nil || !strings.Contains(err.Error(), "updates.state_file") {
		t.Errorf("corrupt state file: err = %v, want an updates.state_file error", err)
	}

	// Parity without the lock: a daemon holding the instance lock must not
	// make the configuration check fail, and the check must not steal the lock.
	live := updatesConfig(t, filepath.Join(base, "live", "updates.json"))
	m, err := updates.Open(live.Updates)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if err := live.finalize(); err != nil {
		t.Fatalf("configuration check beside a running daemon: %v", err)
	}
	if again, err := updates.Open(live.Updates); err == nil {
		again.Close()
		t.Fatal("instance lock was released by the configuration check")
	}
}

// TestRegistryStateDirectoryProbed: the daemon records the adopted registry
// sequence through a temporary file renamed into registry_state's directory,
// and never creates that directory, so -check-config must fail where the
// daemon's first registry load would — without creating the state file.
func TestRegistryStateDirectoryProbed(t *testing.T) {
	dir := t.TempDir()
	manufacturer, _ := trustKey(t, dir, "manufacturer.pem")
	operations, signer := trustKey(t, dir, "registry.pem")
	registry := signedRegistryAt(t, dir, 4, signer)
	configFor := func(state string) *Config {
		return hardwareTrustConfig(t, HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority,
			ManufacturerKeys: []string{manufacturer}, Registry: registry, RegistryKeys: []string{operations}, RegistryState: state})
	}

	writable := filepath.Join(dir, "state")
	if err := os.Mkdir(writable, 0o700); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(writable, "registry.state")
	if err := configFor(state).finalize(); err != nil {
		t.Fatalf("writable state directory refused: %v", err)
	}
	entries, err := os.ReadDir(writable)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("configuration check left %d entries in the state directory", len(entries))
	}

	err = configFor(filepath.Join(dir, "absent", "registry.state")).finalize()
	if err == nil || !strings.Contains(err.Error(), "registry_state") {
		t.Errorf("registry_state under a missing directory: err = %v, want a registry_state error", err)
	}
	if ro := unwritableDir(t); ro != "" {
		err = configFor(filepath.Join(ro, "registry.state")).finalize()
		if err == nil || !strings.Contains(err.Error(), "registry_state") {
			t.Errorf("registry_state under an unwritable directory: err = %v, want a registry_state error", err)
		}
	}
}
