# `internal/server` — the observability listener

**Headline:** the small HTTP server that answers "is this thing alive and what is it doing" —
Prometheus `/metrics`, a health endpoint, and an optional live-state debug view. It is entirely
separate from the native read API and from the ingest path, and that separation is the point.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `server.go` | The whole package: `Server`, `Listen`/`Start`/`Shutdown`, `AddProbe`, `Fail`. |
| `server_test.go` | Health states, probe registration, and the listener lifecycle. |
| `README.md` | This file. |

Enabled when `[metrics].addr` is set. Normally bound to loopback; a non-loopback bind is allowed
with a startup warning, for remote Prometheus deployments.

---

## Summary

```go
func New(addr string, log *slog.Logger, debugState http.HandlerFunc) *Server
func (s *Server) Listen() (net.Listener, error)
func (s *Server) Start(ln net.Listener) error
func (s *Server) Shutdown(ctx context.Context) error
func (s *Server) AddProbe(name string, fn func() string)
func (s *Server) Fail(err error)
```

| Endpoint | What it is |
|---|---|
| `/metrics` | The Prometheus scrape target — every collector from `internal/metrics`. |
| `/healthz` | Aggregate health across the registered data-plane probes. |
| `/debug/state` | A compact diagnostic view of live per-SV state. Served only when `debugState` is non-nil. |

---

## Details

### `Listen` before `Start` — and `Listen` synchronously

Binding and serving are separate calls on purpose. `main` calls `Listen()` **synchronously at
startup, before the daemon logs "ready"**, then runs `Start(ln)` in a goroutine.

Without that split, a malformed or already-bound `[metrics].addr` fails inside the serving
goroutine — leaving the process running with no `/metrics` and no `/healthz` while rc.d
cheerfully reports it healthy. Which is worse than not starting at all, because the monitoring
you'd use to notice is the exact thing that didn't come up.

### The health model — `degraded` is 200, `Fail` is 503

This is the most important decision in the package.

```go
s.AddProbe("ingest", func() string { ... })   // "" = ok, non-empty = degraded reason
s.Fail(err)                                    // a required component terminated
```

- **A degraded probe yields status `degraded` with HTTP 200.** Deliberately *not* 503, so a
  transient ingest gap or a DB blip cannot flap `rc.d`/`daemon(8)` into a restart loop. The
  daemon is running and doing useful work; something upstream is unhappy and a human should look.
- **503 is reserved for `Fail()`** — a required component has terminated and the process is
  heading for controlled shutdown.

The distinction matters operationally: restarting a collector because its database hiccupped
loses in-RAM live state and produces a fresh wave of "no previous ephemeris" gaps, which is
strictly worse than reporting degraded and carrying on.

Probes must be registered **before `Start`**. Today's probes are the ingest-staleness check
(no frame from any source or observer for 5 minutes, regression fix) and the store's `Degraded()`.

### `/debug/state`

`New` takes the handler; **the caller owns the access-control policy.** `main` gates it on the
request peer address, because "who may see live state" is a deployment question, not a library
one.

The view itself is `state.Snapshot` — a deliberately compact diagnostic subset of the richer v2
`svs` feed, not a second read API.

### Why three separate listeners

| Listener | Package | Exposure |
|---|---|---|
| Observability | this one | loopback (warned if not) |
| Native read API | `internal/serve` | loopback, TLS-fronted |
| Fleet push | `internal/ingest` | public, TLS mandatory |

Separate listeners mean the public ingest surface cannot reach `/metrics` or `/debug/state`, and
a misconfigured proxy in front of the read API cannot accidentally expose either. Collapsing them
onto one port would make every one of those a routing bug away from being wrong.

---

## Tests

`server_test.go` covers the ok/degraded/failed health transitions and their status codes, probe
registration and aggregation, `Listen` returning a real error on a bad address, and graceful
shutdown.

```sh
go test ./internal/server/
```

---

## See also

- `../metrics/README.md` — the collectors this exposes.
- `../../cmd/navlistener/README.md` — probe registration, the peer gate on `/debug/state`, and
  startup ordering.
- `../state/README.md` — `Snapshot`, and the regression fix no-persistence design fact.
