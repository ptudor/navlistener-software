# navlistener — identity, organizations, groups, audiences, and federation

**Status: normative target architecture (2026-08-10).** This document closes the ownership,
privacy, and federation-policy gap between the hardware identity design
(`HARDWARE-OBSERVER.md`), the shared Django AAA plane (`radiolistener`), the collector data
plane, the native output contract (`OUTPUT.md`), and the operator clients. It distinguishes
what is implemented today from the target contract so a partially migrated deployment fails
closed rather than silently publishing private observations.

> One line: **hardware proves which physical observer spoke; the enrolling authority assigns
> who owns it and where it may operate; collections assign operational purpose; publication
> policy decides which audiences may use its observations; federation moves only explicitly
> exported observations between explicitly trusted collector instances.**

---

## 0. The passport, licence, fleet, and treaty analogy

The analogy is useful as long as the layers stay separate:

| System concept | Analogy | Can it change? |
|---|---|---|
| MCP79412 EUI-64 | passport number — the globally unique public name | only on RTC replacement |
| manufacturer attestation | the issuing authority's anti-forgery proof over the original hardware | no; manufacturing record |
| ATECC operational private key | the holder's ability to prove possession | yes; slot 0 is deliberately regenerable |
| operational mTLS certificate | a driver's licence issued by the admitting collector/organization CA | yes; issue, rotate, revoke |
| `Organization` | legal owner / employer | yes; audited transfer |
| `Collection` | the fleet, programme, or public feeder group in which it operates | yes; many memberships |
| `PublicationPolicy` | dispatch/privacy rules governing who may use its work | yes; policy-controlled |
| collector `InstanceCertificate` + peer edge | a treaty between two licensing jurisdictions | yes; directional and revocable |

The hardware identity MUST NOT contain an organization, customer, collection, hostname, or
public/private state. Those facts change during a board's life. The operational certificate's
single DNS SAN remains only the canonical EUI-64-derived observer id; the certificate issuer
and server-side enrollment record establish the current jurisdiction.

---

## 1. Current implementation and the gaps this contract closes

### 1.1 Already implemented

- The hardware design assigns distinct roles to RTC EUI-64, EEPROM EUI-64, and ATECC serial.
- The feeder/collector mTLS handshake binds exactly one DNS SAN byte-for-byte to the canonical
  observer id; HELLO cannot rename an authenticated observer.
- The shared radiolistener Django control plane has `Organization`, role-carrying
  `Membership`, and `Device.organization`, plus credential history, feed grants, enablement,
  EUI-64, ATECC serial, label, and site.
- navlistener authenticates a configured observer, stamps a canonical `source_id`, enforces
  feed grants, and persists replay-safe raw frames.
- `FEDERATION.md` defines a future collector-peer graph, directional inbound trust,
  end-to-end observer-signature passthrough, and trust quarantine.

### 1.2 Not implemented before this contract

- navlistener's config-backed authenticator returns only an observer id, not ownership or
  publication context; the shared Django AAA model is not wired into navlistener.
- There is no named server-side collection/group. The planned Swift/Kotlin clients' “My
  Stations” lists are local bookmarks, not authorization boundaries.
- There is no public/private or audience policy. The v2 serve listener is a single view, and
  `svs.perrecv`, confidence, events, RF state, and the observer feed can reveal a station.
- `nav_frames` carries only `source_id`; events and snapshots have no organization,
  collection, audience, enrollment, collector-instance, or provenance scope.
- Federation specifies inbound trust (what peer data may do here) but not outbound export
  authorization (which local data the peer is allowed to receive).
- The hardware document says all three factory identifiers are recorded, but the shared
  `Device` row has no separate EEPROM/board EUI-64. Manufacturer attestation v1 binds the
  ATECC serial, RTC EUI-64, and board revision, but not the EEPROM EUI-64.
- The current CA implementation is one CA pair per deployment. That supports an Airport F
  standalone installation, but not several unrelated CA realms inside one process.

No deployment may claim tenant privacy or safe federation until the applicable items above
are migrated.

---

## 2. The administrative model

These are separate models. A convenient UI may edit several in one workflow, but neither the
database nor the collector may collapse them into one `group` or `public` flag.

### 2.1 `Organization` — ownership and human authorization

An observer has exactly one current owning organization. Organization membership carries the
human role (`owner`, `operator`, `viewer`) used to enroll, transfer, rotate, revoke, assign
collections, and change publication policy. Organization is the billing, contractual, and
default privacy boundary.

An enabled observer with no organization is **unassigned**: superuser-visible and ingest may
remain enabled for migration, but it is private, non-exportable, and absent from every
non-superuser read view. Deleting an organization is prohibited while it owns devices.

### 2.2 `CollectorInstance` — a deployment and trust domain

A collector instance is a stable server identity, not a process boot:

```text
CollectorInstance
  id              stable opaque id (for example airport-f-onsite)
  organization    administrative owner
  display_name
  instance_cert   federation certificate/fingerprint
  ca_fingerprint  operational observer CA trusted by this instance
  mode            hosted | standalone
  enabled
```

`standalone` means no central service is required: the control plane, CA, collector,
historian, and read API can all be on site. It does not imply federation; the empty peer set
is the normal default.

### 2.3 `Enrollment` — admission into a collector realm

Enrollment is the auditable, time-bounded binding between permanent hardware and an operating
realm:

```text
Enrollment
  id
  device
  collector_instance
  credential
  organization_at_enrollment
  rtc_eui64
  board_eui64
  atecc_serial
  manufacturer_attestation_status
  manufacturer_attestation_fingerprint
  manifest_fingerprint
  enrolled_at / enrolled_by
  ended_at / ended_by / end_reason
```

Only one active enrollment per `(device, collector_instance)` is allowed. A stricter
single-active-realm policy may be selected for sold hardware; the schema keeps history either
way. The collector stamps the enrollment id resolved at authentication time and never accepts
one self-reported by the feeder.

### 2.4 `Collection` — operational group, programme, or shared data product

A collection is a server-side policy and presentation group:

```text
Collection
  id / slug / display_name
  owning_organization       nullable only for a deliberately cross-org collection
  kind                      private_fleet | public_community | institution | site | other
  publication_policy
  enabled

DeviceCollectionMembership
  device
  collection
  valid_from / valid_until
  assigned_by
```

A device has one owner but may belong to several collections. For example, an Institution C
receiver can belong to its internal “roof stations” collection and, with explicit permission,
the cross-organization “Public Community Feeders” collection. Collection membership never
changes device ownership or credential validity.

Cross-organization collections require an explicit grant from each device's owning
organization. A collection administrator cannot make somebody else's observer public.

---

## 3. Publication is a capability set, not a boolean

The satellite payload is public broadcast, but an observation discloses private facts about
the observer: existence, location, uptime, clock, RF environment, jamming/spoofing conditions,
hardware capability, and organizational activity. A single `public=true` cannot express safe
use.

`PublicationPolicy` therefore has independent grants:

```text
station_metadata   none | pseudonymous | coarse_location | full
aggregate_use      private | public_anonymous | public_attributed
event_visibility   private | public_redacted | public
raw_export         deny | named_peers | public
federation_peers   explicit collector-instance allow-list
constellations     optional (gnssId, sigId) allow-list; empty means all supported
```

Definitions:

- `station_metadata` controls the observers/stations surface. `pseudonymous` uses an
  audience-stable alias and omits exact coordinates, site, hardware serials, RF detail, and
  owner. It is not a weak hash of the global EUI-64.
- `aggregate_use=private` means the observation contributes only to its organization's
  authorized views. It cannot affect public positions, confidence, events, or counters.
- `public_anonymous` permits contribution to public state but never a `perrecv` entry or
  source-identifying event. Public confidence reports a count bucket rather than an exact
  source list when necessary to prevent differencing.
- `public_attributed` permits the configured public station identity and allowed metadata.
- `raw_export` is separately gated because original frames, timestamps, and signatures are
  more identifying than a computed satellite state.

### 3.1 Policy inheritance and precedence

The organization sets the maximum disclosure. Collection and device policy may narrow it;
they may not widen it without an owning-organization owner/operator action. Multiple
collection memberships combine by audience, not by “most public wins.” A device contributes
to an audience only through a valid membership or explicit device grant for that audience.

Defaults are always:

```text
private; no station metadata; no public aggregation; no raw export; no federation peers
```

Missing, malformed, stale, or temporarily unavailable policy resolves to those defaults.

---

## 4. Hardware originality, enrollment, and transfer

### 4.1 Identifier roles

| Part | Stored role |
|---|---|
| MCP79412 EUI-64 | canonical observer id and certificate SAN |
| 24AA025E64 EUI-64 | immutable board/PCB inventory serial |
| ATECC608C serial | immutable secure-element identity tied to the operational key and attestation |

All three are normalized and uniquely indexed in the control plane. Replacement is an audited
hardware event, never an in-place silent edit. The collector stores immutable enrollment
snapshots rather than joining historical observations against the device's current values.

### 4.2 Manufacturer attestation v2

If “original hardware” covers the complete three-part board identity, the manufacturer
statement includes the EEPROM EUI-64:

```text
SHA-256(
  "ATECC-MFG-ATTEST-v2" ||
  atecc_serial[9] ||
  rtc_eui64[8] ||
  board_eui64[8] ||
  board_rev_u16be
)
```

v1 remains verifiable and is recorded as `verified_v1_partial`; it proves the ATECC + RTC +
board-revision binding but makes no claim about the EEPROM. v2 is `verified_v2_complete`.
Unknown version, signature failure, duplicate factory identifier, or a mismatch between live
reads, CSR SAN, and the signed statement fails hardware enrollment. It may fall back to an
explicitly approved software/bootstrap class, but must never be labelled hardware-attested.

The manufacturer key is distinct from every operational CA. A board can be shipped clean,
later enroll under Airport F's standalone CA, and retain the same originality proof.

### 4.3 Enrollment transaction

Hardware enrollment is one atomic/audited workflow:

1. Read RTC EUI-64, board EUI-64, ATECC serial, manifest, attestation record, and public key.
2. Verify manufacturer signature and uniqueness of all three identifiers.
3. Require the CSR signature to verify under the ATECC public key and its sole DNS SAN to equal
   the normalized RTC EUI-64.
4. Select owning organization, collector instance, initial collections, and publication policy.
5. Sign the operational certificate with that instance's configured CA.
6. Store the device, immutable attestation result, credential, active enrollment, membership,
   policy, and audit rows in one transaction.
7. Return the public certificate/chain and policy revision; no private key leaves the ATECC.

At every boot the feeder checks certificate SAN against the live RTC EUI-64. On connection the
collector verifies certificate chain, SAN, enabled device, active credential/enrollment, feed
grant, and current policy. A policy revision is server state, never a feeder assertion.

### 4.4 Transfer

Transfer ends the old enrollment and collection memberships, revokes old credentials, changes
ownership in one audited workflow, and enrolls under the recipient's realm. Permanent hardware
identity and manufacturer attestation do not change. Slot 0 may be regenerated so the former
owner cannot retain operational key access.

---

## 5. Collector data-plane enforcement

### 5.1 Authentication returns an immutable context

The authenticator contract returns an `ObserverContext`, not a string:

```text
ObserverContext
  observer_id
  organization_id
  enrollment_id
  collector_instance_id
  credential_tier          token | software_mtls | hardware_mtls
  attestation_tier
  feed_grants
  declared_capabilities
  policy_revision
  audience_grants[]
```

The collector caches this indexed DB lookup for a bounded interval and invalidates on control
plane `NOTIFY`; it never calls Django on the ingest hot path. An active connection is closed or
re-authorized when device, credential, enrollment, or policy is revoked.

The current config authenticator remains a bootstrap/dev provider. Its observations receive
the synthetic organization `local-unassigned` and the private/no-export policy unless every
field is explicitly configured. Configuration cannot claim manufacturer attestation.

### 5.2 Every observation is stamped before decode

`RawFrame` and the durable row carry the server-resolved context:

```text
source_id, organization_id, enrollment_id, collector_instance_id,
provenance, credential_tier, attestation_tier, policy_revision
```

Federated observations additionally carry immutable origin observer/peer/certificate
fingerprints and received trust tier. None of these values comes from ordinary DATA envelope
metadata without cryptographic/control-plane resolution.

Historian rows retain the policy/audience decision made at receipt. Later policy changes do
not relabel history. Authorized reprocessing re-evaluates export at read time; it never treats
old “public” state as irrevocably downloadable after a legal withdrawal unless retention law
requires it.

### 5.3 Audience-safe live state

Privacy is enforced **before aggregation**. Building one global state and deleting observer
fields during JSON serialization is unsafe: private sources change `conf`, last-seen, selected
ephemeris, detector transitions, counters, and timing.

The canonical decode may be shared, but every contribution and detector input is selected for
an `Audience`:

```text
public
organization:<id>
collection:<id>
operator:<collector-instance-id>
```

Audience views are materialized/cached from authorized source sets. Events inherit the
audience(s) whose authorized inputs caused them. Cross-audience corroboration may be computed
for internal defense, but its result cannot be emitted into an audience that could infer a
hidden observer unless the contributing policy explicitly permits anonymous aggregate use.

### 5.4 Persistence

The target schema adds immutable scope columns to raw frames, events, and snapshots, and
separate membership/grant tables in the control plane. At minimum:

- `nav_frames`: organization, enrollment, collector instance, provenance, credential tier,
  attestation tier, policy revision.
- `gnss_events`: audience id/type and redaction class.
- `gnss_snapshots`: audience id/type in both uniqueness/query indexes.
- quarantine: origin peer and received trust tier, physically/logically excluded from
  authoritative audiences unless a corroboration read explicitly requests it.

Direct client database access is not an authorization mechanism. The read service owns scope
checks; PostgreSQL RLS is defense in depth for Django/reporting roles.

---

## 6. Read API and clients

### 6.1 Authentication and audience selection

The native field shapes remain v2. Audience is request context, not a new satellite schema:

- Anonymous requests receive only the `public` audience.
- Authenticated users receive organizations/collections granted by their Django membership.
- Machine clients use audience-scoped API tokens or mTLS, never observer ingest credentials.
- An explicit audience is selected by a path or header validated against the principal. The
  server never accepts a free-form organization id and never returns all audiences for the
  client to filter.
- Standalone clients use the on-site base URL and its local identity provider/CA. They do not
  require `intsat.space` or any federation connection.

Cache keys include audience and authorization policy revision. Shared proxy caches must never
cache a private response as public; private responses use `Cache-Control: private` and `Vary`
on the authorization/audience selector. Anonymous public responses remain cacheable.

### 6.2 Public surface

The public `observers` view includes only policy-approved attributed or pseudonymous stations.
Hardware serials, exact site, exact location, internal organization ids, policy, CA, peer path,
and RF security detail are absent unless individually authorized for public release.

`svs.perrecv`, `conf`, `global.total_live_receivers`, events, station search, and SSE are all
built from the same public audience. No endpoint is allowed to reveal a station omitted by
another public endpoint.

### 6.3 Operator clients

Swift, Kotlin, .NET, and web clients share these rules:

- Store the server URL and a read-side credential in platform secure storage. Never hold the
  feeder bearer token, ATECC key, or GNF1 credential.
- Discover allowed organizations/collections after login; “My Stations” is a server-scoped
  view, while local ordering/labels remain presentation preferences.
- Include the selected audience on polling, history, station lookup, and SSE reconnect.
- Treat `403` as loss of authorization, not an empty group; erase private cached payloads on
  logout, revocation, server change, or audience loss.
- Partition last-good caches and SSE cursors by `(server, principal, audience)`.
- Certificate-pin or explicitly trust the on-site CA for standalone deployments according to
  platform policy; never disable TLS verification in release builds.

The public Integrity Constellation Map remains credential-free and receives the public
audience only. Integrity Station is the authenticated organization/collection client.

---

## 7. Federation: acceptance and export are two different directed edges

`FEDERATION.md`'s inbound trust map remains authoritative for what received observations may
do. This document adds the missing outbound half.

### 7.1 Inbound trust

For peer B sending to collector A:

```text
InboundTrust(A <- B, gnssId, sigId) = trusted | read_only | pending | revoked
```

It controls authoritative state, detector arming, and quarantine at A. It does not grant B
access to any of A's local observations.

### 7.2 Outbound export grant

For collector A sending to peer B:

```text
FederationExportGrant
  source collector instance A
  destination peer B
  permitted organizations / collections / devices
  permitted (gnssId, sigId)
  data class              aggregate | decoded | raw | signed_raw | cert_directory
  attribution             anonymous | origin id | full provenance
  max retention / purpose
  valid_from / valid_until
  approved_by / revision
  enabled
```

An observation is exported only when both its receipt-time publication policy and a current
destination-specific export grant allow the requested data class. The intersection wins.
There is no wildcard export default. A peer's request, subscription, or `feeds` HELLO field
can only narrow an existing server-side grant.

Revoking an export grant stops new transmission immediately, closes/re-authorizes the peer
session, and journals the policy transition. It cannot recall bytes already delivered, so raw
export requires deliberate contractual approval.

### 7.3 Federation envelopes and privacy

Full `origin_receiver_id`, leaf-cert fingerprint, and observer certificate are provided only
for `signed_raw` grants that require end-to-end verification. Anonymous aggregate grants use a
peer-scoped opaque source assertion and cannot be promoted to `trusted` observer provenance.
Pseudonymization never pretends to preserve ATECC end-to-end identity.

`path[]` and loop prevention remain required. Every relay re-checks its own outbound grant;
permission from the origin to peer A does not imply permission from A to peer C. Federation is
not transitive data licensing.

### 7.4 Control-plane journal additions

In addition to peer/trust/certificate entries, the journal records export-grant revision,
revocation, and source-policy tombstones without publishing private grant contents to peers
that are not parties. Observation traffic remains outside the journal.

---

## 8. Required deployment shapes

| Scenario | Ownership/enrollment | Publication | Collector/CA | Federation |
|---|---|---|---|---|
| Public Community Feeder Group | device keeps its owning org; explicit membership in the cross-org public collection | public anonymous or attributed, per owner grant | hosted fleet CA | only peers named by public collection export policy |
| Customer A, not public | `Organization=Customer A`; customer collection | private; no public aggregation unless contract separately permits it | hosted collector or customer realm | default deny; explicit destination grants only |
| Institution C, public | `Organization=Institution C`; institution + public collection memberships | metadata and attribution chosen independently | hosted or institution CA | allowed only for the public collection/data classes approved |
| Airport F, private/on-site | `Organization=Airport F`; site collection; local enrollment | private organization/operator audiences | on-site standalone collector and Airport F CA | empty peer set by default; no central dependency |

An installation can change rows in this table without changing the board's permanent hardware
identity.

---

## 9. Migration and fail-closed rollout

Implementation is staged; each stage has a safe compatibility mode:

1. **Control-plane schema:** board EUI, attestation result, collector instance, enrollment,
   collection/membership, publication policy, audience/API grants, export grants, audit rows.
2. **Hardware attestation v2:** shared formatter/verifier, bench tool, firmware read/report,
   v1 partial migration status, duplicate-id constraints.
3. **Collector auth context:** DB-backed authenticator, bounded cache + invalidation, config
   bootstrap mapped to private/unassigned.
4. **Scoped persistence:** immutable context columns and audience fields; existing rows migrate
   to private `legacy-unassigned`, never public.
5. **Scoped state/detectors:** audience-selected inputs; parity tests proving public output is
   byte-identical when all sources are public and unaffected by a private-only source.
6. **Read authorization:** public audience, authenticated org/collection audiences, cache
   separation, redacted station/search/event/SSE consistency.
7. **Clients:** authenticated discovery and selection, secure credentials, audience-partitioned
   caches/cursors, standalone URL/CA support.
8. **Federation egress before federation transport:** export-grant evaluation and audit are
   implemented/tested before the first peer can receive a frame.
9. **Federation transport/inbound trust:** proceed with `FEDERATION.md` P10 using the egress
   gate on every relay.

Until stages 1–6 are complete, the sole v2 view is classified as an operator/development view;
it must not be exposed as a multi-tenant public/private service.

---

## 10. Security and conformance invariants

Tests and review must keep these statements true:

1. Reassigning an organization or collection never changes hardware identity.
2. A certificate or HELLO cannot self-assign organization, enrollment, audience, or policy.
3. Unknown policy is private and non-exportable.
4. Adding a private observer cannot change any byte of the public feeds, events, SSE, station
   search, confidence, or counters unless anonymous public aggregation is explicitly granted.
5. A client never receives unauthorized data and filters it locally.
6. A peer's inbound trust never grants that peer outbound access.
7. Federation export is the intersection of source policy and destination grant, checked at
   every hop.
8. Anonymous/pseudonymous federation data cannot be presented as end-to-end hardware signed.
9. Revocation closes active ingest/read/peer authority within the documented cache bound.
10. Historical rows preserve the identity/enrollment/policy decision at receipt.
11. Public caches cannot contain private responses; private caches cannot cross principals or
    audiences.
12. Manufacturer attestation and operational CA issuance remain separate trust roots.

These invariants are more important than a particular table or endpoint spelling. Any future
implementation that preserves them can evolve without repeating the identity/privacy design.
