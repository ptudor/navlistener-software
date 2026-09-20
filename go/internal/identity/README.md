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

The context separates evidence (`CredentialTier`, `AttestationTier`, `HardwareTrust`) from
authorization (`PublicationPolicy`). A genuine board is not automatically public, and a public
contribution is not automatically allowed to disclose its station identity.

`OperationalAuthorityID` and `ManufacturerAuthorityID` are independent enrollment
facts, as are the exact issuing/core signer SPKIs, core-record fingerprint and
manufacturer-scoped product/revision. Changing operational authority does not
relabel the manufacturer. Software observers have no manufacturer (SQL NULL).
The collector compares these fields during authorization rechecks. Commissioning
and registry signer SPKIs belong to session evidence and are stored at receipt.

`HardwareTrust` and `CommissioningFingerprint` are **session evidence**: what the collector
itself verified from the hardware evidence presented on one GNF1 session
(`../../../docs/COMMISSIONING.md`). They differ from every other field in two ways.

- No authorization source resolves them. A config row, a control-plane row and a periodic
  recheck all yield `none` and an empty fingerprint; `WithSessionEvidence` is the only way a
  value enters a context, and `Normalize` requires the pair to be consistent — a fingerprint
  exactly when trust is established.
- `AuthorizationEqual` deliberately ignores them. They select no audience and no publication
  rule, so two sessions of one observer that proved different things share one policy, and a
  recheck that resolves no evidence is not a change. Each receipt carries what its own session
  proved.

`ReadPrincipal` is the corresponding client-side authorization result: a stable principal id,
grant revision, and explicit canonical operator/organization/collection audience keys. Parsing
an audience validates syntax only; `ReadPrincipal.Allows` is still required before a server
resolves it. Public is credential-free and is never stored as a private grant.

See `../../../docs/GROUPS-AND-FEDERATION.md` for the complete model and migration plan.
