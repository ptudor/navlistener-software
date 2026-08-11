# `internal/serve` — the native v2 read API and the events SSE stream

**Headline:** the SERVE stage. It publishes live per-SV state as versioned JSON feeds from an
**in-RAM snapshot refreshed on a slow cadence** — satellites move slowly, so there's no reason a
request should ever touch the state store — plus a windowed query API over the historian and a
server-sent-events stream for confirmed integrity events.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `serve.go` | `Server`, the route table, the cached-envelope refresh loop, and the standard response envelope. |
| `events_api.go` | `GET /gnss/api/events` and `/events/summary` — windowed reads over the historian, with their bounds. |
| `sse.go` | `Broker` — the SSE fan-out, the reconnect replay ring, and the client-resource bounds. |
| `sanitize.go` | Making receiver-originated strings safe to place in a JSON feed. |
| `*_test.go` | Feed shapes, envelope, parameter validation, SSE lifecycle and bounds, sanitization. |
| `README.md` | This file. |

**It binds loopback** and is fronted by a TLS reverse proxy (a reverse proxy) as `intsat.space`. It
is physically separate from the ingest write path and from the metrics listener. Enabled only
when `[serve].addr` is set.

---

## Summary

### Routes

| Path | What it serves |
|---|---|
| `GET /gnss/api/v2/audiences` | Public plus the authenticated principal id, opaque authorization/policy revision, and materialized audience grants. |
| `GET /gnss/api/v2/svs` | Per-satellite×signal live state — the main feed. |
| `GET /gnss/api/v2/global` | Fleet-wide counts, per-constellation totals, leap seconds. |
| `GET /gnss/api/v2/observers` | Station list (only `remark` from config — never dial addresses). |
| `GET /gnss/api/v2/almanac` | Coarse orbits, including SVs currently out of ephemeris view. |
| `GET /gnss/api/v2/sbas` | Per-GEO SBAS message-type and health tracking. |
| `GET /gnss/api/events` | Filtered, paginated window over persisted integrity events. |
| `GET /gnss/api/events/summary` | Rolling-window counts and breakdowns. |
| `GET /gnss/events` | The SSE stream of confirmed events. |

### The envelope

Every JSON response is wrapped in the standard v2 envelope (`docs/OUTPUT.md §0`):

```json
{ "ok": true, "time": "2026-08-01T12:00:00Z", "data": { ... } }
```

### The public API

```go
func New(addr string, st *state.Store, events EventStore, sources []config.Source,
         fast, slow time.Duration, log *slog.Logger) *Server
func NewForAudience(addr string, st *state.Store, events EventStore, sources []config.Source,
         fast, slow time.Duration, log *slog.Logger, audience identity.Audience) *Server
func (s *Server) EnableAudienceSelection(auth ReadAuthorizer, resolver ViewResolver,
         reauthorizeEvery time.Duration)
func (s *Server) Listen() (net.Listener, error)
func (s *Server) Start(ln net.Listener) error
func (s *Server) Run(ctx context.Context)          // the refresh loop
func (s *Server) Shutdown(ctx context.Context) error
func (s *Server) PublishEvent(e EventMsg)
func (s *Server) PublishEventForAudience(a identity.Audience, e EventMsg)
func (s *Server) SnapshotFeeds() map[string][]byte  // for the historian's snapshot writer
func (s *Server) Audience() identity.Audience
```

`NewForAudience` requires an already isolated state store; it never filters one global
aggregate at serialization time. Runtime configuration defaults to the `public` projection.
The explicit `operator` view is private/no-store and must sit behind authenticated access.
`New` remains the operator-view convenience constructor for internal tests and single-user
embedding.

---

## Details

### Public cache and isolated private rendering

The server **marshals the default/public feed's complete envelope on a timer and swaps the byte
slice atomically.** Authenticated organization/collection bodies are rendered from their
physically separate state after authorization and are not put in the shared cache; this keeps
private entries from crossing principals or audiences.

Two cadences, because two kinds of data move at different speeds:

| Cadence | Config | Feeds |
|---|---|---|
| fast | `[serve].refresh_interval` (default 30s) | `svs`, `global`, `observers`, `sbas` |
| slow | `[serve].almanac_refresh_interval` (default 90s) | `almanac` |

A marshalling failure is logged and **leaves the previous body in place** rather than serving a
broken or empty feed.

`SnapshotFeeds()` returns a copy of the fixed/default warmed envelopes. `SnapshotAllFeeds()` is
the historian path: it keeps those exact bytes and separately renders every trusted materialized
organization/collection/operator view. Private renders never enter the shared response cache,
and every persisted row carries its own audience key.

Public feed responses are cacheable and identify `data.audience = "public"`. Private responses
send `Cache-Control: private, no-store` and vary on authorization/audience selectors. Event
queries are forced to the request's server-resolved audience; clients cannot turn a free-form
scope into authority. SSE receives only the matching detector pipeline, re-authorizes long-lived
sessions, and uses that audience's private monotone sequence rather than the global row id.
Policy invalidation clears warmed default bodies, empties the affected SSE replay ring, forces
connected streams to re-authorize, and clamps history to the audience's current policy epoch.

### The events query API and its bounds

```
GET /gnss/api/events?since=&until=&sv=&type=&severity=&limit=&offset=
GET /gnss/api/events/summary?hours=
```

- `limit` is capped at 500; `hours` at 720 (30 days), default 24.
- Results are newest-first, with the total count *before* pagination.
- The summary returns totals, active critical and warning counts, the last critical time, and
  breakdowns by event type and constellation.

**Two parameter-handling rules worth stating explicitly:**

**Malformed is an error, absent is a default.** `atoiParam` and `parseTimeParam` return
the default only for an *absent* parameter. A malformed non-empty value — `limit=abc` — returns
an error rather than silently falling back. A typo must never become a confidently-wrong result
with `ok: true`; that's the failure mode where a consumer displays the wrong window and nobody
notices.

**String filters are length-bounded** at the same 256-byte limit `sanitize.go` applies to
receiver strings — an unbounded `sv` or `type` filter reaching the database is exactly the kind
of thing that gets found later.

### SSE — the `Broker`

`GET /gnss/events` streams confirmed integrity events for this server's audience. The broker fans
out to connected clients and keeps a **bounded ring of recent events for `Last-Event-ID` reconnect replay**, so a client
that drops and reconnects gets what it missed instead of a silent gap. The cursor is monotone only
within that audience, so private incidents cannot be inferred from gaps in public ids. Heartbeat
every 60 s.

This is the *server-driven push* side of the events contract. The database trigger's `pg_notify`
serves external LISTENers separately — two independent paths, deliberately, so an external
consumer doesn't have to hold an HTTP connection to this daemon.

**Three resource bounds, each closing a specific hole:**

- **`sseMaxClients` ** — each connection costs a goroutine, a buffered channel, and a
  socket held open indefinitely. An attacker, or just a buggy reconnect loop, opening unbounded
  streams exhausts server resources.
- **`sseWriteTimeout` ** — every write and flush is bounded. A client whose TCP receive
  window is full — dead but not reset — must not be able to park the handler goroutine, and its
  buffered events, indefinitely.
- **Overflow kicks rather than drops.** When a client's queue overflows, the stream is
  **terminated** (idempotently, via `sync.Once`) so `EventSource` reconnects with its last
  delivered id. That converts an invisible gap into a visible reconnect that replays the missed
  events. Silently dropping events on a still-connected stream would be the worse outcome — the
  client would believe it had seen everything.

Both bounds are `var` rather than `const` so tests can shrink them instead of opening a thousand
real connections or waiting out a production timeout.

**`Close()` ** signals every active handler to return, and is registered via
`http.Server.RegisterOnShutdown` — so a graceful shutdown completes promptly instead of polling
until the timeout while a client holds a permanent `EventSource`.

### `sanitize.go` — receiver strings are untrusted

Anything a receiver or feeder originated is hostile until proven otherwise
(`docs/INTEGRITY.md §9`). Before a string reaches a feed it is:

- **coerced to valid UTF-8** — malformed UTF-8 breaks JSON consumers in interesting ways,
- **stripped of control characters** — the log-injection and terminal-escape class,
- **length-bounded** at `maxStringField` (256) — so a hostile or buggy observer cannot bloat a
  response.

### What is deliberately not served

- **Dial addresses.** The observers feed carries only the operator-supplied `remark`.
  Publishing `addr` would disclose network topology and the exact `host:port` of an
  unauthenticated raw receiver TCP stream.
- **Other API formats.** navlistener serves its v2 contract; consumers
  implement that contract. Independent formats can be mapped in differential tests.
- **Absent means unknown.** Optional fields are omitted rather than zeroed, throughout — see
  `../state/README.md`.

---

## Tests

Feed shapes and the envelope, refresh-loop behavior including the keep-previous-on-marshal-error
path, parameter validation in both the absent and malformed directions, event query
bounds and ordering, SSE connect/replay/heartbeat/overflow-kick/shutdown, the client cap, the
write timeout, and sanitization of malformed UTF-8, control characters, and over-long strings.

```sh
go test ./internal/serve/
```

---

## See also

- `../state/README.md` — where the feed models are built.
- `../store/README.md` — the event tables and the `pg_notify` contract.
- `../../../docs/OUTPUT.md` — the authoritative served contract: §0 envelope, §1 feeds,
  §2.1 events query, §3 event contract, §5 cadence and caching, §6 consumer migration.
