package config

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/authority"
	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/testauthority"
)

const testManufacturerAuthority = "test-manufacturer"

func TestAuthorityKeyRolesAndNamespaces(t *testing.T) {
	ab, cd := testauthority.New(t, "navlisten", "ab"), testauthority.New(t, "customer", "ab", "cd")
	makeConfig := func() *Config {
		c := hardwareTrustConfig(t, HardwareTrust{ManufacturerAuthorityID: "ab", ManufacturerKeys: ab.ManufacturerPaths})
		c.OperationalAuthorities = []authority.Operational{ab.Config, cd.Config}
		c.Push.Observers[0].OperationalAuthorityID = "navlisten"
		c.ManufacturerAuthorities = append(c.ManufacturerAuthorities, HardwareTrust{Active: true, ManufacturerAuthorityID: "cd", ManufacturerKeys: cd.ManufacturerPaths, Products: []commissioning.ProductPolicy{{Product: 1, Revision: 99}}})
		return c
	}
	if err := makeConfig().finalize(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Config){
		"manufacturer key in two authorities": func(c *Config) { c.ManufacturerAuthorities[1].ManufacturerKeys = ab.ManufacturerPaths },
		"root slot zero offered as manufacturer": func(c *Config) {
			der, _ := x509.MarshalPKIXPublicKey(&ab.RootKeys[0].PublicKey)
			c.ManufacturerAuthorities[0].ManufacturerKeys = []string{testauthority.Write(t, t.TempDir(), "ca.pem", "PUBLIC KEY", der)}
		},
		"manufacturer and registry roles overlap": func(c *Config) {
			h := &c.ManufacturerAuthorities[0]
			h.RegistryKeys = ab.ManufacturerPaths
			_, signer := trustKey(t, t.TempDir(), "registry.pem")
			h.Registry = signedRegistry(t, t.TempDir(), signer)
		},
		"issuing key in two operational authorities": func(c *Config) {
			c.OperationalAuthorities[1].Roots = ab.Config.Roots
			c.OperationalAuthorities[1].Issuers = ab.Config.Issuers
		},
		"registry key in two manufacturer authorities": func(c *Config) {
			signer, err := commissioning.NewKeySigner(ab.RegistryKey, nil)
			if err != nil {
				t.Fatal(err)
			}
			for i := range c.ManufacturerAuthorities {
				h := &c.ManufacturerAuthorities[i]
				h.RegistryKeys = []string{ab.RegistryPath}
				h.Registry = filepath.Join(t.TempDir(), "registry.json")
				data, err := commissioning.SignRegistry(commissioning.Registry{ManufacturerAuthorityID: h.ManufacturerAuthorityID, Sequence: 1, IssuedAt: time.Now().UTC(), LedgerHead: strings.Repeat("ab", 32)}, signer)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(h.Registry, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
		},
		"unknown product policy":                 func(c *Config) { c.ManufacturerAuthorities[1].Products = nil },
		"unknown enrolled operational authority": func(c *Config) { c.Push.Observers[0].OperationalAuthorityID = "device-claim" },
	} {
		t.Run(name, func(t *testing.T) {
			c := makeConfig()
			mutate(c)
			if err := c.finalize(); err == nil {
				t.Fatal("ambiguous or unsupported authority configuration accepted")
			}
		})
	}
}

// trustKey writes a P-256 public key PEM and returns its path with a signer
// for the matching private key.
func trustKey(t *testing.T, dir, name string) (string, commissioning.Signer) {
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

func signedRegistry(t *testing.T, dir string, signer commissioning.Signer) string {
	t.Helper()
	return signedRegistryAt(t, dir, 1, signer)
}

func signedRegistryAt(t *testing.T, dir string, sequence uint64, signer commissioning.Signer) string {
	t.Helper()
	data, err := commissioning.SignRegistry(commissioning.Registry{
		ManufacturerAuthorityID: testManufacturerAuthority,
		Sequence:                sequence, IssuedAt: time.Unix(1789650000, 0).UTC(), LedgerHead: strings.Repeat("ab", 32),
	}, signer)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "registry.json")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func hardwareTrustConfig(t *testing.T, h HardwareTrust) *Config {
	t.Helper()
	cert, key := testKeypair(t)
	c := pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key,
		Observers: []PushObserver{{Station: "observer16", TokenSHA256: goodHash, Feeds: []string{"ubx"}}}})
	h.Active = true
	h.Products = []commissioning.ProductPolicy{{Product: 1, Revision: 258}}
	c.ManufacturerAuthorities = ManufacturerAuthorities{h}
	return c
}

func TestHardwareTrustDisabledByDefault(t *testing.T) {
	c := defaults()
	if err := c.finalize(); err != nil {
		t.Fatal(err)
	}
	if c.ManufacturerAuthorities.Enabled() {
		t.Fatal("hardware trust enabled with no manufacturer keys")
	}
}

func TestHardwareTrustValid(t *testing.T) {
	dir := t.TempDir()
	manufacturer, _ := trustKey(t, dir, "manufacturer.pem")
	operations, signer := trustKey(t, dir, "registry.pem")

	keysOnly := hardwareTrustConfig(t, HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}})
	if err := keysOnly.finalize(); err != nil {
		t.Fatalf("manufacturer keys alone rejected: %v", err)
	}
	if !keysOnly.ManufacturerAuthorities.Enabled() {
		t.Fatal("manufacturer keys did not enable hardware trust")
	}

	full := hardwareTrustConfig(t, HardwareTrust{
		ManufacturerAuthorityID: testManufacturerAuthority,
		ManufacturerKeys:        []string{manufacturer}, Registry: signedRegistry(t, dir, signer),
		RegistryKeys: []string{operations}, RequireRegistryEntry: true,
	})
	if err := full.finalize(); err != nil {
		t.Fatalf("complete hardware trust config rejected: %v", err)
	}
	if full.ManufacturerAuthorities[0].RegistryReload != 30*time.Second {
		t.Errorf("registry_reload default = %v, want 30s", full.ManufacturerAuthorities[0].RegistryReload)
	}
	verifier, err := full.ManufacturerAuthorities[0].NewVerifier()
	if err != nil {
		t.Fatal(err)
	}
	// NewVerifier pins keys only; adopting the registry is the daemon's watch.
	if verifier.Registry() != nil {
		t.Error("NewVerifier loaded a registry")
	}
}

func TestHardwareTrustRejectsIncompleteOrUnverifiableSettings(t *testing.T) {
	dir := t.TempDir()
	manufacturer, manufacturerSigner := trustKey(t, dir, "manufacturer.pem")
	operations, signer := trustKey(t, dir, "registry.pem")
	registry := signedRegistry(t, dir, signer)
	notPEM := filepath.Join(dir, "garbage.pem")
	if err := os.WriteFile(notPEM, []byte("not a key"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "foreign")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	foreignRegistry := signedRegistry(t, foreign, manufacturerSigner)

	cases := map[string]struct {
		h    HardwareTrust
		want string
	}{
		"registry without manufacturer keys": {HardwareTrust{Registry: registry, RegistryKeys: []string{operations}}, "require manufacturer_keys"},
		"require entry without any keys":     {HardwareTrust{RequireRegistryEntry: true}, "require manufacturer_keys"},
		"missing manufacturer authority":     {HardwareTrust{ManufacturerKeys: []string{manufacturer}}, "manufacturer_authority_id"},
		"missing manufacturer key file":      {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{filepath.Join(dir, "absent.pem")}}, "manufacturer_keys"},
		"manufacturer key is not PEM":        {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{notPEM}}, "manufacturer_keys"},
		"registry without registry keys":     {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}, Registry: registry}, "requires registry_keys"},
		"registry keys without a registry":   {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}, RegistryKeys: []string{operations}}, "without registry"},
		"require entry without a registry":   {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}, RequireRegistryEntry: true}, "without registry"},
		"reload without a registry":          {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}, RegistryReloads: "10s"}, "without registry"},
		"state without a registry":           {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}, RegistryState: filepath.Join(dir, "registry.state")}, "without registry"},
		"state without any keys":             {HardwareTrust{RegistryState: filepath.Join(dir, "registry.state")}, "require manufacturer_keys"},
		"relative state path": {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer},
			Registry: registry, RegistryKeys: []string{operations}, RegistryState: "registry.state"}, "absolute path"},
		"missing registry file": {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer},
			Registry: filepath.Join(dir, "absent.json"), RegistryKeys: []string{operations}}, "manufacturer_authority.registry"},
		"registry signed by an unpinned key": {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer},
			Registry: foreignRegistry, RegistryKeys: []string{operations}}, "pinned registry key"},
		"non-positive reload": {HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer},
			Registry: registry, RegistryKeys: []string{operations}, RegistryReloads: "0s"}, "registry_reload"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			err := hardwareTrustConfig(t, tc.h).finalize()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one mentioning %q", err, tc.want)
			}
		})
	}
	t.Run("evidence needs the push endpoint", func(t *testing.T) {
		c := defaults()
		c.ManufacturerAuthorities = ManufacturerAuthorities{{Active: true, ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}}}
		if err := c.finalize(); err == nil || !strings.Contains(err.Error(), "push endpoint") {
			t.Fatalf("err = %v, want a push endpoint requirement", err)
		}
	})
}

func TestHardwareTrustTOMLSpelling(t *testing.T) {
	dir := t.TempDir()
	manufacturer, _ := trustKey(t, dir, "manufacturer.pem")
	operations, signer := trustKey(t, dir, "registry.pem")
	registry := signedRegistry(t, dir, signer)
	cert, key := testKeypair(t)
	path := filepath.Join(dir, "navlistener.toml")
	body := `
[push]
addr = "127.0.0.1:5580"
tls_cert = "` + cert + `"
tls_key = "` + key + `"

[[manufacturer_authority]]
enabled = true
product_policy = [{product = 1, revision = 258}]
manufacturer_authority_id = "` + testManufacturerAuthority + `"
manufacturer_keys = ["` + manufacturer + `"]
registry = "` + registry + `"
registry_keys = ["` + operations + `"]
registry_reload = "45s"
require_registry_entry = true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	h := c.ManufacturerAuthorities[0]
	if !h.Enabled() || h.ManufacturerAuthorityID != testManufacturerAuthority || h.Registry != registry || h.RegistryReload != 45*time.Second || !h.RequireRegistryEntry || len(h.RegistryKeys) != 1 {
		t.Fatalf("decoded hardware_trust = %+v", h)
	}
}

// TestHardwareTrustRegistryStateParity: -check-config applies the recorded floor
// read-only, so it refuses exactly the registry the daemon would refuse at
// startup — and never creates or rewrites the state file.
func TestHardwareTrustRegistryStateParity(t *testing.T) {
	dir := t.TempDir()
	manufacturer, _ := trustKey(t, dir, "manufacturer.pem")
	operations, signer := trustKey(t, dir, "registry.pem")
	state := filepath.Join(dir, "registry.state")
	configFor := func(registry string, withState bool) *Config {
		h := HardwareTrust{ManufacturerAuthorityID: testManufacturerAuthority, ManufacturerKeys: []string{manufacturer}, Registry: registry, RegistryKeys: []string{operations}}
		if withState {
			h.RegistryState = state
		}
		return hardwareTrustConfig(t, h)
	}
	warned := func(c *Config) bool {
		for _, w := range c.Warnings {
			if strings.Contains(w, "registry_state") {
				return true
			}
		}
		return false
	}

	registry := signedRegistryAt(t, dir, 4, signer)
	first := configFor(registry, true)
	if err := first.finalize(); err != nil {
		t.Fatalf("no recorded sequence yet: %v", err)
	}
	if warned(first) {
		t.Error("warned about a missing registry_state that is set")
	}
	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Fatalf("configuration check created the state file: %v", err)
	}
	unrecorded := configFor(registry, false)
	if err := unrecorded.finalize(); err != nil || !warned(unrecorded) {
		t.Fatalf("registry without registry_state: err = %v, warned = %t; want a warning only", err, warned(unrecorded))
	}

	// The daemon has since adopted sequence 5; the file on disk is older.
	if err := os.WriteFile(state, []byte(`{"manufacturer_authority_id":"`+testManufacturerAuthority+`","registry_sequence":5}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := configFor(registry, true).finalize(); err == nil || !strings.Contains(err.Error(), "already adopted") {
		t.Fatalf("rolled-back registry: err = %v, want a refusal", err)
	}
	newerDir := filepath.Join(dir, "newer")
	if err := os.Mkdir(newerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := configFor(signedRegistryAt(t, newerDir, 5, signer), true).finalize(); err != nil {
		t.Fatalf("registry at the recorded sequence refused: %v", err)
	}
	if err := os.Chmod(state, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := configFor(registry, true).finalize(); err == nil || !strings.Contains(err.Error(), "registry_state") {
		t.Fatalf("world-writable state: err = %v, want a registry_state error", err)
	}
}
