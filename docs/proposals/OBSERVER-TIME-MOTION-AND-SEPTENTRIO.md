# Observer time recovery motion profiles and Septentrio integration

Status: **topics 1 and 2 implemented in software**, 2026-10-17; physical RTC
retention/service qualification remains pending. Topic 3 remains separate work.
The baseline descriptions below describe behavior before this implementation.
Current interfaces are documented in [RTC service](../../esp32/docs/RTC-SERVICE.md)
and [observer installation](../OBSERVER-INSTALLATION.md). Receiver dynamics
control and SBF integration remain outside this software change.

The observer needs explicit service and time-bootstrap behavior, installation
profiles that describe expected movement, and a complete SBF path for a custom
mosaic-G5 P3 board. The existing RTC drivers, station integrity engine and board
identity architecture provide useful foundations. SBF capture alone does not
provide the receiver integration needed for a fleet observer.

## Implementation workstreams

The work divides into software for topics 1 and 2 and hardware for topic 3.
The technical requirements and acceptance cases below are the shared reference.

| Topic | Scope | Completion evidence |
|---|---|---|
| 1. RTC service and first boot | Software: source-aware system-clock bootstrap, explicit RTC service/factory-time operation, continuity and maintenance records, recovery from invalid time. | Host tests for source selection, stale/invalid time, service writes and failures; target build; a separate record of physical retention and powered-swap checks when hardware is available. |
| 2. Installation and motion analysis | Software: compatible installation profiles, configurable displacement/kinematic limits, explicit portable relocation/gap policy, candidate survey and pinned reference handling. | Configuration/validation tests and replay scenarios for each installation, uncertainty gates, continuous implausible motion, gaps and relocation; existing fixed/mobile behavior preserved unless explicitly configured otherwise. |
| 3. Custom mosaic-G5 P3 observer | Hardware: a dedicated P3 schematic and PCB in the hardware repository, combining suitable X20 circuits with the MAX Measure's open placement approach and fewer sensors. | Editable design, reviewed BOM and pin/interface contract, electrical/layout checks, assembly/bring-up procedure, and an explicit list of remaining physical qualification. |

For topic 3, preserve the factory-serial EEPROM identity, provisionable ATECC,
manifest, independent RTC, power protection/control, service access and status
indication. Select environmental sensing and Ethernet deliberately; omit the
MAX Measure's thermocouple circuits from the P3 design unless separately needed.
Check the existing mosaic-G5 P3 component and observer project notes in
ExtraParts as design context, with the requirements here governing the new work.

SBF receiver, transport and decoder implementation remains a software dependency
of topic 3. Record the hardware's interface requirements for that work without
expanding topics 1 and 2 into a second receiver-integration project. Direct that
future implementation toward the production ESP32 and collector path. Native
receiver USB supplies diagnostics and reference captures through existing tools;
neither a separate USB parser project nor an evaluation kit is a prerequisite.

## RTC service and first boot

### Baseline before implementation

The MCP79412 driver checks oscillator state and calendar validity. Its PWRFAIL
flag records loss of the primary supply while backup is available; it is not a
battery-replacement detector or proof that time was lost. Firmware preserves
power-failure timestamps in NVS before a weekday-register write clears them.
DS3231M/MAX31328 firmware rejects a calendar with the oscillator-stop flag set.
Neither driver's backup-enabled indication proves a cell is fitted or healthy.
There is no implemented battery voltage/presence measurement.

With valid main power at the RTC throughout a battery swap, the calendar can
continue without a reset. A powered swap can therefore leave no observable RTC
event. The DS3231M manufacturer specifically recommends applying VCC before
battery attachment to avoid contact-bounce corruption; see the
[DS3231M datasheet, Initial VBAT Attachment](https://www.analog.com/media/en/technical-documentation/data-sheets/DS3231M.pdf).
Keep the applicable receiver's main rail powered too when servicing a shared
backup cell. Check each board's backup wiring rather than assuming every board
shares one cell.

A stopped/invalid RTC is initialized automatically after three advancing GNSS
UTC reports spanning at least two seconds, with valid fix/date/time, fully
resolved UTC and reported time uncertainty at most 100 ms. Writes are read back
and checked for advancing seconds. A running valid calendar is retained even
if it disagrees with later GNSS time. There is no console command to force a
running RTC to a new time. A DS3231M/MAX31328 whose OSF cannot be cleared enters
an unusable state for the boot. See
[`rtc.c`](../../esp32/components/rtc/src/rtc.c) and
[`rtc_policy.c`](../../esp32/components/rtc/src/rtc_policy.c).

The firmware's GNSS-qualified time is not authenticated GNSS time. A retained
RTC is a continuity reference, and its valid status does not establish the
correct date or independent absolute accuracy.

| Function | Without a valid RTC or qualified GNSS UTC |
|---|---|
| Receiver parsing, local status and RAM spooling | Continue using uptime and boot/session sequence identities. |
| Lifecycle journal | Records unknown UTC with boot and uptime; a later time anchor does not recover the exact capture dates of all earlier records. |
| Observation reception stamp | Zero until the system wall clock is plausible; the collector can use its receipt time when records arrive. Backlogged capture time is then unknown. |
| Collector TLS and WireGuard | Certificate date verification needs a usable system wall clock; WireGuard waits for one. Capture can continue while the uplink cannot connect. |
| HTTPS OTA | Download waits up to 30 seconds for a plausible system clock, then times out. Local first-boot confirmation does not depend on SNTP. |
| Edge reception forecasts | Acceptance checks the system wall clock, so a valid RTC alone does not make forecasts usable. |

There are currently two time paths. Qualified GNSS/RTC time feeds the journal,
board reports and updater metadata decisions. SNTP sets the system clock used
by reception stamps, TLS, WireGuard and forecasts. The RTC driver does not call
`settimeofday`; factory-setting the RTC alone does not close that gap. Inspect
[`board.c`](../../esp32/main/board.c),
[`main.c`](../../esp32/main/main.c),
[`update_runtime.c`](../../esp32/components/ota/src/update_runtime.c),
[`ota_download.c`](../../esp32/components/ota/src/ota_download.c) and
[`reception_runtime.c`](../../esp32/main/reception_runtime.c).

### Requirements and acceptance

Add a time service that publishes source, age, uncertainty and discontinuities,
and bootstraps the system clock from a qualified source before time-dependent
network operations. Preserve monotonic timers and record a clock change without
rewriting earlier capture timestamps. Network time availability and independent
time evidence must remain distinguishable.

Add a USB service operation to set/verify the RTC from qualified GNSS UTC or a
factory time reference. Record the old calendar and status before writing, the
source, readback result and a maintenance reason. An explicit service action
should allow retrying a part marked unusable. A battery-service event records
operator maintenance; automatic detection requires additional hardware such as
an isolated cell measurement or holder contact. Neither voltage nor a running
oscillator alone identifies a new battery.

Factory time setting can occur before or after installing the manufacturer
record: the commissioning statement has no wall-clock precondition, and its RTC
fields are historical metadata rather than board identity. Perform time setting
and a power-off retention test before the first network acceptance test. The
current USB commissioning commands do not implement factory time setting; a
factory reference device needs a defined interface. RTC maintenance must not
trigger re-enrollment or replacement of board credentials. See
[COMMISSIONING.md](../COMMISSIONING.md#2-identities-on-one-board).

Acceptance should cover a fresh board without a cell, a powered cell swap, loss
of both supplies, no network time, no GNSS fix, failed RTC writes and recovery,
and a retained but incorrect calendar. Verify the time source seen by TLS and
forecasts separately from updater metadata and the journal.

## Installation and motion analysis

### Baseline before implementation

`[[integrity.station]]` accepts `fixed` or `mobile`. A fixed station may have a
surveyed position; a mobile station requires `max_speed_mps`.
`[[reception.station]]` implies a fixed site and also anchors the stationary
received-power model. These are collector analysis settings; they do not
configure receiver dynamics or start a receiver survey-in.

Fixed checks compare position and velocity against the installation. Default
instantaneous horizontal bands start at 15 m inconsistent and 50 m unassured;
the sustained mean thresholds are 8 m and 25 m. They do not enforce one metre.
Mobile `motion_bound` tests displacement against maximum speed times elapsed
receiver time, both position accuracies and a margin. It does not directly cap
reported speed, acceleration or turning. Position-versus-velocity consistency
is a separate check. There are no pedestrian, bicycle, road-vehicle or aircraft
presets and no implemented survey workflow or receiver dynamics control.

A receiver-time gap greater than ten seconds clears position-difference history,
but the mobile motion reference can remain for up to ten minutes. Loss of fix
does not itself replace that reference. Receiver restart/backward time reset
does clear it. Thus a portable unit cannot currently declare that relocation
across a data gap is normal. See
[`position.go`](../../go/internal/integrity/position.go),
[`profile.go`](../../go/internal/integrity/profile.go) and
[`config/integrity.go`](../../go/internal/config/integrity.go).

### Requirements and acceptance

The [G5 firmware reference v1.0.1](https://docs.sparkfun.com/SparkFun_GNSS_mosaic-G5_P3/assets/component_documentation/firmware/v1.0.1/mosaic-G5%20Firmware%20v1.0.1%20Reference%20Guide.pdf)
documents `setReceiverDynamics`/`getReceiverDynamics`, including static,
pedestrian, automotive and UAV motion models. This is a receiver capability to
integrate; the collector's current fixed/mobile configuration does not select it.

Keep three distinct concepts: the antenna installation, the receiver's dynamics
configuration, and the evidence used to establish a reference position. Record
the effective installation policy in the assessment/configuration hash.

| Installation | Analysis policy to add |
|---|---|
| Fixed structure | Explicit displacement tolerance, surveyed-reference uncertainty and quality requirements. A one-metre rule needs measurements that resolve that movement; standalone fix noise near a wall must not be called physical displacement. |
| Pedestrian or bicycle | Speed and acceleration limits, plus an explicit rule for continuity after missing data. |
| Road vehicle | Reported-speed and displacement limits, acceleration and lateral-acceleration/turn plausibility. A speed bound alone cannot reject every impossible circular trajectory. |
| Aircraft | Separately configured speed, vertical motion and acceleration envelopes; do not inherit road limits. |
| Portable test | New segment after a configured gap or declared relocation; reset motion/clock comparison windows and report unavailable while they refill. Preserve earlier alarms and raw evidence. |

Survey-in should produce a candidate reference with duration, sample count,
dispersion, solution quality and source recorded. An operator accepts and pins
the reference; continuously averaging suspect fixes into it would hide slow
movement. Learning a position from this receiver is not independent confirmation
of its absolute location. A site change also needs a new reception/power-model
identity so a previous location's sky history is not reused.

Keep receiver survey/timing/static modes deliberate: a receiver configured to
hold a fixed position may suppress the movement that an independent position
check is meant to observe. Map supported dynamics only after checking the exact
model and firmware, and read back the effective configuration. Missing input
and insufficient accuracy should remain unavailable, with liveness handled
separately. Test normal stops, reversals, tight turns, fix-quality changes,
short/long gaps, relocation and implausible motion.

## Custom mosaic G5 P3 observer

### Current software readiness

| Layer | Current support | Remaining work |
|---|---|---|
| Collector SBF input | TCP dial capture, framing/CRC, original header/revision/body persistence and exact reconstruction | Real P3 captures and block-specific decoding. |
| C feeder | UBX input | SBF source/configuration support. |
| ESP32 receiver | UBX parser, MON-VER discovery and UBX configuration; on a P3 the port is only listened to, with nothing transmitted | SBF framing, Septentrio command/reply handling, COM2 flow control, capability discovery and status/time/solution adaptation. |
| GNF1 push and enrollment | `ubx`/`rtcm` feed grants; SBF rejected | Versioned SBF representation preserving the full block header; feeder, collector, grant validation, spool and replay updates together. |
| Station analysis | Receiver-neutral solution record and integrity engine | Map P3 solution, uncertainty, time, tracking and RF evidence; retain unavailable fields rather than inventing UBX equivalents. |
| Board platform | Identity EEPROM, manifest, ATECC, commissioning, OTA, RTC, panel and selected sensors; the P3 board ID, revision 1 parts list, pins, OTA target and rail monitor | Receiver reset control, SHT35 and HIH8121 drivers. |

The present GNF1 DATA `frame_type` cannot carry an SBF block number. Widening or
truncating it locally is not a transport design. A new representation needs
explicit format/version negotiation and compatibility tests. Central raw-nav
decoders can reuse signal-level GNSS parsers after SBF unpacking; receiver PVT,
raw measurements and raw navigation bits remain separate inputs.

Use the X20 power/protection, rail control and monitoring, identity and service
patterns, and the MAX Measure's open placement approach as design starting
points. Keep an ESP32-S3, independent RTC, factory-serial EEPROM, provisionable
ATECC, receiver-native USB, UART/PPS, reset/recovery access and status indication.
Choose environmental sensors and Ethernet for this board's needs; thermocouples,
IMU and magnetometer are not prerequisites for collector participation. The MAX
Measure layout is still unrouted, so it supplies placement ideas rather than a
qualified routed template. Put schematic/layout implementation in the hardware
repository.

The target is **mosaic-G5 P3, ordering code 410501**. The
[G5 hardware manual v2.0.0](https://www.septentrio.com/system/files/support/mosaic-g5_hardware_manual_v2.0.0.pdf)
specifies 3.3 V UART/PPS, at least 500 mA supply capability, and internal antenna
bias via VANT. Review the G5 power, RF, USB and unused-pad requirements afresh;
do not transplant the u-blox bias tee or an X5 backup circuit. For home assembly,
plan stencil/reflow and moisture handling for the 54-pad LGA: the manual
recommends a 0.150 mm stencil, limits reflows to two and specifies no-clean
processing. A roomier board helps access but does not expose hidden joints.
The manufacturer's [G5Dot design](https://github.com/septentrio-gnss/G5Dot)
provides another circuit reference.

### Implementation and bring-up

1. Develop the custom P3 board as the receiver integration platform, exposing
   native receiver USB, UART/PPS and test access. In the receiver-integration
   software work, prioritize ESP32 SBF, push transport, telemetry adaptation and
   central navigation decoding. Bound UART output by measured throughput.
2. Record model, serial, firmware, permissions and actual emitted blocks on the
   receiver. Check required raw navigation bits as well as raw measurements;
   advertised tracking and an SBF reference entry do not prove output support.
   Native USB can establish receiver communication independently of ESP32
   firmware and provide diagnostic captures through the existing collector
   SBF connector and a serial-to-TCP bridge.
3. Retain real fixtures and verify framing, CRC, revisions, reconnect, sustained
   rate, exact persistence/replay and decoded results against equivalent u-blox
   observations. USB and UART carry the same SBF format; the existing USB capture
   route does not itself provide the decoded results of the production path.
4. Qualify the assembled custom board's rails, RF, identity, RTC retention,
   provisioning and recovery. Validate PPS electrically and its time meaning;
   USB arrival timestamps do not measure PPS accuracy. Commission the board
   through the existing identity lifecycle after bring-up.

A custom board is a reasonable integration platform when receiver-native USB
and test access let hardware and firmware be debugged independently. An eval
kit can still serve as a known-good comparison, but the software capture step
can run on either platform. Full fleet support requires the transport, telemetry
and decoding work regardless of which PCB supplies the first captures.
