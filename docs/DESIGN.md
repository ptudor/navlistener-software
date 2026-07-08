# navlistener — the GNSS collector, design report

**Status: design (2026-07-07).** No code yet; this is the agreed spec-level shape for
`navlistener`, the "Space" sibling of `radiolistener`. It is written to be read by a
developer who knows Go and networking but not GNSS internals — GNSS terms are defined on
first use, with the heavy math deferred to `docs/MATH.md`. Read this first, then `MATH.md`.

> One line: **dumb receivers ship raw broadcast navigation frames over an authenticated,
> spooled, acked TLS link to one central Go daemon that decodes every constellation, propagates
> orbits and clocks, cross-checks broadcast-vs-observed for integrity, stores raw + decoded in
> TimescaleDB, and serves the satellite feed behind the Integrity Constellation Map.**

---

## 0. What GNSS collection actually is (orientation)

A GNSS satellite continuously broadcasts a low-rate **navigation message** interleaved with
its ranging signal. That message contains, per satellite (SV):

- **Ephemeris** — a compact orbit model (Keplerian elements + perturbation terms, or for
  GLONASS a Cartesian state vector) valid for a couple of hours, from which a receiver
  computes the SV's precise position at any instant. Re-broadcast every ~30 s, refreshed
  every ~2 h with a new **IODE/IOD** (issue-of-data) tag.
- **Clock correction** — polynomial coefficients (`af0, af1, af2`) that map SV time to
  system time, plus group-delay terms per signal.
- **Almanac** — a coarse, long-life orbit model for *all* SVs (for acquisition).
- **Health, accuracy (URA/SISA), ionospheric coefficients, time-system offsets,** and
  constellation-specific extras (Galileo OSNMA authentication, SBAS corrections, QZSS
  disaster messages, …).

A commercial receiver (u-blox F9P/F9T, Septentrio mosaic) demodulates the signal and can
hand us the **raw broadcast bits** of each navigation frame *before* it interprets them —
u-blox as **UBX-RXM-SFRBX**, Septentrio as **SBF** nav-bit blocks, or as **RTCM3** decoded
ephemeris messages. `navlistener` collects those raw frames from many receivers, decodes them
itself, and does something no single receiver can: compares what *different* receivers heard,
and compares each new ephemeris against the last, to catch orbit/clock discontinuities,
spoofing, and unhealthy satellites — the **integrity** mission.

Why collect raw frames instead of the receiver's already-computed positions? Because (a) a
decoder bug must be fixable in *one* place and replayable over stored history, (b) the raw
bits are the forensic system of record, and (c) integrity monitoring needs the *broadcast*
values, not a receiver's smoothed solution. This is galmon's central insight and we keep it.

---

## 1. The five-stage pipeline

`navlistener` is one long-running Go process, five in-process stages — the same skeleton as
`radiolistener`, so the two are structurally familiar:

```
  INGEST (N receivers)  →  DECODE  →  PROPAGATE + INTEGRITY  →  PERSIST   →   SERVE
  raw nav frames over      internal/gnss    per-SV ephemeris store;   TimescaleDB    galmon-compatible
  GNF1 (authenticated      frame decoders   Kepler/RK4 ECEF; orbit-   (raw frames +  svs/global/observers/
  push) + dev-LAN pull     → typed nav      disco + clock-disco vs    decoded +      almanac/sbas.json,
  from our own receivers   messages         last eph; delta-Hz;       events)        schema-1.1, SSE
                                            health/URA; spoof gates
```

### Stage 1 — Ingest: forward raw frames, never decode at the edge

Mature receivers already demodulated the signal on the observer's box; we read their raw
nav-frame output and forward it untouched. Per source:

| Source | Producer (on the receiver) | Raw frames `navlistener` consumes |
|---|---|---|
| **u-blox** | the F9P/F9T/F10 itself | **UBX-RXM-SFRBX** (raw nav words, per gnssId/sigId) + **UBX-RXM-RAWX** (pseudorange/Doppler/carrier-phase) + **UBX-NAV-SAT/SIG** (C/N0, el/az, prRes) + **UBX-NAV-HPPOSECEF/PVT** (receiver position) + **UBX-MON-HW** (jamming). |
| **Septentrio** | mosaic-X5 / PolaRx | **SBF** raw-nav blocks (`GPSRawCA` 4017, `GALRawINAV` 4023, …, NavIC `IRNSSRaw` 4093) + `MeasEpoch` + `PVTGeodetic`. |
| **RTCM3** | any RTCM source / caster | ephemeris messages **1019/1020/1041/1042/1044/1045/1046** + SSR orbit/clock **1057–1068** (precise-vs-broadcast integrity input). |

The `internal/gnss/frame` decoders live centrally; the feeder is dumb. Ingest is a **registry
of thin connectors** — read a local feed → frame → ship — so a new receiver type (Unicore,
Quectel, a bare NMEA+RTCM caster) is one connector, not a rewrite.

Two ingest modes, exactly as radiolistener:
- **Dial** (dev / our own boxes): `navlistener` opens a TCP connection to a known receiver's
  UBX/SBF port on the LAN. Trusted RF, unauthenticated (we control the box).
- **Push** (the fleet): the C `navfeeder` on each receiver connects *out* to the collector's
  authenticated push endpoint over TLS (GNF1). This is the production path.

### Stage 2 — Decode: the `internal/gnss` library

Each raw frame is dispatched by `(gnssId, sigId)` to its ICD decoder, which reverses the
receiver's word packing back to ICD bit order, checks parity/CRC, and fills a typed nav
message (ephemeris / almanac / iono / time-offset / health). See `docs/CONSTELLATIONS.md` for
the per-constellation frame layouts and `docs/MATH.md` for what the fields *mean*. A frame that
fails CRC is dropped from decode but its raw bytes are still persisted (forensic record).

### Stage 3 — Propagate + integrity: the part that's really ours

- **Per-SV state** (`internal/state`): a sharded `map[SVKey]*SVState` keyed by `name@sigid`,
  holding the current + previous ephemeris (the **ephemeris store**), clock model, health,
  URA/SISA, per-receiver reception (`perrecv`), and derived integrity signals.
- **Propagation:** on demand and on a tick, propagate each SV to "now" (and to receiver
  epochs) via the Kepler or GLONASS-RK4 propagator → ECEF position → az/el per observer.
- **Integrity computation** (`docs/INTEGRITY.md`): when a *new* ephemeris (new IOD) arrives,
  propagate **both** the old and new sets to the same epoch and record `orbit-disco =
  |Δposition|`; record `time-disco =` the clock-offset jump; compute per-receiver **delta-Hz**
  (observed Doppler − ephemeris-predicted Doppler); fold in RTCM SSR precise-vs-broadcast
  deltas; track health/URA/OSNMA transitions. Cross-receiver corroboration (an SV only one of
  many receivers reports is suspect) and physics/plausibility gates apply here — **"verify
  physics, not signatures,"** the design rule, applied to orbits.

### Stage 4 — Persist: raw + decoded, TimescaleDB, ingest-time organized

Same discipline as radiolistener (see `docs/OUTPUT.md §5` for the schema): a `nav_frames`
hypertable holds the **raw frame bytes + the decoded jsonb** side by side, organized on the
near-monotonic ingest clock, tagged with `decoder`/`decoder_ver` so a decoder fix can re-run
history. `gnss_snapshots` stores periodic feed dumps; `gnss_events` stores confirmed integrity
transitions (Phase 2). Written via `pgx CopyFrom`, compressed + retained per policy.

### Stage 5 — Serve: galmon-compatible now, schema-1.1 native

Live feeds from RAM; history/SSE from the DB via `LISTEN/NOTIFY`. Ingest (write) and serving
(read) are physically separate listeners with separate DB pools and authz — ingest is locked
to authenticated observers, serving is public behind a TLS front. The feeds and their exact
shapes are `docs/OUTPUT.md`.

---

## 2. The GNF1 wire (feeder ↔ collector)

`navfeeder` speaks **`GNF1`** — the same length-prefixed, TLS, zstd-negotiated, acked framing
as radiolistener's `RLF1`, carrying **raw nav frames** instead of decoder text. Reusing the
shape means one mental model and a near-verbatim port of `radiolistener/feeder/feeder.c`.

```
  "GNF1"  then  [1B type][4B big-endian length][payload …]      (over TLS 1.2/1.3)
```

| Frame | Dir | Payload |
|---|---|---|
| `HELLO` (0x01) | feeder→collector | JSON `{token, station, feed, sw, zstd?}` — `feed ∈ {ubx, sbf, rtcm, nmea}` |
| `WELCOME` (0x02) | collector→feeder | JSON `{ok, error?, ack_interval_ms?, zstd?}` |
| `DATA` (0x03) | feeder→collector | `[8B seq][framed raw record]` — see the record shape below |
| `ACK` (0x04) | collector→feeder | `[8B seq]` last contiguous sequence stored |
| `PING`/`PONG` (0x05/0x06) | both | keepalive |
| `SIGNED_DATA` (0x07) | feeder→collector | *(hardware tier, vNext)* a `DATA` batch + trailing ATECC ECDSA signature over `EUI-64 ‖ rtc_unix_ns ‖ sha256(payload) ‖ counter` |

**The raw record inside a `DATA` frame** carries just enough envelope for the collector to
dispatch without decoding: `{recv_unix_ns (from the observer/RTC), gnssId, svId, sigId,
frame_type (GNF1 nav-type from the registry in CONSTELLATIONS.md), raw_bytes}`. For UBX the
feeder can forward the whole SFRBX message verbatim and let the collector split it; the record
envelope is what lets a Septentrio and a u-blox feeder converge on one collector code path.

Resilience (galmon's "never go down," radiolistener-proven): a bounded RAM ring with monotonic
sequence numbers; replay all unacked frames on reconnect; spill the oldest to a disk spool on
overflow (`--spool-file`) that survives reboot; backoff-reconnect forever, never exit. zstd
gives ~3–4× on the repetitive nav bitstream.

**Wire implementation.** GNF1 uses a fixed record header that can be implemented
in a small C binary or ESP-IDF firmware. Adapters for other wire formats run
as separate programs.

---

## 3. Node identity & the hardware observer

`navlistener` reuses radiolistener's **AAA control plane verbatim** (`radiolistener/docs/
IDENTITY-AND-AAA.md`): one shared PostgreSQL on `collector-host`, Django owns identity/policy/CA/audit,
the collector reads `Device`/`Credential`/`FeedGrant` for a single indexed auth lookup and
writes accounting back. A GNSS observer is just a `Device` whose `FeedGrant`s are
`ubx`/`sbf`/`rtcm`. Nothing about AAA is GNSS-specific, so we do not re-invent it — we add GNSS
feed types and reuse the CA, enrollment, revocation (`enabled=false`), and trust scoring.

Three credential tiers → trust (radiolistener's ladder, unchanged):
1. **Bearer token** (bootstrap) — SHA-256 stored, shown once.
2. **Software mTLS cert** — CN = `receiver_id`.
3. **ATECC608-anchored mTLS cert** — the **high-assurance receiver class**. The board is the
   ESP32-S3 + ATECC608B + DS3231 RTC + EUI-64 design in `radiolistener/docs/HARDWARE-OBSERVER.md`,
   ported here as `firmware/navfeeder-esp`. The ATECC generates a non-extractable P-256 key,
   signs a CSR (`CN = EUI-64-derived receiver_id`) that the Django CA signs; the private key
   never leaves silicon. The DS3231 stamps a **trusted time-of-transmission** — and for GNSS
   there's a bonus: the receiver *is* a clock source, so the board can discipline the RTC from
   GPS PPS, closing the loop. The optional `SIGNED_DATA` frame (0x07) raises the provenance
   tier to hardware-signed batches.

> Hardware auth proves *who* sent a frame and *when* — it cannot make a spoofed *signal*
> honest. A hardware observer fed a spoofing transmitter still emits perfectly-signed garbage.
> That is exactly why integrity is **"verify physics, not signatures"**: signatures prove
> provenance, the orbit/clock cross-checks prove plausibility. Both are required.

**tudorgps integration (P9):** each observer's *hardware capability fingerprint* — what
signals its silicon can actually decode, from `tudorgps`'s clean-room capability probe
(the F9T-00B L1+L2 vs F9T-10B L1+L5 discriminator) — is recorded on its `Device`. The integrity
layer then knows what a node *should* be able to report: a node whose silicon can't hear L5
suddenly reporting L5 frames is loudly suspect (a proxied/forged feed), and a node that *can*
hear E6/L5 but never does flags a receiver-config or antenna problem. The fingerprint DB and
the observer registry are the same `Device` rows.

---

## 4. GPL isolation — the `galmon-bridge` adapter

The Apache-2.0 core must never link GPL code. Interop lives entirely in an **optional,
separate, out-of-process** binary:

```text
galmon.eu <--> optional RNIE/protobuf adapter <--> GNF1 socket <--> navlistener
```

- The planned `galmon-bridge` adapter translates RNIE/protobuf and GNF1 over a
  local socket, allowing observations to be shared between systems.
- The adapter is a separate optional program with its own licensing requirements.
  The Apache-2.0 core does not link its code or import its protocol schema.
- An import-graph test enforces this separation.
- Numerical validation can compare captured outputs from independent
  implementations without incorporating their code into the core.

**galmon as a test oracle (differential testing).** Distinct from the runtime bridge: in *CI/dev*
we may run the real galmon (`third_party/galmon`) over the *same* captured raw frames and diff
its computed ECEF/clock/disco values against ours. Agreement cross-validates both; a mismatch is
a bug worth root-causing. This reads galmon's *outputs* to check ours — it copies no code and
ships nothing GPL in the product. See `docs/MATH.md §validation`.

---

## 5. Configuration & deployment

TOML at `/usr/local/etc/navlistener/navlistener.toml` (never `.env`), `-config` flag, FreeBSD
**rc.d** on `collector-host`; DB on `zroot`. Sections mirror radiolistener: `[logging] [metrics] [serve]
[state] [store] [[ingest]] [push]`. Each `[[ingest]]` is a connector (`type = ubx|sbf|rtcm`,
`addr`, `listen`). `[push]` is the authenticated fleet endpoint (token + optional mTLS
`client_ca_file`), separate listener from `[serve]`. Prometheus `/metrics` + `/healthz` on
loopback; counters for SVs tracked, frames/s/constellation, decode failures, receiver up/down,
integrity events, DB lag, per-observer drops — so it slots into the same Zabbix/Prometheus
monitoring as radiolistener.

---

## 6. Engineering priorities

**Architecture:** dumb-edge/smart-center; raw-frame-as-record;
spool+ack+replay resilience; the generic Keplerian propagator abstraction; the integrity
signal set (orbit-disco, time-disco, delta-Hz, health/SISA, RTCM precise-vs-broadcast); the
debounced alert state machine; the JSON feed shapes.

**Implementation priorities:**
- **Signal coverage** with explicit implemented and deferred status (`docs/CONSTELLATIONS.md`).
- **Consistent propagation** using per-constellation constants, rotating-frame GLONASS
  coordinates, and the required epoch-wrap corrections.
- **Untrusted-input checks:** bound frame reads and allocations, validate strings,
  synchronize shared state, and fuzz decoders (`go test -fuzz`).
- **A reusable math library** with independent numerical validation.

---

## 7. Non-goals

- **We do not demodulate raw RF.** The receiver's correlator does that; we start at the
  demodulated nav frame. (An SDR front-end is out of scope, same as radiolistener's Rust note.)
- **We do not compute PVT/RTK solutions.** That's the receiver's job and `tudorgps`'s lab; we
  monitor the *broadcast*, we don't navigate.
- **We do not gold-plate.** Every civil signal we can decode, decoded well; no speculative
  support for signals no receiver in the fleet can hear.
