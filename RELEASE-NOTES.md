# Unreleased

- A NEO observer's manifest names its barometer exactly. The board's U2 is a BMP388 in the
  BOM and a BMP384 on the first-article lot; the two data sheets give one register map,
  chip ID, trimming table and compensation formula, so the driver is unchanged and only the
  manifest can say which is fitted. The discovery library gains `PRESSURE_BMP384` and its
  NEO list takes the barometer from the batch, with no default: a manifest-initialization
  build for a NEO needs `NVF_MANIFEST_BARO=bmp388|bmp384` beside `NVF_MANIFEST_HDC`. The
  firmware logs and reports the part the manifest names (`BMP384 temperature=…`, and
  `pressure chip ID=0x50; BMP388/BMP384 family verified (the manifest names the BMP384)`),
  ObserverDetails tag 15 sends the BMP384 as part 5 while part 1 now means the BMP388
  alone, and the collector's `pressure_sensor` is `bmp388` or `bmp384` where it was
  `bmp388_bmp384`, as is the name a station's barometer policy gives. No unit reporting to
  a collector had a manifest that named either part, so part 1 had not been sent.
  `bench_check.py --expect neo --barometer bmp384` holds a board's manifest to the part.
- A manufacturing image can correct a manifest that was written with the wrong batch part.
  `NVF_MANIFEST_CORRECT=1` beside `NVF_MANIFEST_INIT` builds the initialization image with
  `NVF_MANIFEST_FACTORY_CORRECT`: where the stored manifest is, byte for byte, the same
  board revision's list for the other humidity sensor or barometer, it writes the list for
  the selected parts and reads it back, and it leaves any other manifest as it is
  (`esp32/docs/COMMISSIONING.md`, A manifest written with the wrong batch part).
- `rf signals` on the USB console lists every healthy signal the receiver has code lock on,
  with its C/N0 and its satellite's elevation, as one `NVF-RF-SIGNALS {json}` line:
  `["G05","L1C/A",45,61]` for each, a data and pilot pair as one entry. The same signals
  read before and after a change of antenna, cable, place or LNA mode say what the change
  did to reception, a comparison that took a diagnostic build before. Checked on a
  ZED-X20P. A Septentrio receiver fills the same list and has not been tried.
- An observer no longer holds its ATTENTION lamp for a rotating radar. A long-range L-band
  radar turns once in 10 to 12 seconds inside the band of GPS L2, GLONASS L2, BeiDou B3 and
  Galileo E6, and a ZED-X20P in its sight flags that path's four frequencies as jammed for
  one report at each crossing of the beam: about 30 km from one, 148 of 152 turns were
  flagged, the lamp was lit 88 % of the time, and the receiver's lower detector sensitivity
  left 47 of 50 turns flagged. The firmware now learns such a source from the flags' timing:
  four warning-level crossings one report long, on the same frequencies, whole turns of one
  period of 4 to 20 seconds apart; adaptation remains in that range, and a source at its
  very edge keeps its place when a report comes some milliseconds early or late. Once it is known,
  warning flags that begin when a crossing is due on those frequencies are neither a cause
  for ATTENTION nor an interference event. They become unexplained after 3.5 seconds even
  without another RF report. Critical MON-RF indications always count and never train the
  tracker; warnings that outlast a crossing, come when none is due or name other frequencies
  count as before. Two
  misfits in a row or ten turns of silence forget the source, and so does a restart, after
  which the lamp lights for the first four crossings. The console logs `periodic source:
  warning flags for one report every 12.0 s on … MHz`, `rf lna` gives `periodic_ms`, and the
  display's GNSS page reads `PULSE 12s`. The telemetry is unchanged and still carries every
  flag (`esp32/README.md`, The ATTENTION lamp).
- A ZED/X20 observer sets the mode of its receiver's three internal LNAs, low gain unless
  the unit is set to normal gain. In normal gain, the ZED-X20P's default, the receiver takes
  17 to 40 dB of gain ahead of `RF_IN`, and in low gain 20 to 50 dB. With a survey antenna
  whose amplifier is given as 38 ± 2 dB, three boards on an indoor bench sat in normal gain
  at the lowest programmable gain they report (3 dB on all three RF paths, AGC 7 %) and the
  receiver flagged every frequency it assessed as jammed, through a 3D fix, so the ATTENTION
  lamp stayed lit. In low gain two of them read 11 dB on two paths with none of those
  paths' frequencies flagged, thirteen changes of mode on one board moved the flags with the
  mode each time, within about six seconds, and C/N0 was no lower (0.4 dB higher on average
  over 290 paired signals). The mode is kept per unit in the `nvf_rx` NVS namespace: `rf lna`
  on the USB console reports it with each RF block's AGC and the flagged frequencies, and
  `rf lna low` or `rf lna normal` saves it and applies it at once, for an antenna that
  brings less than low gain takes. The display's SYSTEM page shows it beside the receiver.
  The modes apply only from a restart of the GNSS subsystem, so the firmware reads the
  setting first: a receiver that holds it, as after a restart of the ESP32 alone, logs
  `already set` and keeps its fix, and otherwise the write is followed by a UBX-CFG-RST of
  the GNSS subsystem alone, a hot start that leaves the rest of the RAM configuration in
  place. The bench report's `receiver.config` counts 15 settings on a ZED-X20P, the three
  LNA keys as one, accepts `already set` and names the unit's mode. With its gain in range
  the third path, which carries L2, E6 and B3, showed what normal gain had hidden on that
  bench: a narrow signal near 1282.5 MHz that took the path to its floor for about a second
  every 12 seconds, when the receiver at times flagged that path's four frequencies alone.
  An RF survey taken in normal gain is not comparable in level with one taken in low gain,
  and a jamming warning with the PGA at 3 dB on every path points at the gain ahead of the
  receiver before it points at interference (`esp32/README.md`, The ZED-X20P's LNA mode;
  `docs/OBSERVER-TELEMETRY.md`, Isolate an X20 jamming warning). A u-blox ANN-MB2 (23 dB
  at the end of its 5 m cable) was in range in both modes: 13 / 13 / 11 dB in normal gain
  and 24 / 22 / 19 dB in low gain, with no frequency flagged and the same C/N0 (0.25 dB
  apart on average over 194 paired signals, standard error 0.23 dB). Lying beside the bare
  board the same antenna heard 6 to 11 dB more and no satellite, which the receiver
  flagged in normal gain, where the L1 path sat at its floor, and did not in low gain.
- A ZED/X20 observer drives a status display on its display header. H3 takes the common
  128×64 SSD1306 I2C module at `0x3c`, which the firmware looks for every two seconds, so
  it can be fitted or removed while the board runs; a board without one logs a single line
  and runs as before. Seven pages follow one another every six seconds: the UTC clock with
  its origin, the fix and its accuracy, the uplink, the collector link and the weather in
  the box; satellites per constellation with the antenna, jamming and spoofing states;
  signals per band with a C/N0 bar for each satellite; the network, or the setup network's
  name and address on a board not yet provisioned (the setup password stays on the label);
  the sensors and supply rails; the board, serial and firmware; and, while the ATTENTION
  lamp is lit, what it is lit for, shown at once when a cause joins. A short press of BOOT
  holds the next page for a minute, and the armed configuration reset takes the whole
  display. The display shares the I2C bus by writing only the bytes that changed, 32 to a
  transfer at most, and by staying off it while the panel chain is read back and while the
  secure element is in session; on the bench that came to about 200 bytes a second. Its contrast follows the panel brightness, the
  picture moves a pixel a minute to spare the panel, and `oled status`, `oled rotate`,
  `oled off`, `oled on` and `oled dump` on the USB console report on it, turn it for a
  module mounted the other way up, blank it and print its frame as text. The receiver
  status keeps a fix's horizontal accuracy estimate and PDOP from NAV-PVT for it
  (`esp32/README.md`, Status display).
- Reading a board's identity no longer fails while another part on the I2C bus is being
  spoken to. The ATECC wakes on any 60 µs of SDA held low, which a zero byte sent to another
  part is, and goes back to sleep when its watchdog ends, 0.7 to 1.7 seconds later. A session
  opened on a part already awake had only the rest of that time, and a read of its serial or
  its attestation record failed when the time ran out between commands. A session now puts
  the part to sleep first and wakes it afresh, so the whole watchdog period is its own.
  ESP-IDF 5.5 also refuses to remove an I2C device while any transfer is in flight on the
  bus, and leaves it allocated: the handles that were added and removed around each identity
  read are now added once and kept. With a display on the bus written to every second, two
  of 15 reads of the secure element's serial had failed before the first change and three
  of 30 reads of the board's serial before the second; after both, 45 of 45 were complete.
- An observer with the 162mm panel lights its red ATTENTION lamp for what the receiver
  itself reports. Until now the lamp followed only the reception alarm, the observer's check
  against the collector's forecast, so a receiver that reported jamming lit nothing. It is
  now lit while the receiver indicates jamming or spoofing, while an RF block's antenna
  supervisor reports a short or an open circuit, while the receiver has sent no navigation
  solution and no satellite list for 15 seconds (once the observer has run for a minute),
  while the INA3221 holds a rail alert, and while a reception alarm is active. For critical
  jamming, which is jamming with no fix, and for spoofing it flashes with the alarm
  pattern's 250 ms; otherwise it is steady. Each of the receiver's causes is held for ten
  seconds after its last report, and the console names the causes whenever they change
  (`attention: jamming warning; the panel's ATTENTION lamp is lit`). The lamp shows the
  receiver's own flags as it gives them and does not say where the energy comes from; the
  telemetry is unchanged. On the bench a ZED-X20P reported a jamming warning with its AGC
  at 7 % on all three RF paths, and the lamp lit with it (`esp32/README.md`, The ATTENTION
  lamp).
- An MCP79412 observer can show that its backup cell works. The part cannot sense its cell,
  but it stamps each loss of main power from its own calendar, when the power goes and when
  it returns. The firmware now saves each record it finds on a running clock and clears it,
  so that the part stamps the next loss; before, the first record stayed and later losses
  went unrecorded. Stamps at least two minutes apart are more than a minute counted on the
  backup input, which a board without a working cell cannot do. That record is kept as the
  backup's proof and named in the boot log and in `rtc status`, where the log used to say
  "battery presence unverified" for good. A shorter loss is logged and proves nothing, and
  the proof is withdrawn if the part later shows it lost its backup as well. The telemetry's
  power-fail flag now means a loss was found recorded since this start; it used to stay set
  from the first loss on (`esp32/docs/RTC-SERVICE.md`, Backup proof).
- A mosaic-G5 P3 observer tracks satellites down to the horizon and keeps the low ones out of
  its fix. The receiver has two elevation masks, both 5 degrees as shipped: below the
  tracking mask a satellite is not tracked, measured or read, and below the PVT mask it is
  tracked and left out of the position, the time and the pulse. The firmware now reads both
  and sets tracking to 0 degrees and PVT to 10, each only when it differs, so a repeated
  configuration sends the receiver nothing. Satellites near the horizon, which is where the
  distant regional ones are, appear in the forwarded blocks and in the console's list of
  names. The collector's reception checks keep their own mask (20 degrees by default), so
  they and the power baselines are unchanged (`esp32/docs/SBF-RECEIVER.md` §4).
- A mosaic-G5 P3 observer has its receiver track every signal it offers. The receiver starts
  with 20 of its 30 signals on. The firmware now asks which are tracked and adds each missing
  one by name: GPS L1C, GLONASS L2P and L3, Galileo E5 AltBOC, SBAS L5, BeiDou B2b and QZSS
  L6, L1C, L1S and L1C/B. Like the rest of the configuration this is in the receiver's RAM and
  repeated when needed. The command restarts the receiver's tracking loops, so it is sent only
  for a signal that is not on: an observer that restarts beside a running receiver sends
  none, and the receiver keeps its satellites. A signal the receiver does not offer is named
  once in the log and is not a command error. The added signals' measurements are in the
  forwarded MeasEpoch blocks, which grow by about a tenth; their navigation pages are not
  requested yet. A satellite's C/N0 is still its strongest signal's, so it can read a little
  higher where an added signal is the strongest. Once a minute the console names the tracked
  satellites, with elevation and C/N0, and counts the satellites on each signal
  (`esp32/docs/SBF-RECEIVER.md` §4 and §6).
- Station RF baselines heal themselves. A band or front end that has sat away from its
  learned baseline for a day without a break is now learned again at the level it holds:
  the departure is reported for that day, then `rf_trust` returns to 1 and the events clear,
  carrying `baseline_relearned` so a clear of that kind is not read as the interference
  going away. Before, a gain cut was never learned, so a changed antenna or a repair nobody
  announced left a station degraded until its `power_model_epoch` was changed. Set
  `rf_baseline_relearn = "never"` under `[integrity]` to keep that behaviour where the
  hardware is your own and an unannounced change should stay an alarm, or give it another
  duration of at least fifteen minutes. A u-blox band above its baseline still starts again
  after fifteen minutes; a Septentrio front end's risen gain is now held as long as a cut,
  so an antenna unplugged for an hour comes back to the baseline it left. The hold counts
  only time in which the station was reporting, so a station that falls silent while it is
  away from its baseline does not come back healed. The hold survives a collector restart,
  and a baseline checkpoint is now restored by when it was saved (within a day) instead of
  by when its baseline last moved. The setting is part of the operating profile, so every
  station's `config_hash` changes (`docs/DEFENSE-PNT.md §2`). A station that has healed is
  marked for 48 hours: `rf.baseline_relearned_at` in the `observers` feed, with the
  departure that was accepted on the band or front end it happened to, and Integrity
  Station shows it beside the station as recently self-healed. Its RF card now lists a
  Septentrio station's front ends too: gain, gain against the baseline, sample variance and
  pulse blanking.
- The collector decodes a Septentrio receiver's ReceiverStatus block (SBF 4014) and learns a
  baseline for each RF front end's AGC gain, by the AGC baseline's rules on a dB scale. A gain
  cut of 4 dB makes a front end interference-suspect and one of 10 dB is a gain collapse, with
  the same corroboration, `rf_trust` and `jamming_detected`/`station_rf_degraded` events as a
  u-blox AGC departure. A gain rise of 4 dB, which is the antenna path giving less, is a
  `station_rf_degraded` and never a jamming claim. The `observers` feed's `rf` lists such a
  station's `front_ends` (name, `gain_db`, `gain_departure_db`, `sample_variance`,
  `blanked_percent`), and the two events carry `gain_cut_db`, `gain_rise_db` and the front end
  showing each. The thresholds come from one mosaic-G5 P3 and are observational
  (`docs/DEFENSE-PNT.md §2`). Upgrade notes: the `agc` integrity check is version 5 and the
  operating profile gains two thresholds, so every station's `config_hash` changes;
  ReceiverStatus blocks are now stored whole in `rf_samples` as kind `receiver_status` instead
  of in `nav_frames`, and the schema migration widens that table's kind constraint in place
  (an older collector still runs against the migrated database); gain baselines are
  checkpointed in `agc_baselines` beside the AGC ones. Every other SBF block remains
  capture-only.
- The raw-frame historian now creates one-day rather than one-hour TimescaleDB chunks. Production
  measurements put a day of active indexes well inside the memory sizing target, while the wider
  interval avoids thousands of undersized relations and their planner and lock-table overhead.
  Existing chunks are unchanged; upgrades apply the interval to future chunks only.
- Surveyed fixed stations can opt one characterized, vented pressure sensor into regional
  weather context and barometric GNSS-height corroboration. The collector compares 30-minute
  log-pressure tendencies from at least two qualified neighbours within 100 km, exposes missing,
  stale or incoherent context as unknown, and emits `regional_pressure_anomaly` and
  `barometric_height_consistency`. Only an uncertainty-bounded height divergence while regional
  pressure is coherent contributes one lower-only physics domain; local pressure changes and
  ordinary weather do not claim jamming or spoofing. The initial operating points are documented
  in `docs/INTEGRITY.md`, carried in the configuration hash, and require explicit
  `[integrity.station.barometer]` installation qualification.
- Environmental telemetry now reaches two station checks instead of stopping at history.
  `environment_sensor_health` distinguishes invalid, stale, saturated, flatlined, disagreeing
  and calibration-failed sensors; `environment_change` tracks a station-specific temperature,
  humidity and local-pressure baseline, magnitude, rate, 24-hour sustained change and dew-point
  spread. Both are advisory:
  their durable warning events preserve context without changing GNSS assurance or claiming
  jamming or spoofing. The documented operating profile, filter and recovery state are included
  in event evidence and the station configuration hash.
- Integrity Station now decodes and displays the main environment, separately qualified
  barometer, SHT3x, HIH8121, humidity-heater, regulator, and RTC-temperature records. Sensor
  history keeps each physical temperature and humidity channel separate and labels enclosure,
  regulator, and oscillator readings without implying outdoor ambient conditions.
- A collector can send its boards' manufacturer a daily field report: each board
  enrolled under that manufacturer, with the collector's hardware-trust verdict,
  the firmware and update status the board reported, and the hour it was last
  seen. It is off unless the operator sets `[fleet_report]`; boards never report
  to their manufacturer themselves. The format is in docs/FLEET-REPORTS.md.
- `esp32/tools/bench_check.py` tests an assembled S3 board from its console and
  writes a bench report the manufacturer can record against the unit.
- Pre-enrollment QA: `bench_check.py --expect BOARD` holds a board to its
  revision A before it is signed or enrolled, with no Factory CA or network. Every
  part its manifest lists must answer as that part, the receiver must confirm its
  model, and the ZED/X20's W5500 must answer and its panel chain read back.
  `NVF_MANIFEST_INIT` and `NVF_MANIFEST_HDC` build the image that writes a blank
  manifest EEPROM for that sequence (esp32/docs/COMMISSIONING.md). A u-blox receiver
  must acknowledge every setting the firmware makes in its RAM (`receiver.config`), and the
  report gives the UART1 rate the link runs at beside the rate the receiver was found at.
  The console names each key. `receiver.uid` records the receiver's UBX-SEC-UNIQID chip
  ID, `receiver.antenna` fails an antenna SHORT or OPEN (and on the ZED/X20 anything but
  OK), and `receiver.signals` reports the bands tracked. The tool is version 5; its
  reports remain bench report v1.
- The S3 panel plays a boot sequence before it shows reception, the same at every boot.
  A lamp chase comes first: a run of five lit lamps moves along the top row from left to
  right, then along the bottom row, with RTK and ATTENTION at the ends of their rows on a
  24-bit panel, so every lamp is lit once and one that does not light is a gap in the
  run. It takes about three and a half seconds and never lights every lamp at once.
  Then the last 32 bits of the board's EEPROM serial are swept across the columns, green
  for 1 and amber for 0, so a board shows the same sweep at every boot and none when its
  identity was not read. Boots are counted in NVS and the console names the count, so a
  board that keeps restarting shows it (esp32/README.md, Custom-board status LEDs).
- The ZED/X20 console reports the receiver's RTK_STAT pin, which drives the panel's RTK
  lamp: high (no carrier-phase solution), low (RTK fixed) or toggling (corrections in use,
  no fix), one line whenever that changes over 10-second windows.
- The ZED-X20P enables QZSS and NavIC, which every X20 signal plan leaves off, so the
  panel's QZSS and NavIC columns can turn green inside their service regions. It also
  enables UBX-SEC-SIG and takes the jamming state from it: from HPG 2.00 on, MON-RF's
  jamming flag always reads 0, so ZED/X20 boards never reported jamming before. The
  JammingStats records the collector's RF detector reads carry it too: the ESP32, the
  feeder and the collector's UBX reader put SEC-SIG's state in each block's jamState while
  SEC-SIG is current.
- The MAX-M10S tracks GPS, Galileo, BeiDou and GLONASS together: one RAM setting turns
  BeiDou B1I off and B1C, GLONASS and BeiDou on, the combination the M10 needs for all
  four, where its defaults leave GLONASS (and in the SPG 5.10 table BeiDou) off.
- The receiver console logs the u-blox chip ID (UBX-SEC-UNIQID) once, and every 10
  seconds an `RF:` line (jamming and its source; each MON-RF block's antenna supervisor,
  power, AGC, noise and CW suppression) and a `signals:` line (code-locked signals per
  system and kind from NAV-SIG, and the bands they cover). Antenna supervisor changes are
  logged as they happen, SHORT and OPEN as warnings.
- The DS3231M (or MAX31328) RTC's own temperature is read with each environmental sample,
  printed on the console and reported as ObserverDetails tag 26 (`rtc_temperature`), in
  0.25 °C steps. A zero that may be the part's power-on default is withheld.
- The S3 observer sets up over its WPA2 setup network; Bluetooth is off. BLE
  provisioning beside the setup network exhausted internal RAM, so an
  unprovisioned board reset in a loop. The setup label is now a Wi-Fi network QR
  that a phone camera joins. BLE setup remains as the `NVF_BLE_PROVISIONING`
  build option, which does not fit beside the setup network, and the build
  refuses a configuration generated with Bluetooth on.
- The Station app sets an observer up over that network: it reads the Wi-Fi
  label, joins the network, and runs the same Security 2 provisioning and
  `nav-config`, `nav-identity` and `nav-tunnel` endpoints, now served by the
  observer on the browser form's web server.
- The collector reads its authority tables from an optional `authority_file`.
  `navcontrol -authorities` loads the same credential-free file, so the control
  plane no longer needs the collector's DSNs or push TLS key.
- `navcontrol` changes an active enrollment's collections and publication
  without a new device credential, and issues and disables read credentials,
  returning each token once and recording every change by digest.
- Enrollment `publication` and signal selectors use snake_case JSON names that
  match the `[[push.observer]]` keys.
- Collector and firmware clients can retry through matching secondary domains.
- Development setup boots show the persistent setup Wi-Fi credential again.
- The S3 updater verifies signed metadata and firmware, stages downloads, and
  waits for durable observation acknowledgments before unattended installation.
- Release tooling supports separate signing adapters, explicit test keys,
  reproducible builds, resumable publication, promotion, and withdrawal.
- Releases travel on separately rooted tracks: trusted for locked hardware and
  open for boards that are never locked. Devices report which they follow.
- The collector verifies hardware evidence itself. `[[manufacturer_authority]]` pins the
  manufacturer keys; a commissioned device's record and session-bound proof are
  checked in the GNF1 handshake, and every receipt carries the result as
  `hardware_trust` (`none`, `open`, `test`, `trusted`) with the verified
  record's fingerprint. Evidence labels data and never refuses a session.
- An optional signed registry can withdraw a board, including one that is
  connected, and `registry_state` keeps a restart from accepting an older one.
- Board and update output show the verified `hardware_trust` beside the
  device-reported `trust_profile`, and security drift also flags a device that
  reports the trusted track on a session that did not verify as trusted.
- `mfgattest commission-verify` and `registry-verify` check commissioning
  records and registry files offline with public keys only.
- The fresh v1 historian schema retains receipt-time hardware trust, both
  authority IDs, core/commissioning fingerprints and signer/issuer identities.
  Software observers have a null manufacturer authority.
- `navcontrol` provides dedicated Go/PostgreSQL enrollment and service APIs with
  exact Issuing-intermediate authorization, scoped manufacturer/registry policies,
  controlled certificate activation, immutable history and revocation. Explicit
  pairings permit customer-issued credentials on NavListen-manufactured hardware
  without changing manufacturer provenance. No prototype parser or schema
  migration is included.
- A commissioned observer names itself: it connects as the `board-<kind>-<serial>`
  observer ID its commissioning record names, and BLE or browser setup supplies
  only the collector and its token. The Station app reads the name over the
  encrypted `nav-identity` endpoint. Previously the 43-character board name did
  not fit the 32-character station field, so no commissioned board could be
  provisioned under the name its evidence requires.
- Observer IDs beginning `board-` are reserved for hardware enrollment. The
  control plane and its database refuse a software station with such a name, and
  an existing database that holds one stops its upgrade and names the station.
- The collector assesses each station's PNT integrity
  ([station assurance](docs/proposals/STATION-ASSURANCE.md)): position, velocity,
  receiver clock and time checks against the installation configured in
  `[[integrity.station]]`, beside the C/N₀, C/N₀-drop, AGC and receiver-flag checks.
  Authorized observer rows serve the assessment as `integrity`, with every
  check's evidence and the configuration hash. `station_assurance` reports
  confirmed changes, and `spoofing_suspected` now needs two independent physics
  domains, or one with the receiver's own flag.
- Observers and the C feeder send the receiver's solution as telemetry `0x03`
  (NAV-PVT, NAV-CLOCK and NAV-STATUS) and NAV-SAT reception as `0x01` body
  version 2, which adds azimuth, pseudorange residual, quality indicator and
  health. Upgrade the collector first: an older collector discards both as
  malformed. The collector still accepts version 1 reception records.
- A moderate AGC departure is reported as jamming when a simultaneous C/N₀ drop
  across the station's signals accompanies it, for as long as the departure
  lasts, or when a station within 30 km shows interference at the same time;
  the event names those neighbours. Station RF alarms clear at info severity, after five minutes.
- AGC baselines are learned over six hours and restored across collector
  restarts.
- `[[integrity.baseline]]` pairs two co-located stations whose antennas are a
  known distance apart. The `baseline` check compares the distance between
  their positions at each epoch with it, and names a collapse to one position,
  the signature of a single transmitter feeding both receivers.
- The `svs` feed adds `conf_weighted`: the fresh corroborating sources counted
  by vote weight, so a jammed, inconsistent or spoofed station's testimony
  counts for less or nothing. `conf` is unchanged.
- Station events keep a durable copy of the inputs behind them, served by the
  private `/gnss/api/v2/event-evidence`. `stationreplay` reruns the station checks
  over stored inputs or one event's evidence and compares the outcome with the
  stored events.
- Integrity Station shows the assessment on private station details and treats
  `station_assurance` as a station condition.
- The universal ESP32-S3 image recognizes the mosaic-G5 P3 board (`CAT_INTSAT` 5) and
  drives its panel, RTC, sensors, rail monitor and Wi-Fi uplink.
  Single-board P3 test images carry the `gnss-color-mosaic-p3` release family.
- On the P3, the MCP9808 on the LDL1117 regulator's heat spreader throttles Wi-Fi: the
  radio's maximum transmit power steps down from 16.5 to 8.5 dBm while the spreader is at or
  above 95 °C and recovers at or below 85 °C. ObserverDetails tag 23 reports the spreader
  temperature, throttle level and limits as `regulator`, apart from the environmental
  readings; it raises no alarm.
- The P3's SHT35 and HIH8121 humidity sensors are driven when the manifest lists them:
  ObserverDetails tags 24 and 25 report each one's temperature and humidity as `sht3x` and
  `hih8121`, apart from the environment component, with the HIH8121's status bits.
- On the P3, the INA3221's `PWR_ALERT_N` on GPIO41 interrupts on its falling edge: the
  board task reads and logs which rail tripped which limit at once, rather than at the next
  rail sample, and that read releases the latched line.
- The P3's mosaic-G5 is spoken to in SBF on COM2: RAM-only configuration with RTS/CTS flow
  control, 1 Hz PVT, time, measurement, visibility and RF status blocks into the receiver
  status the panel, tag 6, the reception assessment and the RTC use, identity and constellation
  discovery, and a GPIO13 reset pulse, at most every 10 minutes, for a receiver that stops
  answering.
- GNF1 gains the `sbf` push feed, SBF record format 1: the HELLO names the format, the
  WELCOME confirms it, and each Septentrio block travels whole with its own header. The
  collector checks each block's Length and CRC and stores it as it stores a dial-mode SBF
  capture, so it rebuilds byte for byte. `sbf` is a valid feed grant in configuration and the
  control plane. The P3 forwards every block it configures, navigation pages included, and
  pushes on `sbf`: upgrade the collector and grant `sbf` before this firmware reaches a P3,
  since an older collector refuses the session. Of these blocks the collector decodes
  ReceiverStatus only (above).

Trusted rollout remains gated by commissioning and hardware acceptance in
[Update operations](esp32/docs/UPDATE-OPERATIONS.md).
