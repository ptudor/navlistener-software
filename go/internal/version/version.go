// Package version holds build identity, injected at link time via -ldflags -X.
package version

import "fmt"

// Set via -ldflags "-X github.com/ptudor/navlistener/internal/version.Version=…
// -X github.com/ptudor/navlistener/internal/version.BuildTime=…".
var (
	Version   = "dev"
	BuildTime = "unknown"
)

// String renders a human/log-friendly identity line.
func String() string {
	return fmt.Sprintf("navlistener/%s (built %s)", Version, BuildTime)
}
