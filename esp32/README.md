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

Toolchain is the house ESP-IDF v5.5 at `~/esp/esp-idf`, with its Python venv built on
**3.12** (MacPorts `/opt/local/bin/python3.12`) under `~/.espressif`. Drive it explicitly —
the system `python3` is 3.14, which IDF 5.5 doesn't support yet, so `export.sh` must find the
3.12 env, not one keyed to the system interpreter:

```sh
export IDF_TOOLS_PATH=$HOME/.espressif
export PATH=$HOME/.espressif/python_env/idf5.5_py3.12_env/bin:$PATH
. $HOME/esp/esp-idf/export.sh

idf.py set-target esp32c6
idf.py build
idf.py -p /dev/cu.usbmodem* flash monitor      # C6 shows up as a USB-Serial-JTAG device
```

`./build-navfeeder-esp.sh` wraps the first two steps (and `./build-navfeeder-esp.sh flash`
flashes + monitors; set `PORT=` to pick the serial device).

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
left intact. On the next boot `netcfg_load` finds no provisioned SSID/host and raises a newly
passworded SoftAP portal. Use the actual serial device path for the board.

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
- **The receiver must never go down** — backoff-reconnect forever, spool across outages,
  survive reboots (littlefs tier). Never `exit()`.
- **Wire parity** — `gnf1`/`ubx` must stay byte-identical to `../go/internal/wire/wire.go` and
  `../feeder/navfeeder.c`. TLS 1.2 pinned; nav words big-endian on the wire.
- **Clean-room** — author from the u-blox ICD and our own Apache-2.0 code, using the cited interface specifications.
