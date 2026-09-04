# `internal/authorization` — control-plane authorization cache

The production collector reads a stable, read-only SQL contract rather than joining Django
tables on its ingest hot path. `navlistener_observer_authorization_v2` has one enabled row per
active operational credential and exposes:

```text
token_sha256, observer_id, organization_id, enrollment_id, collector_instance_id,
collection_ids[], feed_grants[], declared_capabilities[], credential_tier, credential_fingerprint,
attestation_tier, aggregate_use, station_metadata, event_visibility, raw_export,
federation_peers[], publish_signals[], policy_revision, enabled
```

`token_sha256` and `credential_fingerprint` are lowercase 64-character SHA-256 hex. The view
must already intersect enabled device, active enrollment/credential, current memberships,
feed grants, and effective owner-narrowed policy. Duplicate active rows fail closed.

The cache key contains only the token digest plus presented station/feed. PostgreSQL
`NOTIFY navlistener_authorization_changed` clears positive and negative entries immediately;
the configured TTL remains the revocation bound if the listener is disconnected. Lookup or
row-validation failure denies the connection. There is no config fallback when database
authorization is enabled.

`navlistener_read_authorization_v1` is the corresponding read contract:

```text
token_sha256, principal_id, audience_grants[], revision, enabled
```

Each `audience_grants` entry is canonical `operator:<instance>`,
`organization:<organization>`, or `collection:<collection>`. Public needs no credential and is
rejected as a stored private grant. Duplicate rows, malformed audiences, and empty grant sets
fail closed. The same cache TTL, generation-safe invalidation, and digest-only key discipline
apply to read credentials.

Retained push contributors are also reconciled while disconnected. The push server retains
only credential digests, with a ceiling of 1,024 observer policy identities and eight
credential/feed pairs per current policy. New admissions beyond those ceilings fail closed;
existing unresolved policy evidence is not evicted to make room. Every configured recheck
interval, up to 16 workers bypass the authorization cache under one five-second sweep
budget. Unreconciled or changed identities advance the ingest policy generation and enqueue
an ordered audience reset, independent of another device connection. The offline withdrawal
bound is the recheck interval plus five seconds and ordered decoder-drain latency; overload
or database failure withdraws unverified derived visibility instead of extending that bound.
Raw history keeps its immutable receipt-time context. A process restart starts a new derived
history epoch. Normal shutdown cancels and joins reconciliation before closing ingest.
