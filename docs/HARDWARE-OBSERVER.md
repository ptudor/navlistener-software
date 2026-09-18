# The hardware observer board

The GNSS port of `radiolistener/docs/HARDWARE-OBSERVER.md`. That document specifies the
**generic** ESP32 + secure-element + RTC observer and the credential ladder; this one covers
what is specific to a *navigation* observer: which receiver, what the manifest must record for
the P9 capability fingerprint, the physical-sensor integrity gates a GNSS node can run that a
radio node cannot, and the provisioning constraints the collector actually enforces today.

Read `docs/DESIGN.md §node identity` first for the credential tiers, and `docs/DEFENSE-PNT.md`
for the detector family the sensor gates below extend.

> **Status: hardware integration contract and revision proposals.** Board projects
> and fabrication exports are maintained in the separate `navlistener-hardware`
> repository on ptudor.net. This software checkout includes support for the custom
> ESP32-S3 observer and the Waveshare ESP32-C6 board; see the
> [firmware guide](../esp32/README.md). Use the hardware repository for the current
> board revision and qualification status. Figures marked **[verify]** remain
> estimates requiring datasheet confirmation before use in firmware or thresholds.

---

## 1. Board variants

The hardware manifest (`esp32-hardware-discovery`) already reserves
`PROJECT_GNSS = 1` with `GNSS_PCB_MAIN / SENSOR / DISPLAY`. Its ID enums are a **baseline to
extend, not a fixed menu** — §5.3 lists the entries this product needs added.

| Variant | Receiver | RTC | Why |
|---|---|---|---|
| **A — first spin** (`GNSS_PCB_MAIN`) | **NEO-format** M9 / M10 / F10 | MCP79412 + Seiko 7 pF 32.768 kHz crystal (`C97604`) | NEO's larger pitch and edge-castellated pads survive a missed airwire and a rework iteration. A ZED footprint punishes a first board for a routing mistake that costs a bodge wire on a NEO. |
| **B — precision** | ZED-format (F9P / F9T) | DS3231 | Spend the TCXO where the receiver already justifies the board cost. |
| **C — modular** | pin headers, third-party module | either | Every vendor breakout has its own pinout; an project all-in-one PCB beats a breadboard stack. See §5.4 — a socketed receiver **cannot** be trusted from the manifest. |

Variant A is deliberately the cheap, reworkable one. It is also the one that gets built five
times, so it carries the full identity and sensor stack — nothing about the trust model is
deferred to the expensive board.

### 1.1 Receiver choice within variant A

**The footprint is the decision; the module is a populate-time choice.** `NEO-M9N`, `NEO-M10`,
`NEO-F10N`, `NEO-F10T`, `NEO-M8T`, `NEO-M8Q`, `NEO-M8U` and `NEO-D9C` all share the same 24-pin
NEO land pattern (`GPSM-SMD_24P-L16.0-W12.2-P1.10` in LCSC's library). Reusing that footprint
still requires qualification of the receiver interfaces and antenna supply. The current
passive bias circuit is not qualified for the ANN-MB1/ANN-MB5; see §7.3.1 for the required
supply revision before using those antennas.

| Part | Bands | Status |
|---|---|---|
| **NEO-M9N-00B** (`C5119087`) | L1 only **[verify]** | **First populate.** Best availability of the modern parts by a wide margin; already `GPS_NEO_M9N` in the manifest enum. |
| NEO-F10N-00B (`C21709333`) | L1 + L5, no GLONASS | The upgrade. Same footprint, roughly 2× the cost, thin stock. Needs an enum entry (§5.3). Bands from `UBX-23002117`: GPS L1C/A+L5, Galileo E1+E5a, BDS B1C+B2a, QZSS L1C/A/L1S/L1Sb/L5, NavIC L5, SBAS L1C/A. |
| NEO-F10T-00B | L1 + L5 **[verify]** | Timing variant; ~8× the cost for features this board does not use. |
| NEO-M10 | L1 only **[verify]** | Needs an enum entry. No existing design uses one. |

The band split decides what the board can contribute:

- **L1-only** is a complete, useful observer: GPS L1 C/A, GLONASS L1OF, Galileo E1, BeiDou B1I,
  QZSS L1. Most of the existing fleet is exactly this — and **GLONASS L1OF is what the regression fix
  calibration work consumes**, so an M9N board contributes to the active task from day one.
- **L1/L5** additionally reaches GPS L5 CNAV, Galileo E5a F/NAV, QZSS L5 and BeiDou B2a; enables
  RXM-RAWX dual-frequency work, which is the standing blocker on measured-ionosphere and
  receiver-DCB calibration; and is a precondition for NavIC. Note the F10N has no GLONASS
  (`UBX-23002117`), so its band set is not a superset of the L1-only parts' — the §5.3 node
  descriptor must not claim L1OF on one.

**Verify before assuming the ionosphere work is a firmware toggle:** whether the M9N supports
`RXM-RAWX` at all. Raw measurements are often restricted to timing and high-precision parts, and
if the M9N lacks it, dual-frequency work waits for an F10N rather than a config change. It does
not affect nav-message collection either way.

**GPS L5 health.** L5 is still pre-operational and broadcast unhealthy. u-blox's default
excludes it from the *navigation solution* (`UBXDOC-963802114-12193` §1.1, §2.1.4) — a
position-fix default, which this product never exercises: we forward raw `UBX-RXM-SFRBX`
and decode centrally, so health bits get recorded, not obeyed. Provisioning sets
`CFG-SIGNAL-*` and `CFG-MSGOUT-UBX_RXM_SFRBX_*` per node; configuration item `0x10321001`
overrides L5 health with the corresponding L1 C/A status if wanted (ready-made RAM/BBR/flash
strings in Tables 4 and 5).

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
| Identity, public | `CAT_RTC` | **MCP79412T-I/SN** + Seiko `SC-32S32.768kHz20PPM7pF` crystal (LCSC/EasyEDA `C97604`; `RTC_MCP79412`) | RTCC + SRAM + EEPROM + **factory EUI-64** — the observer's public name. |
| Identity, private | `CAT_CRYPTO` | **ATECC608C-SSHDA-T** (`C28975195`, `CRYPTO_ATECC608C`) | Non-extractable P-256 key; the proof of entitlement to that name. |
| Hardware manifest | `CAT_MEMORY` | `24AA025E64T-I/SN` (LCSC `C615601`; `MEMORY_24AA025E64`) | The installed-hardware descriptor array and board EUI-64. The addressable `025` variant is required — see below. |
| Status panel | `CAT_LED` | 16 × 0805 (8 green + 8 yellow) via 2 × **TLC5916** | Constellation/health indication — §2.1. |
| Pressure | `CAT_PRESSURE` | **BMP388** placed; BMP390 and BMP580 are footprint alternates | **Vertical spoofing gate** — §6.1. |
| Temperature | `CAT_TEMP` | **MCP9808-E/MS** (`C94847`) | Crystal-drift characterisation and thermal health — §6.2. |
| Humidity | `CAT_SENSOR` | **HDC2080** | Dew point / enclosure-seal diagnostic — §6.4. Not GNSS math. |
| Backup | `CAT_BATTERY` | 1 × primary 3 V CR123A in MYOUNG `BH-123A-A1CJ002` holder (LCSC/EasyEDA `C5290177`; needs enum entry, §5.3) | Shared GNSS/RTC backup with annual replacement — §3.2. |
| GNSS antenna | `CAT_ANTENNA` | SMA jack, right-angle, 4-leg THT (Amphenol `132289` class) | Protected `VCC_RF` active-antenna bias tee; no first-spin open/short supervisor — §7.3. |
| Wi-Fi antenna | `CAT_ANTENNA` | u.FL → RP-SMA pigtail off the 1U module | Physically separable from the GNSS path — §7.1. |
| USB | `CAT_CONNECTOR` | USB-C, 16-pin USB 2.0, THT shield legs | §7.6. |

**Use the addressable `24AA025E64`, not the non-addressable `24AA02E64`.** The
`24AA02E64` treats address bits A0/A1/A2 as don't-cares and therefore acknowledges
the entire `0x50–0x57` range. That collides with the MCP79412's EEPROM/EUI-64 at
`0x57`, making the observer identity unreadable. Use the addressable
**`24AA025E64T-I/SN`** instead: strap pins 1/A0, 2/A1 and 3/A2 to GND so only
`0x50` is acknowledged. Pin 4 is GND/VSS, pin 5 SDA, pin 6 SCL, pin 7 NC and
pin 8 `3V3_SENS`. The manifest is stored in the `24AA025E64`; the ATECC608C
holds the non-extractable private key. LCSC `C615601` is the SOIC-8 device,
although it was out of stock on 2026-08-09 and may need JLC global sourcing or
distributor purchase.

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

Do not add a dedicated `JAM` or `SPOOF` LED. Those are interpreted receiver/firmware
states rather than direct hardware truths, and their definitions may evolve. Reserve an
unmistakable panel-wide flashing pattern for jamming, spoofing, clock-integrity, or other
operator-attention alarms. The two discrete red LEDs remain narrowly defined: always-on
`3V3_SYS` present and hardware-buffered GNSS PPS present.

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

**First-spin schematic contract.** Call the green driver `U12` and the yellow
driver `U13`; the top physical LED row is green and the bottom row is yellow.
Both drivers use this pin-level circuit:

| TLC5916 pin | Connection |
|---:|---|
| 1 `GND` | GND plane. |
| 2 `SDI` | `U12`: `LED_SDI` from ESP GPIO14. `U13`: `U12` pin 14 `SDO`. |
| 3 `CLK` | Shared `LED_SCLK`, ESP GPIO11. |
| 4 `LE` | Shared `LED_LATCH`, ESP GPIO12. The internal pull-down is sufficient while both drivers are blanked at reset. |
| 5–12 `OUT0`–`OUT7` | Corresponding LED **cathodes**. All LED anodes go directly to protected `+5V`; fit no per-LED ballast resistors. |
| 13 `OE/ED2` | `U12`: `LED_GREEN_OE_N`, GPIO47. `U13`: `LED_YELLOW_OE_N`, GPIO48. Fit one 10 kΩ pull-up to `3V3_SYS` on each net so reset means blank. |
| 14 `SDO` | `U12` to `U13` pin 2. `U13` to `LED_SDO`, ESP GPIO13, preserving error-register readback. |
| 15 `R-EXT` | 2.49 kΩ, 1%, 0603 to GND. The power-on current is approximately 7.5 mA/channel; trim the two values independently after the diffuser is chosen. |
| 16 `VDD` | `3V3_SYS`, with 100 nF X7R directly from pin 16 to pin 1. Do **not** power TLC logic from 5 V: its guaranteed high threshold is 0.7 × VDD, so a 3.3 V ESP output would not be a guaranteed high. |

Use the same channel order in both colours: `OUT0` GPS, `OUT1` SBAS,
`OUT2` Galileo, `OUT3` BeiDou, `OUT4` QZSS, `OUT5` GLONASS, `OUT6`
NavIC, `OUT7` uplink. Since the data enters `U12` first, firmware transmits the
yellow byte first and the green byte second before pulsing `LED_LATCH`. Place one
10 µF, 10 V X7R bulk capacitor from the `+5V` LED-anode bus to GND near the
middle of the row; it is in addition to the 100 nF logic bypass at each driver.

---

## 3. Power and backup domains

### 3.1 The rail tree — three regulators, three noise domains

VBUS (USB-C, §7.6) is the board's only input; after the connector protection and eFuse,
the resulting `+5V` rail feeds the LED panel anodes directly (§2.1). Everything else
splits into three 3.3 V rails, one per noise domain, each a linear regulator fed from
that protected `+5V` node — no switcher anywhere near a 1575 MHz front end, and no
cascaded LDOs:

| Rail | Regulator | Feeds | Why this part |
|---|---|---|---|
| `3V3_SYS` | **`LDL1117S33R`** (ST, SOT-223-4, 1.2 A, LCSC `C435835`). Do not substitute the similarly pinned AMS1117 without rechecking its output-capacitor ESR requirements. | ESP32-S3 module, USB logic, TLC5916 `VDD` | Espressif recommends a supply capable of at least 0.5 A; digital rail, so current and thermals matter, noise does not. Worst case from 5 V: (5.0 − 3.3) V × 0.5 A ≈ 0.85 W — acceptable for burst duty only with a real `3V3_SYS` copper heat spreader around the SOT-223 tab, and a first-prototype thermal test remains mandatory. |
| `3V3_GNSS` | **ADM7150ARDZ-3.3** (SOIC-8-EP, 800 mA, 1.0 µVrms 100 Hz–100 kHz, PSRR > 90 dB 1 kHz–100 kHz at 400 mA, VIN 4.5–16 V — so it must feed from protected `+5V`, not from 3V3) | NEO `VCC`; the NEO generates its own filtered `VCC_RF` active-antenna bias output (§7.3) | The receiver's RF chain is the one place supply noise is directly signal noise. 800 mA is deliberate headroom: variant B's ZED-F9P (~130 mA **[verify]**) reuses this rail unchanged. |
| `3V3_SENS` | **`RT9193-33GB`** (genuine Richtek, SOT-23-5, 300 mA, LCSC `C15651`), with its required 22 nF BP capacitor populated for low-noise mode | ATECC608C, 24AA025E64, MCP79412 `VCC`, MCP9808, HDC2080, BMP388 | The vertical gate lives on the barometer's noise floor (§6.1); supply noise on the BMP388 is spent directly out of that budget. Load is trivial — the ATECC's ECC operations dominate at ~16 mA **[verify]**; everything else is microamps to low milliamps. |

Rules that make the split work:

- **I²C pull-ups tie to `3V3_SENS`** — the slaves' rail. If the sensor rail ever
  collapses with the MCU alive, the bus reads stuck-low (detectable); pull-ups on the
  MCU rail instead would back-power the dead rail through every slave's ESD clamps.
- **`3V3_SYS` is always on; the GNSS and sensor rails are GPIO-gated,
  default-on.** The LDL1117 has no enable and the MCU cannot be allowed to gate its
  own brain, but the other two EN pins go to S3 GPIOs with 100 kΩ pull-ups **to
  `3V3_SYS`** — never to VBUS: the S3's
  GPIOs are not 5 V tolerant, the same trap §7.2 documents for the blanking MOSFET.
  High-Z at reset means the pull-ups win and both rails are ON before firmware runs,
  through a crash, and if firmware never boots — the §7.2 fail-safe argument, applied
  to power. Sequencing falls out naturally: `3V3_SYS` rises first, the peripheral
  rails follow it through their pull-ups.

  Why gate at all: **remote power-cycle is the fix for this fleet's observed failure
  mode.** The F9P on `observer16` dropped off its bus after a hot spell (2026-08-05,
  §6.2) and stayed dark until someone could reach it. A wedged receiver or a hung I²C
  slave holding SDA low both clear with a rail cycle, and a GNSS-rail cycle is a *warm*
  restart — `V_BCKP` rides the shared CR123A (§3.2), so ephemeris, almanac and receiver
  time survive the bounce. Firmware contract: idle/tristate every pin driving into a
  domain before de-asserting its EN (a driven UART TX or pulled-up I²C line would
  back-power the dead rail through ESD clamps), hold off long enough for the domain's
  capacitance to bleed, re-init on the way back up.
- **Budget sanity** (all **[verify]** at datasheet time): S3 ≤ 500 mA bursts, GNSS
  ~35 mA + up to 50 mA antenna bias, sensors < 25 mA, panel ~110 mA on VBUS. Aggregate
  worst case is ~700 mA-class at 5 V — inside USB-C's advertised-current regime but
  above legacy USB 2.0's 500 mA default, so a pessimistic C-to-C source could brown
  out under simultaneous Wi-Fi TX + full panel; TX burstiness and §2.1's panel
  dimming are the practical mitigations, and the bench check belongs on the first
  prototype **[verify]**.

First-spin regulator networks, all fed from the protected `+5V` node after the
TPS259531 eFuse:

- **`3V3_SYS`, `LDL1117S33R`:** pin 3 `VIN` to `+5V`; pin 1 to GND; pin 2 and
  tab/pin 4 together to `3V3_SYS`. Place 10 µF, 10 V, X7R from `VIN` to GND and
  22 µF, 10 V, X7R from `3V3_SYS` to GND at the regulator. ST's stability minima
  are 1 µF input and 4.7 µF output; the larger fitted values also cover the S3's
  burst load. The tab is **output, not ground**: use `3V3_SYS` copper for its heat
  spreader and keep the ground plane intact immediately below it.
- **`3V3_SENS`, `RT9193-33GB`:** pin 1 `VIN` to `+5V`; pin 2 to GND; pin 3 to
  `SENS_EN`; pin 4 `BP` through 22 nF directly to GND; pin 5 to `3V3_SENS`.
  Fit 2.2 µF, 10 V, X7R at both `VIN` and `VOUT`. Pull `SENS_EN` up to
  `3V3_SYS` with 100 kΩ and also take it to **GPIO21 (module pin 23)**, so it is
  on by default but firmware can power-cycle it. **Library audit:** the EasyEDA
  symbol seen on 2026-08-09 incorrectly called pin 4 `NC`; the Richtek data sheet
  calls it `BP`, says it cannot float, and requires 22 nF or more to GND.
- **`3V3_GNSS`, `ADM7150ARDZ-3.3-R7`** (ADI, SOIC-8-EP, LCSC `C658444`):
  pin 8 `VIN` to `+5V` with 10 µF to GND; pin 7 to `GNSS_EN`; pin 6 `REF`
  shorted to pin 5 `REF_SENSE`, with 1 µF from that pair to GND; pin 4 and EP to
  GND; pin 3 `BYP` through 1 µF to GND; pin 2 `VOUT` to `3V3_GNSS` with 10 µF
  to GND; pin 1 `VREG` through 10 µF to GND. Pull `GNSS_EN` up to `3V3_SYS`
  with 100 kΩ and take it to **GPIO38 (module pin 31)**. The four local
  capacitors are not interchangeable decoration: each serves a named internal
  node and belongs at its corresponding pin.

The control GPIO choices are closed: `SENS_EN` is GPIO21, `GNSS_EN` is GPIO38,
and the open-drain `EFUSE_FAULT_N` input is GPIO9. The regulator enables avoid
both the S3 strapping pins and the GPIO1-through-GPIO18 group that can emit a
short low-level glitch during power-up. GPIO9's startup-low pulse is harmless
on the pulled-up, open-drain fault net and occurs before firmware samples it.

The two **gated** regulators are manifest slots after all — an earlier revision of
this section exempted them as "nothing to probe," which was wrong the moment their EN
pins landed on GPIOs: a rail firmware can switch is firmware-relevant hardware, and
the manifest already encodes GPIO-addressed entries (`CAT_BUTTON`, addr = GPIO pin).
So `3V3_GNSS` and `3V3_SENS` get `CAT_POWER` entries with the descriptor's address
byte carrying the EN GPIO (enum additions in §5.3), and the status byte earns its
keep: `INSTALLED` = gated rail, `NOT_POPULATED` = a build variant that strapped the
EN — which tells the collector whether "power-cycle the receiver" is a command this
node can execute. Only the always-on `3V3_SYS` regulator stays plain BOM: no control,
no bus presence, and its failure announces itself as the death of everything on it.

The shared backup cell below is independent of all three regulated rails by design.

### 3.2 Backup supply — one annually serviced CR123A

One **primary 3 V CR123A** supplies both backup functions:

- **Receiver `GNSS_VBCKP`** preserves battery-backed RAM, orbit data, last
  position and the receiver's clock, buying hot/warm starts.
- **RTC `RTC_VBAT`** preserves the observer's independent time reference.

This deliberately makes the battery a shared failure domain. A missing or dead
cell loses both warm-start state and independent RTC time; the design accepts that
trade for the simpler, mechanically cleaner first spin. Replace the cell during
annual maintenance **while USB power is present**, so normal VCC preserves both
devices' state during the swap, and write the installation date on the enclosure
label. If a cell is instead left for two or three years and the receiver begins
cold-starting or the RTC reports oscillator stop, treat the overdue battery as
the first suspect, not an unexplained board failure.

Use a polarized MYOUNG `BH-123A-A1CJ002` horizontal through-hole holder,
LCSC/EasyEDA `C5290177`, in the center of the main PCB. Place the complete EasyEDA
device by its LCSC number so its two electrical pads and anti-misinsert locating
post come from the matched library footprint; do not substitute a generic CR123A
footprint. Its long, narrow body preserves the status-LED edge and allows the
low-current devices to form a row above it. Keep the holder courtyard and the cell
insertion/removal sweep free of top-side components; keep USB and RF traces out
from under it.
Hand-solder the holder after normal SMT assembly and insert the cell only after
all soldering and cleaning are complete. If the enclosure can be dropped or
vibrated, place a PCB support or enclosure boss near the holder so the 16 g cell
does not flex the board.

Connect holder negative to GND and holder positive to a source net named
`BACKUP_BAT`. Split `BACKUP_BAT` through two separately removable 0 Ω, 0603 links:
one to `GNSS_VBCKP` and one to `RTC_VBAT`. The links add no intentional voltage
drop but let either load be isolated during bring-up and current measurement. Add
a `BACKUP_BAT` test point accessible with the enclosure open. Fit 100 nF X7R from
`GNSS_VBCKP` to GND directly beside NEO pin 22; the cell is still connected with
negligible intentional series resistance, as u-blox requires during the backup
switchover current transient. Do not add external
switchover or isolation diodes; both loads already implement their required
switchover, and diode drop only consumes backup headroom. Leave `RTC_VBAT`
directly connected after its 0 Ω link: the MCP79412 reference connection does
not require a local capacitor, and leaky bulk capacitance would be counterproductive
on a sub-microamp backup load. Do not fit tantalum, electrolytic, or supercapacitor
bulk on either backup branch.

The NEO-M9N specifies 45 µA from `V_BCKP` at 3 V with VCC absent. A nominal
1550 mAh CR123A therefore represents about 3.9 ideal years of *continuous
unpowered* GNSS backup before capacity and temperature derating; it is not a
five-year continuous-off guarantee. In the intended normally USB-powered service,
the cell supplies that load only during outages, so annual replacement is highly
conservative. Reserve one optional `BACKUP_BAT_SENSE` path on GPIO1, but leave its
divider DNP until the off-state isolation is closed: a plain always-connected ADC
divider can inject current into an unpowered ESP32. GPIO2 returns to the spare pool.

Silkscreen the holder **CR123A 3 V PRIMARY ONLY — NO RCR123/16340**. A rechargeable
RCR123/16340 cell can reach 4.2 V and would exceed the NEO-M9N `V_BCKP` absolute
maximum.

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

### 4.1c Commissioning — the microcontroller is a fourth identity

The attestation above covers the secure element, the RTC and the EEPROM. It does not mention
the ESP32-S3, and whether a unit is locked is entirely a microcontroller property: Secure Boot
and flash encryption are ESP32-S3 eFuses. Replace a locked module with a blank one and every
attested identifier is unchanged, while the ATECC — whose slot 0 must stay usable without
authorization so a buyer can re-enroll it — signs for whichever microcontroller drives the bus.

So the microcontroller gets an identity of its own, and the manufacturer signs a second
statement once the board is locked:

- **The key.** The ESP32-S3 Digital Signature peripheral holds a 3072-bit RSA key that
  software can never read: the private parameters are stored encrypted under an HMAC key in a
  read-protected eFuse block. Commissioning firmware generates it on the device after Secure
  Boot is in force, so the private key has never existed anywhere else. It costs one eFuse key
  block — count it in the production eFuse list beside the three Secure Boot digests and the
  flash-encryption key.
- **The commissioning record.** A fixed 147-byte statement binding ATECC serial, RTC EUI-64,
  board EUI-64 and board revision to the microcontroller's key, its lock state, a profile
  (trusted, open or test) and a generation number, signed by the same manufacturer key under
  its own domain string. It is made *after* slot 14 is locked, so it lives in the
  manufacturer's records and in the device's flash rather than in the ATECC.
- **The session proof.** On every connection the microcontroller signs a value derived from
  that TLS session, and the collector verifies it against the key the record names.

A replaced module is a recorded service event: the replacement generates its own key and the
board is commissioned again at the next generation. Only the manufacturer can do that, because
only the manufacturer key can sign the new statement. Open boards get a record and no key;
commissioning one changes no eFuse.

Formats, verification order and the registry that withdraws boards are normative in
`docs/COMMISSIONING.md`.

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

The MCP79412 and the 24AA025E64 each carry a factory EUI-64, and the ATECC carries a 9-byte
serial. Assign them distinct roles and record all three at enrollment:

| Source | Role |
|---|---|
| MCP79412 EUI-64 | **observer identity** — the `receiver_id` in the cert SAN and the `devices` row |
| 24AA025E64 EUI-64 | **board serial** — identifies the PCB, not the network node |
| ATECC serial | binds the key material to the enrollment record |

The ESP32-S3 is a fourth identity of a different kind (§4.1c): it has no factory-programmed
serial worth trusting — its MAC is a label — so it is named by the SHA-256 of the key
generated inside it at commissioning. The manufacturer's unit record lists all four, together
with the serial of every other fitted part that has one, so that "which parts are on this
unit" has one answer.

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

The ESP32 feeder now reads and validates the manifest during custom-board startup and exposes
the shared I2C bus for the RTC, sensor and secure-element drivers. It logs the discovered GNSS
model rather than treating the compiled board choice as the installed-hardware truth. The
next adoption step is control-plane reporting: the feeder should report the validated manifest
at enrollment and on change; the collector can then store it with the device and derive the expected
`(gnssId, sigId)` set from the `CAT_GPS` entry. That set is the reference the existing
capability detectors compare observed frames against — the detector machinery already exists;
this supplies its declared side with something hardware-rooted instead of configured by hand.

Blank EEPROM handling is deliberately narrower than “magic byte absent means write.” Startup
first reads the immutable factory EUI-64 and compares it with the EUI remembered in the separate
`hwmanifest` NVS namespace:

| EEPROM observation | Known EUI state | Action |
|---|---|---|
| valid programmed manifest | none or same | use it; remember the EUI when first adopted |
| blank | never seen | offer/use the compiled defaults only in an explicit factory-init build |
| blank | same EUI was already adopted | recovery required; never silently overwrite |
| blank or programmed | different EUI | replacement confirmation required |
| malformed manifest, unreadable EUI or I2C error | any | reject; never write |

`CONFIG_NVF_MANIFEST_FACTORY_INIT` is off by default. When deliberately enabled by the
programmer, it writes only the blank/never-seen row, uses `force=false`, reads the complete
image back, validates the 24AA025E64 self-reference at `0x50`, and only then records the EUI.
The ordinary eight-second configuration-reset gesture erases only `navfeeder`; it does not
erase `hwmanifest`. The custom-board build also refuses the generic fallback that automatically
erases the complete default NVS partition when its format is incompatible or full, because that
would discard the locally known EUI and weaken the replacement check. A deliberate whole-flash
erase can still remove local history; once control-plane reporting lands, the collector's last
adopted EUI is the durable authority for that recovery case.

### 5.3 Baseline extensions supplied for this product

The shared public library now includes these stable entries:

| Enum | Add | Why |
|---|---|---|
| `eeprom_gps_id_t` | `GPS_NEO_M10`, `GPS_NEO_F10N`, `GPS_NEO_F10T`, `GPS_ZED_F9T` | Variant A candidates and the F9T fleet part. |
| `eeprom_pressure_id_t` | `PRESSURE_BMP390` | The BMP390 footprint alternate. |
| `eeprom_battery_id_t` | `BATTERY_CR123A` | This board's primary 3 V cylindrical backup cell. |
| `eeprom_sensor_id_t` | `SENSOR_HDC2080` | The combined temperature/humidity part (§6.4); `TEMP_MCP9808` already exists for the dedicated sensor. |
| `eeprom_power_id_t` | `POWER_ADM7150`, `POWER_RT9193` | The GPIO-gated rails (§3.1); descriptor address byte = the EN GPIO, following `CAT_BUTTON`'s addr-is-GPIO convention. |

`MEMORY_24AA025E64 = 6` is now part of the shared baseline rather than a
navlistener-specific extension. This board's manufacturing manifest must use:

```c
caps.components[0] =
    IC_EEPROM_SELF_24AA025E64(EEPROM_I2C_ADDR_0);  // U28, A0/A1/A2 low => 0x50
```

**The band discriminator is the one that matters for integrity.** `GPS_ZED_F9P = 1` names a part
family, not a band capability — but DESIGN.md's worked example is precisely the **F9T-00B (L1+L2)
vs F9T-10B (L1+L5)** distinction, and the id byte cannot express it. Two options:

- variant-level enum entries (`GPS_ZED_F9T_00B`, `_10B`, …) — simple, but combinatorial; or
- a **band bitmap** carried in the descriptor's reserved bytes, which the header already earmarks
  for "feature flags" with an explicit forward-compatibility rule.

The bitmap is the better fit: bands are orthogonal to part identity, and it keeps one enum entry
per part. This remains a schema decision to settle before writing a production manifest for a
receiver whose orderable variants share a model ID but differ in bands. It does not block the
first-spin NEO-M9N manifest or model-level RTC/GNSS discovery.

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

The S3 adds one wrinkle the C6 convention did not have: GPIO4, GPIO5 and GPIO10
can briefly drive low during S3 power-up. Fit 330 Ω in series in both UART paths
and between the PPS buffer and GPIO10. At 460800 baud their edge delay is
negligible, while the GPIO5 and GPIO10 resistors limit contention if the
already-powered receiver or PPS buffer is high during an ESP-only reset.
These resistors are not fuses, external-short protection, or power-domain
isolation; firmware must still leave the UART and PPS GPIOs high-impedance before
turning `3V3_GNSS` off. Their contention limit is about 10 mA for a direct 3.3 V
logic fight (`3.3 V / 330 Ω`).
Do not add additional series ferrite beads to the first-spin UART. The generic
ZED-F9P EMI guidance recommends grounding, shielding, layout optimization and
low-pass filtering of digital noise sources, but does not specify ferrites as a
UART requirement; the NEO-M9N guidance likewise emphasizes a continuous ground
reference and via shielding around serial lines. The existing 330 Ω series parts
provide predictable edge damping and contention protection. Route both lines over
the uninterrupted ground plane and away from `RF_IN`; place the resistor in the
NEO-to-ESP receive path close to the NEO TXD driver and the resistor in the
ESP-to-NEO path close to the ESP GPIO4 driver.

#### NEO-M9N local support and recovery circuit

At NEO pin 23 `VCC`, place **1 µF X7R directly at the pad** and a 4.7 µF,
10 V X7R bulk capacitor beside it, both to the nearest ground pins. Feed the pad
from `3V3_GNSS` with a short, wide connection and no bead or series resistor;
u-blox permits less than 0.2 Ω in the VCC path and specifies a 100 mA-class peak.
Pin 22 `V_BCKP` is `GNSS_VBCKP` with the local 100 nF capacitor specified in
§3.2. Connect pins 10, 12, 13 and 24 to the uninterrupted ground plane with short
returns and local vias.

Use UART mode: leave pin 2 `D_SEL` open; pin 20 `TXD` goes through 330 Ω to
`GNSS_UART_RX` / ESP GPIO5 and pin 21 `RXD` goes through 330 Ω to
`GNSS_UART_TX` / ESP GPIO4. Leave pins 4, 14–19 unconnected on this revision
(`EXTINT`, `LNA_EN`, reserved pins, and the unused NEO I²C pins). The active
antenna is biased from pin 9 `VCC_RF` as §7.3 specifies, so `LNA_EN` is not part
of that circuit. In particular, do not connect NEO pins 18/19 to the sensor I²C
bus: its pull-ups are on switched `3V3_SENS`, while the module I/O domain is on
independently switched `3V3_GNSS`; joining them defeats clean power cycling and
can back-power a disabled domain. A future GNSS I²C option requires its own
GNSS-referenced pull-ups or explicit isolation.

Expose module-side test pads for `NEO_TXD`, `NEO_RXD`, `NEO_SAFEBOOT_N` pin 1,
`NEO_RESET_N` pin 8, and GND. Do not add capacitors to RESET or external pull-ups
to RESET/SAFEBOOT; the module supplies them. Pulling RESET low for at least
100 ms performs a destructive cold start, while holding SAFEBOOT low during
power-up enters recovery mode. SAFEBOOT recovery **cannot use USB**—it must use
UART, I²C or SPI—so these pads remain useful even after adding the normal USB
service port.

Fit an unpopulated four-pin 2.54 mm service-header footprint such as Ckmtw
`B-2100S04P-A110`, LCSC/EasyEDA `C124378`, in USB order: pin 1
`NEO_USB_VBUS`, pin 2 `NEO_USB_DM_CONN`, pin 3 `NEO_USB_DP_CONN`, pin 4 GND.
This is a self-powered USB port: **do not connect `NEO_USB_VBUS` to board VBUS
or `+5V`**; the board remains powered through its main USB-C connector.

Implement the NEO USB supply exactly as a host-present supply. Reuse one
`RT9193-33GB` (`C15651`): `NEO_USB_VBUS` to pin 1 `VIN` with 1 µF X7R to GND,
pin 2 GND, pin 3 `EN` to `3V3_GNSS`, pin 4 `BP` through 22 nF to GND, and pin 5
to net `NEO_VUSB` with 1 µF X7R to GND; `NEO_VUSB` then feeds NEO pin 7. Thus
the host's VBUS creates the required 3.3 V detect rail only while the receiver is
powered. Add 100 kΩ from `NEO_VUSB` to GND so the detect input is decisively low
when the service cable is absent. Protect the header with ST `USBLC6-2SC6`,
LCSC `C7519`, placed beside the header; connect its VBUS reference to
`NEO_USB_VBUS`. Fit **27 Ω, 5%** from `NEO_USB_DM_CONN` to `NEO_USB_DM` and
from `NEO_USB_DP_CONN` to `NEO_USB_DP`, close to NEO pins 5 and 6 respectively.
Route D−/D+ together as a 90 Ω differential pair with no branches. This USB port
supports ordinary communication and firmware update; the separate SAFEBOOT/UART
pads cover corrupted-flash recovery.

This retired the *stated* justification for the S3 re-spin, which was exactly this hazard. The
S3 was then chosen on its own merits instead — see §9.6, now closed.

### 7.2 PPS, and the two lights that survive a crash

Bring the receiver's `TIMEPULSE` output to a GPIO — easy to omit, impossible to add later,
and §6.3 depends on it. **Also drive an LED from the buffered PPS line
directly**, not through the LED drivers. It then blinks whenever the receiver emits
TIMEPULSE, independent of the ESP32 entirely. It is a time-lock indication only when
the receiver is configured to suppress or distinguish TIMEPULSE before valid time.

Pair it with a red **power LED hardwired to the always-on `3V3_SYS` output**:
`3V3_SYS` through 1 kΩ to the LED anode, with its cathode directly to GND. A typical
1.8–2.2 V red LED then draws about 1.1–1.5 mA. No transistor or GPIO belongs in this
path. This verifies the eFuse plus main 3.3 V regulator—the useful board-level power
truth—while raw VBUS remains a multimeter diagnostic. The two discrete LEDs therefore
survive wedged or crashed firmware: one says *the system rail is alive* and one says
*the receiver is emitting PPS*. The TLC panel supplies all MCU-controlled status and
fault indications; do not add redundant dedicated MCU LEDs.

Buffer PPS for fan-out, not for level: the receiver and the MCU are both 3.3 V,
so nothing needs translating. Fit a dual Nexperia `74LVC2G17GV,125`,
LCSC/EasyEDA `C513289` (SC-74-6), powered from `3V3_SYS`: pin 5 VCC, pin 2 GND,
and 100 nF X7R directly between them. NEO pin 3 `TIMEPULSE` is net
`GNSS_PPS_RAW`; connect it to both buffer inputs, pins 1 `1A` and 3 `2A`, and
fit 100 kΩ from that raw net to GND so both outputs remain low while the GNSS
rail is off.

Powering this buffer from `3V3_GNSS` is also electrically valid: the selected
Nexperia part has `I_OFF` partial-power-down protection, so a live ESP GPIO cannot
back-power the disabled buffer. It is not the preferred first-spin connection,
because both outputs become high-impedance instead of actively low when GNSS is
disabled, and the visual output would draw its 1–2 mA LED pulse from the quiet
GNSS rail. If layout forces that choice, retain the raw-input pull-down and add
100 kΩ from `GNSS_PPS_BUF` to GND so GPIO10 has a defined low state while the
buffer is off.

Pin 6 `1Y` is the timing path. Name it `GNSS_PPS_BUF`, place a labeled PPS test
pad and a neighboring GND pad on that net, then pass it through the existing
**330 Ω** contention resistor to `GNSS_PPS` / ESP GPIO10. The test pad belongs
on the buffer side of the resistor so a scope sees the clean edge. Do not replace
330 Ω with the board's existing 22 Ω value: 22 Ω is source damping, while this
resistor also protects against GPIO10's documented power-up low glitch. The
buffer's few-nanosecond propagation delay is constant and can be calibrated if
the RTC is characterised against PPS (§6.3).

Pin 4 `2Y` is the visual path, so LED current never disturbs the measured PPS
edge. Run it through 1 kΩ to the PPS LED anode and connect the LED cathode directly
to GND. The resulting roughly 1–2 mA pulse is bright enough for a diagnostic and
comfortably inside the buffer's drive.

The first spin deliberately has no PPS-blanking MOSFET. Direct grounding removes a
component, a pull-up, a control route, a GPIO assignment, and a failure point while
making PPS a hardware truth that firmware cannot suppress. The TLC panel already
provides software dimming and this LED-centric board is not intended to become fully
dark; a later enclosure-specific revision can restore blanking if field use demands it.

Keep the **power LED on `3V3_SYS`, not either switched rail and not a GPIO**. If
software can extinguish it, "no light" stops meaning "the system rail is absent" and
the ground truth is gone. Run it lean—1–2 mA is legible across a room, and it is lit
continuously for years in a hot box.

### 7.3 Antenna

Prefer SMA for an external **active L1 GNSS antenna that tolerates the loaded
bias voltage** over an on-board patch —
antenna quality dominates data quality by a wider margin than any silicon choice
on this board. The NEO-M9N already contains the LNA, SAW filter, LTE band-13
notch, RF input DC block and 50 Ω match. Do not add another series DC-block
capacitor, matching network, SAW or LNA on the first spin.

> **This section describes the existing passive bias circuit.** Its voltage
> drop matters when choosing an antenna: the ANN-MB1 and ANN-MB5 have a 3.0 V
> minimum that this circuit does not establish under load. §7.3.1 specifies the
> direction for a revised supply; it is not present in the current CAD or exports.

**Current NEO-M8/M9 antenna candidate:** Abracon `AECP0401G4ZS-3000S`,
[DigiKey `535-AECP0401G4ZS-3000S-ND`](https://www.digikey.com/en/products/detail/abracon-llc/AECP0401G4ZS-3000S/13574215).
This magnetic GNSS puck has a 3 m RG-174 cable and standard SMA male connector.
Its [data sheet](https://abracon.com/datasheets/AECP0401G4Z.pdf) specifies
2.2–5.0 V operation, 5–15 mA consumption and 28 dB typical / 30 dB maximum
LNA gain, with GPS, GLONASS and BeiDou coverage; Galileo E1 shares GPS L1's
1575.42 MHz band. At 15 mA, the nominal bias calculation below gives 2.87 V
before choke/wiring losses, comfortably above its 2.2 V minimum. No PCB change
is expected for this antenna; confirm loaded voltage and reception on the
assembled board. This is an L1 choice; select an L1/L5 antenna separately for
the future NEO-F run. ANN-MS is retained below only as a historical reference.

Other active magnetic ceramic pucks can work too. Require standard SMA male
(not RP-SMA), 50 Ω RHCP, and coverage of every intended constellation,
including BeiDou B1I near 1561 MHz and GLONASS near 1602 MHz. Prefer a supply
minimum of 2.7 V or lower and modest current; a generic “3–5 V” rating alone
leaves little margin through R18. Respect the receiver's external-gain limit
after cable loss. With the antenna connected, measure the drop across R18
(`I = voltage drop / 22 Ω`) and `ANT_BIAS`, accounting for L2/cable losses.
Check per-constellation C/N0 and navigation-message reception under open sky,
not just whether the receiver obtains a fix.

Build the u-blox bias tee from pin 9 `VCC_RF`: use a 22 Ω, at least 0.5 W series
resistor—Yageo `RC2010JK-0722RL`, LCSC/EasyEDA `C137041`, is a 0.75 W 2010
choice with an available EasyEDA model—to net `ANT_BIAS`; place 100 nF, 16 V,
X7R from `ANT_BIAS` to GND; then
connect `ANT_BIAS` through a 27 nH RF inductor to the antenna feed near the SMA.
Use Murata `LQG15HN27NJ02D`, LCSC/EasyEDA `C115488`: 0402, 300 mA, 1.6 GHz
minimum self-resonance. The 22 Ω value follows u-blox's later short-circuit
requirement of at least 19 Ω at 3.3 V and keeps a hard coax short below roughly
150 mA; its wattage is deliberate because a cable short can be continuous.

Connect NEO pin 11 `RF_IN` directly to the same 50 Ω feed. At the SMA, shunt the
feed to the ground plane through TI `TPD1E01B04DPYRQ1`, LCSC/EasyEDA
`C3705129`, a 0.2 pF RF-capable ESD diode; its ground pad gets an immediate via.
Put the ESD diode first at the connector, and attach the bias-inductor branch
without creating a long RF stub. The 2010 resistor is not an RF-path component:
place it behind the 27 nH inductor and bypass capacitor, outside the straight
`RF_IN`-to-SMA routing corridor. This passive bias/current-limit network does
**not** measure open or short current and therefore does not create valid
`MON-HW antStatus` telemetry. Report antenna status as unknown on this revision;
a later active current-limiter/supervisor can close that feature explicitly.

### 7.3.1 Antenna supply headroom — regulated 3.3 V revision

**Status: design direction for a future PCB revision; component selection and
bench qualification remain open.** The current schematic, PCB, BOM and dated
exports retain the §7.3 circuit: NEO pin 9 → R18 (22 Ω) → `ANT_BIAS` → L2 → SMA.

**The compatibility gap is specific to the antenna load.** The
[NEO-M9N data sheet, Table 11](https://content.u-blox.com/sites/default/files/NEO-M9N-00B_DataSheet_UBX-19014285.pdf)
specifies `VCC_RF` as VCC − 0.1 V typical, without a minimum. With nominal
3.3 V VCC, the illustrative bias calculation is `3.2 V − 22 Ω × I`:

| Antenna | Operating supply | Published current | Calculated bias before choke/wiring losses |
|---|---|---|---|
| [Abracon AECP0401G4ZS-3000S](https://abracon.com/datasheets/AECP0401G4Z.pdf), L1 | 2.2–5.0 V | 5–15 mA | 2.87 V at maximum current |
| [ANN-MS](https://content.u-blox.com/sites/default/files/ANN-MS_DataSheet_(UBX-15025046).pdf), historical L1 reference | 2.7–5.5 V | 8.5 mA typical, ±4.5 mA | 3.01 V at typical current |
| [ANN-MB1](https://content.u-blox.com/sites/default/files/ANN-MB1_DataSheet_UBX-21005551.pdf), L1/L5 | 3.0–5.0 V | 15 mA typical at 5 V | 2.87 V at that current |
| [ANN-MB5](https://content.u-blox.com/sites/default/files/documents/ANN-MB5_Datasheet_UBX-22038811.pdf), L1/L5 | 3.0–5.0 V | 17 mA typical at 5 V | 2.83 V at that current |

These are nominal calculations, not measurements or worst-case guarantees.
The MB currents are characterized at 5 V; their actual draw at a lower bias
must be measured. Nevertheless, this circuit provides no supported voltage
budget for their 3.0 V minimum. Do not qualify either antenna from a position
fix alone. This finding does not establish that every L1/L5 antenna fails, or
require replacing existing M8/M9 assemblies whose chosen antenna meets the
loaded supply budget.

**Revised supply:** feed `ANT_BIAS` from the existing quiet `3V3_GNSS` rail
through an active current limiter with low on-resistance and fault shutoff.
Remove the R18 series drop and disconnect the bias branch from `VCC_RF`.

```text
3V3_GNSS → active current limiter → ANT_BIAS → L2 (27 nH) → ANT_RF_IN / SMA
                                      │
                                  C30 (100 nF)
                                      │
                                     GND
```

The limiter takes its input from the ADM7150 output and follows the existing
`GNSS_EN` power domain, so a rail cycle also resets the antenna supply. Keep
the RF trace, D2 shunt protection and L2 connection described in §7.3; place
the supply circuit behind the choke, outside the RF routing corridor.
The connector remains a regulated 3.3 V antenna supply. D2's
[±3.6 V working range](https://www.ti.com/lit/gpn/TPD1E01B04-Q1)
must also hold during startup and hot plugging. Current limiting is required
on the antenna branch; the 800 mA regulator's own protection is not a
substitute, since a coax short must not collapse the receiver rail.

**Reference circuit to qualify.** Use
[TPS2553DBVR-1](https://www.ti.com/product/TPS2553-1/part-details/TPS2553DBVR-1)
as the candidate. It provides active-high enable and latches off after a
sustained fault; cycling its input power resets it. See
[SLVS841F §§6, 7.5, 9.5 and 10.1](https://www.ti.com/lit/ds/symlink/tps2553.pdf).
For the DBV/SOT-23-6 pinout, connect IN (1) and
EN (3) to `3V3_GNSS`, GND (2) to ground, OUT (6) to `ANT_BIAS`, and ILIM (5)
through 226 kΩ, 1%, to ground. Table 2 gives a 101.3–142.1 mA limit including
resistor tolerance. Fit at least 100 nF locally at IN. Expose FAULT (4) on a
test pad with a 10 kΩ pull-up to `3V3_GNSS`; no new MCU GPIO is required for
protection or power-cycle recovery. The DBV on-resistance limit is 0.135 Ω.
Verify the supplier code and EasyEDA model before freezing the BOM; the
DBV package and latch-off suffix must match this reference circuit.

This fault indication does not measure antenna current or detect an open
cable. `MON-HW antStatus` remains unknown. Any later firmware fault reporting
needs its own GPIO assignment and must preserve the switched power domain.
After a short, remove the fault and cycle `GNSS_EN`; do not add automatic
unbounded retries against a persistent short.

**Voltage budget.** The
[ADM7150 data sheet, Table 1](https://www.analog.com/media/en/technical-documentation/data-sheets/ADM7150.pdf)
specifies ±2% over line, load and temperature under its stated operating
conditions. This gives 3.234–3.366 V for the 3.3 V part while in regulation;
the protected input must meet its 4.5 V minimum. The lower bound leaves
234 mV above a 3.0 V antenna minimum. At the 50 mA antenna design envelope,
the reference switch drops at most 6.75 mV. Using L2's published 0.67 Ω DC
resistance limit gives another 33.5 mV, leaving approximately **3.194 V
before PCB, connector and cable losses**. Account separately for choke
resistance over temperature and supply transients. The
[Murata part table](https://www.murata.com/-/media/webrenewal/tool/library/common-pdf/static-model/component-list-ind-s-2602.ashx?cvid=20260515010000000000&la=en-gb)
also rates L2 at 300 mA. These bounds make the topology worth implementing;
they do not qualify arbitrary antennas or cable extensions.

**PCB work and acceptance.** This requires a schematic and PCB revision,
not a resistor substitution or firmware setting:

1. Add the limiter, its programming resistor, local decoupling, fault pull-up
   and test pad. Replace the R18/VCC_RF branch with the protected output;
   never bridge `VCC_RF` to `3V3_GNSS` or bypass branch protection.
2. Update the editable project, declared netlist, placement/routing data and
   BOM together. Preserve the RF feed geometry and ground stitching. The ZED
   fork inherits R18 and needs the same antenna-load qualification before
   its supply circuit is finalized.
3. Qualify each intended antenna and cable at up to 50 mA steady load. Verify
   at least 3.0 V at the antenna's specified supply reference plane, with a
   design target of 3.05 V or higher, across supported input and temperature
   conditions and simultaneous board loads. Check startup/inrush, noise and
   reception, no-load/hot-plug overshoot, and actual antenna power-down.
4. Test a coax short both at power-on and during reception: bound the
   transient current and shared-rail droop, verify latch-off and temperature,
   and recover through a GNSS rail cycle. The DC current-limit number alone
   does not bound the initial short-circuit transient.
5. Run schematic/PCB checks and DRC, then generate a new dated fabrication
   export and hashes. Existing exports remain records of the passive circuit;
   they do not acquire this revision through a documentation change.

### 7.4 RF coexistence

An ESP32 radio at 2.4 GHz and USB's broadband noise both sit close enough to desense a 1575 MHz
front end, and a clear enclosure provides no shielding. Separate the GNSS RF path from the ESP32
antenna keep-out and USB routing and pour solid ground; the module already supplies its SAW/LNA.
Use the JLCPCB `JLC04161H-7628` 1.6 mm four-layer controlled-impedance
stack-up, with 1 oz outer and 0.5 oz inner copper. Route `RF_IN` to SMA on layer
1 as a 50 Ω grounded coplanar waveguide referenced to an uninterrupted layer-2
GND plane. Start the EasyEDA rule at **0.34 mm (13.5 mil) trace width** and
**0.15 mm (6 mil) clearance to the layer-1 GND pour**, then enter those choices
in JLCPCB's current impedance calculator at order time and use its returned
production width. Keep solder mask over the trace. Use no layer changes, no
routing under the CR123A holder, and no L2 splits, tracks or voids below it.
Fence both sides with GND vias at 1.0–1.5 mm pitch and place the first vias beside
the SMA ground legs. Place the NEO USB LDO/header below or left of the module,
never between `RF_IN` and SMA. This is where layout effort belongs—not the LED
array.

### 7.5 I²C

**One shared bus at 400 kHz:** `I2C_SDA` is GPIO6 and `I2C_SCL` is GPIO7,
matching `ESP32C6_PINOUT.md`. Fit one 2.2 kΩ pull-up from each line to
`3V3_SENS`; individual devices do not get additional pull-ups.
In EasyEDA Pro, place those names directly on the short wire at every pin; the
net label is the wire's Name property. Because these are ordinary pages under one
`Board1` / `Schematic1`, matching names form the shared project net without a
hierarchical net port. Before importing changes to the PCB, inspect the schematic
Net panel: each of `I2C_SDA` and `I2C_SCL` should contain nine endpoints—ESP32,
one pull-up, Qwiic, and the six slaves listed below.

Fit one board-edge **Qwiic-compatible service/expansion connector** on this bus:
HCTL `HC-1.0-4PWT`, LCSC/EasyEDA `C2845363`, a right-angle 4-position 1.0 mm
SH-series SMD header. Place the complete EasyEDA device by its LCSC number. Wire
the Qwiic pinout as pin 1 GND, pin 2 `3V3_SENS`, pin 3 `I2C_SDA`, pin 4
`I2C_SCL`; mark pin 1 and **3V3 ONLY** on silkscreen. Put a removable 0 Ω 0603
link between pin 2 and `3V3_SENS`. Normally fit the link so the observer can power
an external Qwiic sensor. Remove it before attaching a powered external I²C host,
then keep the observer USB-powered with `3V3_SENS` enabled, hold `ESP_EN` low so
the ESP32 is not a competing bus master, and use the host only for SDA/SCL/GND.
Never let an adapter drive the gated sensor rail while it is off;
that defeats rail power-cycling and can back-power the slaves. The connector gets
no additional pull-ups, and external modules with their own pull-ups must be
counted in the bus's combined pull-up resistance.

**Bench identity read — required on every board revision from the ZED/X20 onward.**
The manufacturer's unit record must not depend on what target firmware says its
identifiers are. At the bench, the factory CA unit reads the ATECC serial, both
EUI-64s and the slot-14 record directly over this connector while the ESP32 is held
in reset, and the result is compared with the firmware's own report; a disagreement
stops commissioning. That needs `ESP_EN` to be reachable by the same fixture that
plugs into the Qwiic connector, without a hand on the RESET button:

- Provide a round, unmasked **`ESP_EN` bench pad** of at least 1.0 mm diameter with a
  **GND pad** beside it on a 100 mil pitch, on the same face as the Qwiic connector
  and within 15 mm of it, both labelled on silkscreen. They may be the §7.7 recovery
  pads if those meet the size, pitch and position; otherwise add them. No component
  is fitted and no MCU pin is used.
- `SENS_EN` is default-on through its 100 kΩ pull-up, so the identity parts are
  powered from USB with the ESP32 in reset; the fixture supplies nothing.
- The 0 Ω link stays fitted for this procedure; removing and refitting it on
  every unit is not a production step. That is safe only because the factory CA
  treats an assembled board as **sink-only**: power the observer from USB first,
  and if the factory CA does not then see the rail on the connector it refuses
  the session instead of sourcing into `3V3_SENS` through the link. The rule
  above about removing the link still applies to any other powered host.
- The factory CA's 2.2 kΩ target-side pull-ups sit in parallel with this board's
  during the read, giving 1.1 kΩ per line — above the 967 Ω minimum for 3 mA sink
  at 3.3 V, and the only additional load permitted on the bus.
- Holding `ESP_EN` low through the pad must be open-drain from the fixture; it shares
  the node with the RESET button and the 1 µF capacitor.

Earlier NEO revisions have a RESET button and no bench pad. They are read the same
way with the button held, which is acceptable for the first articles and not for a
production run.

The three environmental sensors connect as follows; the ESP32 module-pad numbers
are included because they are not the same as GPIO numbers:

| Device | Device pin connections | Address / MCU connection |
|---|---|---|
| BMP388 | 1 `VDDIO` → `3V3_SENS`; 2 `SCK` → `I2C_SCL`; 3, 8, 9 `VSS` → GND; 4 `SDI` → `I2C_SDA`; 5 `SDO` → GND; 6 `CSB` → `3V3_SENS`; 7 `INT` → `BARO_INT_N`; 10 `VDD` → `3V3_SENS`. Fit separate 100 nF capacitors at pins 1 and 10. | `0x76`; `BARO_INT_N` → GPIO41, ESP module pad 34, with 10 kΩ to `3V3_SENS`. Configure open-drain, active-low. |
| MCP9808-E/MS | 1 `SDA` → `I2C_SDA`; 2 `SCL` → `I2C_SCL`; 3 `Alert` → `TEMP_ALERT_N`; 4 GND; 5/A2, 6/A1, 7/A0 → GND; 8 `VDD` → `3V3_SENS`, with 100 nF at the pin. | `0x18`; `TEMP_ALERT_N` → GPIO8, ESP module pad 12, with 10 kΩ to `3V3_SENS`. Configure active-low. |
| HDC2080DMBR | 1 `SDA` → `I2C_SDA`; 2 GND; 3 `ADDR` → GND; 4 `DRDY/INT` → `HUM_INT_N`; 5 `VDD` → `3V3_SENS`, with 100 nF at the pin; 6 `SCL` → `I2C_SCL`; exposed pad 7 soldered to an isolated **floating** land, not GND. | `0x40`; push-pull `HUM_INT_N` → GPIO39, ESP module pad 32, with **no pull-up**. Configure active-low. |

The **MCP79412T-I/SN RTC and its oscillator** are wired as one close-coupled
block:

| RTC pin | Connection |
|---:|---|
| 1 `X1` | One end of the 32.768 kHz crystal; one 10 pF C0G/NP0 0603 capacitor from this pin to GND. |
| 2 `X2` | Other end of the crystal; a separate 10 pF C0G/NP0 0603 capacitor from this pin to GND. |
| 3 `VBAT` | `RTC_VBAT`, supplied from `BACKUP_BAT` through its removable 0 Ω link; no added diode and no local bulk capacitor. |
| 4 `VSS` | GND. |
| 5 `SDA` | `I2C_SDA`; no device-local bus pull-up. |
| 6 `SCL` | `I2C_SCL`; no device-local bus pull-up. |
| 7 `MFP` | `RTC_MFP_N` → GPIO15, ESP module pad 8, with 10 kΩ to `3V3_SENS`; open-drain alarm output. |
| 8 `VCC` | `3V3_SENS`, with 100 nF X7R 0603 directly between pins 8 and 4. |

Use Seiko `SC-32S32.768kHz20PPM7pF`, LCSC/EasyEDA **`C97604`**:
32.768 kHz, ±20 ppm, 7 pF load, 70 kΩ maximum ESR, SMD3215-2P. The two
10 pF load parts may be KEMET `C0603C100J5GACAUTO`, LCSC **`C129620`**, or an
equivalent 10 pF ±5% C0G/NP0 0603. Use the board's common 100 nF X7R 0603
part for the VCC bypass (Samsung `CL10B104KB8NNNC`, LCSC **`C1591`**, is one
suitable choice).

Do **not** fit huaxindianzi `3K32.768XQ`, LCSC `C19723454`, in this RTC
position. Although it has the same SMD3215-2P package and an acceptable 70 kΩ
ESR, it specifies a 12.5 pF load; the MCP79412 data sheet says its oscillator is
optimized for 6–9 pF crystals and explicitly does not recommend 12.5 pF parts.
Capacitor changes do not make that a preferred first-spin combination.

Place the RTC, crystal and both 10 pF capacitors on the same side of the PCB.
Put the crystal immediately beside pins 1 and 2, with the capacitors beside the
crystal and very short, symmetric traces. Surround the oscillator block with a
ground guard returned directly to pin 4, but put **no copper, signal, or power
trace beneath the crystal on any layer**. The crystal manufacturer likewise
requires no PCB pattern under its body. Do not probe X1/X2 with an ordinary
oscilloscope probe; its capacitance can stop this low-power oscillator. For
bring-up, set the RTC `ST` bit to start the crystal, set `VBATEN` before testing
backup operation, and validate frequency at `MFP` in 32.768 kHz square-wave mode
before returning that pin to alarm service. The two 10 pF values are the
calculated first-spin load for a short layout including the RTC's typical 3 pF
pin capacitance; retain accessible 0603 pads so one value can be trimmed after
measuring the assembled board.

The complete fixed address map is **MCP9808 `0x18`**, **HDC2080 `0x40`**,
**manifest 24AA025E64 `0x50`** (A0/A1/A2 to GND), **MCP79412
EEPROM/EUI `0x57`**, **ATECC608C `0x60`**, **MCP79412 RTCC `0x6F`**, and
**BMP388 `0x76`** (SDO to GND).

Two constraints to resolve on paper *before* anything is locked: the manifest EEPROM and the
MCP79412 each expose a protected EUI block, and the **ATECC's address is set in
its config zone**, fixed permanently at lock time. Confirm every address against its datasheet
rather than assumed defaults — noting that the RTC's conventional `0x68` collides with the IMU
address shepherd uses, which is a non-issue here (no IMU on this board) but must not be
copy-pasted forward onto a variant that adds one.

The first-spin interrupt nets are also closed. MCP9808 `Alert` is the
open-drain `TEMP_ALERT_N` on GPIO8, and MCP79412 `MFP` is the open-drain
`RTC_MFP_N` on GPIO15; give each a 10 kΩ pull-up to `3V3_SENS`. Configure the
BMP388 interrupt as open-drain active-low and route `BARO_INT_N` to GPIO41 with
a 10 kΩ pull-up to `3V3_SENS`. HDC2080 `DRDY/INT` is push-pull; configure it
active-low as `HUM_INT_N` and route it to GPIO39 with no pull-up. GPIO39 and
GPIO41 are pad-JTAG pins, but this board deliberately uses native USB-JTAG and
does not expose pad JTAG. Both are input-only in this design, avoiding reset-time
output contention.

The manifest EEPROM's page-write hazard is **already handled** in the shared component —
`esp_hardware_discovery.c` conservatively chunks writes to 8-byte boundaries and ACK-polls
after each; that is safe on the 24AA025E64's 16-byte physical pages — so a
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
- **Direct shell bond for this enclosure.** `EH1`–`EH4` go directly to the board
  GND with short, wide copper and nearby stitching vias. There is no separate
  metal chassis in the plastic enclosure, so do not fit a lone 100 nF
  shield-to-ground capacitor.
- `A6` and `B6` join as `USB_DP_CONN`; `A7` and `B7` join as `USB_DM_CONN`.
  `A8`/`B8` (`SBU1`/`SBU2`) receive explicit no-connect flags.
- `ECLAMP8052P.TCT` (LCSC `C2662214`) protects the **data pair only**: pin 1
  `USB_DP_CONN`, pin 2 `USB_DM_CONN`, pin 4 GND, pin 5
  `USB_DM_FILTERED`, pin 6 `USB_DP_FILTERED`. The EasyEDA/LCSC device uses the
  physical package numbering `1, 2, 4, 5, 6`; do not reinterpret it as
  sequential pins 1 through 5. Pins 1/6 form the D+ path and pins 2/5 form the
  D- path. Continue through separate 22 Ω
  series resistors to `USB_DM`/GPIO19 and `USB_DP`/GPIO20 respectively. Reserve
  one DNP 0603 shunt-capacitor footprint on each MCU-side data net for EMC tuning;
  ship the first boards unpopulated.
- VBUS uses a separate unidirectional `SMF6.0A_R1_00001` TVS (LCSC `C391709`),
  cathode to `USB_VBUS_RAW` and anode to GND. Place it, 4.7 µF 16 V X7R and
  100 nF 16 V X7R at the connector. The 4.7 µF part is ceramic, not tantalum.
- `USB_VBUS_RAW` feeds `TPS259531DSGR` (LCSC `C2155674`): pins 3/4 `IN` and
  pin 2 `EN/UVLO` to raw VBUS; pins 8/EP to GND; pin 7 `ILM` through 1.78 kΩ
  1% to GND (about 1.17 A nominal limit); pin 1 `dVdt` through 47 nF to GND;
  pin 5 `OUT` becomes `+5V` with 22 µF 10 V X7R to GND. Pin 6 `FLT` is
  `EFUSE_FAULT_N`, pulled up to **`3V3_SYS`**, never `+5V`, through 10 kΩ and
  routed to GPIO9.
- No separate 1206 PTC is fitted: the TPS259531 supplies the resettable current
  limit, soft start, short-circuit and thermal protection without adding another
  series drop.

The product power contract is **5 V, 1.5 A minimum**, printed next to the USB-C
connector. The two passive Rd resistors do not report the source's advertised
current to firmware. A source that offers only USB default current may reset-loop
at simultaneous Wi-Fi and LED load and is explicitly unsupported; a USB-C source
that advertises at least 1.5 A is the intended supply.

### 7.7 ESP32-S3 module support and recovery

The module is `ESP32-S3-WROOM-1U-N16R8`, not a bare S3 and not the PCB-antenna
WROOM-1. Audit the EasyEDA symbol-to-footprint association before routing. Connect
module pins 1, 40 and exposed pad 41 to GND; pin 2 to `3V3_SYS`. Place 10 µF,
6.3 V or 10 V, X7R and 100 nF from pin 2 to GND immediately beside the module,
each with a short return into the ground plane. These are in addition to the
LDL1117's output capacitor: one stabilizes the regulator, the other supplies the
load at the point of use.

Use the standard manual recovery circuit:

- `ESP_EN` (module pin 3): 10 kΩ to `3V3_SYS`, 1 µF X7R to GND, and a normally
  open **RESET** button to GND. Keep this trace short.
- `ESP_BOOT` / GPIO0 (module pin 27): 10 kΩ to `3V3_SYS` and a normally open
  **BOOT/DOWNLOAD** button to GND. Do not place a large capacitor on GPIO0.
- Reserve GPIO46 as a strap/recovery input and fit 10 kΩ to GND so Joint Download
  Boot remains available. Avoid external reset-time pulls on the other strapping
  pins GPIO3 and GPIO45.
- For the N16R8 module, do not allocate GPIO35–GPIO37: they are consumed by its
  octal PSRAM interface.

Use the same tactile switch family already present in the recent Eagle boards for
both buttons: **C&K `PTS810SJG250SMTRLFS`**, 4.2 × 3.2 mm SMT, 2.5 mm high,
4.0 N nominal operating force, LCSC/EasyEDA `C221895`. This firmer `SJG` variant
is the price-selected first-spin part; it is rated for 100,000 operations and is
footprint-compatible with the softer `SJM` variant. Verify the four-pad footprint's
internally common pad pairs; connect one contact pair to the signal and the opposite
pair to GND.

Do not add a third Wi-Fi-reset switch or jumper. While the application is running,
holding **BOOT/DOWNLOAD** for eight seconds is the physical configuration-recovery
gesture. The TLC status panel must show an unmistakable confirmation pattern before
the action is armed; erase the `navfeeder` configuration namespace only after the
button is released, then reboot into the password-protected SoftAP provisioning
portal. This clears Wi-Fi, collector, station and bearer-token settings, but must not
erase the ATECC key, RTC or manifest identities, or enrollment material stored outside
that namespace. Do not define the gesture as “hold BOOT during reset”: GPIO0 low at
reset intentionally enters the ESP ROM downloader instead of the application. If the
application is too damaged to recognize the long press, use BOOT plus RESET to enter
the ROM downloader over native USB and erase only the configuration NVS; a complete
firmware reflash is not required for credential recovery.

Native USB is the primary downloader/debug interface, so this board does not need
a USB-to-UART bridge or its DTR/RTS transistor auto-reset circuit. Preserve UART0
as recovery and manufacturing access nevertheless. Use hanxia
`HX PH254-01-03-Z-L11.5` (LCSC/EasyEDA `C52016391`), a straight 1×3, 2.54 mm
through-hole male header, with pin 1 `GND`, pin 2 `U0TXD`/GPIO43, and pin 3
`U0RXD`/GPIO44. Label the board-side signal names clearly; the adapter connects
RX to `U0TXD` and TX to `U0RXD`. This header deliberately omits a power pin.
External TTL adapters must use 3.3 V logic and must not power the board. Retain
separate clearly marked `ESP_EN` and `ESP_BOOT` recovery pads.

The first-spin GPIO allocation is the schematic source of truth:

| GPIO | Net | Direction / note |
|---:|---|---|
| 0 | `ESP_BOOT` | Boot strap and DOWNLOAD button; 10 kΩ to `3V3_SYS`. |
| 1 | `BACKUP_BAT_SENSE` | Reserved ADC1 input; optional shared-cell measurement network remains DNP until off-state isolation is closed. |
| 2 | — | Spare; released by the move from two cells to one shared CR123A. |
| 3 | — | Strapping pin; do not connect. |
| 4 | `GNSS_UART_TX` | ESP TX to receiver RX through 330 Ω. Shepherd parity. |
| 5 | `GNSS_UART_RX` | Receiver TX to ESP RX through 330 Ω. Shepherd parity. |
| 6 | `I2C_SDA` | Shared 400 kHz bus; 2.2 kΩ to `3V3_SENS`. Shepherd parity. |
| 7 | `I2C_SCL` | Shared 400 kHz bus; 2.2 kΩ to `3V3_SENS`. Shepherd parity. |
| 8 | `TEMP_ALERT_N` | MCP9808 open-drain alert; 10 kΩ to `3V3_SENS`. |
| 9 | `EFUSE_FAULT_N` | TPS259531 open-drain fault; 10 kΩ to `3V3_SYS`. |
| 10 | `GNSS_PPS` | Buffered PPS input through 330 Ω. Shepherd parity. |
| 11 | `LED_SCLK` | TLC5916 shared shift clock. |
| 12 | `LED_LATCH` | TLC5916 shared latch-enable. |
| 13 | `LED_SDO` | Final TLC5916 serial/error readback (`U13` pin 14). |
| 14 | `LED_SDI` | TLC5916 chain data; preserves Shepherd's status-data pin. |
| 15 | `RTC_MFP_N` | MCP79412 open-drain alarm/clock output; 10 kΩ to `3V3_SENS`. |
| 16 | — | Spare. |
| 17 | — | Spare; the first-spin PPS LED is not software-blanked. |
| 18 | — | Spare. |
| 19 | `USB_DM_ESP` | Native USB D−. |
| 20 | `USB_DP_ESP` | Native USB D+. |
| 21 | `SENS_EN` | Default-on sensor-rail enable; 100 kΩ to `3V3_SYS`. |
| 35–37 | — | Unavailable: N16R8 octal PSRAM. |
| 38 | `GNSS_EN` | Default-on GNSS-rail enable; 100 kΩ to `3V3_SYS`. |
| 39 | `HUM_INT_N` | HDC2080 push-pull interrupt, configured active-low. |
| 40 | — | Spare; pad-JTAG group. |
| 41 | `BARO_INT_N` | BMP388 open-drain interrupt; 10 kΩ to `3V3_SENS`. |
| 42 | — | Spare; pad-JTAG group. |
| 43 | `U0TXD` | UART0 recovery/test pad. |
| 44 | `U0RXD` | UART0 recovery/test pad. |
| 45 | — | Strapping pin; do not connect. |
| 46 | `ESP_STRAP46` | 10 kΩ to GND; otherwise no functional load. |
| 47 | `LED_GREEN_OE_N` | Green TLC5916 output enable; 10 kΩ to `3V3_SYS`. |
| 48 | `LED_YELLOW_OE_N` | Yellow TLC5916 output enable; 10 kΩ to `3V3_SYS`. |

### 7.8 Four-layer PCB policy

Fit four **3.5 mm unplated M3 clearance holes** as EasyEDA Pro PCB primitives:
on `PCB1`, use **Place → Slot Region**, select a circular region, and set its
diameter to 3.5 mm. EasyEDA Pro emits circular slot regions up to 6.5 mm in the
NPTH drill file, so these holes need no schematic device or library footprint.
Lock them after setting their exact coordinates, and verify all four in the NPTH
Gerber/drill preview before ordering. Keep each center about 5 mm from its adjacent
board edges and reserve at least a 7 mm diameter component/copper-free washer
area. Do not connect the mounting hardware to GND; this revision has a plastic
enclosure and no defined chassis bond.

Convert the board before routing: in EasyEDA Pro open `PCB1`, choose **Tools →
Layer Manager**, add two copper layers, and make both positive-film **Signal**
layers. The first-spin stack and ownership are:

1. **Top:** components, USB differential pair, RF and other critical short routes.
2. **Inner 1:** an uninterrupted full-board GND copper region; no signal routing.
3. **Inner 2:** `+5V`, `3V3_SYS`, `3V3_GNSS`, and `3V3_SENS` regions plus only
   slow signals where necessary.
4. **Bottom:** remaining slow signals and a GND pour.

Shape the Inner 2 regions around source-to-load current paths, not as boxes around
functional blocks. The protected `+5V` region is a wide trunk from the eFuse output
to all three LDO inputs and the LED anodes. `3V3_SYS` is the largest 3.3 V region and
runs from the LDL1117 output/tab to the ESP32-S3, TLC5916s, and USB-side logic.
`3V3_SENS` is a modest region from the RT9193 output to the sensor/secure-element/
EEPROM/RTC cluster and I2C pull-ups. `3V3_GNSS` is a compact quiet region from the
ADM7150 output and output capacitor directly to the NEO `VCC`; keep it away from
LED and USB power paths and out from under the RF feed. Route `GNSS_VBCKP` and RTC
`VBAT` as ordinary clean traces, not plane regions.

Each Inner 2 region needs explicit vias at its regulator output and at every load's
local bypass-capacitor node. Use several vias at the LDL1117 output/tab and ESP32
supply entry, and one or two at each low-current GNSS or sensor entry. Put the
bypass capacitor's GND via directly beside its ground pad into Inner 1. Avoid narrow
necks, isolated slivers, or forcing load current through a capacitor pad in series.
Do not route a fast Bottom-layer signal across a boundary between Inner 2 regions;
USB and RF stay on Top over the continuous Inner 1 ground plane. Remaining Inner 2
area may become a low-priority GND fill after the power regions stabilize, but
Inner 1 remains the primary return plane.

After every schematic tranche is complete, use **PCB → Design → Import Changes
from Schematic**, including wire-net updates, and confirm that pads show real net
names rather than `None`. Ratlines are the routing work list; zero ratlines on an
unrouted board means the schematic netlist has not reached the PCB, not that the
board is finished.

Pour Inner 1 as GND over the whole board with islands disabled and refill with
`Shift+B`; add top/bottom GND pours and stitching vias after routing. Never split
Inner 1 beneath USB, clocks, RF, or any fast edge. Route USB D+/D− together on Top,
without vias, over Inner 1, and derive the 90 Ω differential geometry from the
fabricator's selected four-layer stackup rather than guessing width/spacing.
Likewise calculate the GNSS feed for 50 Ω. Keep the USB/eFuse/SYS-regulator area
physically away from the GNSS module and antenna path.

The implementation order is deliberate: close USB power, then all three
regulators, then the ESP support/recovery circuit, then every peripheral sheet,
then ERC and pin-allocation audit, and only then component placement, pours and
routing.

### 7.9 Flash tiers — this board is standard-tier by construction

Project convention (shared with shepherd; reference layouts are its
`esp32/partitions-4mb.csv` / `partitions-8mb.csv`): **standard** units have 8 MB+ flash and
carry an OTA-ready two-slot partition layout from day one — partition tables cannot change in
the field without serial access, but app code can, so OTA-capable layouts ship before any
updater exists. **Restricted** units are 4 MB, single factory slot, serial-reflash only —
bench/dev class (the non-touch Waveshare C6-LCD-1.47 dev units are this tier). Both tiers
share one app-size ceiling so a single binary serves a mixed fleet.

The §2 MCU choice already settles this board's tier: `ESP32-S3-WROOM-1U-N16R8` has 16 MB
flash — two OTA slots *and* the deep offline spool that motivated the module (§9.6) fit
without contention. The observer's concrete partition map is authored with the firmware, not
here; what this section fixes is only the tier: **an observer that ships is never
restricted-tier.**

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
| Manifest EEPROM | 24AA025E64 @ `0x50` | 24AA02E64 @ `0x50–0x57` | **deliberate electrical fix** — the Shepherd part collides with MCP79412 EEPROM/EUI `0x57` (§7.5) |
| Pressure | BMP390 | BMP388 placed, 390/580 alternates | inventory-led — §2, §6.1 |
| Status LEDs | WS2812B (+ `led_pps_sync.c`) | discrete green/yellow on 2 × TLC5916 | **deliberate divergence** — §2.1 |
| I²C | one shared bus, 400 kHz | same | aligned |
| GPS UART + PPS | UART1, PPS on its own GPIO, clear of GPIO9 | same | **adopted** — this is the fix (§7.1) |
| Board selection | compile-time profiles in `board/board_config.h` with feature flags | adopt for variants A/B/C | **adopt** — §1 variants are exactly this shape |
| Dev board | Waveshare ESP32-C6-LCD-1.47 | same | already aligned (`esp32/README.md`) |
| MCU | C6 and ESP32 (Xtensa) | ESP32-C6 development board and custom ESP32-S3 observer | **software support implemented** — see `esp32/README.md` |
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
   descriptor's reserved bytes (§5.3). It blocks a trustworthy manifest for same-model variants
   with different bands, but not the fixed NEO-M9N first-spin manifest.
2. **Manifest integrity checksum** — magic, bounds and the EEPROM self-reference reject obvious
   malformed images, but the current format has no CRC for bit corruption in otherwise plausible
   header or descriptor bytes. Reserve and specify a versioned CRC before production programming;
   the mutable runtime-status byte needs to be excluded or updated transactionally.
3. ~~EUI-64 text rendering~~ — **decided** (§4.2): lowercase hyphen-separated byte pairs, bare
   label. Remaining work is mechanical: a shared formatter/parser so the C feeder, the
   provisioning flow and the collector cannot disagree on it.
4. **Re-enrollment policy on RTC replacement** (§4.3) — accepted as an auditable event, but the
   operational runbook does not exist.
5. **`SIGNED_DATA` (0x07) granularity** — inherited open question from radiolistener's doc;
   per-batch is specified, per-observation is not ruled out.
6. ~~L1-only or L1/L5~~ — **dissolved** (§1.1). Both share the 24-pin NEO land pattern, so it is
   a populate-time choice, not a board decision. First spin stuffs **NEO-M9N-00B** on
   availability; **NEO-F10N-00B** drops in later without a respin. What remains open is narrower:
   confirm whether the M9N supports `RXM-RAWX`, since that determines whether the ionosphere work
   waits for an F10N.
7. ~~C6 or S3~~ — **decided: `ESP32-S3-WROOM-1U-N16R8`.** Shepherd's GPIO4/5 map retires the
   strapping-pin argument, so the S3 was chosen on three independent merits: its native USB OTG
   can enumerate as composite multi-CDC (MCU console *and* a transparent GNSS passthrough on one
   cable), which retires the USB-bridge question in firmware with no added silicon near the GNSS
   band; PSRAM and 16 MB of flash give the spool real depth, which matters when a collector can
   be offline for a week; and dual-core 240 MHz leaves headroom for TLS, zstd and a continuous
   460 800-baud UART that a single 160 MHz core does not. The **1U** variant is not optional —
   its u.FL lets the Wi-Fi radiator move physically away from the GNSS front end, which is the
   strongest available mitigation for §7.4 and impossible with a PCB-antenna module.
8. **ATECC config-zone bytes — authored, not silicon-validated.** The per-slot
   SlotConfig/KeyConfig words and writable scalars now live in shepherd's
   `atecc608c_config_profile.h` (a field overlay citing `ATECC508A` per table; the
   608-only ChipOptions/UseLock/SecureBoot fields follow the NDA-gated 608 datasheet and
   are flagged as such). What remains is the §4.2 step this document already required:
   write the profile to a scrap ATECC608C, exercise every slot class, then freeze it.
   Nothing locks before that pass.

9. **Antenna supply revision — regulated 3.3 V with active protection** (§7.3.1).
   ANN-MB1/ANN-MB5 qualification requires resolving the existing R18 voltage drop.
   Feed a protected bias branch from `3V3_GNSS`, preserving GNSS rail cycling.
   The reference limiter circuit, voltage budget and acceptance checks are in
   §7.3.1. Exact component selection, schematic/PCB implementation and bench
   qualification remain open. Existing M8/M9 assemblies whose L1 antenna meets
   the loaded supply budget do not require replacement for this issue; §7.3
   gives an antenna candidate for the current circuit.

10. **Microcontroller key — designed, not validated on silicon** (§4.1c). Before any
    production eFuse burn, exercise the whole path on a scrap ESP32-S3 module: on-device key
    generation and its duration, the key-block burn with read and write protection, a test
    signature through the Digital Signature peripheral verified off the device, a session
    proof accepted by a collector, restoration of the stored ciphertext after a flash erase,
    and a count of the eFuse key blocks that remain. The burn is irreversible, so this pass
    has the same standing as the ATECC scrap-part pass in item 8.
11. **Bench pad for `ESP_EN`** (§7.5). Required from the ZED/X20 revision onward; the NEO
    first articles are read with the RESET button held.
