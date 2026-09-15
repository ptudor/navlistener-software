// Package version holds build identity, injected at link time via -ldflags -X.
package version

import "fmt"

// Set via -ldflags "-X github.com/ptudor/navlistener/internal/version.Version=…
// -X github.com/ptudor/navlistener/internal/version.BuildTime=…".
var (
	Version     = "dev"
	BuildNumber = "0"
	Revision    = "unknown"
	BuildTime   = "unknown"
)

// String renders a human/log-friendly identity line.
func String() string {
	return fmt.Sprintf("navlistener/%s (build %s; revision %s; built %s)", Version, BuildNumber, Revision, BuildTime)
}

// Identity is compact but distinguishes rebuilds of the same release/build.
func Identity() string { return Version + "+" + BuildNumber + "." + Revision }
