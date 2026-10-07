package updates

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/navlistener/internal/wire"
)

func TestCatalogUsesExactBoundedObjectsAndRejectsTampering(t *testing.T) {
	for _, track := range []string{"trusted", "open"} {
		t.Run(track, func(t *testing.T) { catalog(t, track) })
	}
}

// writeObject stores one JSON object below root and returns its reference.
func writeObject(t *testing.T, root, path string, value any) reference {
	t.Helper()
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

// Each track reads only its own published tree; the other stays empty here.
func catalog(t *testing.T, track string) {
	m, _ := setup(t)
	root, other := m.config.Repository, "open"
	if track == "open" {
		root, other = m.config.OpenRepository, "trusted"
	}
	write := func(path string, value any) reference { return writeObject(t, root, path, value) }
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

// A channel held at 0 % still names its release: the operator sees a choice
// that is held, not an empty channel. Only a percentage outside the
// publisher's 0..100 is malformed.
func TestCatalogReportsAHeldChannelAsAChoice(t *testing.T) {
	m, _ := setup(t)
	root := m.config.Repository
	publish := func(percentage int) {
		t.Helper()
		value := map[string]any{"schema": 1, "generation": 4, "release_sequence": 31, "percentage": percentage, "priority": "urgent",
			"withdrawn": []uint64{}, "advisory": map[string]string{"classification": "security", "summary": "Held after an incident"}}
		target := writeObject(t, root, "targets/channels/draft.json", value)
		final := filepath.Join(root, "targets/channels/"+target.Hashes["sha256"]+".lab.json")
		if err := os.Rename(filepath.Join(root, "targets/channels/draft.json"), final); err != nil {
			t.Fatal(err)
		}
		metadata := func(fields map[string]any) map[string]any {
			fields["version"] = 1
			fields["expires"] = "2030-01-01T00:00:00Z"
			return map[string]any{"signed": fields}
		}
		channel := writeObject(t, root, "metadata/1.lab.json", metadata(map[string]any{"targets": map[string]reference{"channels/lab.json": target}}))
		snapshot := writeObject(t, root, "metadata/1.snapshot.json", metadata(map[string]any{"meta": map[string]reference{"lab.json": channel}}))
		writeObject(t, root, "metadata/timestamp.json", metadata(map[string]any{"meta": map[string]reference{"snapshot.json": snapshot}}))
	}
	publish(0)
	choice, err := m.choice("trusted", "lab")
	if err != nil {
		t.Fatalf("held channel reported as having no choice: %v", err)
	}
	if choice.Percentage != 0 || choice.Release != 31 || choice.Generation != 4 {
		t.Fatalf("held choice = %+v", choice)
	}
	if choice.Priority != "urgent" || choice.Classification != "security" || choice.Advisory != "Held after an incident" {
		t.Fatalf("advisory fields not surfaced: %+v", choice)
	}
	for _, malformed := range []int{-1, 101} {
		publish(malformed)
		if _, err := m.choice("trusted", "lab"); err == nil {
			t.Fatalf("percentage %d accepted", malformed)
		}
	}
}

// The served tree's expiry dates are reported as they are, including expired
// ones, and only the latest root counts: it is where a device's walk ends.
func TestCatalogReportsRolesExpiringSoon(t *testing.T) {
	m, _ := setup(t)
	now, root, day := m.now(), m.config.Repository, 24*time.Hour
	signed := func(version uint64, expires time.Time, fields map[string]any) map[string]any {
		fields["version"] = version
		fields["expires"] = expires.UTC().Format(time.RFC3339)
		return map[string]any{"signed": fields}
	}
	write := func(path string, version uint64, expires time.Time, fields map[string]any) reference {
		return writeObject(t, root, path, signed(version, expires, fields))
	}
	expected := map[string]time.Time{"targets": now.Add(60 * day), "releases": now.Add(400 * day), "stable": now.Add(30 * day),
		"canary": now.Add(30 * day), "lab": now.Add(-time.Hour), "snapshot": now.Add(30 * day), "timestamp": now.Add(3 * day), "root": now.Add(100 * day)}
	meta := map[string]reference{}
	for _, role := range []string{"targets", "releases", "stable", "canary", "lab"} {
		meta[role+".json"] = write("metadata/1."+role+".json", 1, expected[role], map[string]any{"targets": map[string]any{}})
	}
	snapshot := write("metadata/1.snapshot.json", 1, expected["snapshot"], map[string]any{"meta": meta})
	write("metadata/timestamp.json", 1, expected["timestamp"], map[string]any{"meta": map[string]reference{"snapshot.json": snapshot}})
	write("metadata/1.root.json", 1, now.Add(-day), map[string]any{"keys": map[string]any{}, "roles": map[string]any{}})
	write("metadata/2.root.json", 2, expected["root"], map[string]any{"keys": map[string]any{}, "roles": map[string]any{}})
	expiry, err := m.lifetimes(root)
	if err != nil {
		t.Fatal(err)
	}
	states := map[string]string{"root": "", "targets": "expiring", "releases": "", "stable": "", "canary": "", "lab": "expired", "snapshot": "", "timestamp": "expiring"}
	for role, want := range expected {
		if !expiry[role].Equal(want) {
			t.Fatal(role, expiry[role], want)
		}
		if state := expiryState(role, expiry[role], now); state != states[role] {
			t.Fatal(role, state, states[role])
		}
	}
	var logged bytes.Buffer
	m.observeLifetimes(slog.New(slog.NewTextHandler(&logged, nil)))
	for _, want := range []string{"expires soon", "role=targets", "role=timestamp", "expired", "role=lab", "track=trusted", "unreadable", "track=open"} {
		if !strings.Contains(logged.String(), want) {
			t.Fatal(want, logged.String())
		}
	}
	for _, unwanted := range []string{"role=root", "role=releases", "role=stable"} {
		if strings.Contains(logged.String(), unwanted) {
			t.Fatal(unwanted, logged.String())
		}
	}
	if v := testutil.ToFloat64(metadataExpiry.WithLabelValues("trusted", "targets")); v != float64(expected["targets"].Unix()) {
		t.Fatal(v)
	}
	if v := testutil.ToFloat64(metadataExpiry.WithLabelValues("trusted", "root")); v != float64(expected["root"].Unix()) {
		t.Fatal(v)
	}
	if v := testutil.ToFloat64(metadataExpiry.WithLabelValues("open", "root")); v != 0 {
		t.Fatal("unreadable open track still reports", v)
	}
	// A root whose version disagrees with its name is refused, not misreported.
	write("metadata/3.root.json", 5, expected["root"], map[string]any{"keys": map[string]any{}, "roles": map[string]any{}})
	if _, err = m.lifetimes(root); err == nil {
		t.Fatal("misnamed root accepted")
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
