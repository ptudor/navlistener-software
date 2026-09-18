# Unreleased

- Collector and firmware clients can retry through matching secondary domains.
- Development setup boots show the persistent setup Wi-Fi credential again.
- The S3 updater verifies signed metadata and firmware, stages downloads, and
  waits for durable observation acknowledgments before unattended installation.
- Release tooling supports separate signing adapters, explicit test keys,
  reproducible builds, resumable publication, promotion, and withdrawal.
- Releases travel on separately rooted tracks: trusted for locked hardware and
  open for boards that are never locked. Devices report which they follow.
- The collector verifies hardware evidence itself. `[hardware_trust]` pins the
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
- The historian adds `hardware_trust` and `commissioning_fingerprint` to
  `nav_frames` and `observer_samples`; earlier rows read `none`.

Trusted rollout remains gated by commissioning and hardware acceptance in
[Update operations](esp32/docs/UPDATE-OPERATIONS.md).
