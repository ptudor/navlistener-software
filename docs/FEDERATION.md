# navlistener — federation & inter-collector trust, design report

**Status: design (2026-07-14).** No code yet; this is the agreed spec-level shape for how
`navlistener` collectors **federate** — pool observations across administrative and geographic
boundaries and establish trust between one another. It sits between `docs/DESIGN.md §3` (node
identity, the *feeder→collector* edge) and `docs/INTEGRITY.md` (the physics gates that make
federated data safe to accept). Read `DESIGN.md` first; this doc only adds the new
**collector↔collector** edge.

> One line: **collectors form a small, human-vetted peer graph; each directed edge carries a
> per-constellation trust level; observations flow as GNF1 records with the *original
> observer's* ATECC signature preserved end-to-end; a peer's trust level gates only what an
> observation is *allowed to do* — never whether it is physics-checked. Federation is additive:
> a solo collector works with zero peers.**

---

## 0. What federation is here (and what it is not)

Today (`DESIGN.md §3`) `navlistener` is a **star**: one central daemon on `collector-host`, one Django CA,
one `Device` table, one control plane. Feeders authenticate *inward* up the three-rung ladder
(bearer token → software mTLS → ATECC608-anchored mTLS, CN = EUI-64 `receiver_id`); revocation
is a `Device.enabled=false`. Feeders neither know about nor trust each other. There is no
collector↔collector concept anywhere.

Federation adds exactly one new edge — **between collectors** — and nothing else. The
feeder→collector AAA plane is untouched. This mirrors the organizing principle we borrow
wholesale from `apps/organizefor` (`federation` app): **federation is additive — the platform
works standalone; federation is a layer on top.** A collector with an empty peer list behaves
exactly as it does now.

The one fact that makes GNSS federation fundamentally safer than voter-data federation: a peer
does not send you a *private assertion* you must trust — it sends you an **observation of a
shared physical fact** ("receiver X heard SV E14 broadcast *these raw bits* at *this GST*"). You
never have to trust that. You **verify the physics** — the design rule from `DESIGN.md §3`,
*"verify physics, not signatures,"* applied to a peer's data. The three things a bad federated
observation can be — a bug, a spoof, or a lying peer — are precisely the three the integrity
monitor (`docs/INTEGRITY.md`) already exists to catch.

> **Trust degrades gracefully into an integrity signal, it is not a gate.** Every federated
> observation is physics-checked regardless of the peer's trust level. Trust decides the
> *blast radius* of an observation — what it is permitted to *do* — never whether it is
> examined. A lying peer cannot inject a false orbit; it can at worst add noise the
> cross-receiver corroboration and plausibility gates already reject.

---

## 1. Two fan-outs — the snowflake, mapped honestly

`organizefor`'s snowflake caps a cell at ~7–8 because that is a human's *span of control* (a
director can develop about that many people). For collectors the instinct is right but splits
into **two different fan-outs with two different limits**:

### 1.1 Vertical — the aggregation tree (bounded by load / blast-radius)

```
   feeders  ──star, existing AAA──▶  per-country collector
   per-country collectors  ──federation──▶  per-continent collector
   per-continent collectors  ──federation──▶  root
```

Star-with-AAA at the bottom (exactly what exists), federation *between the collector tiers*.
Each tier re-exports a **deduplicated, corroborated** stream upward, so the root never sees raw
fan-in from thousands of feeders. "How many feeders per country collector" is just "what the
box and its bandwidth handle" — not a ~7 limit.

Intra-org tiers **may share one AAA plane** (a continental collector reading the same `collector-host`
`Device`/`Credential` rows) — that case is pure topology and load-shedding, not trust
negotiation. The interesting edge is *cross-org*, below.

### 1.2 Horizontal — the peer mesh (this is where ~7–8 genuinely applies)

Same-tier collectors peer laterally: you ↔ an allied organization's collector, or two
continental collectors cross-checking globally-visible SVs. The real limiter on a **`trusted`
(high) peering** is *how many organizations an operator can meaningfully vet* — a Dunbar-ish
handful. So:

- Cap **high-trust** direct peers at **~7–8 per operator, by policy** (not by protocol).
- Leave **`read_only` (light / public-feed) peers unbounded** — they are cheap, because the
  physics gates and cross-receiver corroboration bound the damage a light peer can do.

---

## 2. The peer-trust model

Reuse `organizefor`'s four-level ladder verbatim as the peer-trust enum, but **bind each level
to a capability, not to acceptance**:

| Your term | Peer trust | What an observation from this peer may do |
|---|---|---|
| **light / public feed** | `read_only` | Ingested as **corroboration only**. Counts toward cross-receiver agreement (`state.perrecv`) but may **never originate an integrity alert alone**, and lands in a **quarantined historian partition tagged with the peer** — never the authoritative `nav_frames` hypertable. §7. |
| **high / directly-peered org** | `trusted` | Observers treated like our own: raw frames enter the authoritative hypertable, their `receiver_id`s may **arm detectors**, their `SIGNED_DATA` frames are honored as first-class provenance. |
| onboarding | `pending` | Accepted and physics-checked, but quarantined and surfaced in an operator review queue before promotion to `trusted`. Never arms detectors. |
| — | `revoked` | The peer edge of `enabled=false`. All sessions refused; retained observations demoted to quarantine and excluded from live state. |

Two refinements the domain forces:

### 2.1 Trust is per-constellation, not scalar

You would `trusted`-peer a Tokyo organization for **QZSS** — they are the *authority*, you
physically cannot hear QZSS from `collector-host` — while keeping them `read_only` for **GPS** (redundant
with your own fleet; no reason to let them arm your alerts). `Device.feed_types` already models
per-feed grants (`DESIGN.md §3`); the peer relationship carries a **per-constellation /
per-`(gnssId, sigId)` trust map**, reusing that pattern rather than a single scalar. The
default for an unlisted constellation is `read_only`, never `trusted` — trust is granted
explicitly, per signal.

### 2.2 Trust is directional

Peering is a *directed* edge. A grants B `trusted` for QZSS does not imply B grants A anything.
Each collector maintains its own inbound trust map for each peer; there is no symmetric-trust
assumption and no negotiation — the *receiving* side decides unilaterally what an inbound
observation may do.

---

## 3. Observer-level provenance, end-to-end (LOCKED)

**The locked decision: relayed observations preserve the original observer's ATECC signature
untouched. Collectors do NOT decode-and-re-sign on the way up.** A continental collector
forwarding a Japanese observer's QZSS frame passes the exact `SIGNED_DATA` bytes through; it
does not become the new signer.

Why passthrough over hop-by-hop re-signing:

- The `SIGNED_DATA` frame (0x07, `DESIGN.md §2`) already signs, **at the physical observer**,
  `EUI-64 ‖ rtc_unix_ns ‖ sha256(payload) ‖ counter`. That signature is **transitively
  verifiable**: collector A forwards you B's frame and you verify the *original observer's*
  silicon signed it, independent of A, B, and every hop between. This is strictly stronger than
  `organizefor`'s instance-level `signature` on a `FederationJournal` entry — we sign at the
  *edge*, so a high-trust peering does not require trusting the peer's whole pipeline. **Trust
  the org, still verify each observer.**
- It makes mesh **dedup and loop-safety fall out for free** (§8): the observer's monotonic
  `counter` is a stable, path-independent identity for the frame.

**The cost of passthrough — and the answer to the §11-preview question:** the verifying side
must be able to resolve the *original observer's* certificate, which was issued by the
observer's *home* Django CA, not the local one. Therefore:

- A relayed record carries `origin_receiver_id` (the EUI-64-derived CN) and the **observer
  leaf-cert fingerprint** in its envelope. The signed payload is unchanged; this is envelope
  metadata for resolution, outside the signature.
- Each collector maintains an **observer-cert directory** — the union of its own `Device`
  certs plus, for every `trusted`/`pending` peer, that peer's published observer leaf certs and
  the peer's issuing CA (its `InstanceCertificate`, §5). The root, sitting above the whole
  tree, therefore **holds every observer's cert** it is willing to honor as `trusted`. This is
  the accepted, explicit cost of end-to-end verification.
- A `SIGNED_DATA` frame whose `origin_receiver_id` is **not** resolvable to a cert chaining to
  a known peer CA is **not** rejected — it is *demoted* to the `read_only`/quarantine path
  (unsigned-equivalent), physics-checked, and flagged. Unverifiable provenance loses
  privileges; it does not lose the observation.

Unsigned frames (software-mTLS observers behind a `read_only` peer) simply travel as plain
`DATA` and can only ever reach the quarantine path — consistent with §2.

---

## 4. Corroborate vs. accept-with-provenance

GNSS has a geographic partition voter data lacks: **an SV is only visible from part of the
Earth, and the regional systems are inherently local** (QZSS over APAC incl. its disaster
messages, NavIC over India, SBAS per region). So a federated observation is handled in one of
two modes, decided per SV:

- **Corroborate** — for **globally-visible MEO** SVs (GPS, Galileo, BeiDou-MEO, GLONASS) that
  your own fleet *also* hears. The peer's frame must **agree** with what you decoded from your
  own observers (same IOD → same ephemeris bits; propagated positions within the
  `docs/INTEGRITY.md` orbit-disco threshold). Agreement raises confidence; disagreement is an
  integrity event, not a silent merge. This is only meaningful when both sides can see the SV.
- **Accept-with-provenance** — for **regional** SVs you physically cannot hear (QZSS from a
  European root, NavIC from anywhere in the current fleet, a far-hemisphere SBAS). There is
  nothing local to corroborate against, so the observation is accepted **on the strength of its
  provenance tier**: a `trusted` peer's ATECC-`SIGNED_DATA` regional frame is authoritative; a
  `read_only` peer's is quarantined. Provenance substitutes for corroboration exactly where
  corroboration is physically impossible — which is why the observer signature (§3) is the
  load-bearing primitive for regional coverage.

The mode is derived, not configured: an SV the local fleet has *never* heard is
accept-with-provenance; one it currently tracks is corroborate.

---

## 5. The federation journal — control plane only

Borrow `organizefor`'s `FederationJournal` (append-only, hash-chained via `previous_hash`,
signed) and `InstanceCertificate` (trust levels `trusted`/`read_only`/`pending`/`revoked`) —
but **scope the journal strictly to control-plane events**, never to observations:

- Journal entries: `peer_added`, `trust_changed` (with the per-constellation map, §2.1),
  `observer_cert_published`, `observer_cert_revoked`, `peer_revoked`. Each is signed by the
  emitting collector's **instance cert** and hash-chained to the previous entry, so trust
  history is tamper-evident and a peer can replay it to reach current state.
- **Observations do NOT go through the journal.** They ride the existing GNF1 `DATA` /
  `SIGNED_DATA` firehose (§6). Journal = *who-may-do-what*; GNF1 = *the data*. Conflating them
  would put a hash-chain in the hot path for no benefit — nav frames are already individually
  signed at the edge.
- **Revocation propagates through the journal.** A `peer_revoked` or `observer_cert_revoked`
  entry gossips peer-to-peer; a collector applies it by demoting all affected retained
  observations to quarantine and refusing further sessions. This is the federated analogue of
  `Device.enabled=false`.

**Peer authentication** uses mTLS with **instance certificates**, registered out-of-band and
pinned. Add each peer deliberately,
by exchanging instance certs, not discovered. Intra-org tiers may instead share the org's
Django CA (§1.1). There is **no** automatic peer discovery, DHT, or gossip-of-strangers — the
peer set is small and human-vetted by construction (§1.2), which is the entire point of the
~7–8 cap.

---

## 6. The wire — reuse GNF1, add a peer session

Inter-collector transport is **GNF1** (`DESIGN.md §2`), unchanged in framing. A peer session is
a GNF1 session whose `HELLO` declares `role: "peer"`:

| Frame | Dir | Peer-session payload |
|---|---|---|
| `HELLO` (0x01) | initiator→peer | `{peer_id, role:"peer", instance_cert_fp, feeds, zstd?}` — no self-asserted trust; the receiver decides. |
| `WELCOME` (0x02) | peer→initiator | `{ok, error?, ack_interval_ms?, zstd?}` — refused if the instance cert is unknown or `revoked`. |
| `DATA` (0x03) | peer→peer | relayed **unsigned** record + envelope `{origin_receiver_id, recv_unix_ns, gnssId, svId, sigId, frame_type, raw_bytes, path[]}`. |
| `SIGNED_DATA` (0x07) | peer→peer | relayed record **with the observer's original ATECC signature intact** (§3) + the same envelope incl. `origin_receiver_id` and observer leaf-cert fingerprint + `path[]`. |
| `ACK` (0x04) | peer→peer | highest sequence received this connection (regression fix semantics). |
| `PING`/`PONG` | both | keepalive. |
| `JOURNAL` (new, 0x08) | both | a batch of signed, hash-chained control-plane entries (§5), pull-or-push. |

The record envelope already carries `{recv_unix_ns, gnssId, svId, sigId, frame_type,
raw_bytes}` (`DESIGN.md §2`); federation adds exactly `origin_receiver_id`, the observer
leaf-cert fingerprint, and `path[]`. The **signed payload is never re-encoded** — a relaying
collector copies the `SIGNED_DATA` frame body byte-for-byte and only prepends/extends the
envelope, or the signature breaks. All existing GNF1 resilience (bounded RAM ring, monotonic
seq, replay-on-reconnect, disk spool, backoff-forever, `MaxFrameLen` validation, TLS-1.2 floor)
applies to peer sessions verbatim.

---

## 7. Historian partitioning — quarantine by trust

The `nav_frames` hypertable stays date-partitioned (`YYYY/MM/DD` via Timescale chunks,
`DESIGN.md §4`). Federation adds a **trust dimension**:

- Every row already tags `receiver_id`; federated rows additionally tag `origin_peer_id` and
  `provenance ∈ {local, trusted, pending, read_only}`.
- **`trusted` federated frames** land in the authoritative table alongside local frames (they
  earned it in §2) — distinguishable by `origin_peer_id` for audit, but first-class for live
  state and detectors.
- **`read_only` / `pending` frames** land in a **separate quarantine partition** — persisted
  (forensic record kept, same as a CRC-failed frame is kept) and available for corroboration
  *reads*, but excluded from the authoritative dataset the detectors treat as ground truth, and
  never able to originate an alert.
- A `trust_changed` promotion (`pending`→`trusted`) does **not** retroactively rewrite history;
  it changes handling from that point forward. Quarantined history stays labeled as what it was
  when received.

---

## 8. Dedup & loop safety — free, from the observer counter

In a mesh the same observer's frame arrives via multiple paths. Both problems resolve from
primitives already in hand:

- **Dedup.** Signed frames dedup on **`(origin_receiver_id, counter)`** — the observer's
  monotonic counter (`EUI-64 ‖ … ‖ counter`, §3) is a stable, path-independent identity, so the
  N-th copy to arrive is dropped regardless of route. Unsigned `read_only` frames, lacking a
  counter, dedup on `(origin_peer_id, origin_receiver_id, recv_unix_ns, sha256(raw_bytes))`.
- **Loop prevention.** Each relayed record carries `path[]` — the ordered list of collector
  `peer_id`s it has traversed. A collector **drops any frame whose `path[]` already contains its
  own `peer_id`** before relaying, and appends itself on relay. Bounded by tree depth; a hop
  cap (`MaxPeerHops`, default 8 — matching the §1.2 span) is the hard backstop. `path[]` is
  envelope metadata, outside the observer signature, so relaying does not invalidate it.

---

## 9. Transitive trust — the one real risk, and its mitigation

A `trusted` peer whose *own* AAA is weaker than yours becomes your weakest link: you inherit
every observer they admit. Mitigation falls out of §3 rather than requiring new machinery:

- A peer may be granted **`trusted` for a constellation only if its observers on that
  constellation present verifiable ATECC `SIGNED_DATA`.** Software-mTLS-only observers behind
  that peer are accepted at `read_only` regardless of the peer's edge trust — their frames take
  the quarantine path (§7). *Trust the org, verify each observer end-to-end.*
- Because provenance is verified per-observer (§3), a compromise at a peer cannot forge frames
  for an observer whose non-extractable ATECC key it does not hold. The blast radius of a bad
  peer is bounded to the observers it can *legitimately* sign for — and every one of those is
  still physics-checked (§0).
- `revoked` propagates fast through the journal (§5); an observer cert can be pulled
  independently of the peer (`observer_cert_revoked`) without tearing down the whole peering.

---

## 10. Why bother — coverage you cannot otherwise get

Corroboration of globally-visible SVs is the *defensive* win. The *offensive* win is coverage:

- A `trusted` Tokyo peer is your **only** source of QZSS L1S and disaster messages from a
  European root — a signal `collector-host` cannot hear at all.
- **This is the concrete unlock for NavIC.** NavIC is a signposted stub today (
  `gnss/frame/navic.go` → `ErrNavICDeferred`) *precisely because "no receiver in the fleet can
  see NavIC to validate it."* A directly-peered Indian collector **is** that live NavIC stream —
  federation is what retires the deferral and lets `DecodeNavICSPS` be finished and validated
  against real IRNSS SPS frames (`docs/CONSTELLATIONS.md`).
- Regional SBAS, per-hemisphere GEO visibility, and multi-station spoofing corroboration
  (`docs/DEFENSE-PNT.md` — a jammer seen by one station and not its neighbor is diagnostic) all
  improve monotonically with peer geographic diversity.

Federation provides a way to acquire observations of regional constellations
from stations within their coverage areas.

---

## 11. Configuration & deployment

TOML at `/usr/local/etc/navlistener/navlistener.toml` (`DESIGN.md §5`), a new optional section —
absent = solo collector, unchanged behavior:

```toml
[federation]
enabled       = true
peer_id       = "eu-root"                  # this collector's stable id
instance_cert = "/usr/local/etc/navlistener/instance.pem"   # our InstanceCertificate
instance_key  = "/usr/local/etc/navlistener/instance-key.pem"
max_peer_hops = 8

[[federation.peer]]
peer_id       = "jp-qzss"
addr          = "collector.jp-org.invalid:9443"
instance_cert = "/usr/local/etc/navlistener/peers/jp-qzss.pem"   # pinned, out-of-band
trust         = { qzss = "trusted", sbas = "trusted", gps = "read_only" }  # per-constellation, §2.1
observer_cas  = "/usr/local/etc/navlistener/peers/jp-qzss-ca.pem"          # to verify their observers, §3

[[federation.peer]]
peer_id       = "public-pool"
addr          = "pool.example-pool.invalid:9443"
trust         = { "*" = "read_only" }      # light trust, quarantine-only, unbounded
```

`navlistener -check-config` (regression fix startup parity) extends to federation: it stat/PEM-validates
`instance_cert`/`instance_key`, every peer `instance_cert` and `observer_cas`, and rejects a
peer that asserts `trusted` on a constellation with no `observer_cas` to verify observers
against (the §9 rule, enforced at config time). Prometheus gains per-peer counters: frames
in/out, dedup drops, loop drops, physics-gate rejections, journal lag, trust-map state.

---

## 12. Clean-room & GPL

Federation is entirely our own design over our own GNF1 wire and our own trust layer. It touches
**no** galmon artifact — not `navmon.proto`, not `libnavmon`, not the RNIE wire. The
`galmon-bridge` adapter (`DESIGN.md §4`) is orthogonal: bridging galmon.eu and peering with a
`navlistener` collector are unrelated edges. CI's import-graph check is unaffected. The
` .invalid` hosts are reserved placeholders (never a routable example host in
config a user might fill with real peer credentials).

---

## 13. Where it slots in the build plan

Federation is **post-P8** (after intsat collect/detect is absorbed and the single-collector
contract is proven end to end). Proposed **P10 — federation**, staged so each step is useful
alone:

1. **Instance identity + peer session:** `InstanceCertificate`, the `role:"peer"` GNF1 session,
   mTLS peering with pinned instance certs. Two of our own collectors peer; no trust tiers yet
   (both implicitly `trusted`, intra-org CA).
2. **Trust map + quarantine:** the per-constellation trust enum, the quarantine historian
   partition, `read_only` corroboration-only ingest.
3. **Observer-cert directory + end-to-end verify:** passthrough `SIGNED_DATA` relay, the
   observer-cert directory, §9 transitive-trust enforcement.
4. **Control-plane journal:** hash-chained `JOURNAL` frames, revocation propagation.
5. **First real cross-org peer — NavIC unlock:** stand up a `trusted` regional peer and finish
   `DecodeNavICSPS` against the live stream it provides.

---

## 14. Non-goals

- **No peer discovery / DHT / gossip-of-strangers.** Peers are added deliberately, out-of-band,
  by exchanging instance certs. The peer set is small and vetted (§1.2).
- **No blockchain / consensus.** The journal is a per-collector hash-chained audit log for
  tamper-evidence and replay, not a distributed ledger seeking global agreement.
- **No symmetric-trust assumption and no trust negotiation.** Each collector decides
  unilaterally, per direction, per constellation, what an inbound observation may do (§2.2).
- **Federation never weakens the local security model.** A federated frame can do *at most* what
  a local frame of the same provenance tier could do, and always less than an unverifiable one —
  and every frame, local or federated, is physics-checked (§0).
