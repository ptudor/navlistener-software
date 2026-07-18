package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// regression fix/regression fix/regression fix guard tests: secret-bearing file permissions, sizing
// ceilings, and non-loopback bind warnings.

func loadFromString(t *testing.T, body string, perm os.FileMode) (*Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestConfigPermissionWarningWhenSecretsWorldReadable(t *testing.T) {
	// A DSN makes the file credential-bearing; 0644 must warn (not fail).
	cfg, err := loadFromString(t, "[store]\ndsn = \"postgres://nav:pw@localhost/nav\"\n", 0o644)
	if err != nil {
		t.Fatalf("Load: %v (permission finding must be a warning, not an error)", err)
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "group/world-readable") {
			found = true
		}
	}
	if !found {
		t.Errorf("no permission warning for a world-readable credential-bearing config; warnings = %v", cfg.Warnings)
	}

	// The same file at 0600 is clean.
	cfg, err = loadFromString(t, "[store]\ndsn = \"postgres://nav:pw@localhost/nav\"\n", 0o600)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "group/world-readable") {
			t.Errorf("unexpected permission warning on a 0600 config: %v", w)
		}
	}

	// A credential-less config may be world-readable without noise.
	cfg, err = loadFromString(t, "[logging]\nlevel = \"info\"\n", 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "group/world-readable") {
			t.Errorf("permission warning on a secret-less config: %v", w)
		}
	}
}

func TestPushTLSKeyMustNotBeGroupOrWorldReadable(t *testing.T) {
	certPath, keyPath := testKeypair(t)
	if err := os.Chmod(keyPath, 0o640); err != nil {
		t.Fatal(err)
	}
	c := defaults()
	c.Push.Addr = "127.0.0.1:5580"
	c.Push.TLSCert = certPath
	c.Push.TLSKey = keyPath
	c.Push.Observers = []PushObserver{{
		Station:     "obs1",
		TokenSHA256: strings.Repeat("ab", 32),
		Feeds:       []string{"ubx"},
	}}
	err := c.finalize()
	if err == nil || !strings.Contains(err.Error(), "group/world-readable") {
		t.Fatalf("finalize = %v, want a hard error for a 0640 push.tls_key (sshd/postgres precedent)", err)
	}
	if err := os.Chmod(keyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.finalize(); err != nil {
		t.Fatalf("finalize with a 0600 key: %v, want ok", err)
	}
}

func TestSizingCeilings(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Config)
		want string
	}{
		{"shards", func(c *Config) { c.State.Shards = maxShards + 1 }, "state.shards"},
		{"batch_size", func(c *Config) { c.Store.BatchSize = maxBatchSize + 1 }, "store.batch_size"},
		{"max_conns", func(c *Config) { c.Push.MaxConns = maxPushConns + 1 }, "push.max_conns"},
	} {
		c := defaults()
		tc.mut(c)
		err := c.finalize()
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s over ceiling: finalize = %v, want an error naming %s", tc.name, err, tc.want)
		}
	}
	// The ceilings themselves are valid values.
	c := defaults()
	c.State.Shards = maxShards
	c.Store.BatchSize = maxBatchSize
	c.Push.MaxConns = maxPushConns
	if err := c.finalize(); err != nil {
		t.Errorf("at-ceiling values: finalize = %v, want ok", err)
	}
}

func TestNonLoopbackBindWarnings(t *testing.T) {
	c := defaults()
	c.Metrics.Addr = "0.0.0.0:9100"
	c.Serve.Addr = "192.0.2.10:8080"
	if err := c.finalize(); err != nil {
		t.Fatalf("finalize: %v (public binds must warn, not fail)", err)
	}
	var metricsWarn, serveWarn bool
	for _, w := range c.Warnings {
		if strings.Contains(w, "metrics.addr") {
			metricsWarn = true
		}
		if strings.Contains(w, "serve.addr") {
			serveWarn = true
		}
	}
	if !metricsWarn || !serveWarn {
		t.Errorf("missing non-loopback warnings (metrics=%v serve=%v): %v", metricsWarn, serveWarn, c.Warnings)
	}

	// Loopback binds stay silent.
	c = defaults() // metrics 127.0.0.1:9100
	c.Serve.Addr = "localhost:8080"
	if err := c.finalize(); err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 0 {
		t.Errorf("loopback binds produced warnings: %v", c.Warnings)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:9100":   true,
		"127.8.9.1:9100":   true, // whole 127/8 block
		"localhost:8080":   true,
		"[::1]:8080":       true,
		"0.0.0.0:9100":     false,
		":9100":            false, // empty host = every interface
		"192.0.2.10:8080":  false,
		"[2001:db8::]:443": false,
		"myhost:8080":      false, // unprovable without resolving — conservative
	}
	for addr, want := range cases {
		if got := isLoopbackHost(addr); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", addr, got, want)
		}
	}
}
