# `internal/attestation` — manufacturer hardware binding

This package implements the exact ATECC slot-14 originality statement. It is not an
operational credential and carries no organization, customer, collection, hostname, or
publication policy.

The fixed 72-byte record is:

```text
[version 1][reserved zero 7][P-256 R 32][P-256 S 32]
```

- v1 signs `SHA-256("ATECC-MFG-ATTEST-v1" || atecc_serial[9] || rtc_eui64[8] ||
  board_rev_u16be)` and verifies as `verified_v1_partial`.
- v2 signs `SHA-256("ATECC-MFG-ATTEST-v2" || atecc_serial[9] || rtc_eui64[8] ||
  board_eui64[8] || board_rev_u16be)` and verifies as `verified_v2_complete`.

Parsing rejects unknown versions, nonzero reserved bytes, wrong record sizes, erased factory
ids, non-P-256 keys, out-of-range signature scalars, and signature mismatch. Enrollment stores
the returned statement digest and full-record fingerprint alongside the immutable live-read
identifiers. The manufacturer key remains a trust root distinct from every operational CA.
