//go:build !unix

package commissioning

import "io/fs"

// Ownership and directory integrity are judged on Unix, where the collector
// runs; elsewhere the mode-bit check in ReadRegistryState is all there is.
func stateOwnershipError(string, fs.FileInfo) error { return nil }

func stateDirectoryError(string) error { return nil }
