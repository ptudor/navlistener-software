# Observer board telemetry

GNSS observations retain their receiver cadence and priority. Board telemetry
provides environmental context and diagnostics through GNF1 `ObserverDetails`
(`frame_type=0x04`). The S3 producer and Go collector implement version 1.
Upgrade the collector before deploying this firmware: older collectors discard
unknown telemetry and acknowledge it as permanently malformed.

## Reporting policy

The board task samples temperature, humidity and pressure every 30 seconds.
It queues a complete report at startup, at a five-minute check-in, or when a
measurement has changed from the last queued report by at least:

| Measurement | Change threshold |
|---|---:|
| Any individual temperature | 0.5 °C |
| Relative humidity | 2 percentage points |
| Local absolute pressure | 100 Pa (1 hPa) |
| Sensor validity/readiness or RTC status | Any change |
| Humidity-heater state or run count | Any change |
| MAX thermocouple | 1 °C; any change in its faults |
| MAX IMU or magnetometer availability, still/moving state, FIFO overflows or realignments | Any change |

Ordinary change reports have a one-minute minimum interval. Comparing against
the last report accumulates slow drift instead of ignoring many small steps.
GNSS navigation and fast RF telemetry continue independently, in the higher
priority UART task; I2C conversion waits occur only in the board task.

Changes in receiver-reported MON-RF jamming or NAV-STATUS spoofing state request
a fresh environmental conversion and full report, with a five-second minimum
interval. Initial healthy/unknown readings establish state without a transition;
an initial alarm also requests a snapshot. Both alarm and recovery transitions
count. A monotonic event counter preserves evidence of transitions between board
polls. Closely spaced transitions are coalesced; the header carries the last
transition's time/type/states, not an exhaustive event log. Steady alarms do not
cause an environmental report on every GNSS epoch.

This trigger uses receiver indications, not the collector's learned detectors.
A collector-to-feeder request for an immediate report is not implemented. No
pressure/temperature threshold changes GNSS integrity decisions in this version.
The M9's "no spoofing indicated" state is not proof of authentic reception; see
[NAV-STATUS in the M9 interface description](https://content.u-blox.com/sites/default/files/u-blox-M9-SPG-4.04_InterfaceDescription_UBX-21022436.pdf).

## Measurements and identities

- MCP9808: signed ambient temperature, with alert bits excluded.
- HDC2080: fresh 14-bit temperature and humidity conversion. Its on-chip heater
  stays off except during the rare [condensation recovery](#humidity-sensor-heater-condensation-recovery).
  Temperature follows the Rev C formula, including nominal 3.3 V supply
  compensation: `raw × 165 / 65536 − 40.5 + 0.08 × (3.3 − 1.8)` °C.
  The supply is assumed, not measured. Humidity is `raw × 100 / 65536` %RH.
- HDC2022 alternate: the same fresh-conversion sequence and humidity formula,
  with the Rev A temperature formula `raw × 165 / 65536 − 40` °C. Both parts
  return manufacturer ID `0x5449` and device ID `0x07d0`, so only the manifest
  can say which is fitted: firmware measures the part the verified manifest lists
  at 0x40 (`SENSOR_HDC2080` or `SENSOR_HDC2022`) with that part's formula. It fails
  closed: without a usable manifest (an unconfigured board), or with a manifest
  that lists neither part or both, nothing at 0x40 is measured. The one exception
  is safety: if an HDC answers there with its heater left on by an earlier boot,
  firmware clears the heater bit and measures nothing. Tag 14 reports which part
  was used. At the same raw value the published formulas differ by 0.38 °C at
  nominal 3.3 V; this is a formula difference, not a measured calibration error.
- BMP388/BMP384: temperature and pressure compensated using the chip's factory
  trim and the pinned Bosch BMP3 SensorAPI. Fresh forced conversions use pressure
  8× and temperature 2× oversampling. Chip ID `0x50` does not distinguish these
  two variants. Pressure is local absolute pressure, without sea-level correction.
- MAX board MS5607 (tag 11): the PROM coefficients pass their CRC-4 (TE AN520)
  before any reading is used. Each sample is one pressure and one temperature
  conversion at OSR 4096 with the datasheet's first- and second-order
  compensation. Full accuracy is specified from 300 to 1100 mbar; readings from 10
  to 300 mbar or 1100 to 1200 mbar are flagged extended-range, and any height
  derived from pressure is a model result. Pressure is local absolute pressure.
- MAX board per-unit settings: the mains frequency for the thermocouple notch (60 Hz
  by default, or 50 Hz) and the motion profile (surface by default, or aerial) are
  chosen on the setup page and kept in the `nvf_sensor` NVS namespace, which the
  network configuration reset leaves alone. One image serves every unit; the
  reports carry the values in use. BLE setup through the Station app does not set
  them yet, so a unit set up that way keeps its stored values or the defaults.
- MAX board MAX31856 (tag 12): K type, 4-sample averaging, continuous conversion
  every 100 ms, open-circuit detection for a source below 5 kΩ, and the unit's
  mains notch. A reading is valid only when
  DRDY_N showed a new conversion and the fault register is clear; cold-junction
  faults also withhold the cold junction. The fault register is reported in every
  sample. The reading is a filtered conversion, not an instantaneous or
  PPS-synchronous probe temperature.
- MAX board ICM-45686 and MMC34160PJ (tag 13): the IMU's rate follows motion, like
  a CPU governor. It samples at 12.5 Hz while still and steps up from the first
  sample showing motion: to 50 Hz for the surface profile (vehicles and vessels,
  ±8 g and ±1000 °/s) or 100 Hz for the aerial profile (aircraft and drones, ±16 g
  and ±2000 °/s). It steps down after 30 s without motion. A sample shows motion when
  its acceleration is more than 0.1 g from 1 g in magnitude or from the tracked
  gravity vector (a 2 s average, so a unit parked on a new slope settles), or its
  rotation rate is above 5 °/s. The UI low-pass filters run at a quarter of the rate,
  so vibration above it, such as an idling engine's, is removed rather than aliased.
  The FIFO is drained on INT1, or after twice the watermark's time without it.
  The report carries the latest sample (while it is under five seconds old),
  counters since boot, and a window covering every valid sample since the previous
  queued report: its time-weighted mean acceleration and rotation rate, and the
  extremes of their magnitudes. While the unit is not accelerating, the mean
  acceleration is the gravity direction in the sensor's axes, from which incline
  follows once the mounting is known; subtract the acceleration GNSS velocity
  implies when it is. The server receives only this summary, whatever the rate.
  The magnetometer is measured at each environmental sample as a SET/RESET pair,
  which removes the bridge offset and reports it. Vectors are in each sensor's own
  axes; mounting (hard- and soft-iron) calibration, attitude and heading are not
  computed. A raw sample stream is not implemented.
- The three temperatures remain separate. They measure different dies/locations;
  board heating can produce real differences. They are not averaged into an
  invented ambient temperature. There is no additional enclosure/site calibration.
- RTC flags and calendar describe the last hardware read, whose uptime is included.
  Battery backup enabled does not prove a battery is installed or retention works.
  RTC time is not used for GNF1 timestamps. The GNSS-only initialization policy stays.
- ATECC revision, lock state and RNG screening are boot diagnostics, with their
  check uptime. A screening pass is not entropy certification. Repeating output
  remains explicitly untrusted. No provisioning, key writes or locking occurs.
- Manifest EEPROM `0x50`: boot inspection action, its independently validated 128-bit
  factory serial, and capability validation. Absent, unreadable, blank, invalid and
  replaced devices remain distinct. After fitting a chip, reboot to inspect it. Identity
  reporting does not initialize a blank chip or authorize replacement. The serial is the
  board identity the station is named after ([the hardware contract](HARDWARE-OBSERVER.md#4-identity-and-trust));
  it never replaces the authenticated station identity.
- Spool allocation, queue/drop counters and free heap are sampled at report time.
  These counters describe the RAM buffer, not collector delivery or persistence.
  Battery presence, RTC factory EUI and ATECC serial are not measured by this
  report version. The separate timing component below measures pulse inputs.

Sensor failures clear that sample's validity and values. Other sensors continue.
Transient read failures recover on later samples; devices that failed initial
identification/configuration require a reboot. Invalid trim and conversion
timeouts are rejected. Source references:
[MCP9808 datasheet](https://ww1.microchip.com/downloads/en/DeviceDoc/25095A.pdf),
[HDC2080 Rev C](https://www.ti.com/lit/ds/symlink/hdc2080.pdf),
[HDC2022 Rev A](https://www.ti.com/lit/ds/symlink/hdc2022.pdf),
[BMP384 datasheet](https://www.bosch-sensortec.com/media/boschsensortec/downloads/datasheets/bst-bmp384-ds003.pdf),
[vendored Bosch source and license](../esp32/components/environment/vendor/bmp3/README.md).
Include the Bosch license with firmware binary distributions.

## Humidity-sensor heater (condensation recovery)

The HDC2080/HDC2022 has an on-chip heater, `HEAT_EN` (CONFIG register `0x0E`,
bit 3). [TI SNAS678C](https://www.ti.com/lit/ds/symlink/hdc2080.pdf) section
8.3.3 says to use it only when a condensing condition is detected, to keep
measuring humidity while it is on, to turn it off once humidity reads at or near
0 %RH, and to keep measuring temperature through a cool-down of minutes before
returning to normal service. The NEO observer firmware implements that as a
defined, deliberately rare policy. Initialization still clears `HEAT_EN` on every
boot, and the heater is never enabled outside a run.

The defaults are engineering choices aimed at roughly two runs a year on a site
that condenses. They are **not yet derived from fleet data**; the humidity-dwell
counters below exist so the thresholds can be tuned from telemetry. Each value is
a `menuconfig` option under **HDC humidity-sensor heater (condensation recovery)**.

| Stage | Rule (default) | Kconfig |
|---|---|---|
| Trigger | Every valid HDC sample ≥ 98.0 %RH, continuously for ≥ 4 h | `NVF_ENV_HEATER_TRIGGER_RH_TENTHS`, `NVF_ENV_HEATER_TRIGGER_MINUTES` |
| Trigger | More than 30 min without a valid sample restarts the count | `NVF_ENV_HEATER_GAP_MINUTES` |
| Trigger | HDC temperature −20 … +60 °C | `NVF_ENV_HEATER_MIN_TENTHS_C`, `NVF_ENV_HEATER_MAX_TENTHS_C` |
| Trigger | ≥ 14 days of trusted UTC since the previous automatic run | `NVF_ENV_HEATER_INTERVAL_DAYS` |
| Run | Manual HDC and MCP9808 conversions every 10 s | `NVF_ENV_HEATER_STEP_SECONDS` |
| Stop `dry` | Humidity ≤ 5.0 %RH | `NVF_ENV_HEATER_DRY_RH_TENTHS` |
| Stop `timeout` | 300 s on-time | `NVF_ENV_HEATER_MAX_ON_SECONDS` |
| Stop `overtemp` | HDC temperature ≥ 80.0 °C | `NVF_ENV_HEATER_OVERTEMP_TENTHS_C` |
| Stop `bus_error` | Any I2C error in a heater step | — |
| Stop `sensor_lost` | No valid HDC conversion, or `HEAT_EN` read back clear (the part reset) | — |
| Cool-down | ≥ 10 min, and HDC temperature changed < 0.2 °C over the last 5 min | `NVF_ENV_HEATER_SETTLE_MINUTES`, `NVF_ENV_HEATER_STABLE_MINUTES`, `NVF_ENV_HEATER_STABLE_TENTHS_C` |
| Cool-down | Normal service resumes after 60 min regardless | `NVF_ENV_HEATER_RECOVERY_MAX_MINUTES` |

Sources for the limits: the heater draws 90 mA at 3.3 V, and with it on the part
must stay below 85 °C (THEATER −40 to 85 °C), hence the 80 °C stop. The humidity
element operates from −20 to 70 °C. The +60 °C start limit keeps the `3V3_SENS`
regulator within its junction limit while it carries the heater
([hardware contract §6.4](HARDWARE-OBSERVER.md#64-humidity--a-diagnostic-not-a-gnss-input)).
Build-time assertions reject inconsistent settings, such as a start limit at or
above the over-temperature stop.

**Counting.** The board task samples every 30 seconds (sooner for interference
snapshots), so four hours is about 480 samples. Invalid samples do not count
toward the four hours and do not break it; any valid sample below the trigger,
or a gap longer than 30 minutes between valid samples, restarts it.

**Trusted time.** The 14-day interval uses only wall-clock time the firmware
already trusts, the same sources the journal labels: GNSS UTC after three
advancing NAV-PVT solutions (newest at most two seconds old), otherwise a
validated running RTC calendar read in the last 30 seconds; this firmware sets
that RTC only from qualified GNSS UTC. SNTP time is never used. Without
trusted UTC no automatic run starts. The run's start time is saved in NVS
(`nvf_env/heater_utc`) *before* `HEAT_EN` is set; if the save fails, the heater
is not started and the four-hour count restarts. An unreadable record disables
automatic runs for that boot rather than risk a repeat inside the interval. A
start whose `HEAT_EN` does not read back set still counts as a run.

**Heater control.** `HEAT_EN` is changed by read-modify-write that preserves the
interrupt and measurement-mode bits and never writes the self-clearing soft-reset
bit, then confirmed by read-back. While heating, a separate board-task poll takes
a conversion step every 10 seconds, independent of the 30-second environmental
cadence: fresh HDC and MCP9808 conversions, then a `HEAT_EN` read-back. Turning
the heater off is attempted three times; if the read-back still does not confirm
it, an error is logged and the attempt repeats at every step. Until confirmed,
the state is `stopping` and the heater is treated as possibly on.

**Marking during a run and cool-down.** Whenever the heater state is not
`normal`, the environment component (tag 1) reports the HDC temperature and
humidity as invalid (JSON `null`); they are heater and cool-down readings, not
ambient values. The ready bit stays set. MCP9808 and BMP388/BMP384 readings
continue, and tag 10's state marks every such snapshot as taken while the heater
was on or the sensor was recovering. The MCP9808 is kept for a reason: its rise
during a run independently confirms that the heater actually ran, and its
before/peak/end values are in the run record. The sample that ends the cool-down
is the first normal HDC sample published again. Serial logs mark the same samples.

**Run record.** Each run logs its start (UTC and uptime), on-time, stop reason,
humidity before and at stop, and HDC and MCP9808 temperatures before, at peak and
at the end of the cool-down, when the heater turns off and again when normal
service resumes. Tag 10 carries the latest run of the current boot.

**Tuning data.** Tag 10 and each HDC log line carry the time spent at ≥ 95 %RH
and at ≥ 98 %RH since boot, in normal service only. An interval between two
consecutive valid samples counts when both are at or above the bucket and they
are at most 30 minutes apart. These buckets are fixed, not tied to the trigger
setting, so fleet data stays comparable across builds.

The policy is plain C with injected time and samples
([`env_heater.c`](../esp32/components/environment/src/env_heater.c)), covered by
the host test in `esp32/components/environment/test`. It is wired only into the
NEO observer build; the MAX and ZED-X20P boards do not run it yet.

## Wire contract, version 1

The normal 13-byte GNF1 record envelope supplies the reception timestamp and
`frame_type=0x04`. All four GNSS/SV/signal/frequency bytes must be zero. The body
is at most 1024 bytes. All multibyte values below are **big-endian**; signed
temperatures are two's-complement. No floats cross the wire.

| Body offset | Bytes | Meaning |
|---:|---:|---|
| 0 | 1 | Version = 1 |
| 1 | 1 | Reason bits: boot=1, change=2, check-in=4, interference=8; nonzero |
| 2 | 8 | Snapshot uptime, milliseconds since ESP boot; fresh conversions finish before this stamp |
| 10 | 4 | Receiver interference transition counter, since ESP boot |
| 14 | 8 | Last transition uptime, milliseconds |
| 22 | 1 | Last transition type bits: jamming=1, spoofing=2 |
| 23 | 1 | Last transition states: jamming bits 1:0, spoofing bits 3:2 |
| 24… | variable | Component TLVs: tag U8, length U16, then exactly length bytes |

Tags cannot repeat, including unknown tags. Tag zero is invalid. At least one
known tag is required; unknown tags are length-checked and skipped. Each report
is a complete snapshot of its supplied components, not a patch to older values.
Missing tags mean unreported/unsupported, not success. Unknown body versions,
invalid lengths/enums/ranges, and trailing partial TLVs are rejected.

| Tag | Length | Fields in order |
|---:|---:|---|
| 1 environment | 14 | validity U8, ready U8, MCP temperature I16, HDC temperature I16, BMP temperature I16, RH U16, pressure U32 |
| 2 RTC | 17 | flags U8, Unix seconds U64, read uptime U64 |
| 3 ATECC | 16 | boot check uptime U64, revision-valid U8, revision bytes[4], config lock U8, data lock U8, RNG result U8 |
| 4 manifest EEPROM | 40 | action U8, UID-valid U8, board UID bytes[35] (the typed wire field of [BOARD-IDENTITY.md](BOARD-IDENTITY.md)), capabilities-valid U8, board revision U8, component count U8 |
| 5 resources | 29 | spool-in-PSRAM U8, used bytes U32, capacity bytes U32, queued records U32, dropped records U64, free internal bytes U32, free PSRAM bytes U32 |
| 6 receiver context | 29 | supported mask U8, expected mask U8, tracked counts[8], validity U8, jamming U8, spoofing U8, MON-RF uptime U64, NAV-STATUS uptime U64 |
| 7 firmware | 1–32 | Printable ASCII application version (build git description when available) |
| 8 timing | 196 | Versioned GNSS/RTC pulse snapshot; layout below |
| 10 humidity heater | 58 | Versioned condensation-recovery state, dwell counters and latest run; layout below |
| 11 barometer | 10 | MAX board MS5607; layout below |
| 12 thermocouple | 12 | MAX board MAX31856; layout below |
| 13 motion | 95 | MAX board ICM-45686 and MMC34160PJ summary; layout below |
| 14 humidity sensor | 2 | version U8 = 1, part U8: not listed=0, HDC2080=1, HDC2022=2, both listed=3 |

Environment mask bits 0/1/2 identify MCP/HDC/BMP respectively. Valid requires
ready. Temperature units are 0.01 °C, RH units 0.01%, pressure units Pa. Invalid
measurements must be encoded as zero and are served as JSON `null`; valid zero
is preserved. Valid temperature ranges: MCP/HDC −40…125 °C, BMP −40…85 °C;
RH 0…100%, pressure 30000…125000 Pa.

RTC flag bits: readable=1, oscillator-running=2, backup-enabled=4,
power-fail=8, validated running calendar=16. Unix seconds must be zero without
the last flag; validated calendars cover 2000–2099. On the ZED/X20's MAX31328,
backup switching is automatic, so backup-enabled is always set with readable;
oscillator-running means the oscillator is enabled and its stop flag (OSF) is clear;
power-fail is never set, because a lost calendar shows as OSF instead. Read/check/event uptimes
cannot exceed snapshot uptime. Last-read fields do not imply a fresh battery test.

ATECC lock enums: 0 unknown, 1 unlocked, 2 locked, 3 invalid lock byte.
RNG enums: 0 untested, 1 repetition-screening pass, 2 repeating output, 3 I/O error.
Manifest action enums: 0 I/O error, 1 initialization required, 2 recovery required,
3 replacement confirmation required, 4 invalid manifest, 5 use manifest, 6 absent.
A valid UID is a 128-bit kind (`serial128` or `st_uid128`) that is neither all-zero nor
all-FF; an invalid UID is all zero bytes. JSON gives `board_uid_kind` and `board_uid`.
Capability metadata is zero without validation, and capability validation requires a
valid UID.

Tag 10 accompanies the environment component whenever the HDC is ready; it is
absent without an HDC. Collectors that predate it skip it as a bounded unknown
extension; tag 1 still withholds heater-affected HDC values from them.

| Offset within tag | Bytes | Meaning |
|---:|---:|---|
| 0 | 1 | Heater version = 1 |
| 1 | 1 | State: normal=0, heating=1, stopping (off not yet confirmed)=2, recovering=3 |
| 2 | 1 | Flags: trusted UTC available=1, last-run record readable=2 |
| 3 | 1 | Runs since ESP boot, saturating at 255 |
| 4, 8 | 4 each | Seconds at ≥ 95 %RH and at ≥ 98 %RH since boot, normal service only |
| 12 | 4 | Current condensing streak in seconds; zero unless normal |
| 16 | 8 | Unix seconds of the last automatic run, including earlier boots; zero if none |
| 24 | 8 | Latest run's start uptime in milliseconds |
| 32 | 4 | On-time in milliseconds; still counting while heating or stopping |
| 36 | 4 | Cool-down in milliseconds; still counting while recovering |
| 40 | 1 | Stop reason: none yet=0, dry=1, timeout=2, overtemp=3, bus_error=4, sensor_lost=5 |
| 41 | 1 | Validity: humidity before=1, at stop=2, HDC before=4, peak=8, end=16, MCP9808 before=32, peak=64, end=128 |
| 42, 44 | 2 each | Humidity before and at stop, 0.01 % |
| 46, 48, 50 | 2 each | Signed HDC temperature before, peak and at the end of cool-down, 0.01 °C |
| 52, 54, 56 | 2 each | Signed MCP9808 temperature before, peak and at the end of cool-down, 0.01 °C |

With zero runs this boot, offsets 24–57 are zero and the state is normal. A run
always has humidity and HDC temperature before it and a nonzero last-run time;
the last-run time requires a readable record. Only heating has no stop reason.
End-of-cool-down values and a nonzero cool-down time appear only once the state
is normal again. Invalid measurements are zero. Start, on-time and cool-down
cannot extend past the snapshot uptime, and ≥ 98 %RH time cannot exceed
≥ 95 %RH time.

## MAX board sensors (tags 11-13, version 1)

Each environmental report carries a tag when the manifest lists its parts: tag 11 the
MS5607, tag 12 the MAX31856, tag 13 the ICM-45686 or MMC34160PJ (a part that is not
listed reads as not responding). A MAX lists all of them; other boards omit the tags.
Invalid measurements are zero. Multi-byte fields are big-endian.

Tag 11, barometer:

| Offset | Type | Meaning |
|---:|---|---|
| 0 | U8 | Version = 1 |
| 1 | U8 | State: absent=0, calibration PROM rejected=1, ready=2 |
| 2 | U8 | Validity: measurement=1 (requires ready) |
| 3 | U8 | Flags: outside the 30000-110000 Pa full-accuracy range=1 |
| 4 | I16 | Temperature, 0.01 °C, −40…85 °C |
| 6 | U32 | Local absolute pressure, Pa, 1000…120000 |

Tag 12, thermocouple:

| Offset | Type | Meaning |
|---:|---|---|
| 0 | U8 | Version = 1 |
| 1 | U8 | State: not responding=0, configured=1 |
| 2 | U8 | Validity: thermocouple=1, cold junction=2 |
| 3 | U8 | Flags: new conversion (DRDY_N low)=1 |
| 4 | U8 | MAX31856 fault status register: open=1, over/under voltage=2, thermocouple low=4, high=8, cold junction low=16, high=32, thermocouple range=64, cold-junction range=128 |
| 5 | U8 | Configuration: bits 3:0 thermocouple type (3 = K), bit 4 the 50 Hz notch |
| 6 | I32 | Linearized, cold-junction-compensated thermocouple temperature, 0.01 °C |
| 10 | I16 | Cold-junction temperature, 0.01 °C |

Validity needs a new conversion; the thermocouple also needs a clear fault
register, and the cold junction needs none of its three faults. A converter that
is not responding reports no validity, flags or faults.

Tag 13, motion:

| Offset | Type | Meaning |
|---:|---|---|
| 0 | U8 | Version = 1 |
| 1, 2 | U8 each | IMU state, magnetometer state: not responding=0, ready=1 |
| 3 | U8 | Validity: latest IMU sample=1, window=2, magnetometer=4 |
| 4 | U8 | Motion profile: surface=0, aerial=1 |
| 5 | U8 | Governor: still=0, moving=1 |
| 6 | U16 | IMU output data rate in force, 0.1 Hz |
| 8 | U8 | Accelerometer range, ±g |
| 9 | U16 | Gyroscope range, ±°/s |
| 11, 17 | I16×3 each | Latest accelerometer and gyroscope counts; one count is range/32768 |
| 23 | I16 | Latest IMU die temperature, 0.01 °C in 0.5 °C steps |
| 25, 29, 33, 37 | U32 each | FIFO packets decoded, FIFO-full events (samples overwritten), FIFO realignments, rate changes; since boot |
| 41 | U32 | Window: valid samples since the previous queued report |
| 45 | U32 | Window: the time those samples cover, ms |
| 49, 55 | I16×3 each | Window: time-weighted mean accelerometer and gyroscope counts |
| 61, 63 | U16 each | Window: minimum and maximum acceleration magnitude, mg |
| 65 | U16 | Window: maximum rotation-rate magnitude, 0.1 °/s |
| 67 | U64 | Uptime of the latest IMU sample, ms |
| 75 | I16×3 | Magnetic field with the bridge offset removed, 1/2048 G per count |
| 81 | U16×3 | Bridge offset, raw counts (null field reads 32768) |
| 87 | U64 | Uptime of the magnetometer measurement, ms |

Sample uptimes cannot exceed the report uptime. A valid window covers at least one
sample and some time; the window's time over its sample count gives the mix of
rates. Missing IMU samples can be estimated from the rate history, the packet
counter and uptime; overflow events mark when the FIFO filled.

Receiver masks/count indices use u-blox GNSS IDs. Validity bits: current satellite
counts=1, current position fix=2, current MON-RF=4, current NAV-STATUS=8. Current
means no more than 15 seconds old. Count values are zero when stale; RF/status
values retain their last reading and must be interpreted with validity/uptime.
Jamming: 0 unknown, 1 OK, 2 warning, 3 critical. Spoofing: 0 unknown/deactivated,
1 no indication, 2 indication, 3 multiple indications. This is receiver context,
not a replacement for high-rate ReceptionData/JammingStats.

## Timing component (tag 8, version 1)

S3 firmware sends a timing-only record once per second, with reason `check-in`
and zero interference header fields. It does not trigger sensor conversions or
replace the environmental event baseline. Upgrade collectors before firmware:
older collectors that know only tags 1–7 reject an all-unknown timing-only record.

| Offset within tag | Bytes | Meaning |
|---:|---:|---|
| 0 | 1 | Timing version = 1 |
| 1 | 1 | Capture clock = 1 (ESP APB) |
| 2 | 1 | RTC square-wave state: unknown=0, enabled 1 Hz=1, oscillator stopped/invalid calendar=2, alarm/coarse-trim conflict=3, I/O error=4 |
| 3, 4 | 1 each | RTC register readback: MCP79412 CONTROL and OSCTRIM, or MAX31328 control (`0x0e`) and aging offset (`0x10`) |
| 5 | 1 | Validity: relative phase=1, recent TIM-TP metadata=2 |
| 6, 7 | 1 each | Next-pulse TIM-TP flags and reference byte; zero when unavailable |
| 8 | 4 | Actual capture resolution in Hz; zero if unavailable |
| 12 | 4 | Dropped capture queue entries, saturating counter |
| 16 | 8 | Capture start uptime in milliseconds |
| 24 | 4 | Signed RTC minus GNSS phase in ticks, modulo one second in [−0.5, +0.5) seconds; zero when invalid |
| 28 | 8 | TIM-TP receipt uptime in milliseconds; zero when unavailable |
| 36, 116 | 80 each | GNSS, then RTC channel record |

Each channel has the following layout:

| Offset | Type | Meaning |
|---:|---|---|
| 0 | U32 | Flags: enabled=1, hardware count valid=2, fresh rising edge=4, period valid=8, width valid=16, continuous span valid=32 |
| 4, 8 | U32 each | Last period and high width, ticks; zero when invalid |
| 12, 16 | U32 each | Minimum/maximum accepted period since capture start |
| 20 | U32 | Capture discontinuities |
| 24 | U64 | Missing-pulse estimate from observed gaps; separate from actual counts |
| 32, 40 | U64 each | Captured rising-edge records; independent hardware pulse count |
| 48, 56 | U64 each | Continuous span ticks and number of complete rising-to-rising intervals |
| 64 | U64 | Last captured rising-edge uptime in milliseconds |
| 72 | U32 | Hardware counter discontinuities; any ambiguity clears count-valid for this boot |
| 76 | U32 | Reserved zero |

An edge is fresh for 1.5 seconds. Period/width/span validity clears when stale;
counts, extrema and previous span totals remain historical evidence. Zero counts
are valid for an enabled counter that has received no pulses. An unavailable
capture has zero channel flags. Units, clock assumptions, counter limits and
receiver metadata caveats are in [TIMING.md](../esp32/docs/TIMING.md).

## Collector output and retention

`/gnss/api/v2/observers` adds optional `board` in authorized operator,
organization and collection views. Public projections exclude the entire record,
including identifiers, and board-only stations never create public observer rows.
The authenticated GNF1 context selects station and scope; the payload cannot.

`board.latest` is omitted until an environmental/health sample arrives. It contains `received_at`, nullable `sample_time`, `session`,
`sequence`, `hardware_trust`, and `details`. Names/units inside `details.environment` are
`mcp9808_c`, `hdc2080_c`, `bmp388_bmp384_c`, `humidity_percent`, `pressure_pa`.
The existing `hdc2080_c` field carries the HDC2080 or HDC2022 temperature; the
top-level `humidity_sensor` (tag 14) says which part the manifest listed:
`hdc2080`, `hdc2022`, `not_listed` or `conflicting` (the last two measure nothing).
Component health and identifiers appear under `rtc`, `atecc`, `eeprom`,
`resources`, `receiver`, and `firmware`. `humidity_heater` carries tag 10:
`state`, `trusted_utc`, `last_run_readable`, `runs_since_boot`,
`humidity_at_least_95_s`, `humidity_at_least_98_s`, `condensing_streak_s`,
nullable `last_run_unix_seconds`, and `latest_run` (omitted without a run this
boot) with `start_uptime_ms`, `on_ms`, `recovery_ms`, nullable `stop_reason`,
and nullable `humidity_before_percent`, `humidity_at_stop_percent`,
`hdc2080_before_c`, `hdc2080_peak_c`, `hdc2080_end_c`, `mcp9808_before_c`,
`mcp9808_peak_c` and `mcp9808_end_c`. MAX boards add `barometer` (`state`,
nullable `temperature_c` and `pressure_pa`, `extended_range`), `thermocouple`
(`state`, `type`, `mains_notch_hz`, `new_conversion`, `faults` as a list of
names, nullable `thermocouple_c` and `cold_junction_c`) and `motion`
(`imu_state`, `magnetometer_state`, `motion_profile`, `moving`, `imu_rate_hz`,
`accel_range_g`, `gyro_range_dps`, `imu_packets`, `imu_fifo_overflows`,
`imu_fifo_resyncs`, `imu_rate_changes`, and when valid `latest` with `uptime_ms`,
`accel_counts`, `gyro_counts`, `accel_g`, `gyro_dps` and `temperature_c`; `window`
with `samples`, `span_ms`, `accel_mean_g`, `gyro_mean_dps`, `accel_min_g`,
`accel_max_g` and `gyro_max_dps`; `magnetometer` with `uptime_ms`, `field_counts`,
`field_microtesla` and `bridge_offset_counts`). Hardware status is separate from
administrative identity and never changes receiver capability/liveness counts.

`hardware_trust` is `none`, `open`, `test` or `trusted`: what the collector
verified from the hardware evidence of the session that delivered the sample
([commissioning](COMMISSIONING.md)). It is the one field of a sample the device
did not write. Everything under `details`, including `details.update.trust_profile`,
is the device's own account; a device that reports the trusted track in a sample
whose `hardware_trust` is not `trusted` has not proved it. The value is `none`
for software feeders, for a collector that pins no manufacturer keys, and for
evidence that failed verification.

`sample_time` is null when the feeder had no accepted wall-clock timestamp.
`received_at` is collector receipt time, including after spool replay. Uptime
orders samples within a boot; it does not establish UTC or replay age. `stale`
becomes true after 11 minutes without a receipt, or when a known sample time is
older than 11 minutes. Unstamped replay age cannot be determined from receipt
time alone. Duplicate/older sequence numbers within the same session do not
replace live samples.

`board.timing` contains a separate `BoardSample`, with pulse data in
`details.timing`. Raw ticks/counts accompany derived `period_ns`, `width_ns`,
`span_phase_ns` and span-average `period_error_ppm`; invalid derived values are
JSON null. Positive period error means the input period is longer than a nominal
ESP second. `rtc_minus_gnss_phase_ns` is wrapped phase, not UTC offset.
`timing_stale` becomes true after five seconds without a receipt, or with an older
known sample timestamp. Individual channel freshness is separate. Next-pulse
TIM-TP metadata is not associated with a specific captured edge.

`board.last_interference` retains the snapshot associated with the latest changed
event counter and, when available, the preceding report from the same boot as
`before`. That baseline can be up to a check-in interval old, and a report may
describe coalesced transitions. A new boot clears the preceding boot's context.
Retention is bounded live RAM state, evicted after one idle hour and cleared on
authorization-scope reset. This live cache is separate from the persistent
`observer_samples` historian described below. With the historian enabled, board
samples now hold the GNF1 durability watermark until their transaction commits;
without a database, acknowledgment still means receipt only.

The shared [binary fixture](../testdata/observer_details_v1.hex), which includes
a completed heater run in tag 10, is checked by the C encoder and Go decoder.
Host tests cover conversion faults, reporting cadence, receiver transitions,
malformed records, TLS delivery, replay, durability classification, baseline
retention, expiry and private output.

The [timing fixture](../testdata/observer_timing_v1.hex) is shared by the C encoder,
Go decoder and serial plot tests. Timing shares the same private audience rules
and bounded live retention; durable storage uses the separate table below.

## Persistent environmental and clock history

When `[store].dsn` is configured, every valid ObserverDetails sample enters the
existing bounded batch writer. `observer_samples` is a separate private TimescaleDB
hypertable; board samples never enter `nav_frames` or public feed projections.
It stores the original wire body, decoded JSON (including all three temperatures,
humidity, pressure, clock counts/ticks and validity), collector receipt time,
nullable feeder sample time, boot session/sequence and immutable receipt-time
ownership/publication context. Source uptime remains inside the JSON.

The `(source, session, sequence)` replay key is claimed in the **same transaction**
as navigation and board inserts. A failed transaction rolls back its claims;
reconnect replay can retry it. A committed sample is stored once within the ledger
retention window, including after a daemon restart. Queue overflow and exhausted
transient retries withhold acknowledgment. A deterministically invalid database
row follows the existing explicit quarantine policy. GNSS live processing remains
independent of database latency, and the board's finite PSRAM capacity still limits
outage survival. Records already acknowledged by older live-only collectors cannot
be recovered retroactively.

Board samples inherit `[store].raw_retention` (default `7 days`) and
`compress_after` (default `1 day`). The separate table compresses by organization,
source and sample kind. Time partitions use collector ingest time, so an unknown
or replayed feeder clock cannot immediately age a newly received sample out.
Retention deletes old chunks automatically; it is not a keep-forever archive.
[Timescale retention policies](https://github.com/timescale/Tiger-Data-Docs/blob/main/src/content/docs/reference/timescaledb/data-retention/add_retention_policy.mdx)

`navlistener_store_board_rows_total{kind="environment"|"timing"}` counts committed
samples. Existing writer queue/error/drop metrics and historian health apply.
`navlistener_store_rows_total` now counts navigation and board records together.
The live observer API stays separate; historical board samples are currently
queried through authorized database access, not a public HTTP endpoint.

For example, on an administrator's private database connection:

```sql
SELECT received_at, sample_time, source_session, source_seq, data
FROM observer_samples
WHERE source_id = 'observer-example'
  AND received_at >= now() - interval '1 hour'
ORDER BY received_at, source_seq
LIMIT 10000;
```

Use `kind = 'timing'` for clock history or `kind = 'environment'` for environmental
and health snapshots. Plot using sample uptime within a boot and preserve validity
flags; an unknown UTC sample time remains NULL. Grant access only to authorized
operators; GNSS publication permission does not authorize sharing sensor identities
or board clock history. Existing deployments add the table and policies at daemon
startup; no existing navigation rows are rewritten.

Each row also stores the enrolled `operational_authority_id` and nullable
`manufacturer_authority_id`, plus immutable `authority_evidence` signer/issuer
pins. Software stations have no manufacturer. These private historian fields do
not bypass metadata privacy in public projections. Each row stores the delivering
session's `hardware_trust` and the
`commissioning_fingerprint` of the record that established it, beside the other
receipt-time authority columns. Rows stored before those columns existed read
`none` and an empty fingerprint.
