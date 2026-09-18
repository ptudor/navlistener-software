# `internal/wire` — GNF1, the feeder↔collector framing

**Headline:** the protocol a `navfeeder` edge feeder speaks to the collector over TLS. It carries
**raw broadcast nav frames** — the feeder forwards, the collector decodes — with a handshake,
sequence numbers, durable acknowledgements, and reconnect replay. It is authored fresh, the same
resilient shape as radiolistener's RLF1, with separate protocol adapters for other formats.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `wire.go` | The whole protocol: magic, frame types, the handshake messages, the raw-record envelope, and the encode/decode helpers. |
| `wire_test.go` | Round-trips, bounds rejection, and the session-charset rule. |
| `README.md` | This file. |

Three implementations speak this wire: this package (collector side), `feeder/navfeeder.c` (the
C edge feeder), and `esp32/components/gnf1/` (the ESP32 firmware). That matters — see the regression fix
note below.

---

## Summary

### The stream

```
[TLS handshake]
"GNF1"                                    ← Magic, once, before any frame
[1B type][4B BE length][payload]          ← every frame thereafter
```

### Frame types

| Byte | Type | Direction | Payload |
|---|---|---|---|
| 0x01 | `Hello` | feeder → collector | JSON `HelloMsg` |
| 0x02 | `Welcome` | collector → feeder | JSON `WelcomeMsg` |
| 0x03 | `Data` | feeder → collector | `[8B seq][record header][raw bytes]` |
| 0x04 | `Ack` | collector → feeder | `[8B seq]` — the **durable watermark** |
| 0x05 | `Ping` | either | keepalive |
| 0x06 | `Pong` | either | keepalive |
| 0x07 | `SignedData` | feeder → collector | hardware tier (vNext): a Data batch + ATECC ECDSA |
| 0x09 | `UpdateControl` | collector → feeder | versioned 36-byte update command |
| 0x0A | `Evidence` | feeder → collector | commissioning record, microcontroller key and session proof; handshake only |

### Bounds

```go
const MaxFrameLen   = 1 << 20  // 1 MiB
const SessionMaxLen = 64
```

**Every length prefix is validated against `MaxFrameLen` before allocation**
to bound untrusted reads and memory use (`docs/INTEGRITY.md §9`).
`SessionMaxLen` comfortably covers the reference implementations (32 hex chars) while keeping the
pre-auth HELLO small — the frame an unauthenticated peer can make us allocate for.

Errors: `ErrBadMagic`, `ErrFrameTooLarge`, `ErrShortRecord`.

---

## Details

### The ACK contract — read this before changing anything about acknowledgement

This is the most consequential design decision in the package, revised once (regression fix, then regression fix).

**An ACK carries the highest sequence N such that every sequenced frame ≤ N the collector
*received* has been durably resolved** — committed by the historian, deduped as a replay of an
already-committed ledger claim, quarantined as unfixable poison, or classified never-persistable
(telemetry, malformed body).

Why it changed: the feeder **prunes its spool up to the acked sequence.** Before the revision,
ACK meant "queued in RAM" — so the feeder discarded its only replay copy while the historian
could still fail, and a DB outage after ACK permanently erased raw forensic evidence with *both
sides behaving exactly to spec*. Now the ack simply stalls until the store commits, and the
feeder's spool (plus its ack-stall reconnect watchdog) carries the outage.

Two properties survive the revision unchanged, and a second implementer must honor both:

1. **Reception gaps are never waited for.** A sequence that never arrived is skipped past — only
   *received* frames can hold the watermark back. Do not wait for contiguity over unreceived
   sequences.
2. **Reconnect replay makes duplicates harmless.** The feeder resends everything after the last
   ack; live decode tolerates duplicates and the historian dedups on sequence.

A collector running **without** a historian acks on receipt — the explicit, documented live-only
mode. The watermark computation itself lives in `ingest.DurableTracker`.

### regression fix — the `WelcomeMsg` payload must stay compact JSON

**Normative, and it will silently break the fleet if violated.**

The C feeder and the ESP32 firmware accept the handshake by matching the exact byte sequences
`"ok":true` and `"zstd":true` with `strstr` — they do **not** carry a JSON parser, because that
binary has to fit on an OpenWrt router and an ESP32-C6. That's a deliberate size trade, so the
constraint belongs at the write site: `MarshalWelcome` must emit the compact `encoding/json`
spelling, with no space after the `:` and no pretty-printing.

Reformatting to `json.MarshalIndent`, hand-rolling the JSON with a space after the colon, or
interposing a proxy that re-serializes the payload **permanently breaks authentication and zstd
negotiation for the whole fleet** — feeders would reconnect forever, never seeing an accepted
welcome. `json.Marshal`'s output is compact by contract, so today's line satisfies it; the
requirement is on the **wire**, and it's stated in `docs/DESIGN.md §2 (The GNF1 wire)` for any
second implementation. `TestNavfeeder*` — the real C feeder run against this collector — is the
regression guard.

### Hardware evidence in the handshake

A feeder that holds a commissioning record sets `"evidence": true` in its HELLO and sends one
`Evidence` frame straight after it, without waiting for WELCOME. The payload layout, the
evaluation order and the rejection reasons are normative in
[`docs/COMMISSIONING.md` §6](../../../docs/COMMISSIONING.md); `internal/commissioning` parses and
verifies it. This package only names the frame and carries the three JSON fields:

- `HelloMsg.Evidence` — omitted when false, so a feeder without a record emits the same HELLO
  bytes as before.
- `WelcomeMsg.HardwareTrust` and `WelcomeMsg.EvidenceError` — present only in answer to a HELLO
  that announced evidence. They are informational for the device's journal and never change
  admission. The compact `"ok":true` rule above applies unchanged.

The collector reads the frame only after the HELLO has authenticated, under the handshake
deadline and a 2048-byte cap, so an unauthenticated peer can never make it parse a record or
verify a signature. `Evidence` is not valid in the DATA phase.

### `ValidSession`

```go
func ValidSession(s string) bool  // 1..64 bytes of [A-Za-z0-9._-]
```

The charset deliberately mirrors `config.ValidObserverID`, so a session identity can never
smuggle JSON or SQL metacharacters into logs or the historian ledger. A session is a pre-auth,
peer-supplied string; treating it as trusted text anywhere downstream would be exactly the wrong
move.

### Sessions and reconnects

The session identity is what makes reconnect accounting work. A **reconnect** (same session,
replayed frames) resumes the same durability accounting; a **rebooted feeder** (fresh session,
regression fix) starts clean. `DurableTracker` keys on `(observer, session)` for precisely this reason.

### What this package is not

- **Not the dial path.** Dial-mode ingest reads receiver bytes directly and never touches GNF1.
- **Not a decoder.** It defines the envelope; `ingest` and `gnss/frame` interpret the contents.
- **Protocol adapters.** Any galmon interoperability is
  out-of-process in the optional `galmon-bridge`, and CI enforces that the core never imports it.

---

## Tests

`wire_test.go` covers frame round-trips, the magic check, `MaxFrameLen` rejection before
allocation, `ErrShortRecord` on a truncated DATA envelope, ack encode/decode, the
`ValidSession` charset, and the evidence fields' spelling.

The **cross-implementation** tests live in `../ingest`: `TestNavfeeder*` builds the real C feeder
and runs it against this collector (`make check-e2e`). That's the only test that can catch a
regression fix violation, because it's the only one where a non-Go implementation parses the welcome.

```sh
go test ./internal/wire/
make check-e2e          # from go/ — builds the C feeder and runs the real handshake
```

---

## See also

- `../ingest/README.md` — `PushServer` (the collector side) and `DurableTracker`.
- `../../../feeder/` — the C edge feeder.
- `../../../esp32/` — the ESP32 firmware feeder.
- `../../../docs/DESIGN.md §2` — the GNF1 design and the second-implementer notes.
- `../../../docs/INTEGRITY.md §9` — the untrusted-input discipline.
