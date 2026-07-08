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
	out, err := exec.Command("go", "list", "-deps", "./...").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Fields(string(out)) {
		for _, bad := range forbidden {
			if strings.Contains(dep, bad) {
				t.Errorf("core package imports forbidden (GPL-quarantined) path %q via %q", bad, dep)
			}
		}
	}
}
