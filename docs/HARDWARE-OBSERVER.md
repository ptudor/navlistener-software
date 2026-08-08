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

| Part | Bands | Notes |
|---|---|---|
| NEO-M9N | L1 only **[verify]** | Already in the manifest enum (`GPS_NEO_M9N`). |
| NEO-M10 | L1 only **[verify]** | Needs an enum entry (§5.3). |
| **NEO-F10N / F10T** | **L1 + L5 [verify]** | Needs an enum entry. The dual-band option, and the only one in this list that can hear NavIC at all. |

The band split is not cosmetic — it decides what the board can contribute:

- **L1-only** is a complete, useful observer: GPS L1 C/A, GLONASS L1OF, Galileo E1, BeiDou B1I,
  QZSS L1. Most of the existing fleet is exactly this.
- **L1/L5** additionally reaches GPS L5 CNAV, Galileo E5a F/NAV, QZSS L5 and BeiDou B2a; enables
  RXM-RAWX dual-frequency work, which is the standing blocker on measured-ionosphere and
  receiver-DCB calibration; and is a precondition for NavIC.

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
| MCU | `CAT_MCU` | ESP32-S3 (`MCU_ESP32_S3`) | Feeder. S3 rather than C6 — see §7.1. |
| Receiver | `CAT_GPS` | NEO-format M9/M10/F10 | Raw nav frames + PPS. |
| Identity, public | `CAT_RTC` | MCP79412 (`RTC_MCP79412`) | RTCC + SRAM + EEPROM + **factory EUI-64** — the observer's public name. |
| Identity, private | `CAT_CRYPTO` | **ATECC608C** (`CRYPTO_ATECC608C`), I²C `0x60` | Non-extractable P-256 key; the proof of entitlement to that name. House part — §9. |
| Hardware manifest | `CAT_MEMORY` | 24AA02E64 (`MEMORY_24AA02E64`), I²C `0x50–0x57` | The installed-hardware descriptor array; carries its own EUI-64 (see §4.3). |
| Status display | `CAT_LED` | **WS2812B** ×8 (`LED_WS2812B`) | Constellation/health indication. House part — §9. |
| Pressure | `CAT_PRESSURE` | **BMP390** (needs an enum entry, §5.3) | **Vertical spoofing gate** — §6.1. House part — §9. |
| Temperature | `CAT_TEMP` | BME280 / MCP9808 | Clock-drift compensation and thermal health — §6.2. |
| Backup | `CAT_BATTERY` | 2 × CR2032 (needs enum entries, §5.3) | Two independent domains — §3. |
| Antenna | `CAT_ANTENNA` | active, u.FL/SMA | Bias + supervision → `MON-HW` `antStatus`. |

**WS2812B, aligning with shepherdprotocol** (§9). An earlier draft of this document specified
APA102 on the argument that WS2812's single-wire protocol is timing-critical and glitches under
Wi-Fi interrupt load. That argument is materially weaker on ESP32 than it is in general: the
**RMT peripheral clocks the waveform in hardware**, so the jitter APA102 avoids is largely
already avoided. Set against a real WS2812B driver stack in the sibling product — including
`esp32/main/leds/led_pps_sync.c`, which already blinks a strip off **GPS PPS** and is directly
reusable on a board that has PPS wired anyway — code reuse wins. `LED_APA102 = 2` stays in the
enum if a future board has a specific reason to want clocked SPI.

Eight LEDs is the natural width: the gnssId space has exactly seven constellations (GPS 0,
SBAS 1, Galileo 2, BeiDou 3, QZSS 5, GLONASS 6, NavIC 7 — IMES 4 is never emitted), leaving one
for link/health. Assign **position per constellation and colour per state** — seven
distinguishable hues is a worse encoding than seven fixed positions, and state maps onto
semantics the feed already has (not tracked / tracked / ephemeris current / `eph_aged` or
integrity event). Budget the rail or cap brightness in firmware: eight WS2812B at full white is
on the order of half an amp **[verify]**. Shepherd's Waveshare profile carries an independent
thermal warning about sustained full brightness on that dev board's panel — the same caution
applies to a sealed clear enclosure.

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
| `eeprom_battery_id_t` | `BATTERY_CR2032`, `BATTERY_CR123A` | The enum currently covers LiPo/18650/solar/PoE only — no primary coin cells, which is what §3 uses. |

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

This has a consequence worth confronting rather than inheriting (§8): the *stated* justification
for the ESP32-S3 re-spin was moving RX off a strapping pin. If shepherd's pinout already does
that on a C6, the S3 needs its own justification — the persistent spool tier — or the variant
should stay on the C6 the sibling product already targets.

### 7.2 PPS

Bring the receiver's `TIMEPULSE` output to a GPIO. It is easy to omit and impossible to add
later, and §6.3 depends on it.

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

---

## 8. Alignment with `shepherdprotocol`

Both products build ESP32 boards around the same `esp32-hardware-discovery` manifest, so parts
and conventions are shared by default and divergence needs a reason. What this document adopted
from the sibling, and where it deliberately does not:

| Topic | shepherd | here | Status |
|---|---|---|---|
| Secure element | ATECC608**C** @ `0x60` | ATECC608**C** | **corrected** — an earlier draft said 608B |
| RTC | MCP79412 | MCP79412 | aligned |
| Manifest EEPROM | 24AA02E64 @ `0x50–0x57` | same | aligned |
| Pressure | BMP390 | BMP390 | **corrected** — earlier draft picked from the enum list, not the house bus |
| RGB LEDs | WS2812B (+ `led_pps_sync.c`) | WS2812B | **corrected** — earlier draft specified APA102 |
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
5. **Whether variant A ships L1-only or L1/L5** (§1.1) — a cost decision that determines whether
   these boards can contribute to the ionosphere and NavIC work or only to L1 collection.
6. **C6 or S3** (§7.1, §8). The S3 re-spin's stated purpose was moving UART RX off a strapping
   pin, and shepherd's GPIO4/5 map already achieves that on a C6. If the persistent spool tier
   does not independently require the S3, staying on the C6 keeps both products on one MCU
   family, one pinout document, and one board-profile system. Decide before layout — it changes
   the footprint, not just a `#define`.
