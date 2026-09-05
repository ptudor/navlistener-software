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

## 1. Implementation ledger and the gaps this contract closes

### 1.1 Implemented in navlistener

- The hardware design assigns distinct roles to RTC EUI-64, EEPROM EUI-64, and ATECC serial.
- The feeder/collector mTLS handshake binds exactly one DNS SAN byte-for-byte to the canonical
  observer id; HELLO cannot rename an authenticated observer.
- The shared radiolistener Django control plane has `Organization`, role-carrying
  `Membership`, and `Device.organization`, plus credential history, feed grants, enablement,
  EUI-64, ATECC serial, label, and site.
- navlistener authentication returns a complete server-owned `ObserverContext`; both config
  bootstrap and the versioned DB view provider fail closed. The DB provider has a bounded
  digest-only cache, PostgreSQL `NOTIFY` invalidation, active-session rechecks, exact leaf
  fingerprint binding, and refuses hardware-mTLS labels without verified attestation.
- The collector instance is a stable deployment setting and the operator audience, static
  contexts, database-authorized enrollments, and federation-grant source must agree with it.
  Feed grants and declared hardware capabilities are canonical receipt evidence, not merely
  handshake/config checks.
- Every raw row retains immutable ownership, enrollment, collection, credential/attestation,
  publication, signal, and export-policy evidence from receipt.
- Public and operator live state, detector state, events, cursors, snapshots, and caches are
  separated before aggregation; private input cannot change public bytes.
- Authenticated read principals discover only their server-side grants and select physically
  separated organization/collection state. Private feed/history/SSE responses are no-store,
  long-lived streams are re-authorized, and scoped detectors use independent event cursors.
- Audience discovery exposes an opaque revision over the read authorization, materialized
  grant set, process boundary, and current audience-policy epochs. Integrity Station stores
  the collector URL/read token in Keychain, accepts only discovered audiences, applies the
  selector to polls/history/SSE, partitions snapshots/cursors by server/principal/audience/
  revision, and scopes station bookmarks/labels to the stable first three fields. `401`, `403`,
  audience loss, revision change, logout, and server/principal changes erase the applicable
  private cache family.
- A changed active ingest context emits an ordered scope barrier. Every audience touched by the
  old context is conservatively reset and rebuilt from post-change receipts; detectors re-seed,
  SSE replay/clients cross the policy epoch, pending stale events are discarded, historical
  event reads cannot cross the process/current-policy epoch, and snapshots cover each
  materialized audience independently.
- Manufacturer attestation v1/v2 formatting, signing, verification, and the bench CLI are
  implemented; v2 binds ATECC + RTC + EEPROM identities and board revision.
- The transport-independent federation egress gate intersects receipt policy, current policy,
  and an explicit directed destination grant before any peer transport exists.
- `FEDERATION.md` defines the remaining collector-peer transport, directional inbound trust,
  end-to-end observer-signature passthrough, and trust quarantine.

### 1.2 Baseline gaps identified by this contract

- The collector now has named collection audiences and accepts collection memberships only
  from its trusted authorization contract, but the external shared Django control plane still
  needs the authoritative collection/membership/publication/export schema and migrations.
  Integrity Station's “My Stations” remains a scoped presentation preference, never authority.
- Manufacturer attestation v2 binds ATECC + RTC + EEPROM identities and board revision, but
  the external shared `Device` schema/firmware enrollment path still needs its separate board
  EUI-64 migration. V1 remains explicitly partial evidence.
- The current CA implementation is one CA pair per deployment. That supports an Airport F
  standalone installation, but not several unrelated CA realms inside one process.

The shared Django schema/migrations and .NET/web clients live outside this repository and must
consume these versioned authorization/discovery contracts rather than inventing local group
meaning. The repo-local Swift Integrity Station is the reference operator-client
implementation. `kotlin/` currently contains the Android specification but no source scaffold;
neither that absence nor the external clients' locations makes their migrations implicit.

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
station_metadata   none | coarse | full
aggregate_use      private | public_anonymous | public_attributed
event_visibility   private | public_redacted | public
raw_export         deny | named_peers | public
federation_peers   explicit collector-instance allow-list
constellations     optional (gnssId, sigId) allow-list; empty means all supported
```

Definitions:

- `station_metadata` controls the observers/stations surface. `coarse` serves the public
  station row with coordinates rounded to 0.1° (~10 km) and withholds site detail; `full`
  serves the operator-entered position. Both serve the station's per-SV observations —
  that is the product. Hardware serials and RF security detail stay off the public surface
  at every tier (§6.2).
- `aggregate_use=private` means the observation contributes only to its organization's
  authorized views. It cannot affect public positions, confidence, events, or counters.
- `public_anonymous` permits contribution to public state but never a `perrecv` entry or
  source-identifying event. Public confidence reports a count bucket rather than an exact
  source list when necessary to prevent differencing.
- `public_attributed` permits the configured public station identity and allowed metadata.
- `raw_export` is separately gated because original frames, timestamps, and signatures are
  more identifying than a computed satellite state.

**Per-SV geometry is location — accepted, not defended against (regression fix, resolved
2026-08-10).** Azimuth/elevation look angles, pseudorange residuals, and measured-iono slant
terms are location-equivalent: satellite positions are public, so the az/el of two satellites
at one epoch solve the antenna position outright, and one satellite's angles over time do the
same. The original finding proposed redacting geometry below `full`; the product decision
goes the other way, because a GNSS monitor network's data *is* observer-located geometry —
redacting it guts the product, and a "pseudonymous" tier that still serves observations is a
promise the physics breaks. So the contract is honest instead: **a public station is
locatable, period.** `coarse` is display courtesy (don't print someone's rooftop to seven
decimals), never an anonymity claim, and no tier may be documented or marketed as hiding a
publishing station's position. An owner for whom ~10 km courtesy rounding is not enough sets
`station_metadata=none` / stays off the public audience — the private organization and
operator audiences (§5.3) still see everything. This is the galmon posture (public observer
list, meters-scale position fuzz) made explicit.

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
  credential_fingerprint   exact active leaf SHA-256 for mTLS sessions
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
feed_grants, declared_capabilities, provenance, credential_tier,
credential_fingerprint, attestation_tier, policy_revision
```

Federated observations additionally carry immutable origin observer/peer/certificate
fingerprints and received trust tier. None of these values comes from ordinary DATA envelope
metadata without cryptographic/control-plane resolution.

Historian rows retain the policy/audience decision made at receipt. Later policy changes do
not relabel history. Authorized reprocessing re-evaluates export at read time; it never treats
old “public” state as irrevocably downloadable after a legal withdrawal unless retention law
requires it.

Because a derived aggregate event does not retain an exact causal source set yet, navlistener
uses a deliberately conservative current-policy rule: event history exposed by one process
begins at that process start and advances for an affected audience on every authorization
transition. Older rows remain forensic records but are not served automatically. Explicit,
audited republication is the future widening mechanism; a restart or policy edit never widens.

**The intersection rule, stated once for both paths :** a historical row's
visibility — to a read audience or a federation export alike — is the intersection of its
receipt-time decision and the current policy. Re-evaluation only narrows. Widening never
follows from a policy edit: observations collected under a more private policy become visible
to a wider audience only through an explicit owner-initiated republication action that
relabels the rows in an audited migration. §7.2's export rule is this same intersection
stated for the federation edge.

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

- `nav_frames`: organization, enrollment, collector instance, feed grants, declared
  capabilities, provenance, credential tier, attestation tier, policy revision.
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

Discovery includes an opaque authorization/policy `revision`; clients
must treat a revision change as an authorization boundary, discard that principal's private
payloads/cursors for the server, and create the new cache partition before reading data.

**Event ids and SSE cursors are audience-scoped.** The historian may keep one
internal BIGSERIAL, but the served event `id` / `Last-Event-ID` cursor must be per-audience
(a per-audience monotone counter or an opaque cursor mapping). A single visible sequence
shared across audiences leaks through its gaps — a public client counting missing ids learns
the existence, volume, and timing of private events — and a private-only source allocating
ids would perturb the id bytes of subsequent public events, breaking §10.4 and the stage-5
byte-parity test literally. In a single-audience deployment the public cursor may remain the
row id, so `OUTPUT.md §3` is unchanged until a second audience exists.

### 6.2 Public surface

The public `observers` view includes only policy-approved public stations (`coarse` or `full`).
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
- Partition last-good caches and SSE cursors by `(server, principal, audience, authorization
  revision)`; the first three fields are the stable identity and the revision prevents reuse
  across policy changes.
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

**Selectors intersect receipt-time and current context.** The grant's
organization / collection / device selectors are evaluated against the observation's
immutable receipt context *and* the observer's current administrative context (the
authorization provider's latest result for a local observer; the origin's most recently
journaled context for a relayed one), exactly as §5.2's intersection rule already governs
read audiences. Consequences: after a §4.4 transfer the former organization's grant no
longer selects the observer's historical rows (current policy narrows), and the new
organization's grant never selects rows received under the previous owner (widening needs
the audited republication path). Removing a collection membership narrows a collection
grant the same way; a membership joined later never reaches earlier receipts. Selectors
never name enrollment ids, so a same-owner re-enrollment does not change matching. A
device selector follows the permanent hardware identity across a transfer, but both the
receipt-time and the current owner's publication policies still intersect. A current
context that is absent, invalid, or identifies a different observer denies — including a
relayed observation whose origin context has not been journaled. `approved_by` / `revision`
are audit provenance the evaluator requires but cannot verify: a revoked approver or a
superseded revision is expressed by disabling, expiring, or removing the grant row, and a
transport evaluates only grants from its current control-plane snapshot.

Revoking an export grant stops new transmission immediately, closes/re-authorizes the peer
session, and journals the policy transition. It cannot recall bytes already delivered, so raw
export requires deliberate contractual approval.

### 7.3 Federation envelopes and privacy

Full `origin_receiver_id`, leaf-cert fingerprint, and observer certificate are provided only
for `signed_raw` grants that require end-to-end verification. Anonymous aggregate grants use a
peer-scoped opaque source assertion and cannot be promoted to `trusted` observer provenance.
Anonymized federation data never pretends to preserve ATECC end-to-end identity.

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
   byte-identical when all sources are public and unaffected by a private-only source (this
   holds literally only with audience-scoped event cursors).
6. **Read authorization:** public audience, authenticated org/collection audiences, cache
   separation, redacted station/search/event/SSE consistency.
7. **Clients:** the repo-local Swift Integrity Station implements authenticated discovery and
   selection, Keychain credentials, revisioned audience cache/cursor partitions, scoped local
   preferences, authorization-loss erasure, and system-trusted standalone URL/CA support.
   The repo-local Kotlin implementation and external .NET/web parity remain outstanding in
   their owning workstreams.
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
   search, confidence, or counters — including event-id sequences and cursors  —
   unless anonymous public aggregation is explicitly granted.
5. A client never receives unauthorized data; audience filtering and redaction happen
   server-side, never by the client discarding fields from an over-complete response.
6. A peer's inbound trust never grants that peer outbound access.
7. Federation export is the intersection of source policy and destination grant, checked at
   every hop.
8. Anonymized federation data cannot be presented as end-to-end hardware signed.
9. Revocation closes active ingest/read/peer authority within the documented cache bound.
10. Historical rows preserve the identity/enrollment/policy decision at receipt.
11. Public caches cannot contain private responses; private caches cannot cross principals,
    audiences, or authorization revisions.
12. Manufacturer attestation and operational CA issuance remain separate trust roots.
13. Public station metadata tiers are display precision, not anonymity: any publishing
    station is locatable from its served per-SV geometry, and no tier, doc, or UI may claim
    otherwise (regression fix, resolved). The only non-locatable station is a non-public one.

These invariants are more important than a particular table or endpoint spelling. Any future
implementation that preserves them can evolve without repeating the identity/privacy design.
