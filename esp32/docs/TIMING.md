# GNSS PPS and RTC pulse measurements

The S3 measures two independent electrical inputs: the receiver's buffered
TIMEPULSE on **GPIO10** and the RTC's 1 Hz output on **GPIO15** (MCP79412 MFP on the
NEO and MAX, MAX31328 INT/SQW on the ZED/X20). Both edges are captured by one MCPWM
hardware timer, with separate PCNT hardware counters counting rising edges.
This gives pulse interval, high width, pulse totals and relative phase/drift
without steering any oscillator.

## What the numbers mean

The capture clock is the ESP APB clock: normally 80 MHz on this S3, or a **12.5 ns
tick**. That is resolution, not absolute accuracy. Hardware latches the timer at
the input edge; interrupt arrival time is only a coarse gap/ambiguity check.
Both inputs include their own board propagation delays. Buffer delay, RTC
open-drain rise time, oscillator temperature drift and capture quantization
remain part of the measurement.

- `period_error_ppm` is the continuous-span average period error relative to a
  nominal ESP second. **Positive means a longer input period** (a slower input
  clock if the ESP were exact). It is not automatically the RTC's frequency error.
- `span_phase_ns` is total span ticks minus one nominal second per complete
  interval, converted to ns. A gap starts a new span; it does not invent pulses.
- `rtc_minus_gnss_phase_ns` is RTC edge phase minus GNSS edge phase, wrapped to
  [−0.5, +0.5) seconds. Its *change* reveals relative drift. The initial RTC phase
  is arbitrary: RTC calendar initialization does not align its edge to UTC.
  Adjacent captured cycles are normalized using the last measured GNSS period
  before wrapping to the nominal ESP-second display range. This avoids an apparent
  phase step from ESP clock error when polling crosses a pulse boundary. It is a phase estimate;
  variations in the reference period still contribute uncertainty.
- Hardware pulse totals count electrical rising edges, independently of the
  software timestamp queue. The captured-edge count allows comparison with
  software delivery. Neither count proves those edges represent valid UTC.
- Counts restart on ESP reset. Compare within the same boot, using capture-start
  uptime. A pulse close to either sampling boundary can produce a one-count
  difference from rounded elapsed seconds. Capture and hardware counters start
  sequentially, so an edge during that short startup window can also differ.

For example, comparing two snapshots about 86,400 ESP seconds apart can show
86,400 RTC pulses and 86,400 GNSS pulses. The interval/phase data still reveals
fractional drift that whole-pulse totals hide. The host test simulates this full
24-hour count, including rollovers; it is not a 24-hour physical qualification.

Three clocks supply useful cross-checks, but do not uniquely identify truth.
A trusted GNSS lock can support attributing common measured drift to the ESP;
reception loss, interference, GNSS holdover and correlated errors weaken that
inference. This implementation does not apply a three-cornered-hat noise model,
produce an Allan deviation estimate, discipline the RTC, or change system time.

## RTC configuration and time policy

Only a validated running MCP79412 calendar is eligible. Firmware sets CONTROL
`SQWEN=1`, `SQWFS=00` for 1 Hz and verifies readback, preserving other bits and
OSCTRIM. It refuses changes when either alarm or coarse trim is enabled, because
MFP is shared with those functions. CONTROL/OSCTRIM and the configuration result
are reported separately from observed input activity.

On the ZED/X20, only a MAX31328 calendar with its oscillator-stop flag clear is
eligible. Firmware clears INTCN and RS (1 Hz on INT/SQW), BBSQW and CONV, turns off
the unused 32 kHz output, and verifies readback. It never takes over alarm
interrupts and never clears the stop flag outside a verified GNSS set. The timing
record's control and trim bytes carry its control register and aging offset. The
1 Hz output follows the temperature-compensated oscillator and is off on the backup
cell ([ADI 19-100978](https://www.analog.com/media/en/technical-documentation/data-sheets/max31328.pdf),
control and status registers).

The 1 Hz output includes the RTC's digital trim. It is absent in battery-backup
operation. The board supplies MFP's external pull-up; both ESP input pins remain
inputs with internal pull-ups disabled. The CONTROL register table in Microchip
DS20002266H specifies `00` for 1 Hz; its adjacent postscaler table conflicts with
that encoding. Confirm the actual observed period when bringing up hardware.

Stopped or invalid calendars still wait for qualified GNSS UTC, using the
existing initialization checks. There is no compile-time fallback or automatic
trim/calibration write. Battery-backup enable does not prove battery presence.

## Freshness, loss and operating limits

- Edge timestamps use a fixed 128-entry internal-RAM queue. The interrupt handler
  and driver are cache safe (`CONFIG_MCPWM_ISR_CACHE_SAFE`), allowing capture during
  ordinary flash writes. Queue exhaustion drops timestamps and counts the loss;
  it does not block GNSS. Software processing drains a bounded batch every board
  poll, normally 0.5 seconds. PCNT counts independently during task delays.
- A rising edge is fresh for 1.5 seconds. Current period, width, span and phase
  values become unavailable when stale. Historical counts/extrema remain.
- The 32-bit capture timer wraps about every 53.7 seconds at 80 MHz. Unsigned
  subtraction handles wraps between successive edges. Non-monotonic coarse
  times, gaps over ten seconds, or disagreement exceeding 10 ms between coarse
  time and hardware delta break continuity instead of guessing wrap counts.
- Accepted periods are 0.5–1.5 nominal seconds. Other intervals reset the span.
  Missing-pulse estimates use rounded gaps between observed rising edges; they
  are separate from actual pulse counts and cannot diagnose an initially absent
  signal or tell a missing electrical pulse from a lost timestamp.
- PCNT rolls automatically at 30,000 pulses; polling extends it to 64 bits without
  clearing a running counter. This assumes nominal 1 Hz inputs. A polling gap of
  30,000 seconds or a counter read failure invalidates count continuity for the
  boot. Unexpected high-frequency signals can make multiple rollovers ambiguous;
  this input configuration is not a general frequency counter.
- Full-day physical capture, concurrent TLS/OTA load, supply interruption and
  temperature/reference-clock characterization remain service validation tasks.

## Receiver metadata and sawtooth plots

The NEO-M9N configuration enables UART1 `UBX-TIM-TP` using RAM-only CFG key
`0x2091017e`. Recent flags/reference metadata is reported for up to three seconds.
TIM-TP describes the **next pulse**, and is not yet associated with individual
captured edges. Fresh GPIO pulses alone do not imply a valid GNSS time solution.

The u-blox SPG 4.04 release notes state that TIM-TP **quantization error is not
output**. Therefore this firmware does not expose the qErr field as a valid
sawtooth correction. Actual measured period, relative phase and count charts are
available; absolute UTC edge accuracy needs a separately qualified reference.

## Reporting and plotting

A timing-only ObserverDetails sample is queued once per second in the existing
GNF1 PSRAM spool. Environmental conversions retain their slower change/check-in
policy. The collector keeps a separate private `board.timing` snapshot, with a
five-second receipt-age limit. It does not create public GNSS liveness or change
interference baselines. Upgrade the collector before enabling the new firmware;
see [wire fields and private output](../../docs/OBSERVER-TELEMETRY.md).

The same versioned bytes appear on serial as `pulse_timing: sample=<hex>`.
Save a serial monitor log, then run from the `esp32` directory:

```sh
python3 tools/timing_plot.py timing.log --csv timing.csv
# Optional chart dependency: install matplotlib in your Python environment.
python3 tools/timing_plot.py timing.log --csv timing.csv --plot timing.svg
```

SVG, PNG and PDF are supported by the optional matplotlib renderer. Charts show
period error, wrapped RTC/GNSS phase and pulse count minus elapsed ESP seconds.
The CSV also includes widths, span-average ppm, loss/validity flags and counters.
Boots detected by decreasing uptime are plotted separately. Start a new capture
file after switching boards; serial samples contain no unique board identity.
With `[store].dsn` enabled, the collector persists every timing/environment sample
in its private `observer_samples` table, using the configured raw-evidence retention
and compression policies. The [FIFO journal](JOURNAL.md) saves
pulse totals in hourly checkpoints, preserving boot/version context without
writing each second to flash.

## Bench validation

A three-minute S3 capture on 2026-09-14 verified RTC CONTROL `0xc0`, OSCTRIM
`0x00` and 1 Hz activity on GPIO15. It recorded 179 RTC pulses and 52 GNSS pulses
across 178.951 ESP seconds; GNSS PPS began after reception improved, around
127 seconds into capture. Hardware counts matched captured rising edges on both
inputs, with zero queue drops. One GNSS gap broke its continuous span. The
collector decoder accepted all 179 emitted samples, including initial absence,
acquisition, the gap and stable pulse timing. A normal journal checkpoint write
occurred during RTC capture without loss. These checks do not qualify sustained
OTA/TLS load, full-day operation or absolute frequency accuracy.

## References

- [Microchip MCP79410/11/12 datasheet, DS20002266H](https://ww1.microchip.com/downloads/en/DeviceDoc/20002266H.pdf), CONTROL and square-wave/trim sections.
- [ESP-IDF 5.5 S3 MCPWM capture](https://docs.espressif.com/projects/esp-idf/en/v5.5/esp32s3/api-reference/peripherals/mcpwm.html), capture resolution and cache safety.
- [ESP-IDF 5.5 S3 pulse counter](https://docs.espressif.com/projects/esp-idf/en/v5.5/esp32s3/api-reference/peripherals/pcnt.html), automatic counter reset at limits.
- [u-blox M9 SPG 4.04 interface](https://content.u-blox.com/sites/default/files/u-blox-M9-SPG-4.04_InterfaceDescription_UBX-21022436.pdf), TIM-TP and CFG-MSGOUT.
- [u-blox SPG 4.04 release notes](https://content.u-blox.com/sites/default/files/GNSS-FW-SPG404_ReleaseNote_UBX-20036165.pdf), §4.2 timing limitation.
