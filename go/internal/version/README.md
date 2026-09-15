# Build identity

The Makefile injects four strings into the binary at link time:

| Field | Source | Purpose |
|---|---|---|
| `Version` | Repository `VERSION` | Human-facing release, initially `0.1.0` |
| `BuildNumber` | Repository `BUILD_NUMBER` | Manually advanced numbered build, initially `1` |
| `Revision` | Git revision with an optional `-dirty` suffix | Exact source traceability |
| `BuildTime` | UTC build timestamp | When this binary was compiled |

Plain `go build`/`go run` reports development defaults. Use the Makefile for a
numbered build. Building does not increment or edit either release file.

`navlistener -version` and the startup log show these fields separately.
`navlistener_build_info{version,build_number,revision,build_time}` exposes them as
labels on a constant-one gauge. `Identity()` renders the compact
`release+build.revision` form for database decoder provenance, matching firmware's
app descriptor convention. The binary and ELF hashes remain stronger artifact
identifiers than any manually assigned number.

See [the repository release policy](../../../README.md#release-and-build-identity)
and [the Makefile](../../Makefile).
