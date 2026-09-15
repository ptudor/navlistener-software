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
- HDC2080: fresh 14-bit temperature and humidity conversion; heater disabled.
  Temperature follows the Rev C formula, including nominal 3.3 V supply
  compensation: `raw × 165 / 65536 − 40.5 + 0.08 × (3.3 − 1.8)` °C.
  The supply is assumed, not measured. Humidity is `raw × 100 / 65536` %RH.
- BMP388/BMP384: temperature and pressure compensated using the chip's factory
  trim and the pinned Bosch BMP3 SensorAPI. Fresh forced conversions use pressure
  8× and temperature 2× oversampling. Chip ID `0x50` does not distinguish these
  two variants. Pressure is local absolute pressure, without sea-level correction.
- The three temperatures remain separate. They measure different dies/locations;
  board heating can produce real differences. They are not averaged into an
  invented ambient temperature. There is no additional enclosure/site calibration.
- RTC flags and calendar describe the last hardware read, whose uptime is included.
  Battery backup enabled does not prove a battery is installed or retention works.
  RTC time is not used for GNF1 timestamps. The GNSS-only initialization policy stays.
- ATECC revision, lock state and RNG screening are boot diagnostics, with their
  check uptime. A screening pass is not entropy certification. Repeating output
  remains explicitly untrusted. No provisioning, key writes or locking occurs.
- Manifest EEPROM `0x50`: boot inspection action, independently validated factory
  EUI-64, and capability validation. Absent, unreadable, blank, invalid and replaced
  devices remain distinct. After fitting a chip, reboot to inspect it. Identity
  reporting does not initialize a blank chip or authorize replacement. Its EUI
  identifies that EEPROM; it never replaces the authenticated station identity or
  the separate RTC identity described in [the hardware contract](HARDWARE-OBSERVER.md#4-identity-and-trust).
- Spool allocation, queue/drop counters and free heap are sampled at report time.
  These counters describe the RAM buffer, not collector delivery or persistence.
  PPS presence/precision, battery presence, RTC factory EUI and ATECC serial are
  not measured by this report version; no successful status is fabricated for them.

Sensor failures clear that sample's validity and values. Other sensors continue.
Transient read failures recover on later samples; devices that failed initial
identification/configuration require a reboot. Invalid trim and conversion
timeouts are rejected. Source references:
[MCP9808 datasheet](https://ww1.microchip.com/downloads/en/DeviceDoc/25095A.pdf),
[HDC2080 Rev C](https://www.ti.com/lit/ds/symlink/hdc2080.pdf),
[BMP384 datasheet](https://www.bosch-sensortec.com/media/boschsensortec/downloads/datasheets/bst-bmp384-ds003.pdf),
[vendored Bosch source and license](../esp32/components/environment/vendor/bmp3/README.md).
Include the Bosch license with firmware binary distributions.

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
| 4 manifest EEPROM | 13 | action U8, EUI-valid U8, EUI bytes[8], capabilities-valid U8, board revision U8, component count U8 |
| 5 resources | 29 | spool-in-PSRAM U8, used bytes U32, capacity bytes U32, queued records U32, dropped records U64, free internal bytes U32, free PSRAM bytes U32 |
| 6 receiver context | 29 | supported mask U8, expected mask U8, tracked counts[8], validity U8, jamming U8, spoofing U8, MON-RF uptime U64, NAV-STATUS uptime U64 |
| 7 firmware | 1–32 | Printable ASCII application version (build git description when available) |

Environment mask bits 0/1/2 identify MCP/HDC/BMP respectively. Valid requires
ready. Temperature units are 0.01 °C, RH units 0.01%, pressure units Pa. Invalid
measurements must be encoded as zero and are served as JSON `null`; valid zero
is preserved. Valid temperature ranges: MCP/HDC −40…125 °C, BMP −40…85 °C;
RH 0…100%, pressure 30000…125000 Pa.

RTC flag bits: readable=1, oscillator-running=2, backup-enabled=4,
power-fail=8, validated running calendar=16. Unix seconds must be zero without
the last flag; validated calendars cover 2000–2099. Read/check/event uptimes
cannot exceed snapshot uptime. Last-read fields do not imply a fresh battery test.

ATECC lock enums: 0 unknown, 1 unlocked, 2 locked, 3 invalid lock byte.
RNG enums: 0 untested, 1 repetition-screening pass, 2 repeating output, 3 I/O error.
Manifest action enums: 0 I/O error, 1 initialization required, 2 recovery required,
3 replacement confirmation required, 4 invalid manifest, 5 use manifest, 6 absent.
EUI validity excludes all-zero/all-FF reads. Capability metadata is zero without
validation; EUI validity and capability validation are independent.

Receiver masks/count indices use u-blox GNSS IDs. Validity bits: current satellite
counts=1, current position fix=2, current MON-RF=4, current NAV-STATUS=8. Current
means no more than 15 seconds old. Count values are zero when stale; RF/status
values retain their last reading and must be interpreted with validity/uptime.
Jamming: 0 unknown, 1 OK, 2 warning, 3 critical. Spoofing: 0 unknown/deactivated,
1 no indication, 2 indication, 3 multiple indications. This is receiver context,
not a replacement for high-rate ReceptionData/JammingStats.

## Collector output and retention

`/gnss/api/v2/observers` adds optional `board` in authorized operator,
organization and collection views. Public projections exclude the entire record,
including identifiers, and board-only stations never create public observer rows.
The authenticated GNF1 context selects station and scope; the payload cannot.

`board.latest` contains `received_at`, nullable `sample_time`, `session`,
`sequence`, and `details`. Names/units inside `details.environment` are
`mcp9808_c`, `hdc2080_c`, `bmp388_bmp384_c`, `humidity_percent`, `pressure_pa`.
Component health and identifiers appear under `rtc`, `atecc`, `eeprom`,
`resources`, `receiver`, and `firmware`. Hardware status is separate from
administrative identity and never changes receiver capability/liveness counts.

`sample_time` is null when the feeder had no accepted wall-clock timestamp.
`received_at` is collector receipt time, including after spool replay. Uptime
orders samples within a boot; it does not establish UTC or replay age. `stale`
becomes true after 11 minutes without a receipt, or when a known sample time is
older than 11 minutes. Unstamped replay age cannot be determined from receipt
time alone. Duplicate/older sequence numbers within the same session do not
replace live samples.

`board.last_interference` retains the snapshot associated with the latest changed
event counter and, when available, the preceding report from the same boot as
`before`. That baseline can be up to a check-in interval old, and a report may
describe coalesced transitions. A new boot clears the preceding boot's context.
Retention is bounded live RAM state, evicted after one idle hour and cleared on
authorization-scope reset. It is **not a durable environmental historian**.
GNF1 spool/ACK/replay semantics remain unchanged; environmental reports do not
hold the navigation historian's durability watermark.

The shared [binary fixture](../testdata/observer_details_v1.hex) is checked by
the C encoder and Go decoder. Host tests cover conversion faults, reporting
cadence, receiver transitions, malformed records, TLS delivery, replay,
durability classification, baseline retention, expiry and private output.
