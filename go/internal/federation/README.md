# `internal/federation` — outbound licensing gate

This package intentionally contains no peer transport. It implements the authorization gate
that must exist first: every send/relay is the intersection of the observation's immutable
receipt policy, the owner's current (narrowing-only) policy, and one enabled,
destination-specific `ExportGrant`.

The evaluator checks source collector, destination, organization/collection/device selectors,
signal, data class, attribution, retention, purpose, validity window, approval revision,
quarantine status, and path-loop prevention. No selector means deny, not wildcard. Inbound
`trusted` status is never treated as an outbound grant.

Anonymous exports cannot claim to preserve an end-to-end observer signature. `signed_raw`
requires full provenance and actual end-to-end signature evidence; the decision labels it as
hardware-authenticated only when the receipt also carried hardware mTLS and verified
manufacturer attestation.
