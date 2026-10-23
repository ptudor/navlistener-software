# Unreleased

- A collector can send its boards' manufacturer a daily field report: each board
  enrolled under that manufacturer, with the collector's hardware-trust verdict,
  the firmware and update status the board reported, and the hour it was last
  seen. It is off unless the operator sets `[fleet_report]`; boards never report
  to their manufacturer themselves. The format is in docs/FLEET-REPORTS.md.
- `esp32/tools/bench_check.py` tests an assembled S3 board from its console and
  writes a bench report the manufacturer can record against the unit.
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
  since an older collector refuses the session. The collector does not decode SBF yet.

Trusted rollout remains gated by commissioning and hardware acceptance in
[Update operations](esp32/docs/UPDATE-OPERATIONS.md).
