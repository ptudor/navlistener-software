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
	//
	// cover the WHOLE tree of BOTH core modules, including test files. `go test` runs
	// each test with cwd = its package dir, so a bare `go list ./...` here matched only
	// cmd/navlistener's PRODUCTION import graph — a _test.go anywhere, or an internal package
	// not yet imported by main, could import a GPL-quarantined path without failing the gate.
	// Run from each module root (relative to this package dir: go/ = ../.., gnss/ = ../../../gnss)
	// with `-deps -test` so the check spans the full import graph and every test binary.
	for _, mod := range []string{"../..", "../../../gnss"} {
		cmd := exec.Command("go", "list", "-deps", "-test", "./...")
		cmd.Dir = mod
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list -deps -test in %s failed (import-graph boundary unverified): %v\n%s", mod, err, out)
		}
		for _, dep := range strings.Fields(string(out)) {
			for _, bad := range forbidden {
				if strings.Contains(dep, bad) {
					t.Errorf("%s: package imports forbidden (GPL-quarantined) path %q via %q", mod, bad, dep)
				}
			}
		}
	}
}
