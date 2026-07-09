# navfeeder-esp — phased build plan

The ESP32 edge feeder for `navlistener`. It mirrors the C `feeder/navfeeder.c` over the
same GNF1 wire, adds a live status display, and stages toward a hardware-anchored identity.
Each phase is independently buildable and (where possible) host-testable before hardware.

---

## P0 — scaffold + docs *(this commit)*

- `README.md` (product index), this plan, `README.md` (operator runbook).
- ESP-IDF project skeleton: top-level `CMakeLists.txt`, `sdkconfig.defaults` (target
  `esp32c6`), `partitions.csv` (factory + nvs + littlefs spool), `build-navfeeder-esp.sh`.
- `main/` app skeleton that boots, brings up NVS + the display banner, and logs a heartbeat.
- **Milestone:** `idf.py set-target esp32c6 && idf.py build` succeeds; a blank board shows
  the boot banner.

## P1 — the clean-room core: `gnf1` + `ubx` *(test-first)*

The framing components, authored from `wire.go` + the u-blox interface description, byte-for-byte
matched to `feeder/navfeeder.c`. No I/O — pure encode/parse over buffers.

- `gnf1`: `MAGIC`, frame writer/reader, `HELLO`/`WELCOME` JSON, `RawRecord` encode
  (`[8B recv_ns][gnssId][svId][sigId][freqId][frame_type][raw BE]`), `ACK` decode, the
  `(gnssId,sigId) → frame_type` map (CONSTELLATIONS §6).
- `ubx`: sync-hunt + Fletcher-8 framer; `emit_sfrbx` (LE dwrd → BE nav word); `emit_monrf`,
  `emit_monhw`, `emit_navsat` telemetry (UBX LE → GNF1 BE), all bounds-checked (INTEGRITY
  §9 untrusted-input discipline).
- **Milestone:** a host build (`components/*/test/`, `idf.py`-independent) runs the same
  fixtures the Go `feeder_e2e_test.go` uses and produces byte-identical GNF1 records. This
  is the differential check that the firmware and the C feeder agree.

## P2 — spool: bounded RAM ring + seq/ack/replay

- `spool`: fixed-capacity ring (sized for C6 SRAM budget with WiFi+TLS up — start
  conservative, e.g. 1024–2048 frames, documented), monotonic seq on append, drop-oldest
  on overflow with a counter, `collect(after)`, `ack(n)`, `acked()`. FreeRTOS-safe
  (mutex or a stream/ring under a critical section).
- **Milestone:** unit test: append/collect/ack/overflow accounting matches the C spool.

## P3 — pusher: the TLS push consumer

- `pusher`: `esp-tls` client (TLS 1.2 pinned, CA-pinned or bundle), GNF1 `HELLO` with
  token/station/feed, `WELCOME` parse, drain-spool → `DATA`, reader path applies `ACK`
  (prunes spool), `PING` keepalive under the collector's idle timeout, backoff-reconnect
  forever, replay-from-`acked` on every reconnect.
- `netcfg`: `esp_wifi` STA join from NVS; `main` wires UART producer + pusher consumer.
- **Milestone (needs hardware or a laptop collector):** the board authenticates against a
  dev `navlistener` `[push]` endpoint and streams real SFRBX frames end-to-end; the dev
  collector decodes them and they appear in the v2 feed. This is the headline milestone.

## P4 — display: the live dashboard

- `display_st7789`: native `esp_lcd` panel (port shepherd's init for this exact board) +
  `status_led` (RMT WS2812). A live panel: station id, WiFi/link state, frames/s,
  per-constellation counts, spool depth, last-ack lag, and RF/jam hints if MON-RF is on.
- **Better fonts** (the galmon draft's headline nicety): adopt **u8g2** (BSD-2, pure C)
  over the `esp_lcd` framebuffer for proportional/large text, replacing the 8×8 bring-up
  font. Keep the render layer behind a small text API so the font engine is swappable.
- **Milestone:** the board shows a legible, glanceable operational panel on the bench.

## P5 — telemetry + provisioning polish

- MON-RF/MON-HW/NAV-SAT telemetry wired from `ubx` into the DATA stream (feeds the
  collector's PNT-defense layer); optional zstd request in the handshake (only if a small
  ESP32 zstd fits the SRAM budget — otherwise leave off and document).
- `netcfg` SoftAP provisioning UI (provisioning pattern): first-boot AP + a minimal form to set
  wifi + collector + token, persisted to NVS. Generated AP password, never a placeholder.
- **Milestone:** a factory-fresh board is field-provisioned with no serial console.

## P-hw — hardware identity (the P9 high-assurance observer)

- ESP32-S3 variant: bring in `apps/shepherdprotocol/esp32/components/atecc608c`; generate a
  non-extractable P-256 key; station id = EUI-64 from the ATECC serial; implement
  `SIGNED_DATA(0x07)` (a signed DATA batch) and the mTLS `--cert/--key` credential ladder
  (bearer token → software cert → ATECC cert) the collector already accepts.
- DS3231 RTC so `recv_unix_ns` is trustworthy without a fix.
- **Milestone:** an ATECC-anchored observer whose frames the collector can attribute to a
  hardware root — the capability-fingerprint tie-in (top-level P9) closes.

---

## Sizing note (SRAM budget, C6)

WiFi + mbedTLS + the panel framebuffer already claim a large share of the C6's 512 KB HP
SRAM. The spool ring must be sized against what's left, not against the C feeder's 65536
default. Start small (1024 frames), measure `esp_get_free_heap_size()` under load, and lean
on the littlefs disk tier for outage depth rather than a large RAM ring. Document the
measured budget in `spool`'s header when P2 lands.

## Testing methodology (keep using it)

Same as the collector: **differential agreement**. The C `feeder/navfeeder.c` over a real
capture (`../go/internal/ingest/testdata/f9t_capture.ubx`) is the oracle; the ESP32 `ubx` +
`gnf1` components must emit byte-identical GNF1 records over the same bytes. Where a host
build is possible (pure-buffer components), run it in CI; where it needs the radio, verify
on the bench against a dev collector.
