# `cmd/navlistener` — the daemon entry point

**Headline:** `package main`. It parses three flags, loads the config, wires every stage
together in an order chosen so that a misconfiguration fails *before* the process claims to be
healthy, runs the loops, and drains them in dependency order on shutdown. There is no business
logic here — everything it does is composition, lifecycle, and the couple of concerns that
genuinely belong to the process rather than to a package.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `main.go` | The whole command: flags, startup ordering, the decode loop, the detect/propagate/expire tickers, the event pipeline, signal handling, and ordered shutdown. |
| `*_test.go` | Startup-ordering, shutdown-drain, event-pipeline retry, and `/debug/state` access-control tests. |
| `README.md` | This file. |

---

## Summary

### Flags

| Flag | Effect |
|---|---|
| `-config <path>` | Config file path. Without it, `config.DefaultPaths` is searched in order. |
| `-version` | Print build identity and exit. |
| `-check-config` | Load and validate the config, print a summary, exit. **Full startup parity** : it loads the `[push]` TLS keypair and client CA, parses the store DSN, and validates NTRIP CA files. On a bare host without certs it fails by design — that's the point. |

### What lives here rather than in a package

- **Startup and shutdown ordering** — the whole reason this file is long.
- **The decode loop** — pulling `RawFrame`s off the channel into `state.Store.Apply`, wrapped in a
  per-frame `recover`.
- **The periodic tickers** — propagate, expire, detect, and feed-snapshot cadences.
- **The event pipeline** — detector events → historian row → SSE publish, with retry.
- **`/debug/state` access control** — the `server` package serves the handler; the *policy* about
  who may call it is the process's to own.

---

## Details

### Startup order, and why it is this order

The sequence is deliberate. Anything that can fail on a misconfiguration is bound
**synchronously, before the daemon logs "ready"**:

1. **Load config** and set up `slog`. Any non-fatal `Warnings` from validation are logged
   immediately  — a world-readable secrets file or a non-loopback bind of an
   unauthenticated surface is never silent, even though neither is fatal.
2. **Build live state** (`state.New(cfg.State.Shards)`) and install declared capabilities from
   both `[[ingest]]` sources and `[[push.observer]]` entries.
3. **Create the ingest→decode channel**, bounded at 8192 frames. A full queue **backpressures
   ingest** rather than dropping silently.
4. **Bind the metrics listener synchronously**. A malformed or already-bound
   `[metrics].addr` must kill the process immediately, not leave it running with no `/metrics`
   or `/healthz` while rc.d cheerfully reports it healthy.
5. **Bind the push listener synchronously**, before any producer or historian goroutine starts.
6. **Open the store** and start the historian's writer goroutine — on its own context, because
   it must outlive the ingest context during shutdown.
7. **Bind the read API listener**, then start the ingest manager, the push server, the API
   server, the decode loop, and the tickers.

The pattern throughout is **`Listen()` synchronously, `Start()` in a goroutine.** Binding is a
startup concern; serving is a runtime one.

### The decode loop and its panic guard

Every frame goes through a `recover`-wrapped `apply`. The reasoning is specific: a
decoder that panics on a particular broadcast bit pattern will panic on **every** recurrence —
the same SV re-transmits it every few seconds, and crafted frames replay it deliberately. This is
a PNT-defense product; malformed frames are the threat model. So a recovered panic drops one
frame, increments `navlistener_decode_panics_total{gnssid}`, and the daemon keeps running.

A rate limiter (`panicLogLimiter`, keyed on gnssId/svId/sigId) keeps a repeating panic from
flooding the log while still counting every occurrence in the metric. `decode_panics_total` is
the alertable signal for a failure class `decode_errors_total` — which counts *rejections* —
structurally cannot see.

### The health model

`/healthz` distinguishes two very different situations:

- **`degraded` → HTTP 200.** A data-plane probe is unhappy: no frame ingested across every source
  and observer for `ingestStaleAfter` (5 minutes), or the store reports degraded. This is
  deliberately **not** 503, because a transient ingest gap or DB blip must not flap rc.d into a
  restart loop.
- **`Fail()` → HTTP 503.** A required component terminated. Reserved for real failure, followed by
  controlled process shutdown.

The 5-minute staleness window mirrors `state`'s `liveReceiverWindow` and
`detect.ObserverOfflineThreshold`. A healthy collector with any configured source sees frames
every few seconds, so five minutes of total silence means the data plane is down even though
every listener goroutine is still alive.

### The event pipeline

Detector events are not written inline. They go through a small pipeline that:

- **assigns the monotonic id** at persist time (the detector fills everything else),
- **retries** a failed historian write with a bounded policy, and
- **publishes to SSE** after persistence.

On shutdown there is a **final flush** with its own timeout, so events confirmed in the last tick
aren't lost — and any that still can't be written are counted as dropped rather than silently
discarded.

### Shutdown, in dependency order

`SIGINT`/`SIGTERM` (or a fatal from any component) starts a drain bounded by
`cfg.ShutdownTimeout`:

1. Cancel the ingest context — connectors and the push server stop accepting.
2. Drain the frame channel so in-flight frames are applied rather than dropped.
3. Flush remaining events.
4. Shut down the API server, then the metrics server.
5. Cancel the **store's separate context** last, so the historian can commit what's queued.

The store deliberately gets its own context: if it shared the ingest one, cancelling ingest would
cancel the writer mid-batch and lose exactly the frames the drain was trying to save.

**SIGHUP is explicitly ignored.** Log rotation signals the `daemon(8)` supervisor, not the
collector — see `../../deploy/freebsd/newsyslog.conf.d/navlistener.conf`. Ignoring it here is
belt and braces so a mis-aimed rotation can't interrupt an ordered drain.

### `/debug/state` access control

`server.New` takes the handler; `main` decides who may reach it. The policy is peer-address
based, and it lives here because it's a deployment concern — the `server` package explicitly
documents that "the caller owns any access-control policy for that handler."

---

## Tests

The tests in this directory cover the things that only exist at the composition level:
synchronous listener binding before "ready", the ordered shutdown drain (including a hard-down
variant that overrides the flush timeout), event-pipeline retry and final-flush accounting, the
panic-log limiter, and `/debug/state` peer gating.

```sh
go test ./cmd/navlistener/
```

---

## See also

- `../../README.md` — the daemon overview, build targets, and deployment.
- `../../internal/*/README.md` — each stage in detail.
- `../../../docs/DESIGN.md` — the architecture this file assembles.
