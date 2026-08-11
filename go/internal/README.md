# `go/internal` — the collector's packages

**Headline:** an index. Every package that makes up the navlistener daemon, what it owns, and
which direction the dependencies point. Each has its own README with the detail.

---

## Index — what's in this folder

| Package | Stage | Owns |
|---|---|---|
| [`config/`](config/) | — | TOML loading, validation, defaults, and the identity validators. |
| [`identity/`](identity/) | — | Trusted observer context and audience authorization keys. |
| [`attestation/`](attestation/) | — | Manufacturer v1/v2 originality statement formatting and verification. |
| [`authorization/`](authorization/) | — | DB-backed observer grants, digest-only bounded cache, and PostgreSQL invalidation. |
| [`audience/`](audience/) | — | Pre-aggregation public/event privacy projection. |
| [`federation/`](federation/) | — | Receipt/current policy ∩ destination export-grant evaluator. |
| [`wire/`](wire/) | — | GNF1: the feeder↔collector framing, handshake, and the durable-ACK contract. |
| [`ingest/`](ingest/) | **INGEST** | Dial connectors (UBX/SBF/RTCM/NTRIP) and the authenticated push server. Normalizes both into `RawFrame`. |
| [`state/`](state/) | **STATE** | Live per-SV state, decode dispatch, propagation, and the integrity *compute*. |
| [`detect/`](detect/) | **DETECT** | Debounced classifiers turning those metrics into typed integrity events. |
| [`store/`](store/) | **PERSIST** | The TimescaleDB historian: raw frames, events, feed snapshots. |
| [`serve/`](serve/) | **SERVE** | The native `/gnss/api/v2/*` feeds, the events query API, and the SSE stream. |
| [`server/`](server/) | — | The observability listener: `/metrics`, `/healthz`, `/debug/state`. |
| [`metrics/`](metrics/) | — | Prometheus collector definitions. |
| [`version/`](version/) | — | Build identity, injected at link time. |

---

## The dependency direction

```
                    cmd/navlistener
                          │  wires everything, owns lifecycle
     ┌──────────┬─────────┼─────────┬──────────┬─────────┐
     ▼          ▼         ▼         ▼          ▼         ▼
  ingest ───► state ───► detect   store      serve     server
     │          │                    ▲          │         │
     └── wire   └── github.com/ptudor/gnss      └─ state   └─ metrics
                          │
                     config, metrics  (imported broadly)
```

The rules that hold:

- **`state` imports `ingest`** (for `RawFrame`), never the reverse.
- **`detect` imports `state`**, never the reverse. That's why `detect.FreshReceiverThreshold` is a
  documentation constant while `state.freshReceiverWindow` is the one that actually computes —
  regression fix says move both together.
- **`gnss` imports nothing from here.** The reusable math library is I/O-free and never records
  metrics; the daemon records from the typed results and errors it returns.
- **`cmd` owns everything cross-cutting** — startup order, the decode loop's panic recovery, the
  tickers, the event pipeline, shutdown ordering, and access-control policy.

---

## Reading order

If you're new to the daemon, this order builds the picture fastest:

1. **[`wire/`](wire/)** — the protocol, and the ACK contract that shapes the whole durability
   story.
2. **[`ingest/`](ingest/)** — how bytes become a `RawFrame`, either way they arrive.
3. **[`state/`](state/)** — the largest and most interesting package: decode dispatch, the
   ephemeris store, and how orbit-disco and time-disco are actually computed.
4. **[`detect/`](detect/)** — the thresholds and, more importantly, the reasoning behind the
   awkward ones (the asymmetric SISA band, `SilenceMinReceivers`).
5. **[`store/`](store/)** and **[`serve/`](serve/)** — the two ends of the output contract.
6. **[`config/`](config/)**, **[`server/`](server/)**, **[`metrics/`](metrics/)**,
   **[`version/`](version/)** — the supporting cast.

Then `../../docs/DESIGN.md` for the architecture these implement, and `../../gnss/README.md` for
the math library underneath.

---

## Why `internal/`

Go's `internal/` rule makes every package here importable only from within
`github.com/ptudor/navlistener`. That is deliberate: the **reusable** surface of this project is
the separate `gnss` module, which is Apache-2.0, dependency-free, and tagged on its own. The
daemon's packages are free to change shape without anyone's build breaking.
