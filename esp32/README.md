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
  power cycle to clear. Accepted as-is on these dev-class units by design —
  no `DIS_DOWNLOAD_MODE` eFuse is burned; the fleet-production fix is the planned ESP32-S3
  re-spin moving RX to a non-strapping GPIO. Practical mitigation today: keep units on stable
  power (a brownout is the usual trigger) and prefer a lower line rate where the frame budget
  allows, since idle-high UART is safe and the hazard scales with line occupancy. Full
  technical explanation in `main/main.c` at `RX_PIN_RX`.
- **Custom GNSS color observer:** ESP32-S3, receiver UART on GPIO4/GPIO5, shared I2C on
  GPIO6/GPIO7, and an addressable 24AA025E64 manifest at `0x50`. Select
  `NVF_BOARD_GNSS_COLOR_NEO` in menuconfig. Its startup reads the factory EUI-64 and manifest;
  `NVF_MANIFEST_FACTORY_INIT` is a manufacturing-only, default-off permission to initialize a
  blank, never-seen EEPROM from the compiled revision-A component list. It never writes after an
  I2C error, to a known-but-blank EEPROM, or across an EUI replacement.

## Build & flash

Toolchain is the house ESP-IDF v5.5 at `~/esp/esp-idf`, with its tools and Python venv
under `~/.espressif` (stay inside `~/Git`). The pinned venv is **3.12** (MacPorts
`/opt/local/bin/python3.12`, `idf5.5_py3.12_env`), which is what `export.sh` resolves when it
exists. It is a preference, not a hard requirement: `install.sh` builds its venv from the
system interpreter, and IDF **5.5.4 builds this project against a `py3.14` env** — verified
2026-07-24 on this machine, correcting the older note that 5.5 could not use 3.14.

**One-time bootstrap on a machine that has never built this** (`~/.espressif` absent):

```sh
IDF_TOOLS_PATH=$HOME/.espressif $HOME/esp/esp-idf/install.sh esp32c6
```

Then build:

```sh
./build-navfeeder-esp.sh                        # set-target esp32c6 (guarded) + build
./build-navfeeder-esp.sh flash                  # + flash & monitor; PORT= picks the device
```

The script exports `IDF_TOOLS_PATH`, prefers the 3.12 env, sources `export.sh`, and preflights
the venv — a missing environment now prints the exact bootstrap command above and stops,
instead of failing several steps later inside `export.sh` with a path for a Python version you
never asked for. The equivalent by hand:

```sh
export IDF_TOOLS_PATH=$HOME/.espressif
export PATH=$HOME/.espressif/python_env/idf5.5_py3.12_env/bin:$PATH
. $HOME/esp/esp-idf/export.sh

idf.py set-target esp32c6
idf.py build
idf.py -p /dev/cu.usbmodem* flash monitor      # C6 shows up as a USB-Serial-JTAG device
```

## What each phase does

See `docs/PLAN.md`. P0–P5 are built: the firmware boots, brings up the LCD dashboard + WS2812
status LED, runs the clean-room UBX framer, spools frames, and pushes them to the collector
over GNF1/TLS. Config is NVS-first (`netcfg`), falling back to the compiled Kconfig defaults.

**Provisioning a fresh board (no serial console):** an unprovisioned board raises a WiFi AP
`navfeeder-XXYYZZ` and shows its one-time password on the LCD. Join it, open
`http://192.168.4.1/`, enter WiFi + collector + station + token, Save — it writes NVS and
reboots into station mode. (For dev you can still pre-seed everything via `idf.py menuconfig`
→ "navfeeder-esp".) Remaining: P-hw (ATECC608 identity + `SIGNED_DATA`) and the u8g2 font
upgrade.

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

Those offset/size values are the `nvs` row in `partitions.csv`; the LittleFS spool partition is
left intact. On the next boot `netcfg_load` finds no provisioned config and raises a newly
passworded SoftAP portal. Use the actual serial device path for the board. This is a
configuration erase, not a firmware reflash.

**Incomplete configuration also raises the portal**. "Provisioned" means WiFi SSID,
collector host, port in 1–65535, station id, and bearer token are all present — one rule
(`netcfg_validate`), applied both at boot and by the portal before it writes NVS. A board
configured only partly (Kconfig defaults, a partial NVS write, external NVS tooling) therefore
comes up in the portal with the missing field named on the LCD, instead of looping forever on
WiFi/TLS/auth failures that only a serial cable could diagnose. Note the deliberate limit: a
*complete but wrong* config (bad password, unreachable host, revoked token) keeps retrying in
station mode — a unit riding out a collector outage must not drop its uplink over a condition
that is not its fault.

## Durability envelope (read before deploying one as a primary observer)

**The spool is RAM-only, and that is now a decision rather than a gap** (regression fix, closing
regression fix). `partitions.csv` reserves 1.5 MiB for a flash tier and `docs/PLAN.md §P-spool`
records its design, but no component mounts or writes that partition — and on **2026-07-31**
the owner accepted the ESP32-C6 class as **RAM-only / non-durable across reboots** and
deliberately did **not** commission the flash tier for this hardware
(`technical validation`). Do not re-open it as a bug; it is a
scoped limitation with a named successor (see "What would change it" below). What that means
in the field:

- **Outage depth = the RAM ring.** `CONFIG_NVF_SPOOL_FRAMES` (default **1024**) records; on
  overflow the *oldest* unacked record is dropped and counted. Order-of-magnitude: a
  multi-GNSS receiver tracking ~25–30 SVs emits roughly **10 records/s** (GPS subframes every
  6 s per SV, Galileo I/NAV and GLONASS strings every 2 s), so 1024 frames ≈ **100 seconds**
  of collector outage before the earliest records start falling off the back. Treat that as an
  estimate from broadcast cadences and your own sky view — measure yours from the dashboard's
  `dropped` counter, which is the authoritative signal.
- **Any reboot loses every unacked record**, however brief the outage — records live in
  malloc'd RAM, so `esp_restart()`, a brownout, a watchdog reset, or pulling USB all discard
  them. This is the part with no workaround: it is not a "long outage" failure mode.
- **The C feeder is unaffected**: `../feeder/navfeeder.c --spool-file` has the disk tier, so
  the deployed router/SBC fleet keeps its outage durability. This limitation is specific to
  navfeeder-esp.

**Why accepting it is sound — and what the acceptance depends on.** The loss is bounded at
*exactly* the unacked ring only because of **regression fix** (the per-boot session identity in
HELLO, landed 2026-07-31). Before the regression fix a reboot was strictly worse than "lose the ring": the
feeder came back with its sequence space restarting at 0, those numbers collided with the
durable ledger's rows from the previous run, and the collector silently discarded the *fresh,
successfully captured* post-reboot frames as replays. A reboot therefore poisoned the future,
not just the past. With a new session minted every boot, post-reboot frames land in a
brand-new `(observer, session, seq)` space and are ingested normally. **This makes the regression fix
acceptance conditional: if the per-boot mint is ever removed, or the session is persisted
across boots (NVS, RTC memory, anywhere), the acceptance is void** — see `main.c`'s
`session_init()`.

**What would change it.** The planned **ESP32-S3 + ATECC608** board (see `../docs/HARDWARE-OBSERVER.md`) is where durability gets revisited: a spool tier is in scope for that class, along with
the hardware identity and the non-strapping receiver RX pin (below). Nothing is planned for
the C6 units.

Deployment rule: **a navfeeder-esp unit is loss-tolerant-only by explicit design.** Good as an
additional observer in a fleet where another station covers the same sky; not the sole witness
of an event you need forensically complete. Watch the `dropped` counter on the dashboard — a
non-zero value means records were lost, not merely delayed.

## Enrollment (the shared AAA control plane)

A navfeeder-esp observer is just a `Device` in the control plane navlistener shares with
radiolistener (one CA, one `devices` table — `../docs/DESIGN.md §3`), granted the `ubx` feed.
Enrollment mints a **bearer token shown once**; it goes into NVS on the board (P5
provisioning), never into source. The credential ladder is the same as the C feeder: bearer
token → software mTLS cert → **ATECC608 cert** (the P-hw high-assurance class). Revocation is
`enabled=false` in the DB.

## Design rules (do not break)

- **Decode centrally** — never decode an ephemeris here. Frame and forward; fix decoder bugs once,
  centrally, and replay over stored raw frames.
- **The receiver must never go down** — backoff-reconnect forever, spool across outages, never
  `exit()`. Surviving *reboots* is the one part of this rule this hardware class does **not**
  satisfy, by design : the flash tier is designed
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
run `python tools/build_provenance.py` in the IDF environment before archiving.
Clean component host-test artifacts after testing (`make -C
managed_components/esp_hardware_discovery/test/host clean`) before building.

The existing `ESP_HARDWARE_DISCOVERY_PATH=/path/to/esp_hardware_discovery`
development override remains supported. Metadata explicitly marks the actual
local override, records its content hash/path and available Git revision, and
prints a development-build warning. Such builds are excluded from claims of
reproducibility from this repository's committed dependency resolution, even if
the local checkout happens to have the same HEAD. A matching dependency pin alone
is not a claim of byte-identical firmware across toolchains/configurations.
