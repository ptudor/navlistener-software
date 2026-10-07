//go:build unix

package commissioning

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A state directory other accounts can write into without the sticky bit
// would let one of them rename its own file over the floor, so it is refused;
// the sticky bit, or a private directory, is accepted. Ownership by another
// account cannot be arranged unprivileged, so only the directory rule is
// exercised here; the owner check reads the same stat the mode check does.
func TestRegistryStateRefusesAReplaceableDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root owns every file it reads")
	}
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "registry.state")
	if err := writeRegistryState(path, testManufacturerAuthority, 7); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []os.FileMode{0o777, 0o770, 0o707} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRegistryState(path, testManufacturerAuthority); err == nil || !strings.Contains(err.Error(), "sticky") {
			t.Fatalf("directory mode %v: err = %v, want a refusal naming the sticky bit", mode, err)
		}
	}
	for _, mode := range []os.FileMode{0o700, 0o755, 0o777 | os.ModeSticky} {
		if err := os.Chmod(dir, mode); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadRegistryState(path, testManufacturerAuthority); err != nil || got != 7 {
			t.Fatalf("directory mode %v: state = %d, %v; want 7", mode, got, err)
		}
	}
	// The file itself is owned by this process, as every file it writes is.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := stateOwnershipError(path, info); err != nil {
		t.Fatalf("own file refused: %v", err)
	}
}
