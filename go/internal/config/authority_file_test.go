package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptudor/navlistener/internal/testauthority"
)

func quotedList(paths []string) string {
	return `["` + strings.Join(paths, `", "`) + `"]`
}

// authorityFixture writes an authority file for one operational and one
// manufacturer authority, with a signed registry at sequence 4 and a registry
// state path the collector owns.
func authorityFixture(t *testing.T) (dir, path, state string) {
	t.Helper()
	p := testauthority.New(t, "navlisten", "ab")
	dir = t.TempDir()
	operations, signer := trustKey(t, dir, "registry.pem")
	registry := signedRegistryAt(t, dir, 4, signer)
	state = filepath.Join(dir, "registry.state")
	body := `
[[operational_authority]]
id = "navlisten"
enabled = true
roots = ` + quotedList(p.Config.Roots) + `
issuers = ` + quotedList(p.Config.Issuers) + `
manufacturer_authorities = ["` + testManufacturerAuthority + `"]

[[manufacturer_authority]]
enabled = true
manufacturer_authority_id = "` + testManufacturerAuthority + `"
manufacturer_keys = ` + quotedList(p.ManufacturerPaths) + `
product_policy = [{product = 1, revision = 258}]
registry = "` + registry + `"
registry_keys = ["` + operations + `"]
registry_state = "` + state + `"
`
	path = filepath.Join(dir, "authorities.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path, state
}

func collectorWithAuthorityFile(t *testing.T, dir, authorities, extra string) string {
	t.Helper()
	cert, key := testKeypair(t)
	body := `authority_file = "` + authorities + `"
` + extra + `
[push]
addr = "127.0.0.1:5580"
tls_cert = "` + cert + `"
tls_key = "` + key + `"
`
	path := filepath.Join(dir, "navlistener.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestAuthorityFileSharedByCollectorAndControlPlane: both processes register the
// same authorities from one credential-free file. The control plane needs no
// push listener or TLS key, and leaves the registry rollback floor to its own
// database instead of reading the collector's private state file.
func TestAuthorityFileSharedByCollectorAndControlPlane(t *testing.T) {
	dir, authorities, state := authorityFixture(t)
	collector, err := Load(collectorWithAuthorityFile(t, dir, authorities, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.Authorities.Allows("navlisten", testManufacturerAuthority); err != nil {
		t.Fatalf("collector pairing: %v", err)
	}
	if got := collector.ManufacturerAuthorities[0].RegistryState; got != state {
		t.Fatalf("collector registry_state = %q, want %q", got, state)
	}
	control, err := LoadAuthorities(authorities)
	if err != nil {
		t.Fatalf("control plane without push settings: %v", err)
	}
	if err := control.Set.Allows("navlisten", testManufacturerAuthority); err != nil {
		t.Fatalf("control plane pairing: %v", err)
	}
	if got := control.ManufacturerAuthorities[0].RegistryState; got != "" {
		t.Fatalf("control plane kept the collector's registry_state %q", got)
	}

	// The collector has since adopted sequence 5; the file holds sequence 4.
	if err := os.WriteFile(state, []byte(`{"manufacturer_authority_id":"`+testManufacturerAuthority+`","registry_sequence":5}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(collectorWithAuthorityFile(t, dir, authorities, "")); err == nil || !strings.Contains(err.Error(), "already adopted") {
		t.Fatalf("collector accepted a rolled-back registry: %v", err)
	}
	if _, err := LoadAuthorities(authorities); err != nil {
		t.Fatalf("control plane read the collector's state file: %v", err)
	}
}

func TestAuthorityFileRefusals(t *testing.T) {
	t.Run("inline tables beside the file", func(t *testing.T) {
		dir, authorities, _ := authorityFixture(t)
		extra := "[[operational_authority]]\nid = \"other\"\nenabled = true\n"
		if _, err := Load(collectorWithAuthorityFile(t, dir, authorities, extra)); err == nil || !strings.Contains(err.Error(), "cannot both be set") {
			t.Fatalf("err = %v, want a refusal of two authority sources", err)
		}
	})
	t.Run("group-writable file", func(t *testing.T) {
		dir, authorities, _ := authorityFixture(t)
		if err := os.Chmod(authorities, 0o664); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(collectorWithAuthorityFile(t, dir, authorities, "")); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("collector err = %v, want a writable-file refusal", err)
		}
		if _, err := LoadAuthorities(authorities); err == nil || !strings.Contains(err.Error(), "writable") {
			t.Fatalf("control plane err = %v, want a writable-file refusal", err)
		}
	})
	t.Run("collector settings in the authority file", func(t *testing.T) {
		_, authorities, _ := authorityFixture(t)
		f, err := os.OpenFile(authorities, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString("\n[authorization]\ndsn = \"host=/tmp\"\n"); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAuthorities(authorities); err == nil {
			t.Fatal("authority file accepted a credential-bearing table")
		}
	})
	t.Run("control plane without an operational authority", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "authorities.toml")
		if err := os.WriteFile(path, []byte("\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadAuthorities(path); err == nil || !strings.Contains(err.Error(), "no operational authority") {
			t.Fatalf("err = %v, want a missing operational authority refusal", err)
		}
	})
}
