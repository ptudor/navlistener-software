# Unreleased

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

Trusted rollout remains gated by commissioning and hardware acceptance in
[Update operations](esp32/docs/UPDATE-OPERATIONS.md).
