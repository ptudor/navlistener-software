# `internal/authorization` — control-plane authorization cache

The production collector reads a stable, read-only SQL contract rather than joining Django
tables on its ingest hot path. `navlistener_observer_authorization_v1` has one enabled row per
active operational credential and exposes:

```text
token_sha256, observer_id, organization_id, enrollment_id, collector_instance_id,
collection_ids[], feed_grants[], credential_tier, credential_fingerprint,
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
