# navfeeder-esp

The **ESP32 edge feeder** for [`navlistener`](../): a self-contained GNSS observer that reads
raw broadcast nav frames off a local u-blox receiver and pushes them, undecoded, to the
collector over an authenticated, spooled TLS link (GNF1) — with a live status screen on the
board's 1.47" LCD. It's the small-box sibling of the C `../feeder/navfeeder.c`; all decode and
orbit math stay central in the collector (`../docs/DESIGN.md §1`).

> This firmware sends GNF1 records to a navlistener collector. Configure its
> server address and credentials as described below.

## Hardware

- **Waveshare ESP32-C6-LCD-1.47** (ESP32-C6, 4 MB flash, 1.47" ST7789 172×320 IPS, WS2812 LED).
- **u-blox receiver** on UART1: receiver **TX → GPIO9 (RX)**, receiver **RX → GPIO10 (TX)**,
  common ground. UBX output enabled (UBX-RXM-SFRBX; optionally MON-RF/MON-HW/NAV-SAT for the
  RF-integrity telemetry). Default line rate 460800 (u-blox USB-CDC ignores it).
- **Deploy note — GPIO9 is a C6 boot-strapping pin** : a reset that lands while the
  receiver is mid-byte can latch the chip into the ROM serial downloader, which needs a manual
  power cycle to clear. These development boards retain this wiring —
  no `DIS_DOWNLOAD_MODE` eFuse is burned. The supported custom ESP32-S3 board uses
  GPIO4/GPIO5 for the receiver UART. For the C6 board, keep units on stable
  power (a brownout is the usual trigger) and prefer a lower line rate where the frame budget
  allows, since idle-high UART is safe and the hazard scales with line occupancy. Full
  wiring is retained in `main/receiver.c`.
- **Custom GNSS color observer:** ESP32-S3, receiver UART on GPIO4/GPIO5, shared I2C on
  GPIO6/GPIO7, and an addressable 24AA025E64 manifest at `0x50`. Select
  `NVF_BOARD_GNSS_COLOR_NEO`, which `sdkconfig.defaults.s3` sets. Its startup reads the
  factory EUI-64 and manifest;
  `NVF_MANIFEST_FACTORY_INIT` is a manufacturing-only, default-off permission to initialize a
  blank, never-seen EEPROM from the compiled revision-A component list. It never writes after an
  I2C error, to a known-but-blank EEPROM, or across an EUI replacement.
  An absent, never-adopted EEPROM is supported during bring-up: compiled GPIO wiring
  and the provisioned station ID remain usable. Firmware does not synthesize an EUI
  or claim hardware attestation. A previously adopted EEPROM disappearing still
  reports an identity error; its history is retained.

## Build & flash

Install **ESP-IDF 5.5.x** using Espressif's installation instructions, then set
`IDF_PATH` to that checkout. Run its installer once for the ESP32-C6 target.
The wrapper uses ESP-IDF's own toolchain and Python environment selection;
set `IDF_TOOLS_PATH` if you installed the tools outside its default location.

From this directory:

```sh
export IDF_PATH=/path/to/esp-idf
"$IDF_PATH/install.sh" esp32c6             # once, after installing ESP-IDF
./build-navfeeder-esp.sh                  # build the C6 firmware
PORT=/dev/ttyACM0 ./build-navfeeder-esp.sh flash
```

Use your board's actual serial device, such as `/dev/ttyACM0` on Linux or
`/dev/cu.usbmodem...` on macOS. Flashing is requested explicitly by `flash`.
The wrapper preserves an existing C6 `sdkconfig`, warns about differences
from `sdkconfig.defaults`, and records firmware provenance after building.

The wrapper selects ESP32-C6. The custom ESP32-S3 observer builds from the same
sources with ESP-IDF directly, layering `sdkconfig.defaults.s3` (target, 16 MB
flash, `partitions-s3.csv`, `NVF_BOARD_GNSS_COLOR_NEO`) over the shared
`sdkconfig.defaults`:

```sh
export IDF_PATH=/path/to/esp-idf
"$IDF_PATH/install.sh" esp32s3             # once, alongside the esp32c6 install
. "$IDF_PATH/export.sh"
idf.py -B build/s3 -D SDKCONFIG=build/s3/sdkconfig \
  -D 'SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3' \
  -D IDF_TARGET=esp32s3 build
python tools/build_provenance.py --build-dir build/s3
idf.py -B build/s3 -p /dev/cu.usbmodemXXXX flash monitor
```

This creates a separate S3 configuration and build directory, preserving an
existing C6 `sdkconfig`. Existing generated configurations retain their previous
values: regenerating a separate build from the defaults applies PSRAM and OTA
settings. Check that the generated S3 config enables `SPIRAM_MODE_OCT`,
`NVF_SPOOL_PSRAM`, `NVF_OTA` and `BOOTLOADER_APP_ROLLBACK_ENABLE`.
Building for the S3 rewrites only the target field in `dependencies.lock`; the
component revision and content hash remain pinned. Archive the resolved lock
with its firmware provenance, then restore the committed C6 resolution before
committing. Rebuild and record each target sequentially.

The S3 board's console is the module's native USB-Serial/JTAG, so the USB-C
connector both flashes and monitors it and no separate UART adapter is needed.
Its ROM serial downloader is in mask ROM and cannot be missing from a blank
board; `esptool.py` enters and leaves it over USB without touching BOOT or RESET.

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
`NVF_RX_CONFIGURE_M9` configures only a receiver whose MON-VER extension
identifies NEO-M9N: switch to `NVF_RX_BAUD`, then request UBX output, SFRBX,
MON-RF, NAV-SAT and NAV-PVT through RAM-only CFG-VALSET. Firmware logs ACK/NAK and
bounded timeouts for each message setting. Receiver flash and battery-backed
configuration are not written. Unknown models are observed without automatic
configuration. See [the M9 protocol reference](https://content.u-blox.com/sites/default/files/u-blox-M9-SPG-4.04_InterfaceDescription_UBX-21022436.pdf).

The USB console reports `bytes`, checksum-valid `UBX`, checksum-valid `NMEA`,
and emitted `SFRBX` every ten seconds, including before WiFi provisioning.
Zero bytes calls for checking GNSS power and the UART path; bytes without valid
messages can indicate baud/framing trouble. NMEA or MON-VER proves communication,
not raw-navigation support or satellite reception. A rising SFRBX counter is
the evidence that the receiver is supplying raw frames. The C6 default retains
its fixed baud and externally configured receiver.

The 2026-09-14 S3 bench check confirmed NEO-M9N / SPG 4.04 / protocol 32.01,
initial communication at 38400 baud and operation at 460800 after RAM
configuration. All five message/protocol settings returned ACK. Satellite
reception and actual SFRBX output still await the RF connector and antenna.

### Custom-board status LEDs

The two TLC5916 drivers operate the green/yellow rows, including before WiFi
provisioning. Columns are GPS, SBAS, Galileo, BeiDou, QZSS, GLONASS, NavIC and
uplink. Green means a fresh NAV-SAT report contains a code-locked signal with
nonzero C/N0 and no unhealthy flag. Yellow means supported and expected, but
not tracked. Data older than 15 seconds cannot leave an indicator green.
Uplink is green while the collector connection is up, otherwise yellow.

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

### Peripheral and random-number diagnostics

Startup probes the shared I2C bus and reads sensor identification registers.
The 2026-09-14 bench check found MCP9808 at `0x18`, HDC2080 at `0x40`, readable
MCP79412 RTC registers at `0x6f`, and pressure chip ID `0x50` at `0x76`.
That pressure ID is shared by BMP388/BMP384 and cannot identify the exact
variant. RTC status reported a stopped oscillator and disabled battery backup;
the clock was not adopted as system time. These checks do not yet deliver
calibrated environmental telemetry or initialize the RTC.

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
and [BMP384 register reference](https://www.bosch-sensortec.com/media/boschsensortec/downloads/datasheets/bst-bmp384-ds003.pdf).

## What each phase does

See the [implementation status and roadmap](docs/PLAN.md). The P0–P5 core is built:
the firmware boots, brings up the LCD dashboard + WS2812
status LED, runs the clean-room UBX framer, spools frames, and pushes them to the collector
over GNF1/TLS. Config is NVS-first (`netcfg`), falling back to the compiled Kconfig defaults.

**Provisioning a fresh board (no serial console):** an unprovisioned board raises a WiFi AP
`navfeeder-XXYYZZ` and shows its one-time password on the LCD. Join it, open
`http://192.168.4.1/`, enter WiFi + collector + station + token, Save — it writes NVS and
reboots into station mode. (For dev you can still pre-seed everything via `idf.py menuconfig`
→ "navfeeder-esp".) Remaining work includes P-hw (ATECC608 identity + `SIGNED_DATA`),
flash-backed spooling, the u8g2 font upgrade, and additional ESP32 record-parity tests.

**Configuration-reset recovery:** on the custom ESP32-S3 observer, boot the application
normally, then hold **BOOT/DOWNLOAD for eight seconds**. Once the status panel shows the
armed pattern, release the button. Firmware erases only the `navfeeder` configuration
namespace, reboots, and raises a newly passworded SoftAP portal. A short press does nothing,
and merely reaching the hold threshold does not erase anything until a debounced release.

Do not hold BOOT while resetting for this gesture: that enters the ROM downloader instead.
The runtime gesture is intentionally disabled on the current Waveshare ESP32-C6-LCD-1.47
build because its GPIO9 BOOT button shares the receiver UART RX node; pressing it while the
receiver drives TX would create electrical contention. For that dev board—or if application
firmware cannot run on the S3—connect over USB and erase the NVS partition, then reset:

```sh
esptool.py --chip esp32c6 --port /dev/cu.usbmodemXXXX erase-region 0x9000 0x6000
```

Pass `--chip esp32s3` for the custom observer. Those offset/size values are the
`nvs` row in `partitions.csv`, and the S3's `partitions-s3.csv` places `nvs` at
the same offset and size, so the region is identical on both boards; the LittleFS
spool partition is left intact. On the next boot `netcfg_load` finds no
provisioned config and raises a newly passworded SoftAP portal. Use the actual
serial device path for the board. This is a configuration erase, not a firmware
reflash.

**Incomplete configuration also raises the portal**. "Provisioned" means WiFi SSID,
collector host, port in 1–65535, station id, and bearer token are all present — one rule
(`netcfg_validate`), applied both at boot and by the portal before it writes NVS. A board
configured only partly (Kconfig defaults, a partial NVS write, external NVS tooling) therefore
comes up in the portal with the missing field named on the LCD, instead of looping forever on
WiFi/TLS/auth failures that only a serial cable could diagnose. Note the deliberate limit: a
*complete but wrong* config (bad password, unreachable host, revoked token) keeps retrying in
station mode — a unit riding out a collector outage must not drop its uplink over a condition
that is not its fault.

## On-demand OTA updates (ESP32-S3)

`NVF_OTA` enables a laptop-initiated HTTPS download into the inactive OTA slot.
Install this baseline **over USB once**, including its rollback-capable bootloader.
Reserved slots alone do not give older firmware an updater. The C6 remains
serial-update only. Keep the factory application as the recovery image.

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
An entire NVS-partition erase also erases the update key and hardware history.

### Request an update

Serve the application binary (not a merged flash image) at a direct HTTPS URL
with a trusted certificate, `200 OK` and a fixed Content-Length. Redirects and
chunked downloads are rejected. Keep the matching binary locally:

```sh
python3 tools/ota.py --device observer.example.invalid update \
  --key-file /path/to/private/observer-ota.key \
  --image build/s3/navfeeder-esp.bin \
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

Before writing, firmware checks the ESP32-S3 target, project, board marker,
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

## Durability envelope (read before deploying one as a primary observer)

**The spool is RAM-only and non-durable across reboots.** The partition tables
reserve space for a flash tier — 1.5 MiB in `partitions.csv` (C6, 4 MB flash) and
9.875 MiB in `partitions-s3.csv` (S3, 16 MB flash) — and `docs/PLAN.md` records
its design, but no component mounts or writes that partition. This applies to the
current firmware on both supported boards; the larger S3 reservation is flash set
aside, not durability delivered. Plan for these limits:

- **Outage depth has both byte and record limits.** The default C6/internal fallback
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
ESP32 ATECC-backed credential and enrollment path remains P-hw work.

## Design rules (do not break)

- **Decode centrally** — never decode an ephemeris here. Frame and forward; fix decoder bugs once,
  centrally, and replay over stored raw frames.
- **The receiver must never go down** — backoff-reconnect forever, spool across outages, never
  `exit()`. Surviving *reboots* is the one part of this rule this hardware class does **not**
  satisfy: the flash tier is designed
  (`docs/PLAN.md §P-spool`) and deliberately unbuilt for the C6 — see "Durability envelope"
  above. Never describe navfeeder-esp as reboot-durable, and never remove the per-boot session
  mint that bounds the loss.
- **Wire parity** — `gnf1`/`ubx` must stay byte-identical to `../go/internal/wire/wire.go` and
  `../feeder/navfeeder.c`. TLS 1.2 pinned; nav words big-endian on the wire.
- **Clean-room** — author from the u-blox ICD and our own Apache-2.0 code, using the cited interface specifications.

The pusher reconnects when sent records remain outstanding without durable ACK advancement for 30 seconds. Successful writes and PONGs do not reset this monotonic timer; advancing ACKs and an empty outstanding set do. Reconnect preserves the boot session and replays from the last durable watermark, allowing recovery after the collector discarded a record during a historian outage. This does not extend the RAM-only durability envelope or prevent overflow at arbitrary input rates.

### Hardware-discovery dependency and release evidence

Normal builds pin `esp_hardware_discovery` to commit
`5d7e533734c35a6256c1f70a4875ff4cd495b392`, with the IDF 5.5.4/ESP32-C6
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
(or pass `--build-dir build/s3` for an isolated S3 build).
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

Host fault tests cover every modeled chunk/index/commit boundary, both error
returns and reboot interruption, including migration and reset. They exercise the
actual configuration storage code against NVS's documented atomic-blob contract;
they do not simulate physical flash electronics. ESP-IDF implementation evidence:
`components/nvs_flash/src/nvs_storage.cpp` (`writeItem`, `writeMultiPageBlob`,
`populateBlobIndices`) and its NVS power-loss recovery tests. Run
`make -C components/netcfg/test` and the complete firmware build after changes.
