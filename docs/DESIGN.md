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
u-blox as **UBX-RXM-SFRBX**, Septentrio as **SBF** nav-bit blocks, or as **RTCM3** captured
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
  raw nav frames over      gnss module      per-SV ephemeris store;   TimescaleDB    the native v2 API:
  GNF1 (authenticated      frame decoders   Kepler/RK4 ECEF; orbit-   (raw frames +  svs/global/observers/
  push) + dev-LAN pull     → typed nav      disco + clock-disco vs    decoded +      almanac/sbas feeds,
  from our own receivers   messages         last eph; delta-Hz;       events)        events + SSE
                                            health/URA; spoof gates
```

(Radiolistener's NORMALIZE and FUSE stages are GNSS-specific here — DECODE and
PROPAGATE+INTEGRITY — but the skeleton and the stage boundaries are the same.)

### Stage 1 — Ingest: forward raw frames, never decode at the edge

Mature receivers already demodulated the signal on the observer's box; we read their raw
nav-frame output and forward it untouched. Per source:

| Source | Producer (on the receiver) | Raw frames `navlistener` consumes |
|---|---|---|
| **u-blox** | the F9P/F9T/F10 itself | **UBX-RXM-SFRBX** (raw nav words, per gnssId/sigId) + **UBX-RXM-RAWX** (pseudorange/Doppler/carrier-phase) + **UBX-NAV-SAT/SIG** (C/N0, el/az, prRes) + **UBX-NAV-HPPOSECEF/PVT** (receiver position) + **UBX-MON-HW/MON-RF** (jamming; MON-RF on F9+). |
| **Septentrio** | mosaic-X5 / PolaRx | **Capture-only:** SBF blocks are framed, CRC-checked, and raw-persisted; live navigation decode is planned. |
| **RTCM3** | any RTCM source / caster | **Capture-only:** ephemeris/SSR messages are framed, CRC-checked, and raw-persisted; live navigation/SSR decode is planned. |

The `gnss/frame` decoders live centrally; the feeder is dumb. Ingest is a **registry
of thin connectors** — read a local feed → frame → ship — so a new receiver type (Unicore,
Quectel, a bare NMEA+RTCM caster) is one connector, not a rewrite.

Two ingest modes, exactly as radiolistener:
- **Dial** (dev / our own boxes): `navlistener` opens a TCP connection to a known receiver's
  UBX/SBF port on the LAN. Trusted RF, unauthenticated (we control the box).
- **Push** (the fleet): the C `navfeeder` on each receiver connects *out* to the collector's
  authenticated push endpoint over TLS (GNF1). This is the production path.

### Stage 2 — Decode: the `gnss` library (top-level module)

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

Same discipline as radiolistener (see `docs/OUTPUT.md §4` for the schema): a `nav_frames`
hypertable holds the **raw frame bytes + the decoded jsonb** side by side, organized on the
near-monotonic ingest clock, tagged with `decoder`/`decoder_ver` so a decoder fix can re-run
history. `gnss_snapshots` stores periodic feed dumps; `gnss_events` stores confirmed integrity
transitions (Phase 2). Written via `pgx CopyFrom`, compressed + retained per policy.

### Stage 5 — Serve: the native API

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
| `HELLO` (0x01) | feeder→collector | JSON `{token, station, feed, sw, session, zstd?}` — `feed ∈ {ubx, rtcm}` (SBF is rejected because GNF1 `frame_type` cannot carry SBF block numbers; NMEA is unimplemented). **`session` is REQUIRED** (regression fix, contract revision 2026-07-31): an opaque boot identity (1..64 chars of `[A-Za-z0-9._-]`; reference implementations use 32 hex chars) minted fresh whenever the feeder's DATA sequence space restarts from zero and reused while it continues — the C feeder persists it in each disk-spool header and replays prior-run files on separate connections under their original sessions; every process uses a fresh session for new captures; the ESP32's RAM-only ring mints one per boot. The collector's replay-dedup identity is (canonical authenticated observer, session, seq); the session is never trusted as observer identity. A HELLO without a valid session is rejected before WELCOME |
| `WELCOME` (0x02) | collector→feeder | JSON `{ok, error?, ack_interval_ms?, zstd?}`. **Normative: the compact Go `encoding/json` spelling** — servers MUST emit `"ok":true` / `"zstd":true` with no space after the colon (see the note below) |
| `DATA` (0x03) | feeder→collector | `[8B seq][framed raw record]` — see the record shape below |
| `ACK` (0x04) | collector→feeder | `[8B seq]` the **durable watermark** for this session (regression fix, revised by regression fix 2026-07-31): the highest seq through which every *received* sequenced frame has been durably resolved — committed by the historian (or deduped against an already-committed ledger claim), quarantined as unfixable, or classified never-persistable (telemetry, malformed). The feeder prunes its spool up to the ack, so the ack stalls — rather than data being lost — while the collector's database is down; the feeder's ack-stall watchdog (10 min) then cycles the connection so reconnect replay redelivers anything the collector shed during the outage. A collector running **without** a historian acks on receipt (the explicit live-only mode; the spool contract is then best-effort by configuration). gap rule is unchanged: never-received sequences are skipped past, not waited for; reconnect replay makes any resulting duplicate harmless |
| `PING`/`PONG` (0x05/0x06) | both | keepalive |
| `SIGNED_DATA` (0x07) | feeder→collector | *(hardware tier, vNext)* a `DATA` batch + trailing ATECC ECDSA signature over `EUI-64 ‖ rtc_unix_ns ‖ sha256(payload) ‖ counter` |

**`WELCOME`'s compact spelling is part of the wire contract, not an implementation detail**
. The edge feeders — `feeder/navfeeder.c` on OpenWrt routers, `esp32/components/gnf1`
on an ESP32-C6 — accept the handshake by matching the literal byte sequences `"ok":true` and
`"zstd":true`, deliberately trading a JSON parser they cannot afford for a substring match.
Whitespace-formatted-but-equivalent JSON (`"ok": true`) is therefore *rejected*: a second
collector implementation, or a proxy that re-serializes the payload, would leave every feeder
in a permanent reconnect loop with no accepted welcome. Emit compact JSON. The constraint is
repeated at the write site (`wire.MarshalWelcome`) and regression-guarded end-to-end by the
`TestNavfeeder*` suite, which runs the real C feeder against the real collector.

**The raw record inside a `DATA` frame** carries just enough envelope for the collector to
dispatch without decoding: `{recv_unix_ns (from the observer/RTC), gnssId, svId, sigId,
freqId (GLONASS FDMA channel, `k = freqId − 7`; 0 otherwise), frame_type (GNF1 nav-type from
the registry in CONSTELLATIONS.md), raw_bytes}`. For UBX the
feeder can forward the whole SFRBX message verbatim and let the collector split it; the record
envelope is what lets a Septentrio and a u-blox feeder converge on one collector code path.

Resilience (galmon's "never go down," radiolistener-proven): a bounded RAM ring with monotonic
sequence numbers; replay all unacked frames on reconnect; spill the oldest to a disk spool on
overflow (`--spool-file`, with `--spool-disk-mb` cap) that survives reboot; backoff-reconnect
forever (exponential, 30 s cap), never exit. zstd (feeder→collector direction only, ACKs stay
plaintext) gives ~3–4× on the repetitive nav bitstream. TLS: the collector floor is 1.2, and
the C feeder **pins TLS 1.2 exactly** — radiolistener's regression fix finding: its split reader/writer
threads on one SSL object are unsafe under TLS 1.3 KeyUpdate — a constraint `navfeeder`
inherits with the port. Every frame length is validated against `MaxFrameLen` (1 MiB, RLF1
parity) before allocation.

**Wire implementation.** GNF1 uses a fixed record header that can be implemented
in a small C binary or ESP-IDF firmware. Adapters for other wire formats run
as separate programs.

---

## 3. Node identity & the hardware observer

`navlistener` reuses radiolistener's **AAA control plane verbatim** (`radiolistener/docs/
IDENTITY-AND-AAA.md`): one shared PostgreSQL on `collector-host`, Django owns identity/policy/CA/audit,
the collector reads `Device`/`Credential` for a single indexed auth lookup and writes
accounting back. Feed grants are **as-built** the `feed_types` array field on `Device`
(radiolistener's AAA doc sketches a `FeedGrant` model, but the shipped Django implements
`Device.feed_types` — we follow the code). A GNSS observer is just a `Device` whose
`feed_types` include `ubx`/`sbf`/`rtcm`. Nothing about AAA is GNSS-specific, so we do not
re-invent it — we add GNSS feed types and reuse the CA, enrollment, revocation
(`enabled=false`), and trust scoring.

Three credential tiers → trust (radiolistener's ladder, unchanged):
1. **Bearer token** (bootstrap) — SHA-256 stored, shown once.
2. **Software mTLS cert** — a single DNS SAN = `receiver_id` (see the note below; the *SAN*, not
   the CN, is what the collector matches).
3. **ATECC608-anchored mTLS cert** — the **high-assurance receiver class**. The board is the
   ESP32 + secure-element + RTC + EUI-64 design in `radiolistener/docs/HARDWARE-OBSERVER.md`,
   with this product's part choices and their `shepherdprotocol` alignment in
   `docs/HARDWARE-OBSERVER.md` (**ATECC608C**, MCP79412 — note radiolistener's doc still names
   the 608B and a DS3231, which predates the shared `esp32-hardware-discovery` conventions),
   ported here as `firmware/navfeeder-esp`. The ATECC generates a non-extractable P-256 key,
   signs a CSR carrying the EUI-64-derived `receiver_id` as **exactly one DNS SAN** that the
   Django CA signs; the private key
   never leaves silicon. The MCP79412 stamps a **trusted time-of-transmission** — and for GNSS
   there's a bonus: the receiver *is* a clock source, so the board can discipline the RTC from
   GPS PPS, closing the loop — but see `docs/HARDWARE-OBSERVER.md §6.3`: disciplining the RTC
   from the signal it exists to cross-check is a coupling to bound, not to close blindly.
   The optional `SIGNED_DATA` frame (0x07) raises the provenance
   tier to hardware-signed batches.

**The exact certificate shape is enforced, not conventional** (`matchPeerIdentity`,
`go/internal/ingest/push.go:472`): the chain's leaf must carry **exactly one DNS SAN**, byte-equal
to the canonical observer id, and that id must satisfy `config.ValidObserverID` — `[A-Za-z0-9.-]`
only. The comparison is byte-exact rather than DNS-case-insensitive, so the rendering is fixed by
convention and not negotiable per device: **lowercase, hyphen-separated byte pairs, bare label**
(`00-04-a3-ff-fe-12-34-56`). Rationale and the rejected alternatives are in
`docs/HARDWARE-OBSERVER.md §4.2`. A CSR that sets only a CN is rejected at handshake.

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
`client_ca`), separate listener from `[serve]`. Prometheus `/metrics` + `/healthz` on
loopback; counters for SVs tracked, frames/s/constellation, decode failures, receiver up/down,
integrity events, DB lag, per-observer drops — so it slots into the same Zabbix/Prometheus
monitoring as radiolistener.

`navlistener -check-config` validates the config with full startup parity : it
stat/loads the `[push]` TLS keypair and `client_ca`, parses the `[store]` DSN, and
PEM-validates every ntrip `ca_file` — so it **fails on a host whose certs are not yet
provisioned**. That is deliberate (a config that passes `-check-config` must also start), but
it means the check belongs *after* cert provisioning in any deploy runbook, not before.

---

## 6. Engineering priorities

**Architecture:** dumb-edge/smart-center; raw-frame-as-record;
spool+ack+replay resilience; the generic Keplerian propagator abstraction; the integrity
signal set (orbit-disco, time-disco, delta-Hz, health/SISA, RTCM precise-vs-broadcast); the
debounced alert state machine; the five-feed *concept* (svs/global/observers/almanac/sbas —
reshaped to our own contract, `docs/OUTPUT.md`).

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

### C spool recovery and migration 

The NAVSPO01 bytes are unchanged. At startup, a valid prior-run `--spool-file F` is moved to `F.replay.<session>` with a hard link and directory fsync before the old name is removed. Existing replay files are discovered on every restart. They are immutable and sent on separate GNF1 connections under their original session; after durable ACK and removal, the consumer advances to the next file and finally the current capture session. New captures always start a fresh random session, including while old files wait for the collector. Missing entropy delays startup instead of deriving a repeatable identity from an RTC-less clock. Older builds must not be used to drain this new multi-file layout.

Retained files share the configured disk byte budget with the current file, and at most 256 replay files are admitted at startup. Failures to read, validate, archive, or enumerate evidence disable new disk overflow for that run while fresh RAM capture continues. Original uncertain files remain for filesystem repair and a restart; they are never overwritten. Only a clean EOF permits discarding a torn final record; corrupt lengths, nonincreasing sequences, short session headers, and read errors preserve the file. Recognizable pre-session headerless records remain unsupported and are discarded with a diagnostic.

A crash between archive linking and unlinking can leave both names; recovery recognizes the same inode, and collector deduplication makes repeat delivery harmless. Append rollback can lose a torn tail but cannot make a new observation reuse an old identity. Orderly shutdown flushes only the current session's unacked RAM records and fsyncs them; existing replay files retain their original bytes. As before, unclean power loss may lose a page-cache/RAM tail, and configured overflow limits may drop records. Neither case extends a recovered session with new content.

Durable receipt tracking is capped at 1,024 sessions, 4,096 unresolved received sequences per session, and 65,536 unresolved sequences overall. A frame that would exceed these budgets closes the push stream before decoder handoff; it is not added to the watermark. Reconnects can always re-admit an already tracked hole, allowing historian recovery to release space. Unresolved sessions never expire; only fully resolved sessions idle for seven days are reclaimed, with capacity sweeps rate-limited to once per minute. The indexed per-session minimum makes ACK lookup constant time, while receipt and commit bookkeeping take logarithmic time within the fixed per-session budget. Live-only collectors retain receipt-based ACK behavior.
