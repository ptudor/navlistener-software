package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestStationConfiguredInOnePlace: a dial [[ingest]] source and a
// [[push.observer]] may not share a station id. Both would feed live state under
// one Source key with two different observer contexts, so the collision is
// refused at Load naming both tables — including a disabled dial entry, which
// still registers its declaration and identity.
func TestStationConfiguredInOnePlace(t *testing.T) {
	cert, key := testKeypair(t)
	write := func(t *testing.T, ingest string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "navlistener.toml")
		body := ingest + `
[push]
addr = "0.0.0.0:5580"
tls_cert = "` + cert + `"
tls_key = "` + key + `"

[[push.observer]]
station = "obs1"
token_sha256 = "` + goodHash + `"
feeds = ["ubx"]
`
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	for name, ingest := range map[string]string{
		"enabled dial source": `[[ingest]]
name = "obs1"
type = "ubx"
addr = "10.0.0.5:5000"
`,
		"disabled dial source": `[[ingest]]
name = "obs1"
type = "ubx"
addr = "10.0.0.5:5000"
disabled = true
`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(write(t, ingest))
			if err == nil {
				t.Fatal("a station configured as both a dial source and a push observer was accepted")
			}
			for _, table := range []string{"push.observer", "[[ingest]]", `"obs1"`} {
				if !strings.Contains(err.Error(), table) {
					t.Errorf("error %q does not name %s", err, table)
				}
			}
		})
	}
	distinct := `[[ingest]]
name = "obs2"
type = "ubx"
addr = "10.0.0.5:5000"
`
	if _, err := Load(write(t, distinct)); err != nil {
		t.Fatalf("distinct dial and push station names refused: %v", err)
	}
}

// TestPushObserverBoardNamespaceReserved: the control plane refuses a software
// station whose name begins "board-" in any letter case; the config-file
// provider is the other authorization source for push stations and must apply
// the same reservation, or a config row could occupy a board's canonical name.
func TestPushObserverBoardNamespaceReserved(t *testing.T) {
	cert, key := testKeypair(t)
	for _, station := range []string{
		"Board-0003-00112233445566778899aabbccddeeff",
		"board-0003-00112233445566778899aabbccddeeff",
		"BOARD-x",
	} {
		c := pushConfig(Push{
			Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key,
			Observers: []PushObserver{{Station: station, TokenSHA256: goodHash, Feeds: []string{"ubx"}}},
		})
		err := c.finalizePush()
		if err == nil || !strings.Contains(err.Error(), "reserved for hardware enrollment") || !strings.Contains(err.Error(), "board-") {
			t.Errorf("station %q: err = %v, want the reserved-namespace refusal", station, err)
		}
	}
	// A name that merely contains "board" without the prefix is not a board's.
	ok := pushConfig(Push{
		Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key,
		Observers: []PushObserver{{Station: "boardroom-1", TokenSHA256: goodHash, Feeds: []string{"ubx"}}},
	})
	if err := ok.finalizePush(); err != nil {
		t.Errorf("station boardroom-1 refused: %v", err)
	}
}
