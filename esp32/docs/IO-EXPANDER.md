# MCP23008 interrupt collector and panel readback (X20 revision A)

The specification and task list for the firmware that reads U39, the MCP23008 I/O
expander on the 162mm ZED-X20P mainboard (`INTSAT_X20` revision A), kept as the
feature's documentation; section 11 records where the implementation departs from it
and why. It follows the project's rules: one universal image, every driver gated by the
manifest and failing closed, no Kconfig per-board switches, and the universal image built
plus `make -C esp32 host-test` passing before a commit.

## 1. Hardware the firmware is given

All of this is from the board's netlist and the IC review; do not re-derive it.

| Item | Value |
|---|---|
| Part | Microchip MCP23008T-E/ML (UQFN-20), datasheet DS20001919 |
| I2C | 7-bit address `0x24` (A2 high, A1 and A0 low) on the shared manifest bus, GPIO6 SDA / GPIO7 SCL, 100 kHz as the other devices |
| Supply | `3V3_SYS` (always on). The bus pull-ups are on `3V3_SENS` (GPIO21 `SENS_EN`, default on), so the expander answers only while that rail is on |
| RESET# | tied to `ESP_EN`: the expander resets with the ESP32, so it must be configured after every boot |
| INT | to ESP32 **GPIO3**, net `EXP_INT_N`, 10 kΩ pull-up to `3V3_SYS`. Default push-pull active-low output (IOCON ODR = 0, INTPOL = 0) stays. GPIO3 is a strapping pin (JTAG_SEL): read it only, never drive it, never burn `EFUSE_STRAP_JTAG_SEL` |
| GP0 | `PANEL_SDO`, the last TLC5916's SDO of the LED chain (3V3_SYS logic, through 33 Ω). On the 162mm board the chain is 24 bits: U12 green, U13 amber, U14 status. On the 3900 mil square board, which runs the same X20 row, GP0 reads that board's on-board 16-bit chain instead |
| GP1 | `ETH_INT_N`, W5500 INTn (3V3_ETH logic, no external pull-up). The W5500 is polled and its interrupt masks are zero, so this line idles high today |
| GP2 | `BARO_INT_N`, BMP581 INT, 10 kΩ to `3V3_SENS`. Not enabled by firmware today (the sensor is polled) |
| GP3 | `HUM_INT_N`, HDC2080 DRDY/INT, push-pull, no pull-up. Not enabled by firmware today |
| GP4 | `TEMP_ALERT_N`, MCP9808 Alert, open drain, 10 kΩ to `3V3_SENS`. Not enabled by firmware today |
| GP5 | `PWR_ALERT_N`, INA3221 Warning (pin 8) and Critical (pin 9) wired-OR, open drain, 10 kΩ to `3V3_SENS`. The INA3221's limit registers reset to their maximum, so this line never asserts until firmware programs limits (section 4) |
| GP6, GP7 | unconnected |

Inputs on GP1–GP5 come from the 3V3_ETH and 3V3_SENS domains; the internal pull-ups
must stay off on them. GP0 and the unused GP6/GP7 get pull-ups, so GP0 reads 1 with no
chain attached and the unused pins never float.

Board rows: `main/board_reservations.h` already carries `exp_int_n` (3 on the X20 row,
`NVF_NONE` on the NEO and MAX), `panel_status_byte` and `led_panel_sdi`; `board.c`
configures `exp_int_n` as a floating input in `board_inputs()`. The manifest lists the
expander as `IC_I2C(CAT_IO_EXPANDER, IO_MCP23008, 0x24)` (esp32-hardware-discovery
cafbc9e); `observer_board_lists(CAT_IO_EXPANDER, IO_MCP23008)` is the gate.

## 2. Register programme (MCP23008, BANK = 0, DS20001919 Table 1-3)

| Register | Address | Value | Why |
|---|---|---|---|
| IODIR | 0x00 | 0xFF | all inputs |
| IPOL | 0x01 | 0x00 | no inversion; the sources are active-low and are reported as such |
| GPINTEN | 0x02 | 0x3E | interrupt-on-change on GP1–GP5 only |
| DEFVAL | 0x03 | 0x00 | unused with INTCON = 0 |
| INTCON | 0x04 | 0x00 | compare with the previous value, so a steady level does not retrigger |
| IOCON | 0x05 | 0x00 | sequential addressing, slew-rate control on, INT push-pull active-low |
| GPPU | 0x06 | 0xC1 | pull-ups on GP0, GP6, GP7 only |

INTF (0x07) names the pins that caused the interrupt; reading INTCAP (0x08) or GPIO
(0x09) clears INT. Write the programme, read every register back, and treat any
difference as "not configured" (log once, keep polling nothing; retry at the next
board-task pass, because a `3V3_SENS` power cycle takes the bus pull-ups away).

## 3. Interrupt service

1. In `board_inputs()` (or a new call next to it) install a GPIO ISR on `exp_int_n`
   for the falling edge, only when the manifest lists the expander and the register
   programme read back. The ISR does nothing but notify a task.
2. A small task (or the existing board task woken by the notification; the choice is
   the implementer's, but do not block the panel refresh task on I2C) reads INTF, then
   INTCAP. While GPIO3 still reads low after the read, read GPIO again: a change that
   arrived during handling is not lost.
3. For each INTF bit set, record the source (`ETH`, `BARO`, `HUM`, `TEMP`, `PWR`), the
   INTCAP level, a count and the time (`esp_timer_get_time()/1000`). Log at INFO,
   rate-limited to one line per source per 10 s.
4. GP5 (`PWR_ALERT_N`) additionally triggers section 4's alert read, which is what
   releases the INA3221's latched alert line.
5. GP0 never interrupts; it is sampled in section 5.

## 4. INA3221 alert limits and decoding (`components/environment`, SBOS576C)

Today `ina3221.c` leaves the alert outputs unused. Add:

- `ina3221_set_alerts(const env_io_t *io, const int32_t warn_uv[3], const int32_t crit_uv[3])`:
  writes the per-channel Critical-Alert Limit and Warning-Alert Limit registers (the
  register map in SBOS576C §8.2 gives their addresses; take them from the datasheet
  rather than this file) in the shunt-voltage format (bits 15–3, 40 µV LSB, two's
  complement, low three bits zero; clamp at the register range), then sets `CEN` (bit
  10) and `WEN` (bit 11) in Mask/Enable (0x0F) so an alert latches until read, keeping
  the other Mask/Enable bits as read. Reads the limits back. A channel whose limit is
  0 is not programmed.
- `ina3221_read_alerts(const env_io_t *io, ina3221_alerts_t *out)`: reads Mask/Enable
  once and decodes `CF1..CF3` (bits 9, 8, 7), `SF` (6), `WF1..WF3` (bits 5, 4, 3),
  `PVF` (2), `TCF` (1), `CVRF` (0). The read clears CF, WF and SF (Table 8-34); writing
  does not. The board task calls it on a GP5 interrupt and logs which channel and which
  level tripped.
- The existing re-initialisation path after a `3V3_SENS` power cycle must apply the
  limits again.
- Limits live in the board row: extend `observer_rail_t` with `warn_ma` and `crit_ma`
  (0 = no alert). Shunt voltage = mA × shunt mΩ (µV). X20 row defaults, engineering
  values to be confirmed on the bench, like the heater thresholds:

  | Rail (shunt) | warn | critical | reason |
  |---|---|---|---|
  | `+5V` (20 mΩ) | 1000 mA | 1300 mA | below the eFuse's 1.45 A minimum limit, so the alert is seen before the eFuse acts |
  | `3V3_GNSS` (50 mΩ) | 350 mA | 500 mA | receiver plus antenna feed is under 300 mA; the ADM7150 is 800 mA |
  | `3V3_SYS` (20 mΩ) | 800 mA | 1000 mA | ESP32 Wi-Fi peaks plus panel logic; the LDL1117 is 1.2 A |

  The warning compares the averaged measurement (64 conversions as configured), the
  critical limit every conversion.

## 5. Panel chain readback over GP0

The chain is a shift register of L bits whose last stage is `PANEL_SDO`. Before clock
k of a frame write, GP0 holds the bit written L clocks earlier. Rewriting the current
24-bit frame therefore reads the same frame back: expected bit before clock k is
`frame[(k + 24 − L) mod 24]`, with L = 24 on the 162mm panel and L = 16 on the 3900 mil
board's on-board chain (bit 0 is the first bit shifted, the status byte's MSB).

1. Add a verification variant of `panel_write()` that, with OE held high exactly as the
   normal write does, reads the expander's GPIO register before each `clock_bit()` and
   collects the 24 GP0 samples, then latches the unchanged frame. Run it from the
   panel refresh task once every 60 s, and once right after the first frame.
2. On the first run compare the samples against L = 24 and L = 16; adopt the one that
   matches completely and log it ("panel chain: 24 bits"). If neither matches, count a
   readback fault and try again at the next run; after three consecutive faults log at
   ERROR once ("panel chain not detected or wrong length") and keep counting.
3. On later runs compare with the adopted L; a mismatch increments a counter and logs
   the differing bit positions, rate-limited to one line per minute.
4. The I2C read per bit costs about 0.5 ms at 100 kHz, so the verification write holds
   the LEDs dark for roughly 12 ms once a minute. That is acceptable; do not speed up
   the bus for it.
5. The expander's device handle is created once at configuration. The ESP-IDF
   `i2c_master` driver serialises transactions on a bus across device handles; confirm
   that in the IDF version pinned by this project before letting the refresh task and
   the board task share the bus, otherwise route the reads through the board task.

## 6. Rail-off hooks

`3V3_SENS` is never switched off by today's firmware. Provide `io_expander_suspend()`
(write GPINTEN = 0, stop handling interrupts) and `io_expander_resume()` (re-run the
register programme), and state in the README that any future `SENS_EN` control must
call them around the rail change, because GP2–GP5 fall to 0 V with the rail and the
bus pull-ups go with it.

## 7. Status for diagnostics

Expose `io_expander_status_t` through an accessor: configured flag, chain length, last
GPIO image, per-source counts and last times, readback runs and mismatches, and the
last INA3221 alert flags. Print it in the existing boot diagnostics and once per hour at
INFO. Adding it to ObserverDetails telemetry is out of scope for this task (it needs the
Go decoder and fixtures); say so in the README.

## 8. Files and tasks

1. New component `esp32/components/io_expander/` with `include/io_expander.h`,
   `src/io_expander.c` (pure register logic over an `env_io_t`-style read/write
   interface, no FreeRTOS in the pure part) and `test/` (Makefile plus
   `io_expander_test.c` with a mock register file, in the style of
   `components/rtc/test/max31328_test.c` and `components/environment/test/ina3221_test.c`).
2. `components/environment`: the two INA3221 functions above, the `ina3221_alerts_t`
   type, tests in `test/ina3221_test.c` for limit encoding, clamping, Mask/Enable
   decoding and the latch bits.
3. `main/board_reservations.h`: `warn_ma`/`crit_ma` in `observer_rail_t`; X20 values
   above; NEO and MAX rows unchanged (they have no INA3221).
4. `main/board.c`: configure the expander after `observer_board_manifest()` when
   listed; ISR and handler; INA3221 alert programming where `rails_listed`; the
   verification write in the panel refresh task; status in the boot diagnostics.
5. `esp32/README.md`: replace "the expander is in the manifest, and its inputs are not
   yet read" with the behaviour above; add the rail-off hook requirement and the
   telemetry note. `docs/HARDWARE-OBSERVER.md`: one sentence in the X20 paragraph.
6. Build: with `IDF_PATH` naming an ESP-IDF 5.5.4 checkout (and `IDF_TOOLS_PATH` and
   `IDF_PYTHON_ENV_PATH` where its tools and Python environment are installed; set
   `IDF_PYTHON_CHECK_CONSTRAINTS=no` if the environment's constraint check rejects it),
   source `$IDF_PATH/export.sh`, then
   `idf.py -B build/s3-layout3 -D SDKCONFIG=build/s3-layout3/sdkconfig -D 'SDKCONFIG_DEFAULTS=sdkconfig.defaults;sdkconfig.defaults.s3' -D IDF_TARGET=esp32s3 build`.
   `make -C esp32 host-test` must pass.

## 9. Host test cases (minimum)

- Register programme: the exact write sequence and values of section 2; a readback
  that differs in any register leaves the driver unconfigured.
- Interrupt decode: INTF 0x20 / INTCAP 0x1E → `PWR` asserted (low); INTF 0x06 with
  both bits → two sources in one event; INT still low after the read → a second read.
- Readback formula: for random 24-bit frames, the expected sample vector for L = 24 and
  L = 16; detection adopts the right L from a simulated chain of each length; a chain
  stuck at 1 (no panel) matches neither.
- INA3221: 1300 mA × 20 mΩ = 26 000 µV → register 0x1450 (650 × 8); 500 mA × 50 mΩ →
  0x1388; a limit above 163.8 mV clamps to 0x7FF8; Mask/Enable 0x0C90 decodes to
  CEN, WEN, CF3 and WF2 (CF1 is bit 9, so CEN, WEN, CF1 and WF2 is 0x0E10); the alert
  write keeps SCC bits as read.

## 10. Acceptance

- Universal image builds; host tests pass; no new warnings.
- On a board whose manifest does not list `IO_MCP23008`, or whose row has no
  `exp_int_n`, nothing in this feature runs and GPIO3 stays a floating input.
- Boot log shows the expander configured and the chain length; forcing a sensor alert
  (for example an INA3221 warning limit set below the idle current on the bench)
  produces one `PWR` interrupt line and one decoded alert line, and the line releases.
- Pulling the panel's SDO line to ground with the panel fitted produces readback
  mismatches within one minute.

## 11. Implementation notes

The code is `components/io_expander` (`io_expander.c`, pure, with its host test;
`io_expander_service.c`, the task and interrupt glue), the INA3221 additions in
`components/environment`, and the board wiring in `main/board.c`. It departs from the
sections above in these places:

- **A confirming GPIO read (section 3.2).** After INTF and INTCAP the service always reads
  GPIO once, then again while INT stays low (at most four reads). A line can change back
  before the service runs: the INA3221's alert does when a rail sample's Mask/Enable read
  clears it first. INTCAP shows only the assertion and the return raises no interrupt, so
  without that read the image would stay inverted and the next real alert would decode as a
  release. A flagged line's new level is the image's inverse (INTCON = 0); INTCAP holds it
  only for the first line to change (DS20001919 section 1.6.8). INT still low after the
  last read is serviced again every second while it stays low.
- **Starting levels and logging (section 3.3).** A line already low at the first
  configuration counts as asserting then, so a `PWR_ALERT_N` latched across a restart is
  read. A change inside its line's 10 s is counted at once and logged when the 10 s end, so
  the latest level is always logged.
- **Who clears INA3221 flags (section 4).** Every Mask/Enable read clears CF, SF and WF,
  including `ina3221_read`'s conversion-ready polling and `ina3221_set_alerts`'s
  read-modify-write. The flags those reads return are passed on rather than lost:
  `ina3221_sample_t.alert_flags`, and a fourth `ina3221_set_alerts` argument, `cleared`. The
  board logs a decoded alert from whichever read cleared it. The Mask/Enable write keeps the
  reserved bit and SCC1-3 as read and writes the flag bits as 0, since a write does not
  change them. Limits round to the nearest 40 µV step: 350 mA across 50 mΩ, 17.5 mV, is
  0x0DB0. Alert programming follows `rails_listed`, as section 8.4 says, so it does not
  wait for the expander.
- **Failures.** A failed expander transfer, in the interrupt service or the readback,
  leaves the expander unconfigured; the next board-task pass runs the programme again,
  which re-reads GPIO and records any change since the last image.
- **Readback (section 5).** The verification write runs only while the chain holds the
  current frame (no change since the last write). A run both lengths read alike (an
  all-ones frame) decides nothing and counts no fault. A failed read stops sampling, but
  the frame is still written in full. The results are logged by the expander's task, never
  by the panel refresh task, whose stack grew from 2 KiB to 3 KiB for the I2C reads.
- **GP3 is driven by the HDC.** The HDC2080's DRDY/INT output is high impedance until
  firmware enables it (0x0E bit 2, reset 0) and `HUM_INT_N` has no pull-up, so GP3 would
  float under GPINTEN 0x3E. The humidity driver's configuration now masks every HDC
  interrupt source (0x07 = 0) and enables the output active low in clear-on-read mode (0x0E
  bits 2-0 = 100; INT_MODE must be 0 or the pin stays high impedance), each read back, so the
  line holds high (HDC2080 SNAS678C section 8.3.4.1, HDC2022 SNAS774A section 7.3.5.1). A
  `3V3_SENS` power cycle returns the HDC to high impedance, so a future `SENS_EN` control
  must configure it again as well as resume the expander.
