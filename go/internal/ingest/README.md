# `internal/ingest` — raw-frame connectors and the authenticated push server

**Headline:** the front door. Two very different transports — we dial out to receivers on our LAN,
and fleet feeders dial in to us over TLS — normalized into one `RawFrame` type so nothing
downstream has to care which way the bytes arrived. **No decoding happens here.** Read a feed,
frame it, emit it. The edge is dumb and so is this stage.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `ingest.go` | `Manager` — one dial connector per configured source, with forever-reconnect. |
| `rawframe.go` | The `RawFrame` type and the package doc. |
| `ubx.go` | UBX framing and the RXM-SFRBX / RXM-RAWX / MON-RF / MON-HW / NAV-SAT parsers. |
| `sbf.go` | Septentrio SBF block framing. |
| `rtcm.go` | RTCM3 message framing. |
| `ntrip.go` | The NTRIP caster client transport. |
| `push.go` | `PushServer` — TLS termination, feeder authentication, GNF1 stream handling. |
| `durable.go` | `DurableTracker` — the GNF1 ACK watermark under the regression fix durability contract. |
| `telemetry.go` | GNF1 telemetry record codecs (RF state and per-SV C/N₀). |
| `testdata/` | Real captured UBX byte streams — see `testdata/README.md`. |
| `*_test.go` | Unit tests, real-frame regression, and the C↔Go feeder end-to-end suite. |

---

## Summary

### Two transports, one output

```
  dial mode                                  push mode
  ─────────                                  ─────────
  Manager                                    PushServer
    └─ one goroutine per [[ingest]] source     └─ TLS listener, mTLS optional
       dials host:port, frames bytes              authenticates each feeder (HELLO)
       reconnects forever with backoff            reads GNF1 DATA frames
                    │                                        │
                    └────────────► *RawFrame ◄───────────────┘
                                       │
                                  bounded channel
                                       ▼
                                  decode + state
```

`RawFrame` carries reception time, source name, `gnssId`/`svId`/`sigId`/`freqId`, the nav words
or raw bytes, and — for push-path frames — the feeder's global sequence. Telemetry frames carry
`Obs` or `RF` instead of nav words.

### The reconnect discipline

Receivers are treated as flaky, always. A connector **reconnects forever with exponential
backoff and never exits on its own.** That's galmon's rule and we kept it: the receiver must
never go down.

The channel to the decode stage is bounded (8192, set in `main`). **A full channel backpressures
ingest rather than dropping silently** — a slow decoder shows up as ingest pressure, not as
missing data with no signal.

---

## Details

### Dial connectors

| Type | Framing | Notes |
|---|---|---|
| `ubx` | sync `0xB5 0x62`, class/id, LE length, payload, 2-byte Fletcher checksum | The primary path. RXM-SFRBX carries raw nav words; RXM-RAWX carries observables; MON-RF/MON-HW/NAV-SAT carry RF telemetry. |
| `sbf` | sync `$@`, LE CRC-16, ID (low 13 bits = block number), LE length (multiple of 4), body | CRC-16-CCITT covers ID+Length+body. SBF delivers already-de-interleaved ICD nav bits, so the raw block body is emitted tagged with its block number. |
| `rtcm` | `0xD3` preamble, 6 reserved bits + 10-bit BE length, payload, 3-byte CRC-24Q | The message number is the first 12 bits of the payload. The raw payload is emitted tagged with its message number. |
| `ntrip` | HTTP-ish caster subscription wrapping one of the above | Basic auth; TLS by default with explicit opt-outs required for plaintext. |

**SBF and RTCM are currently capture-only inputs** pending central decoders — `capture_only`
persists their raw frames without decoding into live state.

### Two silence timeouts, and why both exist

- **Idle timeout** — no *bytes* for too long. Catches a half-open peer.
- **`max_frame_silence`** (default 5m, regression fix) — bytes flowing but no *decodable frame*. Catches
  an F9 reset to factory NMEA output, a mis-pointed TCP port, a caster streaming an HTML error
  page. Without it, `source_up` stays 1 and `frames_total` stays frozen, indefinitely.

The second one also feeds `navlistener_source_last_frame_timestamp_seconds`. One alerting caveat
worth knowing : the connect-time seed means every watchdog-driven re-dial resets that
gauge, capping the observable age near `max_frame_silence + backoff` — so an age-based alert only
fires reliably for thresholds *below* `max_frame_silence`, and the `frame_silence` error counter
is the unconditional signal for longer outages.

### `PushServer` — the production fleet path

```go
func NewPushServer(cfg config.Push, out chan<- *RawFrame, auth Authenticator, log *slog.Logger) (*PushServer, error)
func (p *PushServer) Listen() (net.Listener, error)
func (p *PushServer) Serve(ctx context.Context, ln net.Listener) error
func (p *PushServer) SetDurableTracker(t *DurableTracker)
func (p *PushServer) SetReauthorizationInterval(every time.Duration)
```

Terminates TLS, authenticates each feeder, and forwards decoded frames into **the same channel
the dial connectors feed** — one code path downstream regardless of ingest mode.

Authentication is layered:

1. **Bearer token** (the bootstrap tier) — always checked. The config stores its SHA-256; the
   token itself is shown once at enrollment.
2. **mTLS** (when `client_ca` is set) — the client certificate is required and verified, and
   `matchPeerIdentity` compares the certificate's single DNS SAN **byte-for-byte** against the
   configured station name. That byte-for-byte comparison is why `config.ValidObserverID` forbids
   case folding and Unicode aliases.

```go
type Authenticator interface {
    Authenticate(ctx context.Context, token, station, feed string) (identity.ObserverContext, ok bool)
}
```

Authentication resolves more than the canonical station string. The returned context contains
the server-owned organization, enrollment, collector instance, collection memberships,
feed grants, declared hardware capabilities, credential/attestation evidence, and receipt-time
publication revision. `stream` stamps that context onto every `RawFrame`; no feeder DATA field
can set or override it. Its collector instance must match this deployment's configured realm.

`NewConfigAuthenticator` is the fail-closed bootstrap implementation. Omitted scope becomes
`local-unassigned/private`; a configured public policy must be explicit. The production
`internal/authorization.Provider` reads a versioned control-plane view, retains only bearer
token digests in its bounded cache, and invalidates on PostgreSQL `NOTIFY`.

Active-session reauthorization does more than close the socket. After the changed session stops
forwarding DATA, the handler emits an ordered `ScopeRevocation` barrier through the same decode
channel. The collector can therefore invalidate every audience built from the previous context
after that session's queued receipts, never before them. The last context per observer also
catches a transfer that occurred while the feeder was disconnected, before its next DATA frame.

When mTLS is enabled, the resolved active credential fingerprint must match the exact leaf
certificate used on the connection. A `hardware_mtls` row additionally requires verified
manufacturer attestation; without both proofs the handshake fails rather than downgrading or
retaining a misleading hardware label. Every active connection is re-authorized on the
configured cadence and is closed if credential, enrollment, ownership, memberships,
attestation, or publication context changes.

### Hardware evidence

A HELLO may announce one `EVIDENCE` frame (`docs/COMMISSIONING.md` §6). The handshake reads it
only after the HELLO has authenticated and named a usable feed and session, under the same
handshake deadline and a 2048-byte cap, so an unauthenticated peer can never make the collector
parse a record or verify a signature. A flag with no `EVIDENCE` frame behind it is a protocol
error that refuses the session, because the stream cannot be resynchronised.

`internal/commissioning.Verifier` evaluates the evidence against the authenticated observer id
and the TLS session's exported keying material, and the result is stamped on the context
**before** admission as `HardwareTrust` and `CommissioningFingerprint`, so every receipt of the
session carries it. Evidence never refuses a session: a rejection leaves the session
`hardware_trust = none`, answers the reason in WELCOME, logs it and counts
`navlistener_push_evidence_rejected_total{reason}`. A collector with no verifier
(`SetEvidenceVerifier(nil)`) still reads the frame and answers `unconfigured`.

Session evidence is deliberately outside the authorization machinery described above.
`ObserverContext.AuthorizationEqual` ignores it, so:

- the periodic recheck, which resolves a context with no evidence by construction, is not a
  change, and nothing in it can raise a session's trust — the context stamped on receipts is
  never replaced;
- sessions of one observer that proved different things share one policy generation, and no
  `ScopeRevocation` barrier is emitted for the difference. Hardware trust selects no audience
  and no publication rule, so there is nothing to reset.

The registry is the one input that can change underneath a live session, and only downward. The
recheck therefore also calls `Verifier.Recheck`: a board withdrawn or superseded while connected
has its session closed, and the feeder's reconnect is evaluated against the registry in force.
That close is not a policy transition either.

`Listen()` is called **synchronously at startup**, before any producer or historian goroutine, so
a bad certificate or an already-bound address kills the process rather than leaving a
half-started daemon.

### `DurableTracker` — the ACK watermark

```go
func (t *DurableTracker) Received(source, session string, seq uint64, persistable bool)
func (t *DurableTracker) Resolved(source, session string, seq uint64)
func (t *DurableTracker) Watermark(source, session string) uint64
```

Computes, per `(observer, session)`, the value sent in a GNF1 ACK — see `../wire/README.md` for
the full contract and why it changed.

Three behaviors worth internalizing:

- **`Received` is called before the frame is handed to decode**, so a resolution can never race
  its own arrival.
- **`persistable = false`** marks frames that will never reach the historian — telemetry,
  malformed bodies — so they advance the watermark without ever holding it back.
- **`Resolved` on an unknown or already-resolved sequence is a harmless no-op**, because the store
  notifies per batch entry and a batch can span a reconnect's replayed duplicates.

`Watermark` can regress transiently when a reconnect replays a low sequence. That's invisible on
the wire: `stream()` only sends monotonically increasing acks per connection, and the feeder
additionally ignores regressions.

**The tracker is nil when the collector runs without a historian** — live-only mode, where the
stream acks on receipt.

### Telemetry over the same DATA frame

Receiver-side metadata — RF front-end state and per-SV C/N₀ — isn't a broadcast nav frame, but
the PNT-defense Tier-0 detector needs it from **every** fleet station, not only the LAN-dialled
ones. So telemetry rides the same GNF1 DATA frame (spool, ack, replay, and zstd all unchanged),
discriminated by the record's `frame_type` byte:

- **telemetry types are `< 0x10`**, raw-nav frame types (§6.1) are `≥ 0x10`
- `TelemReceptionData = 0x01` — NAV-SAT per-SV C/N₀ + elevation → the spoof gate
- `TelemJammingStats = 0x05` — MON-RF/MON-HW AGC + jamming + antenna → the jam gate

The record's `gnssId`/`svId`/`sigId`/`freqId` header fields are unused for telemetry (the sample
is station-scoped, keyed by observer) and the body is carried verbatim.

Bodies are versioned and bounded, and `ErrBadTelemetry` covers truncated, over-long, or
unrecognised-version bodies — the untrusted-input discipline applied to the push path exactly as
the dial-mode parsers apply it. `EncodeReceptionData` caps the satellite count so the body fits
the feeder's record buffer.

Only the two types the RF detector consumes are transported today; the remaining §6.2 types get
their own body codecs when their features land.

### `RawFrame.Seq` and historian dedup

Push-path frames carry the feeder's GNF1 global sequence. On reconnect, DATA frames past the last
ack are **replayed** — decode and live state tolerate the duplicate, but the historian must not
, so `Seq` is the dedup key `store.Store` uses to drop replayed rows. Dial-mode frames
have no sequence and always persist.

---

## Tests

Three layers:

- **Unit** — framing and parser edge cases per transport, telemetry codec round-trips, the
  durable-tracker state machine, authentication paths including mTLS SAN matching.
- **Real-frame regression** (`realframes_test.go`) — committed UBX captures replayed through the
  actual parsers and the real `gnss/frame` decoders. This is where cross-signal agreement is
  proven: `TestRealBeiDouD1AgreesWithBCNAV2` (B1I vs B2a) and `TestRealGalileoFNAVAgreesWithINAV`
  (E5a vs E1-B) would each catch a wrong-but-plausible field offset that no synthetic test can.
  See `testdata/README.md`.
- **Evidence over real TLS** (`evidence_test.go`) — the session proof is bound to keying material
  only a real handshake produces, so these tests dial the push server: trusted over TLS 1.3 and
  1.2, open, every rejection reason, a proof relayed from another session, evidence behind a
  failed authentication, recheck and second-session behaviour, and a live registry withdrawal.
- **End-to-end with the real C feeder** (`TestNavfeeder*`) — builds `../../../feeder/` and runs it
  against this collector, covering the handshake (including the regression fix compact-JSON
  requirement), sequencing, ack-driven spool pruning, session resume, and SIGPIPE behavior.

```sh
go test ./internal/ingest/
make check-e2e     # from go/ — builds the C feeder first
```

`go test ./...` stays toolchain-optional: the feeder tests visibly skip when no C binary exists.

---

## See also

- `../wire/README.md` — the GNF1 protocol and the ACK contract.
- `../state/README.md` — what happens to a `RawFrame` next.
- `testdata/README.md` — the capture fixtures.
- `../../../docs/DESIGN.md §1` — the connector design; `../../../docs/CONSTELLATIONS.md §2, §6` —
  framing and the GNF1 record types.
