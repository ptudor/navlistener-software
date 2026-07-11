package main

import (
	"os/exec"
	"strings"
	"testing"
)

// forbidden import-path substrings: the GPL galmon-bridge and anything galmon's
// protobuf world drags in. The Apache-2.0 core must depend on the bridge only over
// a socket, never as a symbol (docs/DESIGN.md §4). This test is the import-graph
// boundary CI enforces; it fails the build if any core package imports GPL code.
var forbidden = []string{
	"galmon-bridge",
	"navmon",
	"libnavmon",
}

func TestNoForbiddenImports(t *testing.T) {
	// this is the stated CI enforcement of the Apache/GPL provenance
	// boundary -- a provenance control must fail closed. A `go list` failure (a
	// broken import, a network-required module, a sandboxed CI) must not turn
	// the gate into a silent skip that leaves the boundary unverified in an
	// otherwise-green build.
	out, err := exec.Command("go", "list", "-deps", "./...").CombinedOutput()
	if err != nil {
		t.Fatalf("go list -deps failed (import-graph boundary unverified): %v\n%s", err, out)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbidden {
			if strings.Contains(dep, bad) {
				t.Errorf("core package imports forbidden (GPL-quarantined) path %q via %q", bad, dep)
			}
		}
	}
}
