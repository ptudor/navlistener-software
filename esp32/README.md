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

**Factory-reset recovery:** if a well-formed but wrong SSID, password, collector host, or token
was saved, connect the board over USB and erase only the NVS partition, then reset it:

```sh
esptool.py --chip esp32c6 --port /dev/cu.usbmodemXXXX erase-region 0x9000 0x6000
```

Those offset/size values are the `nvs` row in `partitions.csv`; the LittleFS spool partition is
left intact. On the next boot `netcfg_load` finds no provisioned config and raises a newly
passworded SoftAP portal. Use the actual serial device path for the board.

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

**The spool is RAM-only. There is no flash tier yet**. `partitions.csv` reserves
1.5 MiB for one and `docs/PLAN.md §P-spool` records the design, but no component mounts or
writes that partition today. What that means in the field:

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

Deployment rule until the tier ships: **a navfeeder-esp unit is loss-tolerant-only by explicit
design.** Good as an additional observer in a fleet where another station covers the same sky;
not the sole witness of an event you need forensically complete. Watch the `dropped` counter
on the dashboard — a non-zero value means records were lost, not merely delayed.

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
  `exit()`. Surviving *reboots* is the one part of this rule the firmware does **not** yet
  satisfy: the flash tier is designed (`docs/PLAN.md §P-spool`) and unimplemented — see
  "Durability envelope" above. Do not describe navfeeder-esp as reboot-durable until it lands.
- **Wire parity** — `gnf1`/`ubx` must stay byte-identical to `../go/internal/wire/wire.go` and
  `../feeder/navfeeder.c`. TLS 1.2 pinned; nav words big-endian on the wire.
- **Clean-room** — author from the u-blox ICD and our own Apache-2.0 code, using the cited interface specifications.
