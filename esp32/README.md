# navfeeder-esp

The **ESP32 edge feeder** for [`navlistener`](../): a self-contained GNSS observer that reads
raw broadcast nav frames off a local u-blox receiver and pushes them, undecoded, to the
collector over an authenticated, spooled TLS link (GNF1), with a constellation LED panel
and setup-network provisioning on the custom ESP32-S3 observer. It shares the C
feeder's role (`../feeder/navfeeder.c`); decoding and orbit math stay central in
the collector (`../docs/DESIGN.md §1`).

> This firmware sends GNF1 records to a navlistener collector. Configure its
> server address and credentials as described below.

For signed updates, persistent test-key setup and the S3 layout with 4 MiB
application slots, see [Update operations](docs/UPDATE-OPERATIONS.md).
Existing development boards move to layout 3 through USB flashing and fresh
provisioning. Ordinary app-only OTA updates cannot change their partition table.

## Hardware

The supported target is the custom **ESP32-S3-WROOM-1U-N16R8** observer with
**16 MiB flash and 8 MiB PSRAM**. Firmware development, release builds, OTA
capacity and hardware acceptance target this board. The earlier 4 MB ESP32-C6
LCD development board is unsupported; adding flash alone does not make it a
supported substitute. Its drivers and partition table remain as legacy source.

- **Custom GNSS color observer:** ESP32-S3, receiver UART on GPIO4/GPIO5, shared I2C on
  GPIO6/GPIO7, and a 24CS-family manifest EEPROM at `0x50`. One universal image runs on the NEO, the
  ZED/X20, the MAX and the mosaic-G5 P3. At boot it reads the manifest: its `CAT_INTSAT` entry names the
  board and revision, which fix the pins ([`main/board_reservations.h`](main/board_reservations.h)),
  and each driver runs only for a part the manifest lists installed: the receiver it
  configures, the RTC, the environmental sensors, the ATECC, the Ethernet port, the panel
  buttons and the MAX's sensors. The one exception is an accessory: a display module on a
  board's display header is looked for where the board's row names the header
  ([Status display](#status-display)). A board without a usable manifest naming a board this
  image knows runs nothing board-specific and logs that it may not have been configured.
  On the P3 the firmware speaks SBF to the mosaic-G5 on COM2, with RTS/CTS flow control and
  the GPIO13 reset for recovery, reads its blocks into the same receiver status the
  u-blox boards fill, and forwards them whole to the collector on the GNF1 `sbf` feed
  ([docs/SBF-RECEIVER.md](docs/SBF-RECEIVER.md)); it drives the board's
  shared services (panel, RTC, sensors, rail monitor and Wi-Fi uplink) as elsewhere.
  `NVF_BOARD_ASSEMBLY` also offers single-board test builds (NEO, ZED/X20, MAX or P3 only):
  they carry one board's drivers, run nothing board-specific on any other board, and
  install over the air only on their own board. Startup reads the 128-bit factory serial and manifest;
  `NVF_MANIFEST_FACTORY_INIT` is a manufacturing-only, default-off permission to initialize a
  blank, never-seen EEPROM from the selected board-and-revision component list; `NVF_MANIFEST_BOARD`
  chooses which board's list it writes. The lists are esp32-hardware-discovery's Intsat
  templates, tabulated in
  [the observer contract](../docs/HARDWARE-OBSERVER.md#52-how-navlistener-consumes-it). It never
  writes after an I2C error, to a known-but-blank EEPROM, or across an identity replacement.
  An absent, never-adopted EEPROM is supported during bring-up: the receiver streams,
  unconfigured, and the status panel and the provisioned station ID remain usable. Firmware does not synthesize an EUI
  or claim hardware attestation. A previously adopted EEPROM disappearing still
  reports an identity error; its history is retained.

## Build & flash

Install **ESP-IDF 5.5.x** using Espressif's installation instructions, then set
`IDF_PATH` to that checkout. Run its installer once for the ESP32-S3 target.
The wrapper uses ESP-IDF's own toolchain and Python environment selection;
set `IDF_TOOLS_PATH` if you installed the tools outside its default location.

From this directory:

```sh
export IDF_PATH=/path/to/esp-idf
"$IDF_PATH/install.sh" esp32s3             # once, after installing ESP-IDF
./build-navfeeder-esp.sh                  # build the universal firmware
NVF_BOARD=max ./build-navfeeder-esp.sh     # a test build: neo, zed-x20, max or mosaic-p3
PORT=/dev/ttyACM0 ./build-navfeeder-esp.sh flash
```

Use your board's actual serial device, such as `/dev/ttyACM0` on Linux or
`/dev/cu.usbmodem...` on macOS. Flashing is requested explicitly by `flash`.
The wrapper builds the universal image in `build/s3-layout3` and the test builds in
`build/s3-neo-layout3`, `build/s3-zed-x20-layout3`, `build/s3-max-layout3` and `build/s3-mosaic-p3-layout3`,
preserving each directory's generated
configuration, reporting default-setting differences, and recording firmware
provenance. Set `S3_BUILD_DIR` to select
another build directory. It does not erase flash; migration from an older layout
uses the separate erase/reflash procedure below. The development flash command
refuses builds that enable chip locking.

`NVF_MANIFEST_INIT=neo|zed-x20|zed-x20-b|max|mosaic-p3 NVF_MANIFEST_HDC=hdc2080|hdc2022
./build-navfeeder-esp.sh` builds the manufacturing image that writes a blank
manifest EEPROM: the universal image with `NVF_MANIFEST_FACTORY_INIT`, that board's
selected revision list and the lot's humidity sensor, in
`build/s3-manifest-init-<board>-<hdc>-layout3`. Both settings are required. It is
the first image of the [pre-enrollment QA](docs/COMMISSIONING.md#pre-enrollment-qa)
sequence, after which the board runs the ordinary image.

For direct ESP-IDF use, load the shared settings and S3 board profile:

```sh
export IDF_PATH=/path/to/esp-idf
"$IDF_PATH/install.sh" esp32s3             # once
. "$IDF_PATH/export.sh"
idf.py -B build/s3-layout3 -D SDKCONFIG=build/s3-layout3/sdkconfig \
  -D 'SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3' \
  -D IDF_TARGET=esp32s3 build
python tools/build_provenance.py --build-dir build/s3-layout3
idf.py -B build/s3-layout3 -p /dev/cu.usbmodemXXXX flash monitor
```

This creates a separate S3 configuration and build directory for the universal image;
append `;sdkconfig.defaults.neo` (or `.zed-x20`, `.max`, `.mosaic-p3`) for a test build. Existing generated
configurations retain their previous values: generating a separate build from
the defaults applies PSRAM and OTA settings. Check that the generated S3 config enables `SPIRAM_MODE_OCT`,
`NVF_SPOOL_PSRAM`, `NVF_OTA` and `BOOTLOADER_APP_ROLLBACK_ENABLE`, with
`PARTITION_TABLE_OFFSET=0x10000`, and leaves Bluetooth off. The build refuses a
configuration generated before Bluetooth was turned off; start such a build
directory afresh. Startup, including the Security 2 verifier for app setup, runs
on a 12288-byte main task stack (`ESP_MAIN_TASK_STACK_SIZE`); it peaked at about
6.4 KB on the bench, beyond the 3584-byte IDF default. Startup logs the minimum
free stack space before the main task returns and releases its stack.
The committed `dependencies.lock` resolves the S3, including its optional tunnel
components. Archive the resolved lock with firmware provenance. Firmware builds
share the managed-component directory and must run sequentially.

For an unlocked development S3 moving from an older partition layout, save its
connection settings, build into the fresh `build/s3-layout3` directory above,
then replace the flash/monitor command with:

```sh
idf.py -B build/s3-layout3 -p /dev/cu.usbmodemXXXX erase-flash flash monitor
```

This erases the previous firmware, credentials and journal. Provision it again
and replace its setup label. Later app updates use OTA normally.

The S3 board's console is the module's native USB-Serial/JTAG, so the USB-C
connector both flashes and monitors it and no separate UART adapter is needed.
Its ROM serial downloader is in mask ROM and cannot be missing from a blank
board; `esptool.py` enters and leaves it over USB without touching BOOT or RESET.

For an attended first flash of a factory-blank S3, use the dedicated target:

```sh
export IDF_PATH=/path/to/esp-idf
PORT=/dev/cu.usbmodemXXXX make first-boot-flash
```

It builds and flashes the S3, opens the serial monitor, and creates a unique
mode-0700 capture directory under `build/s3-layout3`, with mode-0600 files inside.
Watch for `NEW SETUP LABEL`, then exit the monitor with **Ctrl-]**. The target
extracts that one line and runs
`tools/provisioning_label.py` to display the device name, password, and canonical
QR payload. When `qrencode` is installed it also creates a private QR SVG. Print
and attach the QR plus text password before deployment. Each invocation uses a
new directory and cannot overwrite an earlier device's credential capture. The
target never erases NVS; development builds reprint an existing setup
credential whenever setup starts. Set `FIRST_BOOT_DIR=/new/private/path` to choose a new capture
directory explicitly.

## Finding the onboard GNSS receiver

On the custom PCB, receiver U1 pin 20 (TXD) reaches ESP GPIO5 through R27;
ESP GPIO4 reaches U1 pin 21 (RXD) through R26. No external UART wiring is
needed for the fitted receiver. The separate receiver USB interface is not
used by this firmware.

A factory-default NEO-M9N sends NMEA at **38400 baud, 8N1**. A fixed 460800-baud
listener counting only raw UBX messages cannot establish whether that receiver
is present. [u-blox integration manual, section 3.1.3](https://content.u-blox.com/sites/default/files/NEO-M9N_Integrationmanual_UBX-19014286.pdf)

The S3 default enables `NVF_RX_AUTOPROBE`: try the configured baud, 38400,
115200, 9600, 230400 and 460800, with MON-VER queries. Checksum-valid UBX or
NMEA locks the baud; 15 seconds without valid traffic restarts probing.
`NVF_RX_CONFIGURE` configures only the receiver the manifest lists, once a MON-VER
extension names it: `MOD=NEO-M9N` on the NEO, `MOD=ZED-X20P` on the ZED/X20 and
`MOD=MAX-M10S` on the MAX. With no receiver listed, none is configured. It switches to `NVF_RX_BAUD`, then requests UBX output,
SFRBX, MON-RF, NAV-SAT, NAV-PVT, NAV-STATUS, TIM-TP, NAV-SIG, NAV-CLOCK and NAV-EOE through
RAM-only CFG-VALSET, one setting per request so a receiver that rejects one still gets the
rest; a setting is one key, or keys the receiver accepts only together.
Firmware logs each key by its interface-description name with its ACK, NAK or bounded
timeout, then `receiver RAM configuration done: N of M settings acknowledged`, after the rate the
link runs at: `receiver answering at 460800 baud, set in its RAM from 38400`.
NAV-PVT, NAV-CLOCK and NAV-STATUS of one navigation epoch are joined into the
receiver-solution telemetry record (`0x03`, [contract](../docs/proposals/STATION-ASSURANCE.md#2-receiver-solution-telemetry-gnf1-type-0x03))
that feeds the collector's station integrity checks. NAV-EOE completes each epoch at
once; without it an epoch completes when the next begins. Either way the record carries
the arrival time of the epoch's first message. Each NAV-SAT becomes a reception record
(`0x01` body version 2, [`common/reception_data.h`](../common/reception_data.h)) with
every satellite's C/N₀, elevation, azimuth, pseudorange residual, quality indicator,
health and used flag. Upgrade the collector before this firmware: an older collector
discards version 2 reception and `0x03` records as malformed.
On the ZED-X20P it also sets `CFG-HW-ANT_CFG_SHORTDET`, so the receiver reads the
board's antenna-switch fault on `ANT_SHORT_N` (pin 6, default active-low polarity)
and MON-RF reports SHORT, which the collector raises as `antenna_fault`.
`CFG-HW-ANT_CFG_PWRDOWN` stays off because `ANT_OFF` is not connected. The switch
latches off and the receiver keeps reporting SHORT until `3V3_GNSS` is power-cycled,
which also resets the receiver; the firmware does not cycle it automatically.
The ZED-X20P's first setting after UBX output is the mode of its three internal LNAs, one
to an RF block: `CFG-HW-RF1_LNA_MODE_LOWGAIN` and its two siblings, in one message. In
normal gain, the receiver's default, it takes 17 to 40 dB of gain ahead of `RF_IN`, and in
low gain 20 to 50 dB, "to provide the integrated PGAs with a sufficient dynamic range"
(integration manual 4.3.1). The mode is a setting of the unit, low gain unless `rf lna
normal` has set it otherwise, and the firmware logs it before it speaks to the receiver:
`receiver settings: internal LNAs in low gain`.
[Which mode for which antenna, and what was measured](#the-zed-x20ps-lna-mode)
The modes apply only from a restart of the GNSS subsystem (2.2.3), so the firmware reads
the setting before it writes it, the receiver's default included, since a receiver that
kept its RAM may hold the other mode. A receiver that holds the mode already, as after a
restart of the ESP32 alone, logs `RAM key … (3 keys): already set` and keeps its fix.
Otherwise the write and its ACK are followed by `GNSS subsystem restarted (UBX-CFG-RST,
GNSS only)`: a controlled software reset of the GNSS subsystem alone that clears nothing in
the backup memory, so the UART rate and the rest of the RAM configuration stay and the
receiver starts hot, a second before the next key.
The ZED-X20P gets three more keys. `CFG-SIGNAL-QZSS_ENA` and `CFG-SIGNAL-NAVIC_ENA` come
next: every X20 signal plan leaves both systems off, with their bands
on beneath them (integration manual 2.2.4.1), so without them the panel's QZSS and NavIC
columns could never turn green, even inside their service regions. Each resets the GNSS
subsystem, so the next key waits 500 ms after its ACK, and both go before any status message
is enabled, where the reset would read as a spoofing or jamming transition. The
`UBX-SEC-SIG` rate comes last: from HPG 2.00 on, MON-RF's jamming flag always reads 0 and
SEC-SIG carries the jamming state, which then fills the receiver context's jamming field;
until SEC-SIG first reports a state, its unknown readings set no baseline. GPS L5 is left
as the receiver flags it: pre-operational, unhealthy and out of the fix (2.2.5).
The MAX-M10S gets its four GNSS as one setting, straight after UBX output for the same
reason. Its data sheet gives GPS, Galileo and BeiDou B1I as the default and the M10 SPG
5.10 defaults table leaves BeiDou off; GLONASS is off in both, so the MAX panel's BeiDou and
GLONASS columns could stay amber. All four together need BeiDou on B1C, since B1I cannot
run beside B1C or GLONASS L1OF (integration manual 2.1.3), and the receiver rejects an invalid
combination, so one message turns B1I off and B1C, GLONASS L1, GLONASS and BeiDou on: valid
from either default, and checked and applied whole. It is logged as one `RAM key` line
naming all five. QZSS and SBAS stay on, as by default. The NEO-M9N enables every system it
has by default and gets no signal setting.
Receiver flash and battery-backed configuration are not written. Any other model is
observed without automatic configuration. The shared keys have the same IDs in the
[M9 SPG 4.04](https://content.u-blox.com/sites/default/files/u-blox-M9-SPG-4.04_InterfaceDescription_UBX-21022436.pdf),
M10 SPG 5.10 (UBX-21035062) and
[X20 HPG 2.11](https://content.u-blox.com/sites/default/files/documents/u-blox-X20-HPG-2.11_InterfaceDescription_UBXDOC-304424225-21617.pdf)
interface descriptions. The ZED-X20P starts at 38400 baud and the MAX-M10S at 9600;
both support 460800.

Once the model is confirmed the firmware asks for `UBX-SEC-UNIQID` and logs the chip's
48-bit (X20, M10) or 40-bit (M9) ID once. Every 10 seconds, beside the `tracked:` line, it
logs `RF:` (the jamming state and its source, then each MON-RF block's antenna supervisor
and power, AGC as a share of maximum gain, noise and CW suppression) and `signals:` (the
code-locked signals per system and kind from NAV-SIG, pairs of halves counted once per
satellite and unhealthy ones noted, then the bands they cover). A change in any block's
antenna supervisor is logged when it happens, SHORT and OPEN as warnings.

`rf signals` on the USB console lists the signals behind those counts: every healthy signal
the receiver has code lock on, as one `NVF-RF-SIGNALS {json}` line whose `signals` array
holds `["G05","L1C/A",45,61]` for each, the satellite, the signal, its C/N0 in dB-Hz and the
satellite's elevation in degrees (`null` when the receiver has not given one). A data and
pilot pair is one entry, and `age_ms` is the age of the receiver's report. The same signals
read before and after a change of antenna, cable, place or LNA mode say what the change
did to reception. With no signal report in the last 15 seconds the answer is
`NVF-RF-SIGNALS-ERROR`.

On a ZED-X20P, enter `rf survey` on the USB console while ATTENTION is lit.
The receiver reports which GNSS center frequencies it assessed, the jamming flag at
each center, its recent security events, the active detector sensitivity, and a
256-bin spectrum for each RF path. The firmware also polls these diagnostics once a
minute and after receiver security transitions. Capture the console before cycling
power because the X20 clears its event history on restart. Each `RF evidence=` line
holds the same private record an enrolled observer sends to the collector. To decode
a saved console log into newline-delimited JSON, run `go run ./cmd/rfsurvey < console.log`
from the `go` directory. [Fields, units and interpretation](../docs/OBSERVER-TELEMETRY.md#x20-rf-evidence)

When a customer asks what caused a warning, compare surveys with the connected
antenna at its original position, at a second position and back at the original.
The field guide explains how to use a repeatable change in the spectrum, flagged
frequencies or reception to choose the next test. [Isolate an X20 jamming warning](../docs/OBSERVER-TELEMETRY.md#isolate-an-x20-jamming-warning)

The USB console reports `bytes`, checksum-valid `UBX`, checksum-valid `NMEA`,
and emitted `SFRBX` every ten seconds, including before WiFi provisioning.
Zero bytes calls for checking GNSS power and the UART path; bytes without valid
messages can indicate baud/framing trouble. NMEA or MON-VER proves communication,
not raw-navigation support or satellite reception. A rising SFRBX counter is
the evidence that the receiver is supplying raw frames.

The 2026-09-14 S3 bench check confirmed NEO-M9N / SPG 4.04 / protocol 32.01,
initial communication at 38400 baud and operation at 460800 after RAM
configuration. All five message/protocol settings returned ACK. Satellite
reception and actual SFRBX output still await the RF connector and antenna.
The ZED-X20P and MAX-M10S paths have not yet run on hardware; a rising SFRBX
counter on each is their acceptance test. The NAV-CLOCK and NAV-EOE keys were added
after that check and have not yet been acknowledged on any board; the receiver-solution
records reaching the collector are their acceptance test.

### The ZED-X20P's LNA mode

Each of the ZED-X20P's three RF paths has an internal LNA and, after it, a programmable
gain amplifier (PGA) that the receiver turns down as the level at `RF_IN` rises. UBX-MON-SPAN
gives each path's PGA in dB, in the `MON-SPAN:` lines of an [`rf survey`](../docs/OBSERVER-TELEMETRY.md#x20-rf-evidence),
and the AGC in the `RF:` line is MON-RF's count of the same gain as a share of its maximum.
The internal LNA has two modes. Normal gain, the receiver's default, takes 17 to 40 dB of
gain ahead of `RF_IN`, and low gain takes 20 to 50 dB (integration manual
UBXDOC-963802114-12901, 4.3.1; data sheet UBXDOC-963802114-12690, Table 15). Which one a
unit needs depends on its antenna, its cable and what the antenna hears, so the mode is a
setting of the unit, kept in the `nvf_rx` NVS namespace, which a network configuration
reset leaves alone. A unit that has never been set runs in low gain. The USB console reads
and sets it, and the display's SYSTEM page shows it:

| Command | Effect |
|---|---|
| `rf lna` | `NVF-RF-LNA {json}`: the mode the unit is set to (`low`, `normal`); whether the receiver holds it (`set`, `pending` while it is being applied, `unconfirmed` when the receiver neither held nor acknowledged it); each RF block's AGC in percent; the jamming state; and, from UBX-SEC-SIG, how many of the frequencies it assessed are flagged |
| `rf lna low` / `rf lna normal` | saves the mode and applies it at once: the receiver's settings are sent again and, if the mode changed, the GNSS subsystem restarts, which costs the fix for some seconds. `NVF-RF-LNA-OK <mode>`, or `NVF-RF-LNA-ERROR` with the reason |
| `rf signals` | `NVF-RF-SIGNALS {json}`: each healthy code-locked signal with its C/N0, for comparing reception in the two modes |

Choose the mode from the AGC the antenna gives where it will stay. An AGC of 7 % (a PGA of
3 dB, the lowest these receivers have reported) on every block in normal gain means the
antenna brings more than normal gain takes: set low gain. An antenna and cable that bring
less than 20 dB to `RF_IN` are outside low gain: set normal gain. An antenna in range of
both is a choice of margin. In the measurements below the receiver flagged a path's
frequencies only when that path's PGA reached 3 dB, so how far above 3 dB an antenna's PGA
settles in a quiet place is about the rise in level that the receiver's own detector, and
with it the ATTENTION lamp, waits for. A Beitian BT-800S survey antenna (an amplifier of
38 ± 2 dB) needs low gain, which leaves it 8 dB on two paths. A u-blox ANN-MB2 (23 dB at
the end of its 5 m cable) is in range in both modes with the same reception: normal gain
leaves it 8 to 10 dB and low gain 16 to 21 dB. A level that rose on a running receiver and
stopped short of the floor has not been tried.

On an indoor bench with a BT-800S, three ZED/X20 boards in normal gain reported a PGA of
3 dB on all three paths and an AGC of 7 %. UBX-SEC-SIG flagged all seven frequencies it
assessed as jammed and kept them flagged through a 3D fix, so the ATTENTION lamp was lit
for as long as the boards ran; on the board timed from a cold start, the receiver's event
log put the start 13 seconds after the observer's own. Two of the boards were then switched
between the modes, one of them thirteen times, six in a run that changed the mode every
150 seconds:

| Internal LNAs | PGA, paths 0 / 1 / 2 | AGC, blocks 0 / 1 / 2 | Frequencies flagged | Jamming state |
|---|---|---|---|---|
| normal gain | 3 / 3 / 3 dB | 7 / 7 / 7 % | 7 of 7 | warning |
| low gain | 11 / 11 / 3 to 11 dB | 25 / 25 / 7 to 25 % | 0 of 7, or 4 of 7 for a second at a time | OK, with moments of warning |

Every change of mode moved the flags with it, within about six seconds of the GNSS restart
that applies a mode; a mode written without that restart changed nothing in 35 seconds.
Reception was no worse in low gain: over the alternating run's 290 pairs of one signal in
two neighbouring phases, C/N0 averaged 0.4 dB higher in low gain (standard error 0.2 dB,
median difference 0), and the fix held in both modes. The data sheet gives the receiver
chain a higher noise figure in low gain (11 against 6.5 dB at L1, 10 against 6 dB at L2,
8.5 against 5 dB at L5), which 20 dB or more of gain ahead of it makes small.

In normal gain, then, the warning reported a gain control with no range left on any path.
How much of the level was the antenna's amplifier and how much was noise the antenna
picked up indoors was not separated, and neither mode has been tried under a clear sky.
The ANN-MB2 further down shows how much a place can add.

Low gain showed what normal gain had hidden. Paths 0 and 1 held 25 % throughout. Path 2,
which carries L2, E6 and B3, fell to its floor for about a second every 12 seconds: 20
times in a 240-second series of one reading a second, every interval 12 seconds, and again
with the display switched off. Five times in that series the receiver flagged that path's
four frequencies (1227.60, 1246.00, 1268.52 and 1278.75 MHz) for about a second, and no
others. Of 78 spectra of path 2, 51 had a narrow peak 6 to 20 dB above the passband between
1282 and 1283.5 MHz, strongest in the sweeps with the lowest PGA; path 1 had narrow peaks at
1170 and 1188 MHz in 17 of 78, and path 0 had none.
The period and the band fit a rotating L-band surveillance radar, and the path 1
frequencies fit aeronautical DME, but no source was identified. A warning that comes and
goes with such a period, on one path's frequencies, with the other paths' AGC steady, is
the receiver reporting a signal and not a gain setting.

An ANN-MB2 on one of those boards was read in both modes in two places indoors: lying
beside the bare board on the bench, and moved away from the bench.

| ANN-MB2 | Internal LNAs | PGA, paths 0 / 1 / 2 | AGC, blocks 0 / 1 / 2 | Frequencies flagged | Reception |
|---|---|---|---|---|---|
| beside the board | normal gain | 3 / 3 to 5 / 5 dB | 7 / 7 to 11 / 11 % | those of each path at 3 dB: 1575.42 and 1602.00 MHz, and 1176.45 MHz while path 1 read 3 dB | no signal code-locked |
| beside the board | low gain | 11 / 13 / 13 dB | 25 / 30 / 30 % | 0 of 7 | no signal code-locked |
| moved away | normal gain | 13 / 13 / 11 dB | 30 / 30 / 25 % | 0 of 7 | a fix |
| moved away | low gain | 24 / 22 / 19 dB | 54 / 50 / 43 % | 0 of 7 | a fix |

Beside the board the receiver locked onto no signal in the seven minutes after a cold
start, in either mode, and the level was higher than where the antenna was moved to, by
the same amount in both modes (from the spectra, with each sweep's PGA taken off): 11 dB
across the L1 path, from about 7 dB below 1560 MHz to 17 dB between 1590 and 1600 MHz and
12 dB at GPS L1, 7 dB across the L5 path and 6 dB across the L2 path. In normal gain that
left the L1 path at its floor, and the receiver flagged that path's two frequencies in 134
of 134 reports: a true warning, of an antenna that heard nothing else. In low gain the same
noise fitted inside the gain control's range and the receiver reported no jamming in 138
reports, with no satellite either. The AGC showed it, 25 / 30 / 30 % against 54 / 50 / 43 %
in the quieter place, and that departure is what a collector's AGC baseline measures
([DEFENSE-PNT.md](../docs/DEFENSE-PNT.md#2-jamming-detection-the-rf-front-end)).

Moved away, the receiver had a fix when it was next read and flagged nothing in either
mode through ten changes of mode. In a run that changed the mode every 120 seconds, C/N0
was the same in both: 0.25 dB higher in low gain on average over 194 pairs of one signal
in two neighbouring phases (standard error 0.23 dB, median difference 0). The signals were
weak, 13 to 39 dB-Hz, so this was not a clear sky, and the receiver was still filling in
after a cold start: the healthy signals it held through a phase rose from 21 to 44 over
the seven phases, with no step at a change of mode. The 12-second signal was there in both
modes as one step of path 2's PGA (3 dB) for a reading, 52 times, each 12 seconds or a
multiple of it after the last to the second. It never took that path to its floor and
never raised a flag.

When a ZED-X20P warns of jamming, read the AGC of every block first (`rf lna`, or the
`RF:` line): the floor on every block points at the gain ahead of the receiver before it
points at interference, while a warning with other blocks well above the floor is about
what the antenna hears
([Isolate an X20 jamming warning](../docs/OBSERVER-TELEMETRY.md#isolate-an-x20-jamming-warning)).
Neither the PGA nor a MON-SPAN spectrum includes the internal LNA's gain (interface
description UBXDOC-304424225-19967, 3.14.8), so a survey taken in normal gain is not
comparable in level with one taken in low gain: note the mode with a survey. A change of
mode also moved every block's AGC by 8 to 11 dB of PGA here, so for an enrolled observer
it is a change of the RF path like a new antenna or cable: give the station a new
`power_model_epoch` ([RECEPTION.md](../docs/RECEPTION.md)), which starts its AGC baseline
again.

### Custom-board status LEDs

The two TLC5916 drivers operate the green/yellow rows, including before WiFi
provisioning. Columns are SBAS, GPS, Galileo, BeiDou, GLONASS, QZSS, NavIC and
uplink, left to right: the four systems a receiver anywhere tracks sit together,
and the two regional systems sit beside NavIC. Green means a fresh NAV-SAT report contains a code-locked signal with
nonzero C/N0 and no unhealthy flag. Yellow means supported and expected, but
not tracked. Data older than 15 seconds cannot leave an indicator green.
Uplink is green while the collector connection is up, otherwise yellow.

Before it shows reception, the panel plays a boot sequence (`panel_boot.h`) in two
parts, at every boot.

First a lamp chase, to show that each lamp lights. A run of five lit lamps moves one
lamp every 150 ms along the top row from left to right, then along the bottom row: the
eight green lamps and, on a 24-bit panel, RTK at the end of that row; then the eight
amber lamps and ATTENTION. The run enters one lamp at a time and leaves past the last,
so every lamp is lit once, for 750 ms, and a lamp that does not light is a gap in the
run as it passes. No step lights every lamp. Two dark steps end the chase: 3.6 s in
all on a 24-bit panel and 3.3 s on a 16-bit one. PWR and PPS are wired to the supply
and the pulse, not to the drivers, so they are not in it.

Then the board identity is swept: one column per 100 ms, left to right, four passes,
green for each 1 bit and amber for each 0 bit of the manifest EEPROM serial's last 32
bits, its final eight hex digits, most significant first. For `…89abcdef` that is
`10001001 10101011 11001101 11101111`. The same board shows the same sweep at every
boot and a board whose identity was not read shows none, so the sweep is the panel's
sign that the I2C bus and the EEPROM answered.

Boots are counted in the `nvf_panel` NVS namespace, one commit per boot, and the
console names the count with the sequence, for example
`panel boot 12: lamp chase, identity sweep of serial ...89abcdef; 6800 ms`, so a board
that keeps restarting shows a count that keeps climbing. A count that cannot be read is
named `unknown`. A board without a usable manifest drives no panel and shows neither.

Without a valid, recent position, all supported constellations remain expected.
After NAV-PVT supplies a valid position, broad regional envelopes suppress
missing-signal warnings outside the usual QZSS, NavIC and SBAS service regions.
These are approximate display hints, not satellite visibility predictions or
navigation-integrity boundaries. They never disable receiver signals. SBAS is
the shared indicator for regional services, including WAAS, EGNOS, GAGAN, MSAS
and SouthPAN; it does not mean WAAS everywhere. See the
[QZSS service overview](https://qzss.go.jp/en/overview/services/sv04_pnt.html),
[regional augmentation overview](https://www.gps.gov/augmentation-systems),
[SouthPAN description](https://www.ga.gov.au/scientific-topics/positioning-navigation/positioning-australia/about-the-program/southpan)
and [NavIC coverage](https://www.isro.gov.in/IRNSS_Programme.html).

Actual tracking always turns the corresponding supported column green.
Regional reception is remembered with its location in the `nvf_panel` NVS
namespace. Within approximately 22 km of that location, a previously tracked
regional constellation stays expected across reboots: losing it turns yellow,
even if the coarse map would otherwise turn it off. New observations are
coalesced to at most one flash commit per minute; a sudden reset can lose
observations still waiting for that commit. Unsupported constellations stay
off. The fitted NEO-M9N reports no NavIC support.

### The ATTENTION lamp

A panel with the status driver (the 162mm panel, on the ZED/X20 and the P3) has a red
ATTENTION lamp. It is lit while any of these holds, and the console names them in one line
whenever the set changes, for example `attention: jamming warning; the panel's ATTENTION
lamp is lit`:

| Cause | From |
|---|---|
| spoofing indicated | the receiver: UBX-NAV-STATUS `spoofDetState` 2 or 3, or a Septentrio RFStatus authenticity flag |
| jamming critical | MON-RF critical, or a Septentrio interference indication with loss of fix |
| jamming warning | X20 SEC-SIG warning, MON-RF warning, or a Septentrio interference indication while a fix holds |
| antenna short, antenna open | an RF block's antenna supervisor in UBX-MON-RF |
| receiver silent | no navigation solution and no satellite list for 15 s, once the observer has run for a minute |
| reception alarm | the observer's own check against the collector's forecast ([Reception alarms](#reception-alarms)), whose lamps also alternate |
| power rail alert | the INA3221's Warning or Critical limit on a supply rail, for a minute after its last report |

For critical jamming and for spoofing the lamp flashes, 250 ms on and 250 ms off; for the
rest it is steady. The receiver's reports count while they are at most 15 s old, and each of
its causes is held for 10 s after its last report, so a moment of jamming is a lamp an eye
can catch. These are the receiver's own flags, shown as it gives them: a jamming warning is
the receiver's judgement of its RF input and does not say where the energy comes from. They
appear on the OLED ATTENTION page. For a current X20 warning the page includes the fix,
satellites used and flagged/assessed count. It cycles through up to 16 flagged center
frequencies in MHz. Read the full assessment over USB or in the collector. A panel without the status
driver has no such lamp, and trouble with the network is not among its causes: the uplink
column shows that.

One kind of jamming warning does not light the lamp: the beam of a rotating radar crossing
the antenna, once the observer knows the radar. A long-range L-band radar turns once in 10
to 12 seconds and transmits inside the band of GPS L2, GLONASS L2, BeiDou B3 and Galileo
E6, and a ZED-X20P in its sight flags that RF path's four frequencies for one report at
each crossing. An observer about 30 km from such a radar was flagged in 148 of 152 turns,
which with the ten-second hold kept its lamp lit 88 % of the time; the receiver's own
`CFG-SEC-JAMDET_SENSITIVITY_HI` at 0 still left 47 of 50 turns flagged. The firmware
therefore watches when the flags come and how long they last
([`jam_pulse.h`](components/ubx/include/jam_pulse.h)):

- **A source becomes known** when four warning-level crossings agree: each up for one
  report, or two when it falls across them; all on the same frequencies; whole turns of one
  period apart,
  two of the three intervals single turns; the period between 4 and 20 seconds, including
  as it adapts to later crossings. A turn may measure a quarter of a second outside that
  range, which is the timing of the reports and keeps a source at the range's edge; a turn
  a whole report outside it does not fit. The console
  says so once: `periodic source: warning flags for one report every 12.0 s on 1227.60
  1268.52 1278.75 1246.00 MHz, as from a rotating radar`.
- **Its warning-level crossings then do not count.** Flags that begin when a crossing is due (a quarter
  of the period either side, 1.5 seconds at most), on the source's frequencies, are neither
  a cause for ATTENTION nor an interference event for the reports and RF surveys that such
  events start. A suppressed warning starts counting again after 3.5 seconds even if the
  receiver sends no clearing RF report. A source may go unflagged for ten turns and still be due.
- **Everything else counts as before.** A MON-RF critical indication, interference with no
  fix, counts at once and never trains the tracker. Warning flags still up in a third report
  count from that report. Flags that come when no crossing is due, or name other frequencies,
  count at once.
- **A source is forgotten** after two crossings in a row that fit no turn, or after ten
  turns without one; `periodic source: gone`. A restart forgets it too, so the lamp lights
  for the first four crossings after one.

The receiver's state is not altered: the telemetry carries every flag as the receiver gave
it, and `RF:` and `rf lna` show it, the latter with `periodic_ms` while a source is known.
The display's GNSS page reads `PULSE 12s` in place of `JAM NONE`. What this gives up is
a jammer keyed for a second at a time on a radar's beat and on its frequencies: the lamp
would not show it, and the telemetry would. The tracker judges timing alone; it does not
know what the source is. It follows the u-blox receivers' flags, SEC-SIG's or MON-RF's; a
Septentrio receiver's interference indication counts as it comes.

### Status display

The ZED/X20 has a display header, H3, whose pins read GND, 3V3, SCL, SDA from pad 1: the
order of the common 0.96 inch 128×64 SSD1306 I2C modules. One plugged into it is a status
display at `0x3c` on the shared bus. The module is an accessory and is not in the manifest:
the board's row names the header ([`main/board_reservations.h`](main/board_reservations.h)),
the firmware looks for a module there, and a board without the header, or without a usable
manifest, sends that address nothing.

It is looked for every two seconds, so it can be fitted, unplugged or plugged in while the
board runs. When it answers it is configured with the display off, its memory is written,
and only then is the display turned on, so the noise its memory starts with is never shown.
The controller's status register says whether the display is on. It is read before every
frame: a module found off has been reset or plugged in again and is configured from the
start, and one that no longer answers is looked for again. The console says each
(`SSD1306 at 0x3c configured: display on, ...`).

Every page carries the UTC time, its own name and a mark while the ATTENTION lamp is lit;
the rule under that line is solid where the page stands among those being shown. Six lines
of 21 characters follow. The pages change every six seconds:

| Page | Shows |
|---|---|
| NOW | the date and a large UTC clock with its origin (GNSS, RTC, NET), the fix with its satellites and horizontal accuracy, the uplink's address, the collector link and the spool, and the temperature, humidity and pressure in the box |
| GNSS | the fix, satellites used over tracked, accuracy and PDOP; satellites per constellation in the LED panel's column order (S G E C R J I, a dash where the receiver does not track one); the antenna supervisor, jamming, spoofing and the pulse |
| SIGNAL | code-locked signals per band (L1, L2, L5, E5b, E6), the strongest C/N0, and a bar for every tracked satellite from 15 to 55 dB-Hz, grouped by constellation, with dotted lines at 30 and 40 |
| NETWORK | Ethernet and Wi-Fi addresses with the Wi-Fi signal strength, the collector and whether the link is up or in the tunnel, the spool and the records lost from it, and the station name's last 21 characters |
| SETUP | in place of NETWORK on a board not yet provisioned: the setup network's name, where its password is (the device label; the display never shows it), the setup address, and why the board is in setup |
| SENSORS | temperature, humidity, pressure, the RTC's temperature, each supply rail's voltage and current, and the pulse |
| SYSTEM | the board, the serial's last eight hex digits (the panel's identity sweep), the firmware, the uptime and boot count, the receiver with the [LNA mode](#the-zed-x20ps-lna-mode) the unit sets it to (`LNA LOW`, `LNA NORM`), free internal heap and the clock's origin |
| ATTENTION | what the ATTENTION lamp is lit for, one cause to a line; shown only while it is lit, and at once when a new cause joins |

A page with nothing to show is left out. The first four seconds show a start screen with
the board, its serial and the firmware version. While the configuration reset is armed
(BOOT held for eight seconds) the whole display says so, and how to cancel. A short press
of BOOT shows the next page and holds it for a minute; on a NEO, where that press steps the
panel brightness, it still does.

The clock shown is the time service's: the receiver's qualified time or the RTC's while
either is fresh, named as such, otherwise the system clock with the origin it was last set
from, which is blank for a clock that only ran on through a restart. Frames are timed to
its seconds. The display's contrast follows the panel brightness. To spare the panel, the
whole picture moves one pixel every minute through six positions, and every five minutes
the settings and the whole memory are written again, which also puts right anything a
disturbance on the bus changed.

The bus is shared with the sensors, the RTC, the EEPROM and the ATECC, at 100 kHz. The
display writes only the bytes that changed, at most 32 to a transfer (3 ms), and steps
aside for a tick after each, so no other part waits longer than that. It stays off the bus
altogether in two cases: while the panel chain is read back, since those reads hold the
lamps dark, and for the length of a session with the ATECC, which the display's zero bytes
would otherwise wake and whose commands they would fall between.

The USB console has its commands, each answered with `NVF-OLED-` lines:

| Command | Effect |
|---|---|
| `oled status` | `NVF-OLED-STATUS {json}`: whether a module answers and is on, its address, rotation, status register and contrast, the screen and page shown, and counts of configurations, losses, resets, frames, transfers and failures |
| `oled rotate 0` / `oled rotate 180` | turns the picture half a turn or back, at once, and saves it in the `nvf_oled` NVS namespace: for a module mounted the other way up |
| `oled off` / `oled on` | turns the panel and its charge pump off and keeps the display off the bus, or back on; until the next start, not saved. For comparing reception with and without the display at work |
| `oled dump [page]` | the frame as 64 rows of 128 `#` and `.` between `NVF-OLED-FRAME <name> 128x64` and `NVF-OLED-END`: what is shown now, or the named page (`now`, `gnss`, `signal`, `network`, `sensors`, `system`, `attention`) drawn from the latest data |

### Peripheral and random-number diagnostics

Startup probes the shared I2C bus and reads sensor identification registers.
The 2026-09-14 bench check found MCP9808 at `0x18`, HDC2080 at `0x40`, readable
MCP79412 RTC registers at `0x6f`, and pressure chip ID `0x50` at `0x76`.
That pressure ID is shared by BMP388/BMP384 and cannot identify the exact
variant. The initial RTC status reported a stopped oscillator and disabled
battery backup. Live factory-compensated sensor readings now supply
[ObserverDetails telemetry](../docs/OBSERVER-TELEMETRY.md): all three
individual temperatures, humidity, local absolute pressure and board diagnostics.
HDC2080 and HDC2022 have identical ID registers, so the manifest EEPROM says which
is fitted, and firmware uses that part's published temperature formula. The same
image runs on both assemblies. A board whose manifest is unusable, or lists neither
part, measures no humidity and logs that it may not have been configured; if an
HDC answers anyway with its heater left on, firmware only turns the heater off.
Manufacturing sets the part once: with `NVF_MANIFEST_FACTORY_INIT`, choose
**Humidity sensor fitted to the boards being initialized** for the batch, and it
is written into each new manifest. See [conversion details](../docs/OBSERVER-TELEMETRY.md#measurements-and-identities).

Sampling is every 30 seconds, with meaningful-change reporting, a five-minute
check-in, and fresh snapshots on receiver interference-state transitions.
The collector must support this report before the firmware is deployed.

Each of these parts runs only when the manifest lists it, and each report carries only
the listed parts' tags. The MAX board replaces the BMP388 with an MS5607 at `0x77` and adds an
MMC34160PJ magnetometer at `0x30`, both sampled with the other sensors, and an
ICM-45686 IMU at `0x69` whose FIFO a separate task drains on INT1 (GPIO16). The
IMU runs at 12.5 Hz while still and steps up while moving, to 50 Hz or 100 Hz by
motion profile. Its MAX31856 thermocouple converter has its own SPI bus (CS
GPIO13, SCK 40, MOSI 41, MISO 42, DRDY_N GPIO1) and converts continuously. The
mains frequency for its notch (60 or 50 Hz) and the motion profile (surface for
vehicles and vessels, aerial for aircraft and drones) are per-unit settings on the
browser setup page, so one image serves every unit; a network configuration
reset keeps them. Each environmental report carries the pressure, the thermocouple
and its faults, and a motion summary with the window's mean acceleration; see
[the MAX sensor tags](../docs/OBSERVER-TELEMETRY.md#max-board-sensors-tags-11-13-version-1).
A sensor that does not answer is retried; the others continue.
The IMU follows the manufacturer's FIFO errata: it reads the packet count twice,
retains the newest packet in stream mode, and waits for a flush to complete.
A failed transfer or unverified rate change causes reconfiguration before more
samples are accepted; four seconds without a valid sample also causes recovery.
The host tests simulate these faults, including a transfer that stops mid-packet.

The ZED/X20 replaces the BMP388 with a BMP581 at `0x46`, whose readings fill the same
pressure and temperature fields; tag 15 names the part the manifest lists (`PRESSURE_BMP581`,
or `PRESSURE_BMP580`, which shares its register map and chip ID, where that part is fitted). It also adds
an INA3221 rail monitor at `0x41` (`POWER_INA3221`) that measures +5V after the input eFuse,
`3V3_GNSS` and `3V3_SYS` through the board's shunts; each report carries the three
voltages and currents as tag 16, see
[the rail monitor tag](../docs/OBSERVER-TELEMETRY.md#zedx20-rail-monitor-tag-16-version-1).
Both parts sit on `3V3_SENS`; after a power cycle of that rail each is identified and
configured again at the next sample.

The P3 has the same INA3221 channels (its +5V limits are 1200/1400 mA, between the board's
1.18 A simulated maximum load and its eFuse's 1.446 A minimum limit) and a second MCP9808,
U47 at `0x1F`, on the LDL1117 system regulator's heat spreader. The LDO's loss follows the
`3V3_SYS` current, which Wi-Fi transmission dominates, so U47 drives a Wi-Fi throttle rather
than a temperature report: every 5 s its reading enters `regulator_thermal`, which lowers the
radio's maximum transmit power a step at a time (16.5, 13, 11, then 8.5 dBm, each a step of
`esp_wifi_set_max_tx_power()`'s table) from 95 °C at the sensor, a step every 30 s, or at
once to the lowest from 105 °C, and raises it a step every two minutes at or below 85 °C.
A failed read never relaxes the throttle. The radio's own limit, read before any cap, is
restored at level 0, and a cap a Wi-Fi restart resets is set again at the next sample. The
reading is not ambient temperature: it stays out of tag 1, raises no alarm, and goes out as
its own tag 23, see
[the regulator tag](../docs/OBSERVER-TELEMETRY.md#regulator-spreader-and-wi-fi-throttle-tag-23-version-1).
The thresholds sit in the P3's board row; log tag 23 under sustained Wi-Fi traffic in the
closed enclosure to confirm them. Both MCP9808s keep their Alert outputs disabled, so the
shared `TEMP_ALERT_N` line stays high and is not read.

The P3 also lists an SHT35 at `0x44` and an HIH8121 at `0x27` beside its HDC2080. Each is
sampled with the environment and reported as its own tag, 24 and 25 (`sht3x`, `hih8121`),
see [the humidity sensor tags](../docs/OBSERVER-TELEMETRY.md#humidity-sensors-beside-the-hdc-tags-24-and-25-version-1).
The SHT35 takes a single-shot, high-repeatability conversion with both words CRC-checked; the
HIH8121 wakes on an address-only write and is fetched 60 ms later, a result counting only when
its status bits read fresh. Neither has an identity register. Like the MCP9808's, their
readings continue through an HDC heater run; tag 10's state says when one was running.

The HDC heater stays off except for a rare condensation-recovery run: after four
continuous hours at 98 %RH or more, at most once per 14 days of trusted UTC. The
run's start time is saved in the `nvf_env` NVS namespace before the heater is
enabled, and HDC values are withheld while it heats and cools. The thresholds are
`menuconfig` options; see the
[recovery policy](../docs/OBSERVER-TELEMETRY.md#humidity-sensor-heater-condensation-recovery).

Both LED rows default to 20% brightness using 4 kHz PWM. On the NEO a short BOOT press
(0.1–3 seconds, then release) cycles 20% → 10% → 50% → 20% at runtime,
including before network provisioning while the setup portal is active. The ZED/X20
and MAX step the same presets with their GPIO18 buttons (either front-panel button on
the ZED/X20) where the manifest lists them (`BUTTON_USER_2`), and their GPIO2 trimmer sets an installation brightness from 1% at the
bottom of its travel to 100% at 2.5 V and above. Whichever control changed last wins:
a preset holds until the trimmer moves more than 3%, and turning the trimmer while
the unit is off takes effect at the next boot. Both controls are ignored for the first
second, while the wiper filter settles, and a reading at or above
`NVF_PANEL_TRIMMER_OPEN_MV` (3.0 V) is an open wiper that leaves the saved setting in
place. The board saves each applied brightness change, with the trimmer's position,
in NVS and restores it before enabling PWM on subsequent boots. The ZED/X20's front panel is its only chain: GPIO1 carries a 24-bit frame (status, amber,
green) whose status byte lights RTK while the receiver's RTK_STAT pin is low and ATTENTION
while something needs an operator ([The ATTENTION lamp](#the-attention-lamp)). The console names what the RTK lamp shows: the same
250 ms samples of RTK_STAT (GPIO40), classified over 10-second windows, give one line
whenever the state changes, beginning with the first window: `RTK_STAT high: no
carrier-phase solution`, `RTK_STAT low: RTK fixed`, or `RTK_STAT toggling, N changes in
10 s: corrections received and used, RTK not fixed`, after the receiver's own reading of
the pin (ZED-X20P integration manual, section 2.4.7). On that board GPIO14 reads the console port's VBUS and GPIO3
the MCP23008 expander's interrupt (see the next paragraph). The eight-second network configuration-reset
hold preserves this preference. The dedicated PPS and power LEDs have separate hardware
paths and are not dimmed. PWM pauses during TLC5916 serial/latch writes because
OE also participates in mode selection; see [TLC5916 section 9.4](https://www.ti.com/lit/ds/symlink/tlc5916.pdf).
Lower panel power can change nearby temperature readings.

Where the manifest lists the ZED/X20's MCP23008 expander (`IO_MCP23008` at `0x24`), the
firmware writes its register programme at boot, reads every register back, and retries at
each board-task pass until it does; only then is the GPIO3 falling-edge handler installed.
The expander collects five active-low alert lines behind that one interrupt: the W5500's,
the BMP581's, the HDC2080's, the MCP9808's and `PWR_ALERT_N`, the INA3221's Warning and
Critical outputs. Each change is counted with its time and logged at most once per line per
10 seconds. The INA3221 now has warning (averaged) and critical (per-conversion) current
limits per rail from the board row (`warn_ma` and `crit_ma`: +5V 1000/1300 mA, `3V3_GNSS`
350/500 mA, `3V3_SYS` 800/1000 mA, engineering values to be confirmed on the bench), set to
latch until read and set again whenever the monitor is reconfigured after a `3V3_SENS`
power cycle. A `PWR_ALERT_N` interrupt reads and logs which rail tripped which limit, and
that read releases the line. The P3 has no expander: its `PWR_ALERT_N` reaches GPIO41
directly, where a falling edge wakes the board task for the same read; each 500 ms pass
also reads the flags while the line stays low, so an alert latched before the handler was
installed is still seen. Once a minute, and once after the first frame, the panel
refresh task rewrites the current frame while the expander samples the chain's serial
output on GP0 before each clock: the first conclusive run adopts a 24-bit chain (the 162mm
panel) or a 16-bit one (the 3900 mil board's on-board chain), and later runs log any
mismatched bits. That write holds the LEDs dark for about 12 ms. The firmware never
switches `3V3_SENS` off today; any future `SENS_EN` control must call
`io_expander_suspend()` before the rail goes off and `io_expander_resume()` once it is back,
because GP2–GP5 fall to 0 V with the rail and the I2C pull-ups go with it, and must configure
the HDC2080 again: its configuration drives `HUM_INT_N` high, and a power cycle returns that
output to high impedance, leaving the board's 10 kΩ pull-up to hold the line. The expander's
status (configured, chain length, per-line counts and times, readback runs and mismatches,
and the last INA3221 alert flags) is logged at boot and hourly. It is not in ObserverDetails
telemetry yet: that needs a new tag in the Go decoder and its fixtures. See
[IO-EXPANDER.md](docs/IO-EXPANDER.md).

Include `build/LICENSE-BMP3-SensorAPI.txt` (emitted by the provenance tool)
with firmware binary distributions.

The RTC now waits for a qualified GNSS lock before its first initialization,
such as during a factory antenna test. There is no compile-time fallback.
Initialization requires at least three advancing NAV-PVT reports spanning two
seconds, a valid position and fully resolved UTC date/time, reported time
uncertainty at most 100 ms, and a report no more than two seconds old. Leap-second
samples and inconsistent time progression are deferred. A valid running RTC
keeps its existing calendar; subsequent GNSS reports do not continually reset it.

Initialization stops the oscillator before writing the calendar, enables
battery backup switching, and verifies the calendar, oscillator status and
advancing seconds. Failures leave time unconfirmed and require another qualified
GNSS sequence; stopping an unverified clock is attempted if the bus still works.
Writing the RTC weekday register clears its power-fail evidence in hardware.
When that flag is set, the raw calendar and power-down/up timestamps are first
saved as the latest event in NVS (`nvf_rtc/last_pfail`); a save failure defers
the RTC write. A retained running clock has each such record saved and cleared
as it is found, so that the part stamps the next loss. Backup switching is also
enabled for a retained running clock, which does not by itself prove a battery
is fitted. A recorded loss that the clock counted through for more than a minute
does, and is kept and reported as the backup's proof
([RTC service guide](docs/RTC-SERVICE.md#backup-proof)).
See the [MCP79412 datasheet, sections 5.3 and 5.7](https://ww1.microchip.com/downloads/aemDocuments/documents/MPD/ProductDocuments/DataSheets/MCP79410-MCP79411-MCP79412-Battery-Backed-I2C-RTCC-DS20002266.pdf)
and [NAV-PVT in the M9 interface description](https://content.u-blox.com/sites/default/files/u-blox-M9-SPG-4.04_InterfaceDescription_UBX-21022436.pdf).

The ZED/X20's DS3231M (and the MAX31328 of the superseded square list, which shares its
register map and driver) needs no backup enable: it switches to its cell by itself
and records any oscillator stop, including a failed switch when its supply falls too
fast, in its status register (OSF). Firmware treats a set flag as an invalid
calendar and waits for qualified GNSS UTC. It then writes the whole calendar in one
transfer, clears the flag, and verifies both the calendar and advancing seconds. If
the flag would not clear, it stops retrying and trusts only GNSS time. The part is not
accessed during its 300 ms power-up recovery (tREC). See the
[MAX31328 datasheet](https://www.analog.com/media/en/technical-documentation/data-sheets/max31328.pdf).

Both parts measure their own temperature to compensate the oscillator, and firmware reads
it with each environmental sample: the console prints `rtc: DS3231M temperature=24.25 C`
every 30 seconds and ObserverDetails carries it as tag 26 (`rtc_temperature`). It is a
0.25 °C reading, typically within 3 °C, of the RTC itself, which makes it a fourth
temperature beside the MCP9808, HDC2080 and BMP581 for comparing one board's sensors. A
power-on reset loads 0 °C until the first conversion about 2 s later, and sets the
oscillator-stop flag, so a zero is withheld while that flag is set unless the previous
reading lay within 0.5 °C of it.

This establishes a seconds-resolution RTC baseline. Qualified GNSS or a running
RTC can now bootstrap the system clock, with source and clock changes recorded;
this does not authenticate GNSS time or provide PPS alignment.
The 2026-09-14 S3 flash check confirmed that a stopped RTC waits
for GNSS lock while the NEO-M9N communicates but reports no tracked satellites.
Actual GNSS initialization and battery retention still need a bench check with
reception and a fitted battery.

Battery replacement while valid main power remains at the RTC normally preserves
its calendar and may leave no detectable event. Backup-enabled is not a battery
presence or health test. Firmware recovers a stopped/invalid clock from qualified
GNSS UTC. USB service commands can explicitly reset a running calendar or accept
a stated factory reference and record a maintenance reason. RTC replacement/service
does not change the board's commissioning identity.

Qualified GNSS/RTC UTC feeds the journal, board reports and updater metadata
decisions, and can bootstrap the system wall clock used by TLS, WireGuard,
observation reception stamps and edge forecasts. SNTP availability and independent
evidence retain separate provenance. Without usable UTC, local capture and uptime-based journal records continue;
time-dependent networking can remain unavailable. See the
[RTC service guide](docs/RTC-SERVICE.md) for USB commands, uncertainty and replay
rules, and remaining physical retention and service tests.

The ATECC check uses a wake response, CRC-verified Info revision, configuration
lock-byte reads and three Random requests. It rejects fixed/short-period output
(including alternating `ff00`) and identical consecutive samples. It reports
lock state separately; passing this small diagnostic does not certify entropy,
key provisioning or signing readiness. Samples are discarded and never feed
session IDs or OTA challenges. No keys are generated and no configuration/data
zones are written or locked. The fitted ATECC608C returned revision `00006005`,
unlocked configuration/data (`55/55`), and repeating RNG output that failed
screening. See [Microchip's revision identification](https://onlinedocs.microchip.com/oxy/GUID-EC688158-B182-4FB8-9DA1-4B9EFB5BDCE1-en-US-1/GUID-48DB0960-DAD9-436F-98A9-DD2788AEAE9C.html),
[HDC2080 register reference](https://www.ti.com/lit/ds/symlink/hdc2080.pdf),
[BMP384 register reference](https://www.bosch-sensortec.com/media/boschsensortec/downloads/datasheets/bst-bmp384-ds003.pdf),
[BMP581 register reference](https://www.bosch-sensortec.com/media/boschsensortec/downloads/datasheets/bst-bmp581-ds004.pdf)
and [INA3221 register reference](https://www.ti.com/lit/ds/symlink/ina3221.pdf).

## What each phase does

See the [implementation status and roadmap](docs/PLAN.md). The P0–P5 core is built:
the firmware boots, brings up the constellation LED panel, runs the clean-room
UBX framer, spools frames, and pushes them to the collector over GNF1/TLS. Config is NVS-first (`netcfg`), falling back to the compiled Kconfig defaults.

**Provisioning a fresh board (no serial console):** the custom S3 raises the
password-protected AP `navfeeder-XXYYZZ`. Scan the label's Wi-Fi QR in the
Station app, which joins that network and sends WiFi, the collector, the station
and the enrollment token over ESP-IDF Security 2 provisioning. Or join the
network with a phone camera or the password printed on the label, then open
`http://192.168.4.1/` and submit the same settings in the browser form. Either
path writes one validated NVS record and reboots into station mode. Bluetooth
is off. The firmware/Swift/label contract is [documented here](docs/PROVISIONING.md). (For
development, configuration can still be pre-seeded through `idf.py menuconfig`
→ "navfeeder-esp".)

**Configuration-reset recovery:** on the custom ESP32-S3 observer, boot the application
normally, then hold **BOOT/DOWNLOAD for eight seconds**. Once the status panel shows the
armed pattern, release the button. Firmware erases only the `navfeeder`
configuration namespace, reboots, and returns to browser setup with
the **same** label password. On the NEO, short presses continue to cycle panel
brightness. Merely reaching the hold threshold does not erase anything until a
debounced release.

Do not hold BOOT while resetting for this gesture: that enters the ROM downloader instead.
If application firmware cannot run on an unlocked S3, connect over USB and erase
the NVS partition, then reset. For S3 layout 3:

```sh
esptool.py --chip esp32s3 --port /dev/cu.usbmodemXXXX erase-region 0x11000 0x6000
```

Use the partition table actually installed on the board: older S3 layouts used
`0x9000`. This whole-NVS erase also destroys the setup
credential, update key, and hardware identity history. On the next boot firmware
creates a different setup password and emits its QR payload on the serial
console; the old physical label must be replaced before deployment. Use the
actual serial device path for the board. This is a configuration erase, not a
firmware reflash.

**Incomplete configuration also starts setup**. "Provisioned" means WiFi SSID (not required on
the ZED/X20, which has an Ethernet port), collector host, port in 1–65535, station id, and
bearer token are all present — one rule
(`netcfg_validate`), applied both at boot and by the portal before it writes NVS. A board
configured only partly (Kconfig defaults, a partial NVS write, external NVS tooling) therefore
comes up in setup with the missing field named in diagnostics where
available, instead of looping forever on WiFi/TLS/auth failures that only a
serial cable could diagnose. Note the deliberate limit: a
*complete but wrong* config (bad password, unreachable host, revoked token) keeps retrying in
station mode — a unit riding out a collector outage must not drop its uplink over a condition
that is not its fault.

**Ethernet uplink (ZED/X20).** On a ZED/X20 whose manifest lists it (`COMM_W5500`), firmware drives the on-board W5500 on its own SPI
bus (CS GPIO13, SCLK GPIO8, MOSI GPIO39, MISO GPIO41) at 10 MHz, polling it every 10 ms,
and takes an address by DHCP with the MAC address the eFuse reserves for Ethernet. With both
links up, Ethernet carries the default route; connections already open stay on the link they
started on, and the WireGuard tunnel moves to the new default route. The observer is online
while either link has an address. Leave the browser form's SSID empty for a wired-only unit.
The Wi-Fi station starts even without a network, because
the radio is the random-number generator's entropy source for TLS. With no network it does
not scan or associate. A W5500 that does not answer is logged, and the unit carries on over
Wi-Fi if one is configured. At every boot, setup mode included, the firmware first reads the
W5500's VERSIONR, which must be `0x04`, and logs `W5500 verified` or the unexpected identity,
then frees the bus: an unprovisioned board's port is tested although its uplink never starts.

## Network diagnostics

ESP32 observers report passive link and collector health in their private board
metadata: Wi-Fi RSSI, channel, AP identity and disconnect reasons; Ethernet
carrier, negotiated speed/duplex and autonegotiation; addresses and routing;
connection failures, ACK stalls and queue pressure. A separate network card in
Integrity Station exposes the latest report, events, history and JSON export.

Stable reports run every five minutes, with changes coalesced to at most once a
minute. A bounded persistent journal keeps summaries through reboots. Diagnostics
defer under spool pressure and never evict navigation records. Upgrade the
collector before the firmware. See the [troubleshooting guide](../docs/NETWORK-DIAGNOSTICS.md)
for interpretation, retrieval and the limits of the available counters.

## On-demand OTA updates (ESP32-S3)

`NVF_OTA` enables a laptop-initiated HTTPS download into the inactive OTA slot.
Install this baseline **over USB once**, including its rollback-capable bootloader.
Reserved slots alone do not give older firmware an updater. Keep the factory
application as the recovery image.

The signed-update development baseline adds release channels, automatic checks,
staging, fleet controls and production security profiles. Its remaining acceptance
requirements and implementation contract are in [Software updates for the ESP32-S3 observer](docs/SOFTWARE-UPDATES.md).

### Pair an update credential

Join the board's password-protected provisioning AP. Before saving its WiFi
configuration, pair a separate random key using the helper from this directory:

```sh
python3 tools/ota.py --device 192.168.4.1 pair \
  --key-file /path/to/private/observer-ota.key
```

The helper creates a private key file when absent; keep one per observer.
Pairing is available only on the setup AP. The key is stored in the independent
`nvf_ota` NVS namespace. The collector bearer token cannot authorize firmware
updates. Complete the normal provisioning form to start station mode.

For an already provisioned board, the physical configuration-reset gesture
reopens the AP; network settings must then be entered again. Configuration reset
preserves an existing update key. Pairing through that AP can replace a lost key.
Erasing the configuration NVS partition also erases the update key and hardware
identity history. The separate diagnostic journal survives that operation.

### Request an update

Serve the application binary (not a merged flash image) at a direct HTTPS URL
with a trusted certificate, `200 OK` and a fixed Content-Length. Redirects and
chunked downloads are rejected. Keep the matching binary locally:

```sh
python3 tools/ota.py --device observer.example.invalid update \
  --key-file /path/to/private/observer-ota.key \
  --image build/s3-layout3/navfeeder-esp.bin \
  --url https://firmware.example.invalid/navfeeder-esp.bin
python3 tools/ota.py --device observer.example.invalid status
```

The helper authenticates the exact URL and local binary's SHA-256 using
HMAC-SHA256 and a single-use, 60-second challenge. Port 80 carries the control
request; the update key is never sent in station mode. Status is a local-network
diagnostic response. Firmware bytes arrive over HTTPS with certificate chain,
hostname and date validation. Station-mode SNTP uses `pool.ntp.org`; the OTA
worker waits up to 30 seconds for a plausible clock and refuses an update if
it remains unset. Initial collector TLS connections may retry while time sync
completes.

Before writing, firmware checks the ESP32-S3 target, project, board marker (the universal
image, or a test image for the board this device's manifest names),
rollback support, manufacturing-write flag and available slot size. It checks
the complete SHA-256 and ESP-IDF image validation before changing boot selection.
A reset during a trial boot rolls back unless the new application confirms its
local initialization and receiver-task heartbeat. EEPROM presence, a GPS fix,
WiFi association and collector availability are not boot-confirmation requirements.
A failed metadata commit can leave either boot selection; any newly selected
image has already passed validation. [ESP-IDF OTA and rollback](https://docs.espressif.com/projects/esp-idf/en/v5.5.4/esp32s3/api-reference/system/ota.html)

An accepted request reports **accepted**, then `status` shows downloading,
failed, or rebooting. After reboot, inspect the running version and partition.
The updater gives the collector up to five seconds to acknowledge the backlog
before reboot; every remaining RAM/PSRAM record is lost at reboot. No flash spool
is implemented. USB recovery remains available if no application can run.

Host tests cover request/image rejection, fragmented/truncated downloads, wrong
digests, flash failures and simulated interruption boundaries. Radio/TLS behavior,
real power cuts, PSRAM behavior under flash writes and bootloader rollback still
need hardware validation; the host facades do not model those physical effects.

## Reception alarms

The observer accepts a five-minute forecast from its authenticated collector,
compares expected satellites or signal groups with fresh NAV-SAT/NAV-SIG tracking,
and raises its own alarm after the configured dwell. An affected constellation
alternates green and yellow on a 500 ms full cycle; the uplink column retains its
connectivity indication. Unknown coverage cannot clear an alarm. A restored
journal alarm remains visible after reboot until fresh evidence confirms recovery.
For stationary GPS profiles, reception protocol v2 also learns a local sidereal
C/N₀ history and compares each fresh NAV-SAT value with that history and the
collector's delivered history. Only joint local/remote agreement advances the
received-power alarm. The collector can also request a fresh
receiver/RF/environment snapshot.
See [station configuration, wire contract and limitations](../docs/RECEPTION.md).

## Boot, health, reception and network journal (ESP32-S3)

The S3 keeps four persistent FIFO histories in a dedicated 512 KiB NVS partition:
256 lifecycle events, 1,024 hourly health checkpoints and 128 combined availability
and received-power transitions, plus 128 network summaries. The same partition holds the alternating,
CRC-protected local power-model checkpoints. New records replace the oldest
automatically; logging errors disable logging while GNSS collection continues.
Boot records include firmware version/ELF hash, reset reason and uptime. Qualified
GNSS or running-RTC time anchors preserve clock provenance.
See [installation, retention and laptop readout](docs/JOURNAL.md). Installing the
new partition table over USB once enables the journal on earlier S3 layouts.

## WireGuard tunnel to the collector (ESP32-S3)

`NVF_WIREGUARD` (default on for the S3 observer) accepts an optional WireGuard
profile at provisioning and carries the GNF1/TLS push session inside that tunnel
to the collector whenever the peer session is up, falling back to the public
collector endpoint otherwise. A unit with no profile, or whose tunnel is down, uses
the public endpoint. A private collector connection failure also selects the
public endpoint for at least 60 seconds before retrying the tunnel. GNF1/TLS
is unchanged inside the tunnel, so the collector still authenticates the bearer token and the observer still
verifies the collector's certificate against its configured hostname.

Provision a profile through the Station app's `nav-tunnel` endpoint or by pasting
a wg-quick `.conf` into the browser form; the profile format and how to generate
one are in [PROVISIONING.md](docs/PROVISIONING.md#wireguard-profile).
Notable properties:

- **IPv4 only, one collector.** `AllowedIPs` names exactly the collector's tunnel
  address as a `/32`; that single host is the only destination routed through the
  tunnel. The interface itself uses a `/32` mask even when the profile gives
  its address a broader prefix, so other hosts in that subnet keep the ordinary
  uplink. SNTP, OTA downloads and DNS keep the ordinary uplink.
- **Time first.** A WireGuard handshake carries a timestamp the peer rejects if
  it is not newer than the last from that key, so the first handshake waits for a
  plausible wall clock (SNTP, qualified GNSS or retained RTC continuity). A reboot
  therefore reconnects only once the clock is set, the same bar the collector's
  TLS certificate check already imposes.
- **Reconnect and preference.** The tunnel task waits for Wi-Fi, brings the peer
  up with backoff, and re-resolves the endpoint if no handshake completes for 90 s.
  The pusher moves a live public session onto the tunnel once the peer is up and
  nothing sent is still awaiting its ACK, replaying from the durable watermark as
  on any reconnect. The dashboard heartbeat logs `link=up/tunnel` or `up/direct`
  and a separate `tunnel=up/down`.
- **Keys.** WireGuard uses Curve25519, so the tunnel key is a software secret in
  NVS, separate from the planned ATECC-backed P-256 identity. Treat the profile
  as a secret: it contains the observer's private key.

The port is the BSD-3-Clause `esphome/wireguard` component (the lwIP WireGuard
implementation via `trombik/esp_wireguard`), pinned in the `tunnel` component's
`idf_component.yml` for the supported `esp32s3` target.

## GNSS PPS and RTC timing (ESP32-S3)


Hardware capture measures the GNSS PPS input and the RTC's 1 Hz square wave,
including independent pulse totals, interval, width and relative phase/drift.
Timing reports run once per second; environmental sampling keeps its slower
cadence. Hourly journal checkpoints retain pulse totals within each boot.
See [TIMING.md](docs/TIMING.md) for interpretation and serial-log CSV/chart export.

## Durability envelope (read before deploying one as a primary observer)

**The spool is RAM-only and non-durable across reboots.** The S3 partition table
reserves 3.25 MiB for a future flash tier, and `docs/PLAN.md` records its design,
but no component mounts or writes that partition. Plan for these limits:

- **Outage depth has both byte and record limits.** The internal-RAM fallback
  holds up to 1024 records in a 64 KiB payload arena, plus metadata. With PSRAM
  available, the S3 uses a 4 MiB payload arena and up to 65536 records, plus 1 MiB
  of metadata. Both arenas are explicitly allocated in PSRAM; failure falls back
  to the internal budget and is logged. Reaching either limit drops and counts
  the oldest unacked records. At 10 records/s and at most 64 bytes per record on
  average, 65536 records is about 109 minutes; higher rates or larger telemetry
  shorten this estimate. Watch the actual depth and drop counters.
- **Any reboot loses every unacked record**, however brief the outage — records live in
  volatile RAM, so `esp_restart()`, a brownout, a watchdog reset, or pulling USB all discard
  them. This is the part with no workaround: it is not a "long outage" failure mode.
- **The C feeder is unaffected**: `../feeder/navfeeder.c --spool-file` has the disk tier, so
  the deployed router/SBC fleet keeps its outage durability. This limitation is specific to
  navfeeder-esp.

**Boot sessions bound the loss.** The feeder must mint a fresh GNF1 session
on each boot. The collector deduplicates on `(observer, session, seq)`, so a
new session prevents the restarted sequence counter from colliding with
previously received frames. Persisting a session across boots would break
that guarantee. See `main.c`'s `session_init()`.

A flash-backed spool is a planned feature; the current ESP32-S3 build also
uses the RAM ring. Hardware support alone does not provide reboot durability.

Deployment rule: **a navfeeder-esp unit is loss-tolerant-only by explicit design.** Good as an
additional observer in a fleet where another station covers the same sky; not the sole witness
of an event you need forensically complete. Watch the `dropped` counter on the dashboard — a
non-zero value means records were lost, not merely delayed.

## Enrollment (the shared AAA control plane)

Authorize the observer for the `ubx` feed using the collector's configured
credentials or database authorization provider ([DESIGN.md §3](../docs/DESIGN.md#3-node-identity--the-hardware-observer)).
Current ESP32 firmware authenticates with a **bearer token** stored in NVS through
the provisioning portal. The collector also supports mTLS credentials, but the
ESP32 ATECC-backed credential and enrollment path remains P-hw work. An optional
WireGuard tunnel can wrap the uplink for a station on request; see
[the tunnel section](#wireguard-tunnel-to-the-collector-esp32-s3).

## Commissioning and hardware evidence (ESP32-S3)

Authorization says whose data a session carries. Separately, a commissioned board
presents a manufacturer-signed record on every connection and, when it was locked,
a proof from a key only its own microcontroller can use, bound to that TLS
session. The collector verifies both and labels the session `trusted`, `open`,
`test` or `none`; the verdict is shown by `tools/ota.py status` and journaled when
it changes. [Commissioning an ESP32-S3 observer](docs/COMMISSIONING.md) covers the
bench commands on the USB console, what is irreversible, and the hardware
validation still owed before a production burn; the formats are normative in
[commissioning and hardware trust](../docs/COMMISSIONING.md).

## Design rules (do not break)

- **Decode centrally** — never decode an ephemeris here. Frame and forward; fix decoder bugs once,
  centrally, and replay over stored raw frames.
- **The receiver must never go down** — backoff-reconnect forever, spool across outages, never
  `exit()`. Surviving *reboots* is the one part of this rule this hardware class does **not**
  satisfy: the flash tier is designed
  (`docs/PLAN.md §P-spool`) and remains unimplemented — see "Durability envelope"
  above. Never describe navfeeder-esp as reboot-durable, and never remove the per-boot session
  mint that bounds the loss.
- **Wire parity** — `gnf1`/`ubx` must stay byte-identical to `../go/internal/wire/wire.go` and
  `../feeder/navfeeder.c`. TLS 1.2 pinned; nav words big-endian on the wire.
- **Clean-room** — author from the u-blox ICD and our own Apache-2.0 code, using the cited interface specifications.

The pusher reconnects when sent records remain outstanding without durable ACK advancement for 30 seconds. Successful writes and PONGs do not reset this monotonic timer; advancing ACKs and an empty outstanding set do. Reconnect preserves the boot session and replays from the last durable watermark, allowing recovery after the collector discarded a record during a historian outage. This does not extend the RAM-only durability envelope or prevent overflow at arbitrary input rates.

### Hardware-discovery dependency and release evidence

Normal builds pin `esp_hardware_discovery` to commit
`f0011c5bb3471ca0915653e41506e89b30086254` (24CS256/24CS512 support, the `POWER_TPS7A20`,
`COMM_W5500`, `SENSOR_THERMOCOUPLE_MAX31856`, `PRESSURE_BMP580`, `PRESSURE_BMP581`,
`RTC_DS3231M` and `GPS_MOSAIC_G5_P3` catalog IDs, the `CAT_INTSAT` board category with
`INTSAT_MOSAIC_P3`, part identification, `eeprom_has_ic_at()` and the Intsat board templates,
whose X20 revision-B list adds the receiver-control MCP23008 at `0x25` to the 162mm
assembly's DS3231M, interrupt collector and BMP581, and whose P3 list
names both MCP9808s), with the IDF 5.5.4/ESP32-S3
resolution committed in `dependencies.lock`. Updating the pin is a deliberate
source change: review upstream layout changes and run the component's
`test/host` read/write, page-boundary, interrupted-write, timestamp/footer,
factory-ID protection and example checks, plus this project's manifest-policy
host tests. Existing EEPROM layouts and separate NVS/factory identity storage
must remain compatible; a format migration needs its own explicit plan.

`build-navfeeder-esp.sh` writes `build/firmware-provenance.json` after a successful
build. Archive it and `dependencies.lock` with released binaries. It records the
actual CMake-selected component, immutable revision/content hash, IDF revision,
project revision/dirty state, target, lock hash and firmware hash; modified
managed component contents fail the provenance check. With direct `idf.py build`,
run `python tools/build_provenance.py` in the IDF environment before archiving
(or pass `--build-dir build/s3-layout3` for an isolated S3 build).
Clean component host-test artifacts after testing (`make -C
managed_components/esp_hardware_discovery/test/host clean`) before building.

The existing `ESP_HARDWARE_DISCOVERY_PATH=/path/to/esp_hardware_discovery`
development override remains supported. Metadata explicitly marks the actual
local override, records its content hash/path and available Git revision, and
prints a development-build warning. An override build makes the component
manager rewrite the tracked `dependencies.lock` (a `type: local` entry with a
machine-specific path and no commit or content hash); the provenance step says
so — restore the reviewed resolution with `git checkout -- esp32/dependencies.lock`
before committing. `tools/build_provenance.py` also refuses a firmware image
older than the CMake configuration it would be attributed to (rebuild first). Such builds are excluded from claims of
reproducibility from this repository's committed dependency resolution, even if
the local checkout happens to have the same HEAD. A matching dependency pin alone
is not a claim of byte-identical firmware across toolchains/configurations.

### Atomic provisioning storage

Network settings now occupy one `navfeeder/config_v1` blob: `NFC1`, little-endian
version/length, 64-bit generation, reset/insecure flags, port, fixed-size
NUL-terminated fields, and CRC32. A complete valid blob takes precedence over
legacy individual keys. Devices using legacy keys retain their effective
configuration until the first complete save atomically migrates them; loading
alone never writes storage. Previously mixed legacy credentials cannot be
reconstructed automatically and should be explicitly reprovisioned.

The storage transaction relies on ESP-IDF 5.5 NVS's alternate blob chunk
versions and final blob-index publication, not on `nvs_commit` rolling back
failed setters. A failed save can leave the complete old or complete new
configuration durable. Full flash, write errors and interruptions cannot pair
a new host with an old token. Storage operations are serialized. A malformed or
unreadable blob forces provisioning with empty credentials; it never uncovers
stale keys/defaults. A complete explicit save or physical reset can replace a
malformed blob, while transient read errors refuse writes.

Physical reset publishes an empty record with its reset flag in that same
transaction, suppressing compiled development credentials until a complete save
replaces it. This is logical retirement, not secure flash erasure: legacy keys
and flash history may remain. Factory identity and hardware-manifest namespaces
are untouched. Downgrading to firmware that only understands individual keys
requires explicit configuration erasure/reprovisioning; old firmware cannot
interpret this format or its reset flag.

The independent `nvf_setup/credential_v1` record contains the random setup
password of the setup AP and of the app's Security 2 session. It is created once, checked
against the device name and CRC on every setup boot, and deliberately survives
the logical reset above. Corruption fails closed so firmware cannot silently
replace the secret behind an already printed label. A full NVS partition erase
does replace it and therefore requires a new label. See
[the provisioning contract](docs/PROVISIONING.md#device-name-and-setup-credential).

Host fault tests cover every modeled chunk/index/commit boundary, both error
returns and reboot interruption, including migration and reset. They exercise the
actual configuration storage code against NVS's documented atomic-blob contract;
they do not simulate physical flash electronics. ESP-IDF implementation evidence:
`components/nvs_flash/src/nvs_storage.cpp` (`writeItem`, `writeMultiPageBlob`,
`populateBlobIndices`) and its NVS power-loss recovery tests. Run
`make -C components/netcfg/test` and the complete firmware build after changes.
