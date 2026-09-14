# navfeeder-esp — implementation status and roadmap

The ESP32 edge feeder for `navlistener`. It mirrors the C `feeder/navfeeder.c` over the
same GNF1 wire, adds a live status display, and stages toward a hardware-anchored identity.

**Status: P0–P5 core functionality implemented.** The firmware includes framing,
RAM spooling, TLS push, display, telemetry, and SoftAP provisioning. Current builds
support the Waveshare ESP32-C6 and custom ESP32-S3 observer. Use the
[firmware guide](../README.md) to build, provision, and run it. This ledger retains
the original phase names and marks remaining extensions and validation work.

---

## P0 — scaffold + docs *(implemented)*

- `README.md` (product index and operator runbook), this plan.
- ESP-IDF project skeleton: top-level `CMakeLists.txt`, `sdkconfig.defaults` (target
  `esp32c6`), `partitions.csv` (factory + nvs + littlefs spool), `build-navfeeder-esp.sh`.
- `main/` app skeleton that boots, brings up NVS + the display banner, and logs a heartbeat.
- **Milestone:** `idf.py set-target esp32c6 && idf.py build` succeeds; a blank board shows
  the boot banner.

## P1 — the clean-room core: `gnf1` + `ubx` *(implemented; parity tests partial)*

The framing components, authored from `wire.go` + the u-blox interface description, byte-for-byte
matched to `feeder/navfeeder.c`. No I/O — pure encode/parse over buffers.

- `gnf1`: `MAGIC`, frame writer/reader, `HELLO`/`WELCOME` JSON, `RawRecord` encode
  (`[8B recv_ns][gnssId][svId][sigId][freqId][frame_type][raw BE]`), `ACK` decode, the
  `(gnssId,sigId) → frame_type` map (CONSTELLATIONS §6).
- `ubx`: sync-hunt + Fletcher-8 framer; `emit_sfrbx` (LE dwrd → BE nav word); `emit_monrf`,
  `emit_monhw`, `emit_navsat` telemetry (UBX LE → GNF1 BE), all bounds-checked (INTEGRITY
  §9 untrusted-input discipline).
- **Milestone (partially delivered — regression fix):** the host suite (`components/gnf1/test/`,
  `idf.py`-independent) pins `gnf1_frame_type` against the shared 8×16 golden matrix and
  `gnf1_build_hello` byte-for-byte against the C feeder's spelling. **Still open
  :** host tests for `gnf1_encode_record`/`gnf1_encode_telem`/`gnf1_encode_data`
  and the `ubx` emitters (`emit_sfrbx`/`emit_monrf`/`emit_monhw`/`emit_navsat`) against the
  Go `feeder_e2e_test.go` fixtures — the byte-identical-records differential that would
  catch an regression fix-class field transposition on this leg (Go↔C telemetry parity is already
  covered by `TestNavfeederTelemetryEndToEnd`; only the ESP32 leg lacks it).

## P2 — spool: bounded RAM ring + seq/ack/replay *(implemented)*

- `spool`: fixed-capacity ring (sized for C6 SRAM budget with WiFi+TLS up — start
  conservative, e.g. 1024–2048 frames, documented), monotonic seq on append, drop-oldest
  on overflow with a counter, `collect(after)`, `ack(n)`, `acked()`. FreeRTOS-safe
  (mutex or a stream/ring under a critical section).
- **Remaining validation:** dedicated append/collect/ack/overflow parity tests
  against the C spool. The ring implementation is in `components/spool/src/spool.c`.

## P3 — pusher: the TLS push consumer *(implemented)*

- `pusher`: `esp-tls` client (TLS 1.2 pinned, CA-pinned or bundle), GNF1 `HELLO` with
  token/station/feed, `WELCOME` parse, drain-spool → `DATA`, reader path applies `ACK`
  (prunes spool), `PING` keepalive under the collector's idle timeout, backoff-reconnect
  forever, replay-from-`acked` on every reconnect.
- `netcfg`: `esp_wifi` STA join from NVS; `main` wires UART producer + pusher consumer.
- **Milestone (needs hardware or a laptop collector):** the board authenticates against a
  dev `navlistener` `[push]` endpoint and streams real SFRBX frames end-to-end; the dev
  collector decodes them and they appear in the v2 feed. This is the headline milestone.

## P4 — display: the live dashboard *(implemented; font upgrade planned)*

- `display_st7789`: native `esp_lcd` panel (port shepherd's init for this exact board) +
  `status_led` (RMT WS2812). A live panel: station id, WiFi/link state, frames/s,
  per-constellation counts, spool depth, last-ack lag, and RF/jam hints if MON-RF is on.
- **Planned font upgrade:** adopt **u8g2** (BSD-2, pure C)
  over the `esp_lcd` framebuffer for proportional/large text, replacing the 8×8 bring-up
  font. Keep the render layer behind a small text API so the font engine is swappable.
- **Milestone:** the board shows a legible, glanceable operational panel on the bench.

## P5 — telemetry + provisioning polish *(implemented; compression optional/planned)*

- MON-RF/MON-HW/NAV-SAT telemetry wired from `ubx` into the DATA stream (feeds the
  collector's PNT-defense layer); optional zstd request in the handshake (only if a small
  ESP32 zstd fits the SRAM budget — otherwise leave off and document).
- `netcfg` SoftAP provisioning UI (provisioning pattern): first-boot AP + a minimal form to set
  wifi + collector + token, persisted to NVS. Generated AP password, never a placeholder.
- **Milestone:** a factory-fresh board is field-provisioned with no serial console.

## P-spool — the flash spill tier *(designed, DEFERRED by decision 2026-07-24)*

**Status: deferred; current firmware uses RAM only on both supported boards.** The reserved
1.5 MiB `spool` partition is unmounted; every unacked record dies on reboot. That envelope is
stated to operators in `../README.md` §"Durability envelope", in `components/spool/include/spool.h`,
and in `partitions.csv`. Until this phase runs, **navfeeder-esp is loss-tolerant-only by
explicit design** and must not be deployed as the sole witness of anything forensically
required. The router/SBC fleet is unaffected — `feeder/navfeeder.c --spool-file` has its disk
tier.

The design is recorded here so the deferral is a decision with a plan, not an open question:

- **Spill on RAM overflow, not write-through.** Flash wear on a 1.5 MiB partition is the
  binding constraint, so the tier must absorb only what the ring evicts. Write-through would
  multiply erase cycles by the full record rate for no benefit while the uplink is healthy.
- **Bounded append-only segments**, each record carrying its length, the existing monotonic
  seq, and a CRC. Sequence continuity across RAM and flash is what makes replay-on-reconnect
  correct. Follow the current session and durable-ACK contract in `../../docs/DESIGN.md`:
  with a historian, ACK advances only through durably resolved received sequences;
  live-only collectors acknowledge on receipt. Retain everything above that watermark.
- **Boot-time recovery scan** that truncates the torn tail at the first bad CRC — a power cut
  mid-append must cost the last record, never the segment.
- **Prune a segment only once it is entirely ≤ the acked watermark**, which keeps erases at
  segment granularity instead of per record.
- **Explicit failure fallback**: flash full or write error ⇒ revert to RAM-only, count it, and
  surface it on the LED/display. A silently degraded durability tier is worse than none.
- **Sync policy inherits `navfeeder.c` regression fix**: flush, don't fsync every record — the
  deliberate trade is "survives an orderly reboot/poweroff losslessly, not necessarily an
  unclean power cut". Carry the same words into the component header so the guarantee is not
  overstated.
- **Measure the wear budget before enabling by default**: records/day × record size vs.
  partition size × erase-cycle budget, written into the component header.
- **Implementation choice is open and consequential.** The reserved partition is declared
  `data, littlefs`, but ESP-IDF ships no in-tree LittleFS (it would mean adding the
  `joltwallet/littlefs` managed component), and a filesystem's metadata churn works against
  the wear constraint above. A raw `esp_partition` sector log — records with CRC, 4 KiB-sector
  erase/prune, boot scan — has minimal write amplification and no external dependency, at the
  cost of bespoke code and a partition subtype label that would want updating. Decide this
  first; do not start coding against the label.
- **Verification** (from the regression fix review): collector outages longer than RAM capacity;
  reboot with unacked records in both RAM and flash, then verify replay order and sequence
  continuity; injected torn writes, full flash, and delete/truncate failures; a power-cut rig
  if practical.

## P-hw — hardware identity *(collector attestation implemented; firmware integration planned)*

- ESP32-S3 board selection, manifest reading, and configuration-reset recovery are
  implemented. The collector's manufacturer-attestation verification and bench CLI
  are also implemented; these do not supply the firmware's operational key path.
- Remaining: integrate the ATECC608C non-extractable P-256 key, CSR/enrollment,
  hardware-backed TLS, and `SIGNED_DATA(0x07)` transport.
- Use the **RTC's factory EUI-64** for observer identity, rendered as lowercase
  hyphen-separated byte pairs. The EEPROM EUI-64 and ATECC serial identify separate
  parts; neither substitutes for the RTC identity. See
  [the hardware identity contract](../../docs/HARDWARE-OBSERVER.md#4-identity-and-trust).
- RTC timestamp and PPS integration must follow that contract's timing checks;
  an RTC or hardware credential alone does not authenticate GNSS time.

---

## Sizing note (SRAM budget, C6)

WiFi + mbedTLS + the panel framebuffer already claim a large share of the C6's 512 KB HP
SRAM. The spool ring must be sized against what's left, not against the C feeder's 65536
default. The implemented default is 1024 frames. Measure `esp_get_free_heap_size()`
under load before increasing it, and document the measured budget in `spool`'s header.

**Caveat while P-spool is deferred:** there is no flash tier, so the ring *is* the whole
outage budget (~100 s at 1024 frames; see `../README.md` §"Durability envelope"). Sizing the
ring conservatively is therefore a deliberate acceptance of that limit, not a deferral of it
to a tier that exists.

## Testing methodology (keep using it)

Same as the collector: **differential agreement**. The C `feeder/navfeeder.c` over a real
capture (`../go/internal/ingest/testdata/f9t_capture.ubx`) is the oracle; the ESP32 `ubx` +
`gnf1` components must emit byte-identical GNF1 records over the same bytes. Where a host
build is possible (pure-buffer components), run it in CI; where it needs the radio, verify
on the bench against a dev collector.
