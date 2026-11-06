# Environmental monitoring and integrity audit

Date: 2026-11-06

## Remediated verdict

Environmental readings now reach the integrity engine. NavListen detects and retains
instrument failures and station-local environmental departures, compares qualified pressure
changes across surveyed neighbours, and can use one uncertainty-bounded barometric model as
lower-only corroboration of GNSS vertical movement. Raw temperature, RH, and ordinary weather
remain advisory and cannot themselves claim jamming or spoofing.

| Result | Status after remediation |
|---|---|
| Works now | Per-sensor validity and location survive ingestion; sensor-health, magnitude, rate, 24-hour sustained change, dew-point proximity, baseline freeze, filter, recovery hold, staleness, regional pressure, and qualified barometric-height checks produce explainable results. Events are persisted, streamed, included in current conditions and evidence bundles, and visible in Integrity Station. |
| Collected but intentionally not fused | Enclosure temperature/RH, regulator heat-spreader temperature, RTC oscillator temperature, heater activity, and unqualified pressure remain telemetry or advisory context. They do not become independent GNSS votes. |
| Still limited | The evaluator's 24-hour baseline and pressure windows are memory-resident and restart cold even though raw reports remain in the historian. Reports have one snapshot timestamp rather than per-conversion timestamps. Regional pressure can identify a local residual but cannot by itself diagnose weather, a vent transient, enclosure effects, or a plausible sensor fault. The native app shows separate histories and event-time assessment evidence but does not yet provide one aligned multi-series environment/clock/reception/RF timeline. Field characterization is still required before enabling any barometer policy. |

The default environmental limits and all new barometric operating points are listed in
[`docs/INTEGRITY.md`](INTEGRITY.md), validated in `Profile`, included in the station configuration
hash, and copied into assessment/event threshold evidence. Barometric fusion is off unless an
operator explicitly qualifies one surveyed fixed station's exact vented sensor and installed
relative uncertainty. This avoids silently treating the presence of telemetry as calibration.

### Current end-to-end traceability

| Stage | Current implementation and behavior |
|---|---|
| Sensor measurement and firmware reporting | Manifest selection, forced conversions, cleared invalid samples, about-30-second acquisition, report pacing, units, and ready/valid masks remain at `esp32/main/board.c:252-287,940-1072`, `esp32/components/environment/src/env_sensors.c:103-164`, and `esp32/components/observer_report/src/observer_report.c:25-78`. Firmware pacing thresholds remain transport controls, not anomaly thresholds. |
| Transport and collector parsing | ObserverDetails remains a typed authenticated GNF1 spool record. Main values decode as nullable °C, %RH, and Pa at `go/internal/ingest/observer_details.go:13-73,175-207`; MS5607 state, calibration rejection, validity, and extended range remain explicit at `go/internal/ingest/observer_max.go:5-13,82-110`. |
| Storage and history | Every transmitted snapshot retains receipt time, optional sample time, boot session, sequence, raw bytes, and decoded fields in `observer_samples` (`go/internal/store/schema.sql:381-450`; `go/internal/store/observer_history.go:57-190`). Null stays unknown. History records paced reports, not every conversion. |
| Integrity handoff | `integrityBoard` now maps main, separate barometer, SHT3x, HIH8121, regulator, RTC, and heater state without merging physical locations, then advances both environment and pressure checks (`go/internal/state/integrity.go:309-408`). |
| Sensor health and local behavior | `EnvironmentSample`, location classes, baselines, rates, persistent reference, dew point, flatline, disagreement, fault, saturation, heater exclusion, reboot reset, and freeze logic are in `go/internal/integrity/environment.go:10-138,163-471`. The default profile and documented limits are in `go/internal/integrity/profile.go:64-175`. |
| Regional pressure | The state layer retains a bounded qualified pressure window and builds a median log-pressure tendency from distance-bounded, time-aligned surveyed neighbours at `go/internal/state/integrity_pressure.go:10-165`. Log changes remove constant elevation and sensor-offset differences before comparison. |
| Barometric physics and GNSS assessment | Explicit sensor/vent/uncertainty qualification, synchronized GNSS heights, virtual temperature, hypsometric conversion, regional residual, uncertainty propagation, and ambiguity handling are at `go/internal/integrity/barometric.go:9-149,167-208,244-394`. Registration and fusion handoff are at `go/internal/integrity/station.go:61-83,114-123,191-206`. |
| Events, APIs, and evidence | `environment_sensor_health`, `environmental_change`, `regional_pressure_anomaly`, and `barometric_height_consistency` are classified with unknown preserved at `go/internal/detect/integrity.go:34-162`. Current conditions include all four (`go/internal/store/conditions.go:30-48`), and event evidence captures RF/solution and board samples (`go/internal/store/evidence.go:17-23,76-169`). They use the existing `/gnss/api/events`, `/gnss/events`, `/gnss/api/v2/observers`, `/gnss/api/v2/observer-samples`, and `/gnss/api/v2/event-evidence` interfaces. |
| Native operator view | Integrity Station decodes every environmental component at `swift/IntegrityStation/Models/StationBoard.swift:79-301`, exposes distinct history metrics at `swift/IntegrityStation/Models/SensorHistory.swift:3-65`, and labels each source in `swift/IntegrityStation/Views/BoardTelemetryView.swift:223-307`. Environmental and pressure events are station conditions and appear in the Events and station-detail views through `swift/IntegrityStation/Models/GNSSAPIEvent.swift:63-112`. |

### Current scenario results

The focused fixtures use a one-of-one filter to expose the classifier result immediately. The
shipped profile applies three-of-four filtering, a five-minute recovery hold, and the detector's
60-second transition debounce. The state, reasons, metrics, and thresholds shown below are the
same fields served in `integrity.checks` and event `params`.

| Scenario | Actual fixture result | Operator-visible result |
|---|---|---|
| 1. RH 20→95 % suddenly | `environment_change=unassured`; the fixture reports a 74.8-point RH departure and a 16,326 Pa pressure departure with magnitude and rate reasons. At ordinary enclosure temperature, 95 %RH also approaches the dew point. Sensor health remains separate when the value is valid. | After filtering/debounce, `environmental_change: normal→severe`. No `station_assurance` or `spoofing_suspected` event. |
| 1. RH 20→95 % over 24 h | `environment_change=unassured`, reason `humidity_sustained_change`, and a 71.9-point retained-reference departure even with fast-rate detection disabled; the older reference freezes once anomalous. | The same advisory `environmental_change` event. RH does not imply clouds, interference, or an attack. |
| 2. Target pressure changes while two neighbours are stable | `regional_pressure=unassured`, reason `regional_pressure_departure`, with a 175.26 m pressure-equivalent residual; with stable GNSS height, `barometric_height=assured`, reason `no_significant_gnss_height_change`; `spoofing=false`. | `regional_pressure_anomaly: coherent→severe`; no spoofing event. The result says local residual, not whether its cause is weather, venting, enclosure pressure, or a plausible sensor fault. |
| 3. All three elevations change by the same pressure ratio | `regional_pressure=assured`, residual −0.04 m; `barometric_height=assured`; `spoofing=false`. | No warning transition. With fewer than two fresh qualified neighbours, both checks are `unavailable` with `insufficient_pressure_neighbours`, producing visible `unknown`, never zero or coherent. Regionally incoherent samples likewise leave both checks unknown. |
| 4. Surveyed station GNSS height moves +100 m while regional-corrected pressure is stable | `static_position=unassured` and `barometric_height=unassured`, reason `barometric_height_divergence`; the position and barometric-position domains meet the two-domain quorum, so `spoofing=true`. | `barometric_height_consistency: consistent→diverged`, `station_assurance`, and `spoofing_suspected`. Without explicit barometer qualification or adequate neighbours, the barometric result is unavailable and cannot supply the second domain. |
| 4. Real +100 m vertical movement with the matching pressure ratio | Barometric divergence is `assured`; the local pressure residual is environmental context, not a GNSS accusation. A fixed station's surveyed-position policy can still report real displacement because the installation declaration says it must not move. | No barometric-divergence warning. The position event remains independently correct for a physically displaced fixed antenna. |
| 5. Environment changes with clock, reception, or heating | Each sensor retains its physical identity; regulator and RTC package temperatures do not train the enclosure baseline. Every environmental event carries the contemporaneous station assessment, and the evidence bundle retains board, RF, receiver-solution, and timing samples around confirmation. No causal score is invented. | Event row, current station assurance checks, raw sensor histories, event API/SSE, and private event-evidence API. A single aligned native multi-series timeline remains future work. |
| 6. Invalid, faulted, saturated, disagreeing, or flatlined sensor | Results become `inconsistent` or `unassured` with explicit reasons such as `environment_sensor_invalid`, `environment_sensor_fault`, `environment_sensor_saturated`, `environment_sensor_disagreement`, or `environment_sensor_flatline`. Exact flatline defaults to at least 30 minutes and seven reports. | `environment_sensor_health: ok→degraded/fault`. Values remain nullable telemetry; no zero substitution. |
| 6. Reboot, stop, stale, and recovery | Session/uptime rollback clears baseline, flatline, and pressure windows; change returns `unavailable` with `environment_baseline_warming_up`. After 11 minutes without environment input, health/change and configured pressure checks are immediately `unavailable/stale`. Returning healthy input must still complete the prior five-minute recovery debt. | `environment_sensor_health` reports `stale` or `unknown`, environmental/pressure events report `unknown`, and only a completed held recovery emits the normal/ok transition. Missing input never clears an issue. |

The barometric conversion uses relative pressure change,
`Δz = -(R_d T_v / g) ln(p₂/p₁)`, after subtracting the regional log-pressure change.
The implementation follows NOAA's [hypsometric-equation note](https://www.aoml.noaa.gov/ftp/hrd/annane/prelim_notes/hypsometric_equation.pdf).
Its uncertainty includes the configured installed pressure term at both endpoints, neighbour
uncertainty and spread, GNSS vertical accuracy at both endpoints, and a 3 m model floor. It does
not claim absolute barometric altitude, sea-level pressure, or datum conversion. Manufacturer
limits for supported parts remain inputs to site qualification, including the
[BMP581 data sheet](https://www.bosch-sensortec.com/media/boschsensortec/downloads/datasheets/bst-bmp581-ds004.pdf)
and [MS5607 product specification](https://www.te.com/en/product-MS560702BA03-50.html).

### Remaining prioritized work

1. Validate the documented defaults against representative field data, publish calibration
   provenance, and characterize each enabled vent/enclosure installation. Keep barometric
   qualification disabled until that work is complete.
2. Rehydrate environmental and pressure evaluator windows from retained accepted samples after
   collector restart, without allowing old or cross-session faults to become recovery evidence.
3. Add one native aligned timeline for environmental channels, heater activity, clock, reception,
   RF, GNSS checks, and event markers. Present temporal correlation without assigning cause.
4. Add per-conversion sensor timestamps if firmware/hardware timing supports them, and separately
   account for vent response and enclosure lag instead of relying only on report receipt time.
5. After field validation, consider model-based diagnosis for vent/enclosure transients versus
   sensor faults. Do not split correlated temperature, RH, and pressure terms into extra fusion
   votes.

## Original audit snapshot before remediation

The remainder preserves the evidence and failing outputs observed before the implementation
work. In this historical section, “current” means the pre-remediation state captured by the
initial audit, not the repository state described above.

### Original verdict

NavListen measures, transports, validates, retains, and privately displays environmental telemetry. It does not currently detect environmental anomalies, model station-specific environmental behavior, compare environmental conditions across stations, diagnose most sensor-health failures, or use pressure, temperature, or humidity in station integrity fusion.

The environmental path stops after telemetry and history:

| Capability | Current result |
|---|---|
| Sensor acquisition | Implemented for the configured board sensors, with fresh conversions, validity bits, and bounded wire units. |
| Firmware reporting | Implemented. Measurements are sampled about every 30 seconds and reports are queued at boot, on sufficiently large change, at check-in, or around receiver interference. |
| Transport and parsing | Implemented through the authenticated GNF1 telemetry path. Invalid fields are withheld by firmware, represented as null when validly encoded as unavailable, or cause a malformed report to be rejected. |
| Retained history | Implemented for transmitted reports, not every sensor conversion. The private historian defaults to seven days of raw retention. |
| Environmental baseline and change detection | Missing. Firmware change thresholds control report pacing only. They do not create an anomaly. |
| Sensor-health detection | Partial at acquisition only. Readiness, calibration state, and null values survive transport, but there is no collector event for stale, flatlined, saturated, disagreeing, jumping, or recovered sensors. |
| Cross-station environmental context | Missing. The existing neighbor model is for RF interference, not pressure or weather. |
| GNSS integrity contribution | Missing. `integrityBoard` forwards pulse timing and receiver spoof state only. Environmental readings never reach an integrity input or fusion domain. |
| Operator reporting | Partial telemetry. The private API and native app expose the main environment record and its history, but they do not produce environmental events. Several additional sensor channels decoded by the collector are absent from the native model. |

No current environmental value can raise `environmental_change`, a sensor-health event, `station_assurance`, or `spoofing_suspected`. Unusual weather is therefore not misclassified as interference today, but it also cannot provide useful context for a GNSS observation.

## End-to-end traceability

The read-only hardware repository was used to identify the installed sensor types and their physical roles. In particular, `navlistener-hardware/docs/HARDWARE-OBSERVER.md:92-103`, `593-604`, and `724-786` describe pressure as a possible vertical cross-check, temperature as hardware/clock context, and humidity as an enclosure or condensation diagnostic. Those passages describe intended use, not implemented software behavior. Board-specific notes also distinguish enclosure sensors, vented pressure, regulator heat-spreader temperature, and oscillator temperature; they must not all be labeled ambient.

| Hardware configuration | Environmental channels and location constraints |
|---|---|
| NEO first spin | BMP388 pressure, MCP9808 temperature, and HDC2080 temperature/RH are listed in `navlistener-hardware/docs/HARDWARE-OBSERVER.md:92-103,593-603,626-629`. |
| MAX mobile | MS5607 pressure plus the common MCP9808 and HDC2080 are listed in `navlistener-hardware/docs/HARDWARE-OBSERVER.md:593-603,626-629`. Its enclosure specification says enclosed temperature/RH describe the enclosure and calls for an ambient-pressure vent (`navlistener-hardware/pcb/MAX-BOARD.md:159-176,332-344,539-545`). |
| ZED/X20 | BMP581 pressure, MCP9808, HDC2080, and DS3231M are listed in `navlistener-hardware/docs/HARDWARE-OBSERVER.md:593-604,626-655`. The same document states that the MAX and X20 firmware paths have not yet run on assembled hardware (`navlistener-hardware/docs/HARDWARE-OBSERVER.md:651-665`). |
| Mosaic P3 | HDC2080, SHT35, and filtered HIH8121 sensors occupy different physical placements, with airflow and temperature bias still requiring bring-up checks (`navlistener-hardware/pcb/gnss-mosaic-p3/README.md:66-92`). A separate MCP9808 is on the regulator heat spreader and reports thermal throttling, not ambient temperature (`navlistener-hardware/pcb/gnss-mosaic-p3/README.md:200-214`). |

| Stage | Implementation evidence | Observed behavior |
|---|---|---|
| Sensor selection | `esp32/main/board.c:252-287` | The hardware manifest selects HDC2080/HDC2022 and BMP388/BMP580/BMP581 variants. Conflicting manifest entries disable the affected measurement. |
| Main environment acquisition | `esp32/components/environment/src/env_sensors.c:103-164` | HDC and Bosch devices use fresh forced conversions. Results are range-checked and the destination sample is cleared before each acquisition, so a failed read does not silently reuse the preceding value. |
| MAX-board barometer | `esp32/components/environment/src/environment.c:305-331`; `esp32/main/board.c:751-759` | MS5607 PROM/CRC state, validity, temperature, absolute pressure, and extended-range status are reported separately. |
| Additional temperature and humidity | `esp32/main/board.c:940-999`; `esp32/components/observer_report/include/observer_report.h:86-117` | Regulator heat-spreader temperature, SHT3x, HIH8121, and RTC temperature have distinct records. The regulator measurement is explicitly not ambient. |
| Humidity-heater policy | `esp32/components/environment/src/env_heater.c:13-127`; `esp32/main/Kconfig.projbuild:175-284` | A local condensation-recovery policy tracks high-RH dwell and can operate the HDC heater. It has UTC, interval, temperature, stop, and cool-down gates. It is not an environmental anomaly detector. |
| Sampling | `esp32/main/board.c:1013-1072` | The main environment loop runs about every 30 seconds. An interference snapshot can force another sample after five seconds. |
| Report pacing | `esp32/components/observer_report/src/observer_report.c:25-78`; `esp32/components/observer_report/include/observer_report.h:10-16,160-163` | Boot and five-minute check-in reports are supported. Change reports are suppressed during the first 60 seconds after a successful report and use 0.5 °C, 2 %RH, and 100 Pa differences. Comparison is against the last successfully queued report, so small drift can accumulate. These are transport thresholds, not alarms. |
| Wire units | `esp32/components/observer_report/include/observer_report.h:11-34` | Main temperatures are centi-degrees Celsius, humidity is centi-percent RH, and pressure is pascals. Valid and ready masks are separate. |
| Transport | `esp32/components/gnf1/include/gnf1.h:1-14,48-53,95-98`; `esp32/components/gnf1/src/gnf1.c:66-74` | ObserverDetails is a typed record in the TLS GNF1 spool. The envelope may carry a wall-clock receipt stamp; the record also carries board uptime. |
| Collector ingress | `go/internal/ingest/push.go:1410-1450,1486-1527` | The collector validates the telemetry stamp and strictly decodes ObserverDetails. A malformed ObserverDetails record is rejected rather than partially trusted. |
| Main environment parse | `go/internal/ingest/observer_details.go:13-74,175-207` | Validity, readiness, temperature, RH, and pressure are decoded. Unavailable values remain pointers set to nil. Encoded temperatures and pressure outside the accepted ranges reject the report. |
| Barometer parse | `go/internal/ingest/observer_max.go:5-13,82-110` | MS5607 is explicitly local absolute pressure with no sea-level correction. Calibration rejection, readiness, validity, and extended range remain distinct. |
| Other sensor parse | `go/internal/ingest/observer_humidity.go:5-58`; `go/internal/ingest/observer_heater.go:5-104` | Extra humidity sensors and heater state/dwell are decoded with null-preserving semantics. |
| Live state | `go/internal/state/board.go:14-30,79-138,214-234` | Receipt time, optional sample time, boot session, sequence, and the complete decoded snapshot are retained. A new boot session clears prior live snapshots. The general board view is stale after 11 minutes. |
| Durable history | `go/cmd/navlistener/persistence.go:111-130`; `go/internal/store/schema.sql:381-450`; `go/internal/store/observer_history.go:14-24,57-105,112-190` | Raw and decoded transmitted records are stored with receipt time, optional sample time, session, and sequence. Queries allow up to 30 days, while default raw retention is seven days (`go/internal/config/config.go:193-225`; `go/internal/store/store.go:806-817`). |
| Integrity handoff | `go/internal/state/integrity.go:305-326` | `integrityBoard` returns immediately unless timing or receiver state is present. It applies only `TimingSample` and `ReceiverStatus`; it never reads environment, barometer, heater, regulator, RTC temperature, or extra humidity. |
| Integrity inputs | `go/internal/integrity/inputs.go:37-187` | Inputs cover solution, clock, receiver status, timing, carrier-to-noise, and RF. There is no environmental input type. |
| Station checks | `go/internal/integrity/station.go:8-18,25-76,97-179`; `go/internal/integrity/integrity.go:74-102` | No environmental check is registered and no environmental fusion domain exists. Existing domains are position, signal, clock, time, RF, and receiver. |
| Filtering and fusion | `go/internal/integrity/tracker.go:9-113`; `go/internal/integrity/fuse.go:5-8,24-97`; `go/internal/integrity/profile.go:59-68` | Existing checks use M-of-N filtering and recovery holds. Fusion counts domains, not individual correlated checks. None of this machinery receives environmental evidence. |
| Cross-station context | `go/internal/state/rf.go:503-588,621-682`; `go/internal/integrity/profile.go:50-56` | Neighbor distance, location freshness, and evidence age exist only for RF interference corroboration. There is no environmental neighbor set or regional-pressure model. |
| Event generation | `go/internal/detect/integrity.go:11-65`; `go/internal/store/evidence.go:17-21` | Integrity generates `spoofing_suspected` and `station_assurance`. Environmental event types do not exist. Board samples can be incidentally copied into evidence for another event, but their environment fields are not evaluated. |
| Private API | `go/internal/serve/serve.go:231-232,768-861`; `go/internal/serve/observer_history.go:27-70,73-179` | `/gnss/api/v2/observers` exposes current board context to private audiences. `/gnss/api/v2/observer-samples` exposes authorized history, defaulting to environment records for the previous hour. The public observer view excludes board telemetry. |
| Event interfaces | `go/internal/serve/events_api.go:56-60`; `go/internal/serve/sse.go:18-37,251-333` | `/gnss/api/events` and `/gnss/events` expose persisted and live integrity events. They receive no environmental or sensor-health event. |
| Native display | `swift/IntegrityStation/Models/StationBoard.swift:3-66,79-108,148-164`; `swift/IntegrityStation/Views/BoardTelemetryView.swift:13-25,193-234` | Optional values remain distinguishable from zero, and stale data is visibly labeled. The environment card shows the main temperature, humidity, and pressure fields. The model does not decode barometer, heater, SHT3x, HIH8121, regulator, RTC temperature, or sensor identity fields already supplied by Go. |
| Native history | `swift/IntegrityStation/Models/SensorHistory.swift:3-35,145-166`; `swift/IntegrityStation/Views/SensorHistoryView.swift:18-104` | A user can chart one main temperature, RH, or pressure metric at a time. Nulls, boot changes, and gaps over 660 seconds break the chart. There is no aligned environment/GNSS/clock/RF view or anomaly marker. |

## Data semantics, timing, and retention

| Data | Units and validity | Timestamp and cadence | What is retained and shown |
|---|---|---|---|
| Main `BoardEnvironment` | Celsius, percent RH, and Pa. A ready mask distinguishes initialized devices; individual pointer values distinguish valid measurements from unavailable ones. | One common report uptime and optional envelope sample time cover the whole snapshot. There is no per-sensor conversion timestamp. Firmware samples about every 30 seconds, but transmission is paced by boot/change/check-in/interference rules. | Transmitted snapshots are stored. The private Go API exposes all main fields. The native environment card and history show them, including zero as a real value and nil as unknown. |
| `BoardBarometer` | Celsius and local absolute Pa, plus state, validity, and extended-range flag. Calibration rejection is not represented as a zero measurement. | Same report-level timing. | Stored and exposed by Go, but absent from the native app model and charts. |
| SHT3x and HIH8121 | Celsius and percent RH with device state, valid flag, and HIH8121 freshness status. | Same report-level timing. | Stored and exposed by Go, but absent from the native app model and charts. |
| Regulator and RTC temperature | Celsius, with device-specific state. Regulator values include thermal-throttle context. | Regulator is sampled about every five seconds but only rides on paced reports; RTC temperature rides on the board report. | Stored and exposed by Go, but absent from the native app. Neither should be presented as outside ambient temperature. |
| Humidity heater | State, flags, run/stop reason, high-RH dwell, temperatures, RH before/after, and boot-local counters. | Same report-level timing. A heater state transition can cause a change report; dwell counters otherwise ride on another report. | Stored and exposed by Go, but absent from the native app and all event streams. |

History therefore records reports, not a uniform 30-second measurement series. A stable sensor normally appears at the five-minute check-in cadence. A threshold-crossing change cannot cause another report until 60 seconds after the preceding successful report. A failure and recovery entirely inside that interval can be absent from retained telemetry.

The current missing-data behavior is mostly sound at the data boundary: firmware zeroes failed sample structures, validly unavailable values decode as null, boot sessions are explicit, and history charts break across nulls, boot changes, and long gaps. A stale live sample remains displayed with a stale warning rather than being rewritten to zero or normal. The missing piece is a durable sensor-health state and event; no collector process turns those facts into an operator warning.

## Targeted verification

The following focused suites passed:

```text
make -C esp32/components/environment/test
Environment: HDC2080/HDC2022 selection, units/sign/alerts, Bosch trim compensation, fresh conversions, heater register control, HDC retry, fault isolation and per-device bus-error attribution passed
Humidity heater: condensing trigger, UTC/interval gates, stop conditions, heater-off retry, cool-down stability and dwell counters passed

make -C esp32/components/observer_report/test
observer report golden/cadence tests passed

cd go
go test ./internal/ingest ./internal/integrity ./internal/detect ./internal/state ./internal/store ./internal/serve
ok github.com/ptudor/navlistener/internal/ingest
ok github.com/ptudor/navlistener/internal/integrity
ok github.com/ptudor/navlistener/internal/detect
ok github.com/ptudor/navlistener/internal/state
ok github.com/ptudor/navlistener/internal/store
ok github.com/ptudor/navlistener/internal/serve
```

A temporary state/integrity fixture applied valid 20 %RH and 101325 Pa telemetry, then 95 %RH and 85000 Pa, while maintaining healthy GNSS epochs. It also moved the GNSS solution upward by 100 m while pressure stayed fixed, supplied an invalid environmental sample and a calibration-rejected MS5607 record, and advanced time without another board report. The fixture was removed after the run because the audit does not add a production detector or threshold.

Actual output:

```text
environment-only: humidity=20% pressure=101325Pa board_stale=false integrity_stations=0
20%->95% and 101325Pa->85000Pa: integrity_unchanged=true environmental_checks=0 station=assured spoofing=false
GNSS altitude +100m with stable pressure: static_position=unassured vertical=100.0m station=inconsistent spoofing=false environmental_checks=0
invalid sensors: humidity=<nil> pressure=<nil> barometer_state=calibration_rejected board_stale=false
no later board report: board_stale_after_12m=true
```

This demonstrates both sides of the boundary. The collector preserves environmental state and unknowns, but even extreme valid changes leave integrity unchanged. The altitude move is caught by the surveyed static-position check, not by pressure. A single unassured position domain makes the station inconsistent but does not meet the multi-domain spoofing quorum (`go/internal/detect/integrity_test.go:103-114`).

## Scenario results

### 1. Humidity rises from 20 %RH to 95 %RH

Current behavior:

- Magnitude: the main 75-point change exceeds the firmware's 2 %RH report-pacing difference and is transmitted after the 60-second minimum interval. The collector does not score its magnitude.
- Rate: no rate is computed. A sudden change and a gradual change that eventually accumulates against the last sent report produce the same telemetry classification.
- Persistence: the collector has no humidity dwell or station baseline. Firmware does carry high-RH dwell for the heater policy, but the default heater start requires at least 98 %RH continuously for four hours plus temperature, trusted-UTC, and minimum-run-interval gates (`esp32/main/Kconfig.projbuild:187-239`; `esp32/components/environment/src/env_heater.c:68-84`). A level of 95 %RH therefore does not start the default heater.
- Temperature and condensation: no dew point or surface-temperature margin is calculated. The HDC heater policy is recovery for a sensor exposed to condensation; it is not proof that the enclosure, antenna, sky, or RF path is wet.
- Sensor location: the measured air is at the board/enclosure sensor. It is not automatically outdoor ambient air and says nothing by itself about clouds or RF interference.
- Integrity result: no environmental check, station state change, jamming conclusion, or spoofing evidence.
- Operator result: the latest main RH and a history point are visible through the private observer API and native app. There is no event, notification, anomaly band, rate, or duration. Heater state and extra humidity sensors are not visible in the native app.

Classification today: telemetry only. At 98 %RH under all configured gates, the additional result is local sensor-recovery activity, still not an environmental anomaly or GNSS-integrity input.

The HDC2080 manufacturer specifies a 0–100 %RH measurement range, describes operation as non-condensing, warns that prolonged high-humidity exposure can shift the sensor, and recommends using dew point when managing possible condensation. The relevant primary references are the [HDC2080 data sheet](https://www.ti.com/lit/ds/symlink/hdc2080.pdf) and the [manufacturer's dew-point application brief](https://www.ti.com/document-viewer/lit/html/snaa368). High RH alone must not be converted into a weather, cloud, jamming, or spoofing assertion.

### 2. Local pressure changes while nearby stations remain stable

Current behavior:

- The change is transported when it accumulates to the existing 100 Pa report-pacing difference, subject to the 60-second minimum. This does not create an anomaly.
- No station-specific pressure baseline, rate, sustained-change window, flatline check, reboot quarantine, or cross-sensor consistency check exists.
- Bosch values outside 30000–125000 Pa are rejected at the main decoder boundary. MS5607 calibration rejection, invalidity, and extended range remain explicit. A physically plausible but faulty value inside those broad bounds is accepted as telemetry.
- The collector cannot distinguish local weather, a vent/enclosure transient, HVAC or solar effects, a sensor step, calibration drift, a bus fault that still yields plausible values, or a real vertical displacement.
- No nearby pressure is queried. The operator receives no event. Only the private current sample and retained report history change.

Classification today: telemetry only, or an acquisition state such as calibration rejected. There is no sensor-health or environmental anomaly classification.

Pressure must be compared using a declared reference. The firmware reports local absolute pressure. Raw station pressure differs strongly with elevation, while altimeter and sea-level pressure are reduced quantities that require additional assumptions. The [US National Weather Service pressure definitions](https://www.weather.gov/bou/pressure_definitions) and [NOAA air-pressure overview](https://www.noaa.gov/jetstream/atmosphere/air-pressure) explain why raw pressures from different elevations are not directly comparable. The BMP581's [manufacturer data sheet](https://www.bosch-sensortec.com/media/boschsensortec/downloads/datasheets/bst-bmp581-ds004.pdf) and the MS5607 [manufacturer product page](https://www.te.com/en/product-MS560702BA03-50.html) provide device ranges and accuracy terms, but installed uncertainty must also include board heating, vent dynamics, mounting, aging, and calibration.

### 3. Pressure changes coherently across neighboring stations

Current behavior:

- There is no environmental neighbor comparison, so coherent regional pressure is neither recognized as weather context nor counted as corroboration.
- Distance, station location age, sample age, timestamp alignment, elevation, sensor type, reference pressure, and missing-neighbor handling are absent from environmental processing.
- The existing neighbor machinery considers those concepts only for RF interference. It cannot be reused implicitly as a weather model.
- Missing neighbors do not become zero or stable pressure; there is simply no environmental result at all.
- No operator event is emitted, whether one station changes, all stations change, or no neighbor is fresh.

Classification today: independent telemetry rows at each station, with no regional interpretation. This avoids a false attack conclusion only because the environmental evidence is unused.

A future comparison should estimate a robust regional pressure tendency from fresh, time-aligned stations and compare each station's local tendency or residual. It must normalize the reference/elevation treatment, expose the number and age of usable neighbors, and return unavailable when coverage is inadequate. Temperature, humidity, and pressure from one air mass are correlated observations, not three independent confirmations.

### 4. GNSS position or altitude moves at a surveyed stationary installation

Current behavior:

- The surveyed static-position check compares the GNSS solution with its configured WGS 84 station position. Default instantaneous vertical bands begin at 25 m inconsistent and 80 m unassured; the sustained check uses a ten-minute mean (`go/internal/integrity/profile.go:69-84`; `go/internal/integrity/position.go:313-369`). These are existing GNSS thresholds, not environmental thresholds.
- Other available evidence includes stationary velocity, position/velocity consistency, receiver clock behavior, UTC and pulse timing, carrier-to-noise and AGC behavior, receiver-reported spoof state, RF state, and an optional independent GNSS baseline.
- Pressure is not consulted. Stable pressure cannot contradict a reported 100 m GNSS climb, and a pressure step cannot explain or qualify a GNSS altitude step.
- In the targeted fixture, the 100 m GNSS climb produced `static_position=unassured`, `station=inconsistent`, and `spoofing=false`; `environmental_checks=0` even though pressure was available and stable.
- The operator learns about the GNSS result through `station_assurance` after detector debounce and through current integrity checks. The pressure remains a separate telemetry point with no link to that event.

Classification today: GNSS position evidence only. Pressure contributes nothing.

A defensible barometric cross-check should compare short-window changes, not turn absolute pressure directly into surveyed altitude. A suitable physical relation is the hypsometric equation, `delta_z = (R_d * T_v / g) * ln(p1 / p2)`, using mean virtual temperature. NOAA gives the equation and assumptions in its [hypsometric-equation note](https://www.aoml.noaa.gov/ftp/hrd/annane/prelim_notes/hypsometric_equation.pdf). The implementation would need:

- A station-specific sensor offset and uncertainty established without using the suspect GNSS height as truth during an event.
- Local absolute pressure with a validated vent path, fresh timestamps, and boot/warm-up handling.
- Mean virtual temperature or an uncertainty allowance for temperature and humidity.
- Removal of a fresh regional weather tendency, with an explicit unavailable result when neighbors are inadequate.
- Time alignment and uncertainty for sensor noise, manufacturer relative accuracy, temperature sensitivity, enclosure lag, vent response, regional-model residual, and GNSS vertical accuracy.
- Correct datum handling. GNSS reports ellipsoid height, while pressure-derived and orthometric heights are not interchangeable; NOAA's [National Geodetic Survey vertical-datum guidance](https://www.ngs.noaa.gov/datums/vertical/) documents that distinction.

The cross-check should initially be corroborative and uncertainty-bounded. It should occupy one environmental/physical domain regardless of how many correlated temperature, humidity, and pressure fields contribute to its model. A pressure mismatch can support a position-integrity concern; it must not by itself be labeled jamming or spoofing.

### 5. Environmental change accompanies clock drift, reception change, or hardware heating

Current behavior:

- Main environmental telemetry, board pulse timing, receiver reception, and integrity events coexist in storage and APIs, but no evaluator aligns or correlates them.
- The native UI places board environment, timing, reception, integrity, and event panels in separate views. Environmental history charts only one metric at a time and cannot overlay receiver clock, reception, AGC, carrier-to-noise, or integrity transitions.
- Regulator heat-spreader temperature is a hardware thermal signal and can drive Wi-Fi throttling. RTC temperature describes the oscillator package. MCP/HDC/BMP/SHT3x/HIH8121 values describe their board or enclosure locations. None is automatically outside ambient temperature.
- Extra humidity sensors could reveal gradients or self-heating, but the native model currently ignores them.
- The operator can manually inspect some main environment history beside current GNSS state, but cannot see a time-aligned relationship or receive a correlation event.

Classification today: separate telemetry and GNSS integrity results. There is no causal conclusion, correlation score, or operator-visible relationship.

A future display should align raw values, validity/staleness, heater activity, receiver clock, reception, RF metrics, GNSS checks, and events on one time axis. It should name the sensor and its physical location. Correlation should be reported as correlation unless a validated causal model supports more.

### 6. Sensors flatline, disagree, saturate, jump after reboot, become invalid, or stop

| Failure mode | Current behavior | Operator-visible result |
|---|---|---|
| Invalid read | Firmware clears the value and validity bit. A correctly encoded unavailable value decodes to null. | Main fields show unknown; a retained history point is null and breaks the chart. No event. |
| Impossible wire value | Strict collector bounds reject the complete ObserverDetails record. | The malformed report is absent. There is no environmental event explaining the rejected sample. |
| MS5607 calibration failure | State remains `calibration_rejected` and values remain null. | Available in Go telemetry/history, but not decoded by the native app. No sensor-health event. |
| Stops reporting | The last live board sample becomes stale after 11 minutes. History develops a gap. | The native card labels the old sample stale; it does not turn it into zero or fresh normal evidence. No dedicated environmental-stale event. |
| Flatline at a plausible value | No variance, stuck-bit, or expected-noise check exists. | Continues to look like ordinary telemetry. No warning. |
| Saturates at 0 or 100 %RH | Both are within the accepted RH range. No dwell-at-rail check exists. | Displayed as a measurement. Heater behavior may change at the high rail, but no anomaly event exists. |
| Sensors disagree | Main, extra, barometer, regulator, and RTC values are not compared. | Some channels are in the API/history; several are absent from the app. No warning. |
| Plausible step | A large enough difference causes a paced transport report. No step classifier exists. | The graph changes. No warning. |
| Reboot jump | A new session clears live snapshots and history charts break at the session boundary. There is no environmental warm-up or baseline quarantine. Heater dwell counters are boot-local. | Reboot is inferable from session/uptime. No environmental event. |
| Recovery | Reappearing valid data is reported as a change when pacing permits. | Value returns from unknown. No recovery event or recovery hysteresis. |

Missing and stale data are not converted into zero, normal, or recovery evidence. However, they also do not create the explicit unknown sensor-health state that operators and a future integrity model need. The lack of a flatline check is particularly important for barometric corroboration: a stuck pressure sensor could otherwise appear to confirm a stationary height.

## Prioritized implementation plan

This audit intentionally introduces no production threshold. Existing firmware values cited above are report-pacing or heater-policy settings, not proposed anomaly limits. New limits should be configurable, recorded with provenance, and derived from device characterization and representative retained data before deployment.

### P0: Preserve and expose complete sensor identity, timing, and health

1. Add an `EnvironmentSample` integrity-side input carrying receipt time, accepted sample time, uptime, session, source sensor, physical location class, valid/ready/state flags, values, and uncertainty metadata.
2. Distinguish enclosure/board air, regulator heat spreader, oscillator package, and a validated vented pressure channel. Do not call all temperature values ambient.
3. Add per-channel freshness rather than relying only on the 11-minute whole-board stale flag.
4. Decode and display the existing barometer, heater, SHT3x, HIH8121, regulator, RTC-temperature, and sensor-identity fields in the native app.
5. Add a sensor-health evaluator for stopped/stale, invalid, saturated, flatlined, implausible step, reboot/warm-up, cross-sensor disagreement, calibration rejection, and recovery. Keep unavailable distinct from healthy.

Meaningful tests: invalid-to-null round trips; rejected malformed records; no reuse after failed reads; plausible flatline; rail saturation; a transient failure shorter than report pacing; sensor disagreement; reboot step; stale board; recovery without a false zero.

### P1: Add station-specific environmental behavior

1. Maintain robust per-station baselines only after an explicit warm-up and sufficient valid history.
2. Separate magnitude, rate, and sustained-change evidence. Apply persistence, hysteresis, and recovery holds rather than using report-pacing thresholds.
3. Freeze or quarantine baseline learning during invalid data, heater/recovery activity, reboot warm-up, maintenance, and active anomalies.
4. Compute dew point and a surface-temperature margin only when the required measurements are fresh and physically relevant. Report the model inputs and uncertainty; do not infer clouds or interference from RH.
5. Emit a durable environmental-change event carrying channel, sensor, baseline, delta, rate, duration, validity, and uncertainty, plus a separate sensor-health event for instrumentation failures.

Meaningful tests: sudden and gradual 20-to-95 %RH changes; high-but-stable seasonal RH; dew-point approach with and without a relevant surface-temperature proxy; heater activity; short spike; sustained shift; hysteretic recovery; baseline not learning through an anomaly.

### P2: Model regional pressure without comparing raw elevations

1. Build a fresh-neighbor set using known location, distance, sample timestamp, receipt age, sensor health, and a declared pressure reference.
2. Estimate a robust regional tendency from pressure changes or normalized residuals, not raw Pa values from stations at different elevations.
3. Report neighbor count, spatial extent, age, spread, and missing coverage. Return unavailable rather than zero or stable when evidence is insufficient.
4. Classify coherent change as regional context and a local residual as a possible local environment or sensor issue. Do not count correlated neighbor measurements as independent attack domains.

Meaningful tests: coherent weather front; one local step; mixed elevations; stale and skewed timestamps; one bad neighbor; sparse coverage; mobile/unknown station location; all neighbors missing.

### P3: Add an uncertainty-bounded barometric GNSS cross-check

1. Validate and calibrate the installed pressure path, including venting, offset, temperature behavior, lag, and uncertainty.
2. Compare pressure-derived vertical change with GNSS ellipsoid-height change over aligned short windows after removing regional pressure tendency.
3. Include virtual-temperature, weather-model, sensor, vent, time-alignment, and GNSS vertical uncertainties in the decision.
4. Make sensor-health and freshness prerequisites. A flatlined, heated, recovering, unvented, stale, or uncalibrated sensor must produce unavailable, not confirmation.
5. Add one environmental/barometric fusion domain. Multiple pressure/temperature/RH terms within the model remain correlated and must not become multiple quorum votes.
6. Start as corroborative evidence and require validation before it can increase severity. A mismatch is a position-integrity concern, not automatically jamming or spoofing.

Meaningful tests: GNSS altitude step with stable pressure; real vertical movement; regional pressure shift with stable station; local enclosure pressure step; flatline; pressure fault; temperature transient; delayed vent response; missing neighbors; marginal uncertainty where the result must remain unavailable or inconclusive.

### P4: Make the result operationally visible

1. Persist environmental-change and sensor-health onset, update, clear, and unavailable transitions with evidence bundles.
2. Expose them through `/gnss/api/events`, `/gnss/events`, current conditions, and the native Events view.
3. Add an aligned environment, clock, reception, RF, GNSS-check, and event timeline. Show sensor identity/location, raw value, baseline, rate, duration, health, freshness, and uncertainty.
4. Keep cause labels conservative. Show regional weather, local environmental residual, enclosure effect, sensor fault, and GNSS inconsistency as distinct hypotheses or states.
5. Make recovery explicit and hysteretic. Never let missing data clear an active issue or appear as a normal zero.

Meaningful tests: exact API/SSE event payloads; private/public audience boundaries; event onset and clear; stale becoming unavailable without recovery; evidence linking; chart gaps; extra sensor rendering; time-aligned GNSS/environment display; no attack event from weather alone.

## Acceptance criteria for the completed path

The implementation should not be considered complete merely because a pressure or RH value is displayed. Completion requires all of the following:

- Every configured channel has a documented unit, physical location, validity/state model, timestamp semantics, cadence, uncertainty source, and retained-history path.
- A station-specific evaluator distinguishes data health from environmental change and makes startup, reboot, invalid, stale, and recovery states explicit.
- Magnitude, rate, persistence, hysteresis, and recovery are tested independently.
- Regional pressure context is elevation/reference aware, time aligned, distance bounded, and explicitly unavailable when neighbors are insufficient.
- Barometric evidence uses pressure change and uncertainty, not an uncalibrated absolute pressure-to-altitude shortcut.
- Correlated environmental terms contribute through one justified model and one fusion domain.
- Operators receive a named durable event and can inspect aligned raw evidence, while unusual weather alone never becomes a jamming or spoofing classification.
