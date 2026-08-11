# `internal/identity` — trusted observation context

This package is the boundary between hardware/transport identity and administrative policy.
Hardware gives a globally unique identity and cryptographic evidence; the collector's trusted
config or AAA provider assigns the organization, enrollment, collections, and publication rules.

`ObserverContext` is the resolved result carried on every `ingest.RawFrame` and copied into every
historian row. It is deliberately absent from GNF1 DATA: a feeder may present a station name and
credential during authentication, but it may not declare itself public, change owners, or add
itself to a collection.

The zero/omitted behavior is fail-closed:

- organization: `local-unassigned`
- aggregate use: `private`
- station metadata: `none` (`coarse` and `full` are public display tiers, not anonymity)
- attestation: `none`
- event visibility: `private`
- raw export: `deny`, with no federation peers

`public_anonymous` is the only non-attributed public mode. A station published as `coarse` or
`full` is still locatable from its per-SV geometry; coarse is a display courtesy, never a
privacy promise.

The context also retains the server-resolved feed grants and declared receiver capabilities that
were in force at receipt. These are canonicalized sets and persisted beside the policy snapshot;
they never come from ordinary DATA metadata.

The context separates evidence (`CredentialTier`, `AttestationTier`) from authorization
(`PublicationPolicy`). A genuine board is not automatically public, and a public contribution is
not automatically allowed to disclose its station identity.

`ReadPrincipal` is the corresponding client-side authorization result: a stable principal id,
grant revision, and explicit canonical operator/organization/collection audience keys. Parsing
an audience validates syntax only; `ReadPrincipal.Allows` is still required before a server
resolves it. Public is credential-free and is never stored as a private grant.

See `../../../docs/GROUPS-AND-FEDERATION.md` for the complete model and migration plan.
