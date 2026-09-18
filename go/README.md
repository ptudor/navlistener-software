# `go/` — navlistener, the collector daemon

**Headline:** the central Go daemon. It ingests raw broadcast nav frames from a fleet of
receivers, decodes them with the `gnss` library, propagates orbits and clocks, computes and
detects integrity events, persists everything to TimescaleDB, and serves the native v2 feeds
behind the Integrity Constellation Map. Feeders forward raw frames; decoding and analysis run here.

---

## Index — what's in this folder

| Path | What it is |
|---|---|
| `cmd/navlistener/` | `package main` — flags, startup ordering, wiring, graceful shutdown. |
| `internal/config/` | TOML config loading and validation. |
| `internal/wire/` | GNF1 — the feeder↔collector framing. |
| `internal/ingest/` | Dial connectors (UBX/SBF/RTCM/NTRIP) and the authenticated GNF1 push server. |
| `internal/state/` | Live per-SV state: ephemeris store, propagation, the integrity *compute*. |
| `internal/commissioning/` | Manufacturer commissioning records, session proofs and the signed registry: what the push server verifies about hardware. |
| `internal/detect/` | The integrity *detect* stage: debounced classifiers → typed events. |
| `internal/store/` | The TimescaleDB historian: batched raw frames, events, feed snapshots. |
| `internal/serve/` | The native `/gnss/api/v2/*` read API and the events SSE stream. |
| `internal/server/` | The observability listener: `/metrics`, `/healthz`, `/debug/state`. |
| `internal/metrics/` | Prometheus collector definitions. |
| `internal/version/` | Build identity, injected at link time. |
| `deploy/freebsd/` | The rc.d service script and newsyslog rotation config. |
| `Makefile` | Build, cross-compile, and the quality gates. |
| `navlistener.toml.example` | A commented reference config. |
| `go.mod` / `go.sum` | `module github.com/ptudor/navlistener`. |
| `build/` | Cross-compiled binaries (git-ignored). |

Each `internal/` package has its own README with the detail.

---

## Summary

### The five-stage pipeline

```
   receivers                    ┌──────────────────────── navlistener ────────────────────────┐
      │                         │                                                             │
  ┌───┴────┐   raw frames       │  ┌────────┐   ┌───────┐   ┌────────┐   ┌───────┐  ┌───────┐ │
  │ dial   │───────────────────►│  │ INGEST │──►│ STATE │──►│ DETECT │──►│ STORE │  │ SERVE │ │
  │ push   │   GNF1/TLS         │  └────────┘   └───────┘   └────────┘   └───────┘  └───────┘ │
  └────────┘                    │       │           │            │           ▲          ▲     │
                                │       └───────────┴────────────┴───────────┘          │     │
                                │              raw frames persisted                     │     │
                                └────────────────────────────────────────────────────────┼─────┘
                                                                                         │
                                                                            /gnss/api/v2/*, SSE
```

1. **INGEST** — read a receiver stream or accept an authenticated feeder connection; frame it;
   normalize to a `RawFrame`. No decoding here.
2. **STATE** — decode with `github.com/ptudor/gnss`, assemble ephemerides, propagate, and compute
   the integrity signals (orbit-disco, time-disco, capability, RF).
3. **DETECT** — debounce those metrics into confirmed, typed events.
4. **STORE** — the TimescaleDB historian: raw frames in bulk, plus events and feed snapshots.
5. **SERVE** — publish the live state as versioned JSON feeds, plus an SSE event stream.

Stages 4 and 5 are **optional**: an empty `[store].dsn` or `[serve].addr` leaves them dormant, so
the daemon can run collector-only or without a database.

### Design rules that shape the whole daemon

- **Decode centrally.** Feeders forward raw frames, never decoded output. A
  decoder bug is fixable in one place and replayable over stored raw frames.
- **Treat every receiver as flaky.** Connectors reconnect forever with exponential backoff and
  never exit on their own. The C feeder spools to disk across reboots.
- **The historian is off the hot path.** Frames reach the store through a bounded queue with an
  explicit drop-on-overflow policy, so a slow database degrades the historian and never live
  decoding.
- **No cross-restart state persistence**. Every restart rebuilds live state from
  ingest. The `nav_frames` hypertable is the durable record, and offline replay is the recovery
  path — not a state file.
- **Untrusted input everywhere.** Nav frames, feeder handshakes, and observer-supplied strings
  are all hostile until validated. `gnss/frame` bounds every bit read;
  `internal/serve/sanitize.go` bounds every string that reaches a feed.

---

## Building and testing

Run these commands from `go/` with Go 1.25 or newer and Make installed. Keep
`../gnss` and `../go.work` alongside this module. For a first run, follow the
[repository quick start](../README.md#build-and-run-locally), which creates and
validates a local configuration before starting the collector.

```sh
make            # check + build
make build      # local binary ./navlistener
make build-freebsd   # the collector-host deploy target (also -linux, -linux-arm64, -darwin[-arm64], -all)
make run        # build + run against ./navlistener.toml
make install    # local install to /usr/local/bin (developer box)
```

**`make check`** is the quality gate, and it runs across **both workspace modules** — this one
and `../gnss` (`./...` from here only sees the daemon, so a regression in the
math library would otherwise pass green):

| Target | What it does |
|---|---|
| `fmt-check` | Fails if either module isn't gofmt-clean. |
| `vet` | `go vet ./...` in both modules. |
| `check-refs` | Every committed ICD matches its SHA-256 pin in `reference/SOURCES.tsv`. Non-strict on purpose — the git-ignored local-only ICDs are legitimately absent from a fresh clone. |
| `check-esp32` | The ESP32 component host tests — the firmware C shared with the collector (GNF1 encoder, frame-type map, netcfg validation). Needs only a C compiler. |
| `check-e2e` | Builds the real C feeder and runs `TestNavfeeder*` — the actual C↔Go GNF1 end-to-end tests. |
| `test` | `go test ./...` in both modules. |

Also available: `make race` (the full suite under `-race`, the C1–C11 discipline) and `make fuzz`
(smoke-tests every fuzz target in both modules; `FUZZTIME=5s` for a quick pass). The fuzz target
enumerates every (module, package, `FuzzXxx`) triple and runs each separately, because
`go test -fuzz` accepts exactly one package **and** one matching fuzz function per invocation
.

To prove the whole reference library including local-only documents:

```sh
make -C ../reference missing && make -C ../reference verify STRICT=1
```

---

## Configuration

TOML, never `.env`. Default search order when `-config` isn't given:

```
/usr/local/etc/navlistener/navlistener.toml
/etc/navlistener/navlistener.toml
./navlistener.toml
```

Sections: `[logging]`, `[metrics]`, `[state]`, `[store]`, `[serve]`, `[push]` (with
`[[push.observer]]`), `[hardware_trust]`, and `[[ingest]]`. See `internal/config/README.md` for
the full surface and `navlistener.toml.example` for a commented reference.

`[hardware_trust]` pins the manufacturer public keys the push endpoint verifies device evidence
against, and optionally a signed registry that can withdraw boards
([`../docs/COMMISSIONING.md`](../docs/COMMISSIONING.md)). What a session proves is stamped on
every receipt as `hardware_trust` — `none`, `open`, `test` or `trusted` — and never comes from a
device's own report or from configuration. Without the section every session is `none`.

`[serve].audience` defaults to `public`, which is populated from an isolated pre-aggregation
projection: private receivers cannot affect its confidence, counters, selected ephemeris, or
observer list. Set `audience = "operator"` only for an authenticated private front; those
responses are marked `private, no-store`.

```sh
./navlistener -check-config -config /usr/local/etc/navlistener/navlistener.toml
```

`-check-config` has **full startup parity** : it loads the `[push]` TLS keypair and client
CA, parses the store DSN, validates NTRIP CA files, loads the `[hardware_trust]` keys and
verifies the configured registry. On a bare host without certs it fails by
design. Non-fatal `WARNING:` lines (a world-readable secrets file, a non-loopback bind of an
unauthenticated surface) are surfaced without blocking startup.

---

## Deployment

Target is **`collector-host`** (FreeBSD, PG17 + TimescaleDB), under **rc.d**, served behind a TLS reverse
proxy as `intsat.space`.

```sh
pw useradd navlistener -d /nonexistent -s /usr/sbin/nologin -c "navlistener collector"
install -d -o navlistener -g navlistener -m 0755 /usr/local/etc/navlistener
# install deploy/freebsd/navlistener as /usr/local/etc/rc.d/navlistener (chmod 755)
sysrc navlistener_enable=YES
service navlistener start
```

`service start` runs `-check-config` first and refuses to start on failure —
`daemon(8)` forks and returns 0 immediately, so without that gate a bad config would report
success into a 5 s supervisor crash-loop. The check runs **as the daemon user** so its
file-access rights match the daemon's; chown the config and every file it references, and keep
`tls_key` at 0600 (a world-readable key is rejected outright, regression fix).

Log rotation ships as `deploy/freebsd/newsyslog.conf.d/navlistener.conf`. The SIGHUP goes to the
**`daemon(8)` supervisor pidfile**, never the collector's own — `daemon(8)` runs with `-H` and
reopens the output file, while the collector is never signalled and its ordered drain is never
interrupted. (A SIGHUP delivered to the collector is ignored in `cmd/navlistener/main.go` as belt and braces, but
rotation must target the supervisor.)

---

## Listeners

Three, deliberately separate:

| Listener | Config | Binding | Purpose |
|---|---|---|---|
| Metrics/health | `[metrics].addr` | loopback | `/metrics`, `/healthz`, `/debug/state` |
| Native read API | `[serve].addr` | loopback, TLS-fronted | `/gnss/api/v2/*`, `/gnss/events` (SSE) |
| Fleet push | `[push].addr` | public, TLS mandatory | GNF1 feeder ingest |

The read path and the write path never share a listener, and neither shares one with
observability.

---

## Where to read next

| For… | Read |
|---|---|
| The architecture and the five stages | `../docs/DESIGN.md` |
| The math the state stage runs | `../docs/MATH.md`, and `../gnss/README.md` |
| Which signals decode and how frames arrive | `../docs/CONSTELLATIONS.md` |
| What the detectors compute and their thresholds | `../docs/INTEGRITY.md`, `../docs/DEFENSE-PNT.md` |
| The served contract and the DB schema | `../docs/OUTPUT.md`, `internal/store/schema.sql` |
| How hardware is verified, and what `trusted` means | `../docs/COMMISSIONING.md` |
| A specific package | that package's own `README.md` |
