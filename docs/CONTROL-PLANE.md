# NavListen control plane

`go/cmd/navcontrol` is the dedicated Go enrollment API. It owns the fresh
PostgreSQL enrollment schema, authority registrations, service history and
collector authorization views. It does not depend on Django or share a radio
application's device table. No prototype records or schemas are imported.

The boundaries are deliberate:

| Component | Responsibility |
|---|---|
| Factory CA Root, slot 5 | Sign permanent core attestations and commissioning records |
| Factory-role unit | Relay manufacturer batches to a Root; never sign them locally |
| Registered Issuing intermediate | Sign operational observer certificates |
| `navcontrol` | Validate controlled enrollment evidence, assign authority/policy, activate and revoke credentials |
| Collector | Authenticate the enrolled credential and exact issuer, verify session evidence, isolate audiences |

Root slot-0 CA keys are not manufacturer keys. Customer deployments run the same
firmware and wire formats. Names, key mappings, pairings and product policy are
configuration/control-plane data, not firmware branches.

## Authority registration

Use the same reviewed authority configuration for `navcontrol` and the collector.
[COMMISSIONING.md §10](COMMISSIONING.md#10-collector-configuration) shows the TOML.
Each `[[operational_authority]]` registers an ID, enabled state, self-signed root
certificates, exact Issuing certificates and permitted manufacturer IDs. Issuing
certificates must be non-root CAs with `pathLen=0` and validate against those
roots. Dual-issued certificates carrying the same SPKI map to the same authority.

Each `[[manufacturer_authority]]` registers separate slot-5 public keys, registry
public keys, one registry/floor stream and explicit product/revision/RTC policies.
The full SPKI SHA-256 is the administrative key identity; short wire key IDs are
only record selectors. Issuing/manufacturer/registry keys cannot be assigned to
two authorities or reused across roles. The database reserves historical key
ownership even when an authority is disabled or removed from current config.

For example, NavListen A/B form one manufacturer authority. Customer C/D form
another, with separate policies even if both use product number 1. A customer's
operational authority may explicitly permit both manufacturers. An A/B board
then retains its A/B manufacturer while receiving customer-issued credentials.
There is no flat A/B/C/D manufacturer bundle and no authority discovery from a
device claim, registry payload, CA subject name or root-chain success alone.

Without authority tables the bootstrap config registers only the token-only
`local` operational authority. Bootstrap observer rows cannot assert manufacturer
provenance. Enroll hardware through `navcontrol`; use database authorization for
those stations. Software enrollment has SQL NULL manufacturer authority and
cannot gain hardware trust from its token or certificate.

## Run the API

Build without installing or starting a service:

```sh
cd go
go build -o build/navcontrol ./cmd/navcontrol
```

Start explicitly with an administrator-owned configuration, a private regular
DSN file (0600), and a high-entropy operator bearer token whose SHA-256 is supplied
as `-operator-token-sha256`. Keep the actual token in private operator tooling,
never a command example, source file or URL.

```sh
build/navcontrol -config /etc/navlistener/navlistener.toml \
  -dsn-file /etc/navlistener/control.dsn \
  -operator enrollment-operator \
  -operator-token-sha256 OPERATOR_TOKEN_SHA256
```

The API binds loopback `127.0.0.1:5581` by default and refuses non-loopback binds.
Remote access requires an authenticated TLS reverse proxy. This initial API is
for a single trusted enrollment/service operator, not an end-user account or
self-enrollment service. Do not expose its bearer token to devices or customers.
It initializes the fresh schema and synchronizes authority registrations at
startup; authority config changes require coordinated API/collector restarts.
Use a separate read-only database role for the collector, granting SELECT on
`navlistener_observer_authorization_v3` and `navlistener_read_authorization_v1`.
Database initialization and writes require only the control service's role.

## Controlled enrollment and certificate issuance

1. Read the board EEPROM EUI-64, product/revision and ATECC serial live at the
   controlled bench. Read back the permanent 72-byte attestation and complete
   225-byte commissioning record. Validate component expectations and the live
   identity checks in [the commissioning procedure](COMMISSIONING.md).
2. For hardware mTLS, use the established secure-element procedure to generate
   the operational key and CSR, and validate that CSR/key against the live ATECC.
   A copied record plus an arbitrary software CSR is not proof of hardware key
   custody. Record this controlled check in the private bench ledger; its stable
   reference is `hardware_validation` in the API request.
3. Obtain the leaf through the configured Factory CA Issuing unit using `cactl
   sign` and its verified-result/retry procedure. Do not use a Root slot-5 key or
   add a file-based CA fallback. Store the public ceremony request/result with
   the bench entry so an interrupted issuance can be reconciled by exact retry.
4. POST the request to `/v1/enrollments/validate`, then `/v1/enrollments`, with
   `Authorization: Bearer <operator-token>`. Validation alone changes no database
   state. Activation returns an `enrollment_id` and a newly generated device token
   once. Only its digest is stored. Responses are `no-store`.
5. Install the operational credential using the device's supported procedure,
   reconnect, and confirm collector admission and the independently evaluated
   hardware evidence. Offline signature validation does not establish `trusted`;
   that requires the commissioned MCU's proof bound to the actual TLS session.

The API validates CSR proof of possession, exact sole DNS SAN, leaf/CSR SPKI
equality, P-256 for hardware operational keys, certificate validity/client usage
and the exact registered issuing SPKI. It does not open a serial device, generate
hardware keys, flash, burn eFuses or claim it can prove non-extractability remotely.
Current ESP32 push builds use bearer authentication; integrating an ATECC-backed
TLS client is a separate firmware path, not implied by accepting a controlled
hardware CSR at enrollment. Bearer hardware enrollment still verifies core and
commissioning provenance and must provide MCU session proof for `trusted`.

Required request fields are `observer_id`, `operational_authority_id`,
`organization_id`, `collector_instance_id` and `feed_grants` (`ubx` and/or `rtcm`).
Hardware adds `manufacturer_authority_id`, `hardware_validation`, integer
`product`/`revision`, lowercase unseparated hex `board_eui64`, `atecc_serial`,
`core_record` and `commissioning_record`. The observer ID is exactly lowercase
hyphen-separated board EUI bytes. Software omits all hardware fields.
`csr_pem` and `certificate_pem` must be supplied together for mTLS or both omitted
for bearer-only enrollment. Certificates contain no organization or manufacturer
claims that override the server record.

Optional `collection_ids`, `declared_capabilities` and `publication` are
operator-controlled policy. Omitted publication is private, metadata hidden and
raw export denied. See `identity.PublicationPolicy` for its JSON fields; feed and
audience grants do not come from ordinary device messages. Bodies are bounded to
64 KiB; unknown fields, duplicate JSON keys and trailing content are rejected.

`navl_read_credentials` is deliberately separate. An administrator may register
read-token digests and canonical audience grants there and emit
`NOTIFY navlistener_authorization_changed`; enrolling an observer or knowing the
operator API token does not grant access to historical/private observations.

## Service, revocation and receipts

Service enrollment names `replace_enrollment_id` explicitly. In one serializable
transaction the API validates the existing station, enforces identity uniqueness
and manufacturer registry floor, snapshots the new evidence/policy, revokes the
old credential, updates the current device and appends an operator service event.
The old enrollment ID remains required after revocation. Concurrent or stale
requests fail rather than silently replacing a more recent enrollment.

An operational key/authority/owner change preserves the permanent core and
manufacturer. An RTC or MCU binding change requires a different valid
commissioning record with strictly greater generation. ATECC replacement requires
`service_action: "replace_atecc"`, a stable `service_approval` ledger reference,
a changed ATECC serial, a newly signed core and a higher-generation commissioning
record. A replacement authority is permitted only with that new valid chain and
an allowed operational/manufacturer pairing; relabeling an existing core fails.

EEPROM/PCB replacement has a new board-derived observer ID. Revoke the old
enrollment and enroll the new identity separately; record administrative
continuity in the service ledger, never as an alternate observer-ID parser.

POST `{"enrollment_id":"..."}` to `/v1/enrollments/revoke` to withdraw a credential.
The view filters active credentials, expiry, enabled authorities and current
pairing policy. PostgreSQL NOTIFY invalidates collector caches; configured TTL
and session reconciliation bound withdrawal if notification is unavailable.

For physical replacement: revoke/stop the old session first when its binding is
compromised; record progress in the bench ledger; prepare/install the new signed
record, publish a higher-sequence current registry, activate the replacement and
reconnect. Do not keep both MCU/core bindings current. An interrupted transition
may lose hardware trust; restoration is a new generation, not rollback.

Enrollment history retains the exact core and commissioning bytes, full signer
and issuer pins, certificate, registry sequence/signer and policy snapshot.
Collector raw and board-sample rows independently retain receipt-time operational
and manufacturer authorities, commissioning fingerprint and signer evidence.
Service changes do not relabel history. Board and ATECC IDs are globally unique;
non-null RTC instance IDs are unique, but absent instances and repeated models
are allowed. Metadata privacy still applies to all identifiers.

## Verification and release boundary

`make -C go check` covers Go/C/Python contract tests. Set
`NAVLISTENER_CONTROL_TEST_DSN` to a disposable PostgreSQL instance to run the
additional `go test ./internal/control` transaction tests; each creates and drops
only its own randomly named schema. Store integration tests additionally need
the documented TimescaleDB test instance. Synthetic A/B/C/D tests cover independent
keys, mixed allowed pairings, wrong-issuer/cross-manufacturer rejection, product
policies, registry streams/floors and service snapshots.

Portable no-RTC and model-only fixtures and open/trusted firmware builds are not
a physical basic-board qualification. Before first enrollment, a reviewed board
port must pass cold boot/time bootstrap, reconnect, interrupted commissioning,
live component mismatch and actual MCU session-proof checks. A new model code
alone does not add a driver. No bench result is implied by host tests.
