# `internal/version` — build identity

**Headline:** two strings and a formatter. It exists so the binary can tell you exactly which
commit and build it came from, without importing anything or reading anything at runtime.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `version.go` | The whole package — 16 lines. |
| `README.md` | This file. |

---

## Summary

```go
var (
    Version   = "dev"
    BuildTime = "unknown"
)
func String() string
```

Both variables are **injected at link time** via `-ldflags -X`. The defaults (`"dev"`,
`"unknown"`) are what you get from a plain `go build` or `go run` — which is itself useful
information: if a deployed binary reports `dev`, it wasn't built by the Makefile.

---

## Details

### How the values get there

From `../../Makefile`:

```make
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
PKG        := github.com/ptudor/navlistener/internal/version
LDFLAGS    := -ldflags "-s -w -X $(PKG).Version=$(VERSION) -X $(PKG).BuildTime=$(BUILD_TIME)"
```

Two details worth noticing:

- **`--dirty`** means a binary built from a modified working tree says so in its version string.
  That is exactly the information you want when a deployed binary behaves unlike the tagged
  release it claims to be.
- **`-s -w`** strips the symbol table and DWARF info, which is a size choice, not a security one.

### Where the values surface

| Surface | How |
|---|---|
| `navlistener -version` | Prints `String()` and exits. |
| Startup log | The first line the daemon logs. |
| `navlistener_build_info{version, build_time}` | A Prometheus gauge, constant 1, carrying both as labels. |

The metric is the one that matters operationally: it lets a dashboard show which version each
collector in the fleet is running, and a Prometheus alert can catch a host that didn't get
restarted after a deploy.

### Why a package rather than a `main` variable

Because `-ldflags -X` needs a fully-qualified symbol path, and because the metrics package needs
to read these without importing `main`. Sixteen lines is a fair price for both.

---

## See also

- `../../Makefile` — where the flags are set.
- `../metrics/README.md` — `navlistener_build_info`.
- `../../cmd/navlistener/README.md` — the `-version` flag and the startup log line.
