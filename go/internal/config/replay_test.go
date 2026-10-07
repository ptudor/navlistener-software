package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func replayConfigFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "replay.toml")
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadForReplayDoesNotReadDaemonFiles(t *testing.T) {
	cert, key := testKeypair(t)
	body := fmt.Sprintf(`[collector]
instance_id = "replay"
[store]
dsn = "postgres:///replay"
[push]
addr = "127.0.0.1:4443"
tls_cert = %q
tls_key = %q
`, cert, key) + receptionSite + `
[[integrity.station]]
observer = "van"
mode = "mobile"
max_speed_mps = 35
[[integrity.station]]
observer = "mast"
mode = "fixed"
[[integrity.baseline]]
stations = ["roof", "mast"]
distance_m = 10
`
	path := replayConfigFile(t, body)
	daemon, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(key, 0); err != nil {
		t.Fatal(err)
	}
	// A privileged test process can bypass file mode bits. Keep the key
	// unavailable in that case so this still exercises a failed key read.
	if _, err := os.ReadFile(key); err == nil {
		if err := os.Remove(key); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "push.tls_cert/tls_key") {
		t.Fatalf("daemon accepted unavailable TLS key: %v", err)
	}
	replay, err := LoadForReplay(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(replay.IntegrityStations(), daemon.IntegrityStations()) {
		t.Fatal("replay station installations differ from daemon installations")
	}
	if replay.Collector.InstanceID != "replay" || replay.Store.DSN != "postgres:///replay" || len(replay.Warnings) != 0 {
		t.Fatalf("replay id=%q DSN=%q warnings=%v", replay.Collector.InstanceID, replay.Store.DSN, replay.Warnings)
	}

	// Authority files and inline manufacturer files are separate startup paths.
	for _, prefix := range []string{
		fmt.Sprintf("authority_file = %q\n", filepath.Join(t.TempDir(), "absent-authorities.toml")),
		fmt.Sprintf("[[manufacturer_authority]]\nmanufacturer_authority_id = \"manufacturer\"\nmanufacturer_keys = [%q]\nregistry = %q\nregistry_keys = [%q]\n", filepath.Join(t.TempDir(), "absent-keys"), filepath.Join(t.TempDir(), "absent-registry"), filepath.Join(t.TempDir(), "absent-registry-keys")),
	} {
		if _, err := LoadForReplay(replayConfigFile(t, prefix+body)); err != nil {
			t.Fatalf("replay loaded authority material: %v", err)
		}
	}
}

func TestLoadForReplayRemainsStrict(t *testing.T) {
	for name, body := range map[string]string{
		"syntax":                 "[store",
		"unknown section":        "[stor]\ndsn = \"postgres:///replay\"",
		"unknown daemon key":     "[push]\ntls_kee = \"key.pem\"",
		"collector id":           "[collector]\ninstance_id = \"bad id\"",
		"reception observer":     strings.ReplaceAll(receptionSite, "roof", "bad observer"),
		"reception position":     strings.ReplaceAll(receptionSite, "37.4219", "97.4219"),
		"integrity mode":         "[[integrity.station]]\nobserver = \"van\"\nmode = \"flying\"",
		"duplicate installation": "[[integrity.station]]\nobserver = \"roof\"\nmode = \"fixed\"\n[[integrity.station]]\nobserver = \"roof\"\nmode = \"fixed\"",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadForReplay(replayConfigFile(t, body)); err == nil {
				t.Fatal("invalid replay config accepted")
			}
		})
	}
	cfg, err := LoadForReplay(replayConfigFile(t, ""))
	if err != nil || cfg.Collector.InstanceID != "local" {
		t.Fatalf("default replay collector: %v, %v", cfg, err)
	}
}
