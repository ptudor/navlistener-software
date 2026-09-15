# Unreleased

- Collector and firmware clients can retry through matching secondary domains.
- Development setup boots show the persistent setup Wi-Fi credential again.
- The S3 updater verifies signed metadata and firmware, stages downloads, and
  waits for durable observation acknowledgments before unattended installation.
- Release tooling supports separate signing adapters, explicit test keys,
  reproducible builds, resumable publication, promotion, and withdrawal.

Production rollout remains gated by commissioning and hardware acceptance in
[Update operations](esp32/docs/UPDATE-OPERATIONS.md).
