//go:build unix

package commissioning

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// stateOwnershipError refuses a registry state file another account owns. A
// file the caller cannot be sure it wrote itself is not a floor it can trust,
// however its mode bits read.
func stateOwnershipError(path string, info fs.FileInfo) error {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("registry state %s: owner cannot be determined", path)
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("registry state %s is owned by uid %d, not this process's uid %d", path, st.Uid, os.Geteuid())
	}
	return nil
}

// stateDirectoryError refuses a state directory in which another account could
// rename its own file into place: group- or world-writable without the sticky
// bit, which is what lets a shared directory restrict renames to the owner.
func stateDirectoryError(dir string) error {
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("registry state directory: %w", err)
	}
	if info.Mode().Perm()&0o022 != 0 && info.Mode()&fs.ModeSticky == 0 {
		return fmt.Errorf("registry state directory %s is group- or world-writable without the sticky bit (mode %04o); another account could replace the state file", dir, info.Mode().Perm())
	}
	return nil
}
