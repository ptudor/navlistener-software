package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ptudor/navlistener/internal/wire"
)

func TestCatalogUsesExactBoundedObjectsAndRejectsTampering(t *testing.T) {
	for _, track := range []string{"trusted", "open"} {
		t.Run(track, func(t *testing.T) { catalog(t, track) })
	}
}

// Each track reads only its own published tree; the other stays empty here.
func catalog(t *testing.T, track string) {
	m, _ := setup(t)
	root, other := m.config.Repository, "open"
	if track == "open" {
		root, other = m.config.OpenRepository, "trusted"
	}
	write := func(path string, value any) reference {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(root, path)
		if err = os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(full, data, 0600); err != nil {
			t.Fatal(err)
		}
		hash := sha256.Sum256(data)
		return reference{Version: 1, Length: int64(len(data)), Hashes: map[string]string{"sha256": hex.EncodeToString(hash[:])}}
	}
	value := map[string]any{"generation": 9007199254740993, "release_sequence": 31, "percentage": 100, "withdrawn": []uint64{}, "advisory": map[string]string{"summary": "Test advisory"}}
	target := write("targets/channels/draft.json", value)
	final := filepath.Join(root, "targets/channels/"+target.Hashes["sha256"]+".lab.json")
	if err := os.Rename(filepath.Join(root, "targets/channels/draft.json"), final); err != nil {
		t.Fatal(err)
	}
	metadata := func(fields map[string]any) map[string]any {
		fields["version"] = 1
		fields["expires"] = "2030-01-01T00:00:00Z"
		return map[string]any{"signed": fields}
	}
	channel := write("metadata/1.lab.json", metadata(map[string]any{"targets": map[string]reference{"channels/lab.json": target}}))
	snapshot := write("metadata/1.snapshot.json", metadata(map[string]any{"meta": map[string]reference{"lab.json": channel}}))
	write("metadata/timestamp.json", metadata(map[string]any{"meta": map[string]reference{"snapshot.json": snapshot}}))
	choice, err := m.choice(track, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if choice.Generation != 9007199254740993 || choice.Release != 31 {
		t.Fatal(choice)
	}
	if _, err = m.choice(other, "lab"); err == nil {
		t.Fatal("one track's release offered to the other")
	}
	if _, err = m.choice("test", "lab"); err == nil {
		t.Fatal("a test build was offered a release track's catalog")
	}
	if track == "trusted" {
		// Firmware and persisted records that predate the report follow the trusted track.
		for _, legacy := range []string{"unreported", ""} {
			if choice, err = m.choice(legacy, "lab"); err != nil || choice.Release != 31 {
				t.Fatal(legacy, choice, err)
			}
		}
	}
	if err = os.WriteFile(final, []byte(`{"release_sequence":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.choice(track, "lab"); err == nil {
		t.Fatal("changed target accepted")
	}
	if _, err = m.object(root, "metadata/../../outside", 4096, nil); err == nil {
		t.Fatal("path escape accepted")
	}
}

func TestTracksAreSeparateOptionalRepositories(t *testing.T) {
	_, c := setup(t)
	for name, change := range map[string]func(*Config){
		"trusted only": func(c *Config) { c.OpenRepository = "" },
		"open only":    func(c *Config) { c.Repository = "" },
	} {
		candidate := c
		change(&candidate)
		if err := candidate.Validate(); err != nil {
			t.Fatal(name, err)
		}
	}
	for name, change := range map[string]func(*Config){
		"no repository":  func(c *Config) { c.Repository, c.OpenRepository = "", "" },
		"relative open":  func(c *Config) { c.OpenRepository = "open-repository" },
		"shared tree":    func(c *Config) { c.OpenRepository = c.Repository + "/" },
		"open, no state": func(c *Config) { c.StateFile, c.Repository, c.Principals = "", "", nil },
	} {
		candidate := c
		change(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatal(name, "accepted")
		}
	}
	unserved := c
	unserved.OpenRepository = ""
	m := &Manager{config: unserved}
	if _, err := m.repository("open"); err == nil {
		t.Fatal("an unserved track has no catalog")
	}
}

func TestDriftComparesSecurityFlagsWithTheReportedTrack(t *testing.T) {
	for _, c := range []struct {
		profile  string
		security uint8
		drift    bool
	}{
		{"trusted", 31, false}, {"trusted", 16, true}, {"unreported", 31, false}, {"unreported", 16, true},
		{"open", 16, false}, {"open", 0, false}, {"open", 17, true}, {"open", 31, true}, {"test", 16, false}, {"test", 18, true},
	} {
		if drifted(wire.UpdateStatus{Profile: c.profile, Security: c.security}) != c.drift {
			t.Fatal(c)
		}
	}
}
