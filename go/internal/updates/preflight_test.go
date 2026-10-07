package updates

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPreflightReadsWithoutTheInstanceLock: a configuration check must refuse
// exactly the state file Open would refuse, while a daemon that holds the
// instance lock keeps it and the check creates nothing.
func TestPreflightReadsWithoutTheInstanceLock(t *testing.T) {
	m, c := setup(t) // holds the lock and has committed one record
	if err := c.Preflight(); err != nil {
		t.Fatalf("preflight beside the running manager: %v", err)
	}
	if _, err := Open(c); err == nil {
		t.Fatal("instance lock was lost to the preflight")
	}
	m.Close()

	if err := os.Chmod(c.StateFile, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Preflight(); err == nil || !strings.Contains(err.Error(), "private") {
		t.Errorf("group-readable state: err = %v, want a privacy refusal", err)
	}
	if err := os.Chmod(c.StateFile, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(c.StateFile, []byte(`{"k":{"device":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Preflight(); err == nil || !strings.Contains(err.Error(), "invalid persisted update record") {
		t.Errorf("record under a foreign key: err = %v, want a record refusal", err)
	}
	if err := os.WriteFile(c.StateFile, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := c.Preflight(); err == nil {
		t.Error("truncated state file accepted")
	}
	if err := os.Remove(c.StateFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(c.StateFile, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := c.Preflight(); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Errorf("directory in place of the state file: err = %v, want a regular-file refusal", err)
	}

	// Nothing on disk yet is the first-start case: fine, and still nothing on disk.
	fresh := c
	fresh.StateFile = filepath.Join(t.TempDir(), "state", "updates.json")
	if err := fresh.Preflight(); err != nil {
		t.Fatalf("absent state file refused: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(fresh.StateFile)); !os.IsNotExist(err) {
		t.Fatalf("preflight created the state directory: %v", err)
	}
	if (Config{}).Preflight() != nil {
		t.Fatal("disabled update controls have nothing to preflight")
	}
}
