# `internal/attestation` — manufacturer hardware binding

This package implements the exact ATECC slot-14 originality statement. It is not an
operational credential and carries no organization, customer, collection, hostname, or
publication policy.

The fixed 72-byte record is:

```text
[version 1][reserved zero 7][P-256 R 32][P-256 S 32]
```

The only pre-launch format is core v1. It signs:

```text
SHA-256("ATECC-MFG-CORE-v1" || product_u16be || board_rev_u16be ||
       board_eui64[8] || atecc_serial[9])
```

and verifies as `verified_v1_core`. RTC and microcontroller identities are
replaceable and therefore belong in the commissioning record, not this
permanently locked slot.

Parsing rejects unknown versions, nonzero reserved bytes, wrong record sizes, erased factory
ids, non-P-256 keys, out-of-range signature scalars, and signature mismatch. Enrollment stores
the returned statement digest and full-record fingerprint alongside the immutable live-read
identifiers. The manufacturer key remains a trust root distinct from every operational CA.
