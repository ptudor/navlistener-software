package commissioning

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// registryStateMaxBytes bounds the state file before it is parsed.
const registryStateMaxBytes = 4096

// registryState is the one fact a collector must remember across restarts: the
// newest registry sequence it has adopted. LoadRegistry already refuses to move
// backwards within a process; without this, a restart forgets, and an older
// registry that still carries a valid signature could restore a withdrawn board.
type registryState struct {
	RegistrySequence uint64 `json:"registry_sequence"`
}

// ReadRegistryState returns the recorded sequence, or 0 when no registry has
// been adopted yet. It never writes, so a configuration check may call it.
//
// The file carries no secret, but its integrity is the point: a file another
// local account could rewrite would let that account lower the floor, so one
// that is group- or world-writable is refused.
func ReadRegistryState(path string) (uint64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("registry state: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("registry state %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return 0, fmt.Errorf("registry state %s is group- or world-writable (mode %04o)", path, info.Mode().Perm())
	}
	if info.Size() > registryStateMaxBytes {
		return 0, fmt.Errorf("registry state %s is %d bytes, limit %d", path, info.Size(), registryStateMaxBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("registry state: %w", err)
	}
	var state registryState
	if err := json.Unmarshal(data, &state); err != nil {
		return 0, fmt.Errorf("registry state %s: %w", path, err)
	}
	if state.RegistrySequence == 0 {
		return 0, fmt.Errorf("registry state %s records no sequence", path)
	}
	return state.RegistrySequence, nil
}

// writeRegistryState records sequence durably: a private temporary file in the
// same directory, synced, then renamed over the old one, so a crash leaves
// either the earlier floor or the new one and never a torn file.
func writeRegistryState(path string, sequence uint64) error {
	data, err := json.Marshal(registryState{RegistrySequence: sequence})
	if err != nil {
		return fmt.Errorf("registry state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("registry state: %w", err)
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has succeeded
	if _, err = tmp.Write(append(data, '\n')); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("registry state: %w", err)
	}
	// Make the rename itself durable. Not every platform can sync a directory,
	// and the file's own contents are already on disk, so this is best effort.
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}
