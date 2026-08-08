# The hardware observer board

The GNSS port of `radiolistener/docs/HARDWARE-OBSERVER.md`. That document specifies the
**generic** ESP32 + secure-element + RTC observer and the credential ladder; this one covers
what is specific to a *navigation* observer: which receiver, what the manifest must record for
the P9 capability fingerprint, the physical-sensor integrity gates a GNSS node can run that a
radio node cannot, and the provisioning constraints the collector actually enforces today.

Read `docs/DESIGN.md §node identity` first for the credential tiers, and `docs/DEFENSE-PNT.md`
for the detector family the sensor gates below extend.

> **Status: design, not as-built.** No board has been fabricated. Figures marked
> **[verify]** are engineering estimates that must be pinned to a datasheet before they
> reach firmware or a detector threshold: a constant without a citation
> is a TODO, not a value.

---

## 1. Board variants

The hardware manifest (`esp32-hardware-discovery`) already reserves
`PROJECT_GNSS = 1` with `GNSS_PCB_MAIN / SENSOR / DISPLAY`. Its ID enums are a **baseline to
extend, not a fixed menu** — §5.3 lists the entries this product needs added.

| Variant | Receiver | RTC | Why |
|---|---|---|---|
| **A — first spin** (`GNSS_PCB_MAIN`) | **NEO-format** M9 / M10 / F10 | MCP79412 + 32.768 kHz crystal | NEO's larger pitch and edge-castellated pads survive a missed airwire and a rework iteration. A ZED footprint punishes a first board for a routing mistake that costs a bodge wire on a NEO. |
| **B — precision** | ZED-format (F9P / F9T) | DS3231 | Spend the TCXO where the receiver already justifies the board cost. |
| **C — modular** | pin headers, third-party module | either | Every vendor breakout has its own pinout; an project all-in-one PCB beats a breadboard stack. See §5.4 — a socketed receiver **cannot** be trusted from the manifest. |

Variant A is deliberately the cheap, reworkable one. It is also the one that gets built five
times, so it carries the full identity and sensor stack — nothing about the trust model is
deferred to the expensive board.

### 1.1 Receiver choice within variant A

**The footprint is the decision; the module is a populate-time choice.** `NEO-M9N`, `NEO-M10`,
`NEO-F10N`, `NEO-F10T`, `NEO-M8T`, `NEO-M8Q`, `NEO-M8U` and `NEO-D9C` all share the same 24-pin
NEO land pattern (`GPSM-SMD_24P-L16.0-W12.2-P1.10` in LCSC's library). Lay that down and the
L1-only-versus-L1/L5 question stops being a board decision.

| Part | Bands | Status |
|---|---|---|
| **NEO-M9N-00B** (`C5119087`) | L1 only **[verify]** | **First populate.** Best availability of the modern parts by a wide margin; already `GPS_NEO_M9N` in the manifest enum. |
| NEO-F10N-00B (`C21709333`) | L1 + L5 **[verify]** | The upgrade. Same footprint, roughly 2× the cost, thin stock. Needs an enum entry (§5.3). |
| NEO-F10T-00B | L1 + L5 **[verify]** | Timing variant; ~8× the cost for features this board does not use. |
| NEO-M10 | L1 only **[verify]** | Needs an enum entry. No existing design uses one. |

The band split decides what the board can contribute:

- **L1-only** is a complete, useful observer: GPS L1 C/A, GLONASS L1OF, Galileo E1, BeiDou B1I,
  QZSS L1. Most of the existing fleet is exactly this — and **GLONASS L1OF is what the regression fix
  calibration work consumes**, so an M9N board contributes to the active task from day one.
- **L1/L5** additionally reaches GPS L5 CNAV, Galileo E5a F/NAV, QZSS L5 and BeiDou B2a; enables
  RXM-RAWX dual-frequency work, which is the standing blocker on measured-ionosphere and
  receiver-DCB calibration; and is a precondition for NavIC.

**Verify before assuming the ionosphere work is a firmware toggle:** whether the M9N supports
`RXM-RAWX` at all. Raw measurements are often restricted to timing and high-precision parts, and
if the M9N lacks it, dual-frequency work waits for an F10N rather than a config change. It does
not affect nav-message collection either way.

**NavIC caveat:** L5 silicon is necessary but not sufficient — NavIC is below the horizon from
California. An L1/L5 board only becomes the NavIC unlock if it is deployed within the
constellation's footprint, or reached through `docs/FEDERATION.md`.

**Standing rule — SFRBX is the acceptance test.** The architecture forwards *raw broadcast
frames*; a receiver that will not emit `UBX-RXM-SFRBX` is useless to this product regardless of
its part number. u-blox generations and firmware builds differ here (the NEO-8 on SPG 3.01 in
the parts bin emits none). Poll `UBX-MON-VER` and count SFRBX on the exact part and firmware
before committing a variant to a build.

---

## 2. Slots

Every entry is a manifest slot (§5), so firmware discovers the board rather than being compiled
for it.

| Slot | Category | Part | Purpose |
|---|---|---|---|
| MCU | `CAT_MCU` | **ESP32-S3-WROOM-1U-N16R8** (`MCU_ESP32_S3`) | Feeder. S3 and the **1U** (u.FL) variant both matter — §7.1. |
| Receiver | `CAT_GPS` | **NEO-M9N-00B** (`C5119087`) on the shared 24-pin NEO land pattern | Raw nav frames + PPS. F10N/F10T drop in without a respin — §1.1. |
| Identity, public | `CAT_RTC` | **MCP79412** + 32.768 kHz crystal (`RTC_MCP79412`) | RTCC + SRAM + EEPROM + **factory EUI-64** — the observer's public name. |
| Identity, private | `CAT_CRYPTO` | **ATECC608C-SSHDA-T** (`C28975195`, `CRYPTO_ATECC608C`) | Non-extractable P-256 key; the proof of entitlement to that name. |
| Hardware manifest | `CAT_MEMORY` | **one** 24AA02E64 (`MEMORY_24AA02E64`) | The installed-hardware descriptor array. **One, not two** — see below. |
| Status panel | `CAT_LED` | 16 × 0805 (8 green + 8 yellow) via 2 × **TLC5916** | Constellation/health indication — §2.1. |
| Pressure | `CAT_PRESSURE` | **BMP388** placed; BMP390 and BMP580 are footprint alternates | **Vertical spoofing gate** — §6.1. |
| Temperature | `CAT_TEMP` | **MCP9808-E/MS** (`C94847`) | Crystal-drift characterisation and thermal health — §6.2. |
| Humidity | `CAT_SENSOR` | **HDC2080** | Dew point / enclosure-seal diagnostic — §6.4. Not GNSS math. |
| Backup | `CAT_BATTERY` | 2 × CR2032, `BS-0202-DK-0B` holders (needs enum entries, §5.3) | Two independent domains — §3. |
| GNSS antenna | `CAT_ANTENNA` | SMA jack, right-angle, 4-leg THT (Amphenol `132289` class) | Bias + supervision → `MON-HW` `antStatus` — §7.3. |
| Wi-Fi antenna | `CAT_ANTENNA` | u.FL → RP-SMA pigtail off the 1U module | Physically separable from the GNSS path — §7.1. |
| USB | `CAT_CONNECTOR` | USB-C, 16-pin USB 2.0, THT shield legs | §7.6. |

**One EEPROM, not two.** An earlier revision specced two. The 24AA02E64's A0/A1/A2 pins are
**not functional** — confirmed from the schematic symbol, which shows pads 1/2/3 as NC — so the
part answers at a fixed address and two of them cannot share a bus. No strapping resolves it.
The single EEPROM plus a real ATECC608C is the resolution, and it is also simpler: the ATECC
carries the private identity that a second EEPROM could never have provided.

**The ATECC608C and the 24AA02E64 share a pinout** (1/2/3 NC, 4 GND/VSS, 5 SDA, 6 SCL, 7 NC,
8 VCC), so one SOIC-8 land pattern serves either. If a variant ever wants to swap them, leave
pads 1/2/3/7 unconnected so neither part cares what is on them.

### 2.1 The status panel

**Discrete LEDs on constant-current drivers, not addressable pixels.** Earlier revisions of
this document specified APA102, then WS2812B. Both were over-specified: the encoding this board
actually needs is four states per position, and an RGB pixel is sixteen million colours wearing
a two-bit job.

Eight positions is the natural width — the gnssId space has exactly seven constellations (GPS 0,
SBAS 1, Galileo 2, BeiDou 3, QZSS 5, GLONASS 6, NavIC 7; IMES 4 is never emitted) plus one for
uplink health. Each position is a **green over a yellow**, giving off / yellow / green / both,
which maps onto semantics the feed already has: not tracked / tracked without ephemeris /
ephemeris current / `eph_aged` or integrity event.

Two drivers, **one per colour**, chained. That is not arbitrary: TLC5916 sets its channel
current with a single external resistor, so one driver per colour lets green and yellow be
trimmed to **matched apparent brightness** despite different luminous efficiency — no per-LED
ballast resistors, no firmware compensation. Bringing the two `OE` pins out separately also buys
independent per-colour PWM dimming.

Independent channels rather than the complementary-inverter trick (one signal driving a
green/yellow pair in opposition), because a **fully dark row is a meaningful state** here — the
constellation is not tracked at all — and because independent channels allow top-to-bottom
animation and a distinct pattern for uplink delay or loss.

Run them lean: 5–8 mA per channel is plenty behind a diffuser, and sixteen channels at 20 mA is
320 mA of heat in an enclosure that already runs hot. `TLC5916`'s shift-then-latch structure
means all sixteen change on one edge, so animation cannot tear.

**Package:** 0805 for the first spin, because PLCC-2/3528 is usually a JLC *Extended* part while
0805 in standard colours is *Basic* — and at quantity five the per-part feeder fee is a real
fraction of the build. Take both colours from the same manufacturer series so lens, height and
beam pattern match. The 5 mm through-hole frosted parts remain a second-spin option: driver,
current setting and firmware are unchanged, only the footprint and the per-colour R-EXT trim.

**Rail:** drive the LED anodes from **5 V**, not 3.3 V. TLC5916 sinks current, so the rail sets
its compliance headroom, and 5 V keeps blue/white available for a future indicator (Vf ≈ 3.0–3.2 V
would not light from 3.3 V at all).

---

## 3. Power and backup domains

Two cells, two domains — **not** two cells on one rail:

- **Receiver `V_BCKP`** — preserves battery-backed RAM: ephemeris, almanac, last position, the
  receiver's own clock. Buys hot/warm start.
- **RTC backup** — preserves the observer's independent time reference.

They are kept separate because they fail differently and matter differently.

**Drain is asymmetric.** `V_BCKP` draws substantially more than an RTC backup **[verify against
both datasheets]**, so the receiver cell dies first — and it dies *silently*: the receiver
cold-starts, which presents as "slow to reacquire," not as a fault. Read and report both cells'
health rather than discovering this in a log six months later.

**What each is worth to this product.** The RTC cell is load-bearing (§4.2, §6.3). The receiver
cell is a convenience with one genuine integrity dividend: a cold-starting receiver accepts
whatever the sky appears to say, whereas one with retained almanac has a prior to disagree with.
That is a weak signal, but it is free.

**Heat.** These units run hot, and hot enclosures are where coin cells self-discharge fast and
leak. Mount the cells **off-board on a JST lead** rather than inside the sealed case: it keeps
them out of the thermal mass, and it turns replacement across five units into a ten-second job
instead of a disassembly.

No external switchover diode — the RTCs specified here have internal VCC/VBAT switchover, and an
added series diode only eats backup headroom.

---

## 4. Identity and trust

### 4.1 Public name, private proof

- **MCP79412 EUI-64 → the public identifier.** Readable over I²C by anyone holding the board.
  It is a *name*, never a credential.
- **ATECC608C → the private authenticator.** Generates a non-extractable P-256 keypair, signs
  the CSR, signs the mTLS handshake, and optionally signs `SIGNED_DATA` (0x07) batches over
  `EUI-64 ‖ rtc_unix_ns ‖ sha256(payload) ‖ counter`.

This resolves the open question in radiolistener's doc ("dedicated EEPROM vs derive the EUI-64
from the ATECC serial"), which presumed one part had to supply *the* ID. Identifier and
authenticator are separate jobs and want separate parts. `DESIGN.md` already records both at
enrollment — "the signed cert + the device's EUI-64 + ATECC serial".

The collector already enforces the separation: `matchPeerIdentity`
(`go/internal/ingest/push.go:472`) resolves the observer from the verified certificate, so a
self-reported name in the GNF1 HELLO cannot override it.

### 4.1a ATECC slot map — the shared slot map, not a new one

The slot layout is shared with `shepherdprotocol`, whose
`esp32/components/atecc608c/include/atecc608c_slots_unified.h` is the single source of truth
(**v2**, jointly revised 2026-08-08; the spec is `ATECC_SLOTS_UNIFIED.md` beside it).

One shared slot map, not one per product, for a reason that admits no do-over: **the config zone locks
permanently and cannot be read back afterwards.** Two diverging maps means two provisioning
tools, two validated configs, and two chances to brick a reel of parts.

v2 exists because v1 — including the version this document briefly adopted — was never checked
against the silicon. The ATECC data zone has **size classes** (`ATECC508A` §2.1 Table 2-3;
cite-keys per `reference/REFERENCES.md §2a`, which also records why the 508A complete datasheet
stands in for the NDA-gated 608 one): slots 0–7 are 36-byte private/secret-key slots, slot 8 is
the single 416-byte data slot, and slots 9–15 are 72-byte public-key/signature slots. §2.1
states that *only slots 8–15 can store an ECC public key* (the stored format is 72 bytes,
§4.1.1, and `Verify(Stored)` reads it from the slot, §9.20) — yet v1 placed its verification
public keys, including the slot-1 "CA trust anchor" this document had adopted, in 36-byte slots
where they physically cannot live. v2 sorts every role into its size class; the full map and
per-slot policy live in shepherd's spec.

What this product uses:

| Slot | Slot name | Use here |
|---|---|---|
| 0 | `SLOT_ROVER_IDENTITY` (alias `SLOT_DEVICE_IDENTITY`) | the observer's operational P-256 key — signs the CSR and the mTLS handshake. GenKey-regenerable, never slot-locked, so a sold board re-enrolls against the buyer's CA. |
| 13 | `SLOT_TRUST_ANCHOR` | optional pinned copy of the Django CA public key. The mTLS chain is verified in the TLS stack with the CA cert in flash — this slot only matters if the observer ever verifies signed commands/updates outside TLS. |
| 14 | `SLOT_MFG_ATTESTATION` | the manufacturer authenticity signature — §4.1b. |
| 15 | `SLOT_DEVICE_CONFIG` | genealogy/provenance record (mutable), shared shape with shepherd. |

Slot 12 (`SLOT_PNT_AUTH_PRIMARY`) remains scoped to GNSS broadcast and correction
authentication — unused for now (navlistener verifies OSNMA centrally), but the natural home if
broadcast authentication ever needs an on-device key at the edge.

### 4.1b Manufacturer attestation — a signature, not a key

Selling boards (funded federation nodes, `docs/FEDERATION.md`) wants proof that a unit is genuine
hardware of known provenance, independent of whichever fleet later enrolls it. The obvious design
is a second private key — generated at the bench, non-regenerable — but **it does not need an
on-device key at all.**

Shepherd already stores provenance in the shared slot map as *data* rather than a key: slot 15's
32-byte "genealogy" record (`atecc_provisioning.c`). That is the right shape, and it generalises.

The ATECC's **9-byte factory serial is immutable, unclonable and publicly readable**
(`ATECC508A` §2.2, SN<0:8>). So the manufacturer signs a statement binding it to the board identity —
**off-device, at the bench, with the manufacturer attestation key** — and the signature is stored
in a data slot. The signer is a distinct *role* from the enrolling CA (even while the same
organization holds both): verification must not depend on which CA later signs the operational
certificate. Verification needs only the manufacturer's public key. A counterfeiter can copy the
layout and the BOM; they cannot produce a valid signature over a serial they do not control.

What that buys:

- Provable "genuine hardware from this manufacturer," independent of the operational identity —
  a board can be sold **"clean"** (no operational key provisioned) and still carry its
  authenticity signature.
- **Zero key-slot contention** — no on-device private key, no slot retired from any fleet.
- A buyer regenerates slot 0 freely against their own CA, and **the attestation survives it**,
  because it attests to *silicon*, not to the operational key.
- The same shape as the existing genealogy record, so one provisioning tool writes both.

A P-256 signature is 64 bytes — the 72-byte slot class (9–15) is the datasheet's own size class
for *"the R and S components of an ECDSA signature"* (Table 2-3); slot 15 cannot also host it
because the genealogy already occupies 32 of its 72 bytes. Under v2 the attestation is
first-class rather than squatting on a repurposed domain slot: **slot 14, `SLOT_MFG_ATTESTATION`**.

The record and message formats are normative in `atecc608c_slots_unified.h`
(`ATECC_MFG_ATTEST_*`): the slot holds `[version 0x01][7 reserved][R‖S 64]`, and the signature is
over `SHA-256("ATECC-MFG-ATTEST-v1" ‖ serial[9] ‖ eui64[8] ‖ board_rev_u16be)` — the ASCII prefix
is domain separation, and the EUI-64 is this board's MCP79412 identity (§4.1), binding chip to
board. One shared formatter must be the only writer/parser, so the bench tool and firmware cannot
disagree.

Two properties are enforced at the bench, not hoped for:

- **The slot is permanently slot-locked after write and readback-verify** (`ATECC508A` §2.4.3).
  The signature is unforgeable either way, but an unlocked slot would let an attacker *corrupt*
  the record — denial of provenance.
- **Slot 0 stays regenerable and is never slot-locked** (SlotConfig bit 13 = 1 keeps GenKey
  legal after the data zone locks, `ATECC508A` §9.7) — that is what makes the "clean chip with
  an authenticity signature" sale work.

### 4.2 Provisioning constraints — get these right before writing five parts

Each is enforced by code today, and each is baked into a certificate that an ATECC will sign
exactly once:

1. **DNS SAN, not CN.** `push.go:479-485` requires **exactly one DNS SAN** equal to the
   canonical observer id. `DESIGN.md §node identity` still says `CN = receiver_id`; that wording
   is stale relative to the implementation. A CSR carrying only a CN is rejected at handshake.
2. **Character set.** `config.ValidObserverID` (`go/internal/config/config.go:683`) permits only
   `[A-Za-z0-9.-]`, max 253. The conventional `00:04:A3:FF:FE:12:34:56` EUI-64 rendering is
   therefore invalid — as an observer id *and* as a DNS name.

   **Decided: lowercase, hyphen-separated byte pairs, as a bare label** —
   `00-04-a3-ff-fe-12-34-56`. Applies to radiolistener too; the two products share one CA and
   one `devices` table.

   - *Not bare hex* (`0004a3fffe123456`): a hex string is not guaranteed to contain a letter,
     and an all-numeric single DNS label is a known trouble class (parsers that attempt it as
     an IPv4 literal). Hyphens make that impossible by construction rather than improbable.
   - *Lowercase is load-bearing, not cosmetic.* `matchPeerIdentity` compares byte-exactly
     (`names[0] != observer`), not with DNS case-insensitivity, so a cert provisioned in
     uppercase against a lowercase config fails the handshake — reporting "DNS SAN does not
     exactly match canonical observer", which does not point at capitalisation.
   - Byte-pair grouping matches how the value is printed on a chip marking or case label, so
     transcription is direct; and it is far easier to compare by eye than 16 undifferentiated
     hex characters in a handshake error.
   - No RFC 5891 IDNA conflict: that reserves `--` in the third-and-fourth position, and this
     pattern has `-` at 3 and a hex digit at 4.
   - **Bare label, not a FQDN.** The feeder dials out and is never dialled, so the id needs no
     resolvability. Fleet/org namespacing (`….obs.intsat.space`) would have to be chosen now —
     it is baked into every certificate and retrofitting it means re-enrolling every board —
     but the EUI-64 is already globally unique, so it buys nothing here.
3. **Exactly one SAN** — zero or two are both rejected.

**Buy the provisionable ATECC608C.** Trust&Go / TrustFLEX parts ship pre-provisioned and locked
to Microchip's certificate chain; this design has the device generate its own key and the
**Django CA** sign its CSR. The config-zone lock is permanent, so validate the slot
configuration on a scrap part before locking production units.

### 4.3 Three unique IDs on one board

The MCP79412 and the 24AA02E64 each carry a factory EUI-64, and the ATECC carries a 9-byte
serial. Assign them distinct roles and record all three at enrollment:

| Source | Role |
|---|---|
| MCP79412 EUI-64 | **observer identity** — the `receiver_id` in the cert SAN and the `devices` row |
| 24AA02E64 EUI-64 | **board serial** — identifies the PCB, not the network node |
| ATECC serial | binds the key material to the enrollment record |

Consequence to accept deliberately: a dead RTC changes the observer's identity and forces
re-enrollment. That is defensible — it is an auditable event — but it must be a decision rather
than a surprise, because the failure is otherwise silent.

**Boot-time binding check.** Compare the certificate's SAN against the EUI-64 read live from the
RTC; on mismatch, refuse to feed and report. One I²C read catches a swapped RTC, a cloned
certificate on different hardware, and a mis-provisioned board.

---

## 5. The hardware manifest as the P9 capability fingerprint

### 5.1 What it gives us

`docs/DESIGN.md §P9` describes the capability fingerprint abstractly: "the integrity layer knows
what a node *should* be able to report… a node whose silicon can't hear L5 suddenly reporting L5
frames is loudly suspect." `esp32-hardware-discovery` is the concrete mechanism — a 4-byte
descriptor per IC, `[category][id][i2c_address][status]`, 56 ICs.

The **status byte makes it strictly better than the model DESIGN.md assumes.** A static
capability list says what was designed in; `IC_STATUS_INSTALLED / NOT_POPULATED / FAILED /
DISABLED / TESTING / DEPRECATED` says what is working *now*. A node reporting L5 frames while its
`CAT_GPS` entry reads `FAILED` is a far sharper signal than any static list can express, and
`NOT_POPULATED` cleanly distinguishes "this board never had a barometer" from "its barometer
died" — which the detectors in §6 need in order to disable themselves rather than fire.

### 5.2 How navlistener consumes it

The feeder reports its manifest at enrollment and on change; the collector stores it on the
`Device` row and derives the expected `(gnssId, sigId)` set from the `CAT_GPS` entry. That set
is the reference the existing capability detectors compare observed frames against — the
machinery already exists (`go/internal/state/capability.go`); this supplies its declared side
with something hardware-rooted instead of configured by hand.

### 5.3 Baseline extensions this product needs

The enums are a baseline. These entries do not exist yet and are required:

| Enum | Add | Why |
|---|---|---|
| `eeprom_gps_id_t` | `GPS_NEO_M10`, `GPS_NEO_F10N`, `GPS_NEO_F10T`, `GPS_ZED_F9T` | Variant A's actual candidates; the F9T is most of the current fleet and is absent. |
| `eeprom_pressure_id_t` | `PRESSURE_BMP390` | The house pressure part (shepherd's C6 rover bus) is absent; the enum lists BMP280/BMP388/MS5611 only. |
| `eeprom_battery_id_t` | `BATTERY_CR2032` | The enum currently covers LiPo/18650/solar/PoE only — no primary coin cells, and this board fits two. |
| `eeprom_sensor_id_t` | `SENSOR_HDC2080` | The combined temperature/humidity part (§6.4); `TEMP_MCP9808` already exists for the dedicated sensor. |

**The band discriminator is the one that matters for integrity.** `GPS_ZED_F9P = 1` names a part
family, not a band capability — but DESIGN.md's worked example is precisely the **F9T-00B (L1+L2)
vs F9T-10B (L1+L5)** distinction, and the id byte cannot express it. Two options:

- variant-level enum entries (`GPS_ZED_F9T_00B`, `_10B`, …) — simple, but combinatorial; or
- a **band bitmap** carried in the descriptor's reserved bytes, which the header already earmarks
  for "feature flags" with an explicit forward-compatibility rule.

The bitmap is the better fit: bands are orthogonal to part identity, and it keeps one enum entry
per part. This is the decision to settle *before* the first EEPROMs are written.

### 5.4 A socketed receiver cannot be trusted from the manifest

Variant C's whole point is that the receiver is field-swappable — so the manifest records what
the *board* was built with, not what is plugged in now. For header-based boards the firmware must
probe `UBX-MON-VER` and report the **probed** identity, and the collector must treat a probed
capability as a weaker claim than a soldered manifest entry: a manifest says what the board *is*,
a probe says only what something on the far end of a connector *claims to be*. Reflect that in
the trust tier rather than merging the two into one field.

---

## 6. Physical-sensor integrity gates

A GNSS observer can run gates a radio observer cannot, because it carries sensors measuring
physical quantities an RF attacker has no channel to. These extend the station-scoped detector
family in `docs/DEFENSE-PNT.md`.

### 6.1 The vertical gate — barometric pressure vs GNSS altitude

**Principle.** A spoofer transmitting RF can move the receiver's altitude solution by kilometres.
It cannot change the air pressure in the room. Pressure is therefore an *independent physical
witness* to whether the observer actually moved — the same "verify physics, not signatures"
argument that motivates the orbit and clock cross-checks, applied to the vertical axis.

**Compare short-window changes, never absolute altitude.** Barometric altitude depends on a
sea-level pressure reference that drifts continuously with weather; synoptic variation over days
is tens of hPa, which is hundreds of metres of apparent altitude **[verify against a standard
atmosphere reference before setting any threshold]**. An absolute comparison would fire on every
weather front. The gate quantity is the *disagreement between the two derivatives*:

```
divergence(t) = [h_gnss(t) − h_gnss(t−w)] − [h_baro(t) − h_baro(t−w)]
```

over a window `w` of minutes. For a static observer both bracketed terms are ~0, and weather
enters both sides of the subtraction slowly enough to cancel within the window.

**Threshold placement.** Vertical is the weakest component of a GNSS solution — VDOP normally
exceeds HDOP — so the noise floor is metres to tens of metres, while a meaningful spoof
displacement is orders of magnitude above it. The gate is generous and still catches the attack;
it is not a precision instrument and should not be tuned as one.

**Interlocks.**
- A `CAT_PRESSURE` slot reading `FAILED` or `NOT_POPULATED` must **disable** the gate, not fire
  it (§5.1). A dead sensor reads as a frozen pressure, which is indistinguishable from the attack
  it is meant to detect.
- Known benign sources of real pressure steps: HVAC cycling, a door opening on a sealed room,
  wind gusting across an exposed port. Prefer a vented but wind-shielded location.
- For a surveyed static observer the *stronger* primitive is simply that altitude should not
  change at all. The barometer's added value is that it needs no survey and still witnesses "this
  box did not move" — including in the case where a spoofer holds a plausible but wrong *fixed*
  position, which a survey-free installation cannot otherwise detect.

**Cross-station corroboration.** Observers inside one weather system see correlated pressure.
That separates the two failure modes cleanly: one station's *pressure* diverging from its
neighbours is a sensor fault; one station's *GNSS altitude* diverging while its pressure still
tracks its neighbours is a spoof. This is the fusion pattern DEFENSE-PNT already applies to the
RF detectors.

### 6.2 Temperature

Three distinct uses, all justifying a tracked slot rather than a nice-to-have:

1. **Clock-drift compensation.** Variant A runs a plain 32.768 kHz crystal, not a TCXO. Its
   dominant error term is temperature-dependent — so logging temperature lets that drift be
   characterised and subtracted in software, recovering much of what the DS3231 would have
   provided in hardware. This is what makes the crystal-stock choice defensible rather than merely
   cheap, and it is why the temperature slot is *required* on variant A even though variant B can
   treat it as optional.
2. **Thermal health.** The observed failure mode on this fleet is heat-related: the F9P on
   `observer16` dropped off the USB bus after a hot spell (2026-08-05). Per-board temperature makes
   that correlation visible across five units instead of anecdotal.
3. **Barometric conversion.** The pressure-to-altitude relation is temperature-dependent, so §6.1
   needs it regardless.

### 6.3 RTC versus GNSS time

The RTC's integrity value is that it is **independent of the GNSS signal** — a spoofer who fakes
GNSS time cannot move a battery-backed clock. That creates a tension with DESIGN.md's
"discipline the RTC from GPS PPS, closing the loop": slaving the RTC to GNSS couples the
independent reference to the thing under test, and a patient spoofer can walk it.

Resolution: **monitor the offset as a measured quantity**, and discipline conservatively and
bounded — never faster than the oscillator's own specified drift would require. The RTC-vs-GNSS
divergence is itself a station-level time signal and belongs in the DEFENSE-PNT time gate. Note
the dependency on §6.2: on a plain-crystal board the RTC's thermal drift *is* this detector's
noise floor, so the threshold must be set above the characterised drift, not below it.

**Free trust signal:** the RTC's oscillator-stop flag reports that it lost time. If set, the
observer's time claim should drop a tier at the collector rather than be taken at face value —
one I²C read at boot, one field on the `Device` row.

### 6.4 Humidity — a diagnostic, not a GNSS input

Stated plainly so it is not oversold: navlistener decodes broadcast navigation messages and does
no ranging, so there is **no tropospheric-delay use** for humidity here. It earns a slot for two
other reasons, both real for this fleet:

- **Dew point.** A board in a hot attic that cools overnight can cross the dew point, and
  condensation is a corrosion and intermittent-contact mechanism. This fleet has already lost a
  receiver off a USB bus after a hot spell (2026-08-05); temperature and humidity together make
  that correlation visible instead of anecdotal.
- **Enclosure-seal integrity.** In a sealed clear case, an interior humidity trace that begins
  tracking outdoor conditions means the seal has failed. There is no other cheap way to see that.

Accuracy requirements are correspondingly modest — dew point needs a few percent RH, not a
precision hygrometer. `HDC2080` covers temperature and humidity in one part and one address;
precision Honeywell HIH parts would be wasted here.

**Do not consolidate the barometer into it.** A combined P/T/H part (BME280) would collapse three
slots into one, but its pressure noise is worse than a dedicated barometer's — and pressure noise
is precisely the specification §6.1's gate lives on. Keep the good barometer separate.

---

## 7. Interface and layout notes

### 7.1 Move UART RX off a strapping pin

`esp32/README.md` and MAX **regression fix**: GPIO9 on the C6 is a boot-strapping pin, and a reset landing
mid-byte can latch the chip into the ROM serial downloader until someone power-cycles it.
Accepted on dev-class C6 units by design, with the stated fix being "the
planned ESP32-S3 re-spin moving RX to a non-strapping GPIO."

**The fix already exists as project convention.** `shepherdprotocol/esp32/ESP32C6_PINOUT.md` puts
GPS on **GPIO4 (TX) / GPIO5 (RX)** with **PPS on GPIO10** — deliberately clear of GPIO9. Adopt
that map rather than inventing a third one; it retires the hazard at zero layout cost and makes
the two products' firmware pin tables comparable.

This retired the *stated* justification for the S3 re-spin, which was exactly this hazard. The
S3 was then chosen on its own merits instead — see §9.6, now closed.

### 7.2 PPS, and the two lights that survive a crash

Bring the receiver's `TIMEPULSE` output to a GPIO — easy to omit, impossible to add later,
and §6.3 depends on it. **Also drive an LED from the buffered PPS line
directly**, not through the LED drivers. It then blinks at 1 Hz whenever the receiver has time
lock, independent of the ESP32 entirely.

Paired with a **power LED hardwired to the input rail** — outside any switch — that gives two
truths which survive wedged or crashed firmware: *there is power*, and *the GNSS is locked*. For
a fleet that has already had a receiver silently drop off a USB bus for two days, being able to
distinguish "dead" from "alive but not reporting" from across the room is worth two LEDs.

Buffer PPS for fan-out, not for level: the receiver and the MCU are both 3.3 V, so nothing needs
translating. A `74LVC1G17` Schmitt buffer is the right shape if PPS also reaches a test point or
a second load. Its propagation delay is a few nanoseconds and constant — harmless, but remember
it exists if the RTC is ever characterised against PPS (§6.3).

**Blanking the panel.** An N-channel MOSFET (`AO3400A` class) in the low side of the PPS LED,
gate pulled to **3.3 V** through 100 kΩ, MCU GPIO pulling it low to blank. Two constraints:

- **Pull the gate to 3.3 V, never to the 5 V rail.** ESP32 GPIOs are not 5 V tolerant, and the
  pin is high-impedance at every reset and through boot — so a pull-up to 5 V puts 5 V on it
  before firmware ever runs.
- 100 kΩ, not 1 kΩ. The gate is high-impedance; a 1 kΩ pull-up just sinks milliamps through the
  GPIO for the entire time the panel is blanked.

The fail-safe direction is then correct by construction: high-Z at reset means the pull-up wins
and **the lights are on by default** — before firmware runs, during a crash that predates the
blank command, and if the firmware never boots at all. The diagnostic survives exactly the
failures it exists to reveal.

Keep the **power LED off the switched rail**. If software can extinguish it, "no light" stops
meaning "no power" and the ground truth is gone. Run it lean — 1–2 mA is legible across a room,
and it is lit continuously for years in a hot box.

### 7.3 Antenna

Prefer u.FL/SMA for an external active antenna over an on-board patch — antenna quality dominates
data quality by a wider margin than any silicon choice on this board. Wire the antenna supervisor
(bias, short/open detection): u-blox reports it in `MON-HW` `antStatus`, and the GNF1 telemetry
codec already carries that field, so supervision becomes fleet telemetry rather than a dead byte.

### 7.4 RF coexistence

An ESP32 radio at 2.4 GHz and USB's broadband noise both sit close enough to desense a 1575 MHz
front end, and a clear enclosure provides no shielding. Separate the GNSS RF path from the ESP32
antenna keep-out and USB routing, pour solid ground, and consider a SAW/LNA ahead of the module.
This is where layout effort belongs — not the LED array.

### 7.5 I²C

**One shared bus at 400 kHz**, matching `ESP32C6_PINOUT.md` ("All I²C devices share one bus").
Known house addresses: **ATECC608C `0x60`**, **24AA02E64 `0x50–0x57`** (A0–A2 strapped),
IMU `0x68`.

Two constraints to resolve on paper *before* anything is locked: the 24AA02E64 occupies two
address ranges (the EEPROM array and the protected EUI block), and the **ATECC's address is set
in its config zone**, fixed permanently at lock time. Confirm every address against its datasheet
rather than assumed defaults — noting that the RTC's conventional `0x68` collides with the IMU
address shepherd uses, which is a non-issue here (no IMU on this board) but must not be
copy-pasted forward onto a variant that adds one.

The manifest EEPROM's 8-byte page-write hazard is **already handled** in the shared component —
`esp_hardware_discovery.c` chunks writes to page boundaries and ACK-polls after each — so a
descriptor array larger than one page (this board's ~10 slots is ~40 bytes) writes correctly.
Nothing to do here; recorded so it isn't re-derived.

### 7.6 USB-C

**16-pin USB 2.0 receptacle**, not the 24-pin. The S3's native USB is the only data path; the
24-pin part's SuperSpeed pairs are routing hazards with nothing on the other end.

- **Through-hole shield legs.** A pure-SMD USB-C peels off the board after enough insertions.
- **Two 5.1 kΩ pulldowns, one on CC1 and one on CC2, each to GND.** Not one shared resistor.
  Without them a USB-C source never enables VBUS, and the board is dead on a C-to-C cable while
  working fine off a legacy A-to-C — which is how the bug hides until someone uses a modern
  charger.
- ESD array on D+/D−/VBUS. `ECLAMP8052P` is already validated in the sibling designs.

---

## 8. Alignment with `shepherdprotocol`

Both products build ESP32 boards around the same `esp32-hardware-discovery` manifest, so parts
and conventions are shared by default and divergence needs a reason. What this document adopted
from the sibling, and where it deliberately does not:

| Topic | shepherd | here | Status |
|---|---|---|---|
| Secure element | ATECC608**C** @ `0x60` | ATECC608**C** | **corrected** — an earlier draft said 608B |
| ATECC slot map | `atecc608c_slots_unified.h` **v2** | same header, same numbers | **aligned** — jointly revised 2026-08-08 to the silicon's size classes (§4.1a); one config, one provisioning tool |
| RTC | MCP79412 | MCP79412 | aligned |
| Manifest EEPROM | 24AA02E64 @ `0x50–0x57` | same | aligned |
| Pressure | BMP390 | BMP388 placed, 390/580 alternates | inventory-led — §2, §6.1 |
| Status LEDs | WS2812B (+ `led_pps_sync.c`) | discrete green/yellow on 2 × TLC5916 | **deliberate divergence** — §2.1 |
| I²C | one shared bus, 400 kHz | same | aligned |
| GPS UART + PPS | UART1, PPS on its own GPIO, clear of GPIO9 | same | **adopted** — this is the fix (§7.1) |
| Board selection | compile-time profiles in `board/board_config.h` with feature flags | adopt for variants A/B/C | **adopt** — §1 variants are exactly this shape |
| Dev board | Waveshare ESP32-C6-LCD-1.47 | same | already aligned (`esp32/README.md`) |
| MCU | C6 and ESP32 (Xtensa) | S3 planned | **open** — see §9.6 |
| Identity root | rover pubkey + ATECC serial | EUI-64 name + ATECC proof | **deliberate divergence** |

**On the identity divergence.** Shepherd's rovers join a Thread fleet against an operator pubkey;
navlistener observers present mTLS to a collector behind the Django CA that radiolistener already
runs. navlistener and radiolistener share one CA and one `devices` table, so this product follows
*that* convention (EUI-64 as the certificate name), and the ATECC serial is recorded at enrollment
rather than being the name itself. The two models are compatible — ours is a superset — but they
are not interchangeable, and a future shared provisioning tool must not assume one.

**On the status-LED divergence.** Shepherd drives WS2812B strips, including `led_pps_sync.c`.
This board needs four states per position and nothing more (§2.1), so an addressable RGB pixel is
sixteen million colours doing a two-bit job — and discrete LEDs on constant-current drivers avoid
the 5 V-versus-3.3 V logic question, the chain-failure mode where one dead pixel blanks
everything downstream, and roughly an amp of worst-case supply budget. The PPS-blink *idea* from
`led_pps_sync.c` is kept and improved on: here it is wired in hardware (§7.2) so it survives the
firmware dying, which a strip driven from an RMT channel cannot.

**Reuse worth taking beyond parts:** `led_pps_sync.c` already drives a strip from GPS PPS, which
is a feature this board wants and a reason the WS2812B alignment pays for itself immediately.

---

## 9. Open decisions

1. **Band discriminator encoding** — variant-level GPS enum entries vs a band bitmap in the
   descriptor's reserved bytes (§5.3). Blocks writing the first EEPROMs.
2. ~~EUI-64 text rendering~~ — **decided** (§4.2): lowercase hyphen-separated byte pairs, bare
   label. Remaining work is mechanical: a shared formatter/parser so the C feeder, the
   provisioning flow and the collector cannot disagree on it.
3. **Re-enrollment policy on RTC replacement** (§4.3) — accepted as an auditable event, but the
   operational runbook does not exist.
4. **`SIGNED_DATA` (0x07) granularity** — inherited open question from radiolistener's doc;
   per-batch is specified, per-observation is not ruled out.
5. ~~L1-only or L1/L5~~ — **dissolved** (§1.1). Both share the 24-pin NEO land pattern, so it is
   a populate-time choice, not a board decision. First spin stuffs **NEO-M9N-00B** on
   availability; **NEO-F10N-00B** drops in later without a respin. What remains open is narrower:
   confirm whether the M9N supports `RXM-RAWX`, since that determines whether the ionosphere work
   waits for an F10N.
6. ~~C6 or S3~~ — **decided: `ESP32-S3-WROOM-1U-N16R8`.** Shepherd's GPIO4/5 map retires the
   strapping-pin argument, so the S3 was chosen on three independent merits: its native USB OTG
   can enumerate as composite multi-CDC (MCU console *and* a transparent GNSS passthrough on one
   cable), which retires the USB-bridge question in firmware with no added silicon near the GNSS
   band; PSRAM and 16 MB of flash give the spool real depth, which matters when a collector can
   be offline for a week; and dual-core 240 MHz leaves headroom for TLS, zstd and a continuous
   460 800-baud UART that a single 160 MHz core does not. The **1U** variant is not optional —
   its u.FL lets the Wi-Fi radiator move physically away from the GNSS front end, which is the
   strongest available mitigation for §7.4 and impossible with a PCB-antenna module.
7. **ATECC config-zone bytes.** The slot *map* is decided (v2, §4.1a) — the per-slot
   SlotConfig/KeyConfig words and the I²C address (itself config-zone data) are not yet
   authored. They get written against the datasheet's §2.2 tables and validated on a scrap
   ATECC608C before any production part locks (§4.2).
