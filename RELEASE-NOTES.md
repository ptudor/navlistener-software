# Unreleased

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
  lasts. Station RF alarms clear at info severity, after five minutes.
- AGC baselines are learned over six hours and restored across collector
  restarts.
- The `svs` feed adds `conf_weighted`: the fresh corroborating sources counted
  by vote weight, so a jammed, inconsistent or spoofed station's testimony
  counts for less or nothing. `conf` is unchanged.
- Station events keep a durable copy of the inputs behind them, served by the
  private `/gnss/api/v2/event-evidence`. `stationreplay` reruns the station checks
  over stored inputs or one event's evidence and compares the outcome with the
  stored events.
- Integrity Station shows the assessment on private station details and treats
  `station_assurance` as a station condition.

Trusted rollout remains gated by commissioning and hardware acceptance in
[Update operations](esp32/docs/UPDATE-OPERATIONS.md).
