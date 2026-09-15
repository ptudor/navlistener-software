package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCatalogUsesExactBoundedObjectsAndRejectsTampering(t *testing.T) {
	m, _ := setup(t)
	write := func(path string, value any) reference {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		full := filepath.Join(m.config.Repository, path)
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
	final := filepath.Join(m.config.Repository, "targets/channels/"+target.Hashes["sha256"]+".lab.json")
	if err := os.Rename(filepath.Join(m.config.Repository, "targets/channels/draft.json"), final); err != nil {
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
	choice, err := m.choice("lab")
	if err != nil {
		t.Fatal(err)
	}
	if choice.Generation != 9007199254740993 || choice.Release != 31 {
		t.Fatal(choice)
	}
	if err = os.WriteFile(final, []byte(`{"release_sequence":999}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = m.choice("lab"); err == nil {
		t.Fatal("changed target accepted")
	}
	if _, err = m.object("metadata/../../outside", 4096, nil); err == nil {
		t.Fatal("path escape accepted")
	}
}
