# Proposal: board identity and basic GNSS observers

Status: **the pre-launch v1 wire-format migration is implemented in this
repository; the broader control-plane and basic-board rollout remains under
implementation review**.

Prepared: 2026-09-19. Implementation baseline examined: `f847a1f`.
Wire-format implementation updated: 2026-09-19.

Implementation update: core attestation v1, the 153-byte commissioning
statement/225-byte record, registry authority scoping, board-derived observer
identity, exact live firmware matching, shared fixtures, and the maintained
factory signer format have replaced the development prototypes. There is no
prototype compatibility parser. Remaining work described here includes the
external shared control-plane authority model, an independently ported no-RTC
board target, and physical burn/bench acceptance.

This proposal makes the manifest EEPROM's factory EUI-64 the canonical identity
of a hardware observer, binds it to the ATECC in the permanent manufacturer
attestation, and moves the RTC model and optional instance identity into signed
commissioning metadata. It allows a GNSS-only board to participate without an
RTC or environmental sensors. It preserves the separate path for software feeders
attached to ordinary USB or serial receivers. It also defines how vendor and
customer-owned CA pairs coexist without granting either authority across the
other's enrollments.

NavListen is v1, with zero enrolled clients and no deployed hardware or software
fleet. The record layouts currently in the code are development prototypes, not
adopted standards with compatibility obligations. This proposal defines the
first attestation, commissioning, registry and bench-report standards used for
enrollment as v1 and replaces those prototypes before the first enrollment.
Encoders emit v1 and decoders require v1; a future incompatible change
increments the affected standard. There is no migration layer or alternate
interpretation of prototype records.

The existing [commissioning contract](../COMMISSIONING.md) is authoritative for
the implemented formats. Normative words below describe the wider target where
they go beyond that contract.

Reading guide:

- [Decisions](#1-decisions-proposed) and [current scope](#3-current-implementation-and-scope).
- [Identity model](#4-identity-model), including RTC model constants versus unique identifiers.
- [Trust and capabilities](#5-trust-and-capability-semantics).
- [Core attestation](#6-permanent-manufacturer-attestation-v1),
  [commissioning layout](#7-commissioning-statement-v1) and
  [session evidence](#8-session-proof-and-evidence-transport).
- [Registry/enrollment](#9-registry-enrollment-and-persisted-identity),
  [basic and software contributions](#10-basic-device-mode-and-software-contributions),
  and [time bootstrap](#11-time-bootstrap-without-an-rtc).
- [Service lifecycle](#12-manufacturing-and-service-lifecycle),
  [versioning/rollout](#13-v1-standards-and-development-rollout),
  [implementation tasks](#14-implementation-work-breakdown),
  [verification](#15-verification-and-acceptance-criteria) and
  [review decisions](#16-questions-for-review).

## 1. Decisions proposed

1. The factory EUI-64 in the board EEPROM is the permanent hardware observer
   identity. Use its lowercase, hyphen-separated rendering for the observer ID
   and, when using mTLS, the certificate's sole DNS SAN.
2. The ATECC serial identifies the secure element bound to that board. Its
   operational key proves possession when hardware-backed authentication is
   implemented and provisioned. The serial itself is not a cryptographic proof.
3. RTC model and RTC instance identity are separate. A model code identifies the
   fitted RTC type; a factory EUI-64, where available, optionally identifies an
   individual part. Neither is the canonical observer ID or required on a
   GNSS-only board.
4. Permanent slot-14 attestation binds the EEPROM, ATECC, product and board
   revision. It does **not** include service-replaceable RTC identity.
5. The manufacturer-signed commissioning record declares RTC presence and model,
   with an optional RTC EUI-64, alongside the MCU, security profile and permanent
   attestation fingerprint. RTC models without factory identifiers are supported.
6. Missing expected hardware, failed reads and intentional absence are distinct.
   Optionality is declared in signed data; a failed read cannot manufacture it.
7. Hardware provenance, operational authentication, measurement capabilities,
   current health and sharing permissions remain separate dimensions.
8. Scope operational certificate issuers and manufacturer signing keys by
   server-owned authority IDs. Do not treat all configured roots or manufacturer
   keys as one collector-wide trust bundle.
9. Establish attestation v1, commissioning v1, registry v1 and bench-report v1.
   Use evidence-envelope v1 to carry the length-delimited record and MCU-proof
   v1 to bind its exact fingerprint to the TLS session.
10. Replace all prototype records, fixtures and format definitions before the
   first enrollment. Every parser validates its version and exact layout, with no
   fallback parser for a prototype interpretation.

The main design choice beyond making the RTC optional is **where its signature
lives**. Putting an optional RTC in permanent slot 14 would still make later RTC
replacement or a change of RTC model depend on changing a record in a permanently
locked slot. Keeping the stable board/secure-element binding there, and the RTC binding in the
replaceable commissioning record, avoids that conflict.

## 2. Problems this solves

The current model gives the EEPROM a board identity while giving the RTC the
network identity. That forces an otherwise useful GNSS-only board to carry a
part whose identity is unrelated to its ability to authenticate or contribute.
It also makes replacing an RTC potentially change the station's network name.

Three desired configurations should have clear outcomes:

| Configuration | Identity and evidence | Contribution |
|---|---|---|
| Full observer with EEPROM, ATECC, supported MCU, RTC and auxiliary sensors | Board-based observer ID; optional RTC binding; commissioned profile | Supported GNSS observations and available auxiliary measurements |
| Basic observer with EEPROM, ATECC and supported MCU, without RTC or auxiliary sensors | Same identity model and commissioning profiles; no RTC binding | Supported GNSS observations; auxiliary checks unavailable |
| Ordinary USB/serial GPS attached to a computer | Software station ID and operational credential; no manufacturer hardware evidence required | Supported receiver output, subject to feed grants and sharing policy |

A basic observer can be `trusted` if its MCU and firmware satisfy the trusted
commissioning requirements. A full observer can be `open` or `none`. Sensor
count does not determine provenance.

No configuration acquires permission to publish simply by having a stronger
hardware label. No configuration proves that the incoming GNSS signal is true
simply by signing it.

## 3. Current implementation and scope

### 3.1 Pre-migration dependencies and implemented result

| Area | Pre-migration dependency | Implemented result |
|---|---|---|
| [`internal/attestation`](../../go/internal/attestation/attestation.go) | Two RTC-bound prototype variants | One exact permanent core-v1 statement without RTC |
| [`internal/commissioning`](../../go/internal/commissioning/commissioning.go) | 149-byte statement; RTC mandatory; RTC-derived observer | 153-byte statement, 225-byte record, explicit RTC flags/model, board-derived observer |
| [`registry.go`](../../go/internal/commissioning/registry.go) | Mandatory RTC fields and RTC-derived observer uniqueness | Required nullable RTC field, exact descriptive columns, authority scope, board/ATECC/bound-RTC uniqueness |
| [`mcu_identity_core`](../../esp32/components/mcu_identity/include/mcu_identity_core.h) | Prototype C layout and all-identities matching | Shared v1 bytes and exact mandatory/declared live-state matching |
| [`main.c`](../../esp32/main/main.c) | Station consistency derived from RTC | Station identity derives from board EEPROM |
| [`commission.c`](../../esp32/main/commission.c) | Empty strings for unread identities | Explicit null/read-state report with RTC expectation and presence |
| [`commission_report.py`](../../esp32/tools/commission_report.py) | Loose prototype report schema | Exact v1 schema and cross-field validation |
| [`mfgattest`](../../go/cmd/mfgattest/main.go) | Production-looking PEM signer and prototype inputs | V1 verifiers plus explicitly gated development-fixture signing only |
| Enrollment contract | Certificate SAN equal to RTC-derived name | Certificate SAN equal to board-derived name |
| [`identity/context.go`](../../go/internal/identity/context.go) | Prototype partial/complete tiers | Explicit `verified_v1_core` tier |

The firmware build is currently restricted to the supported ESP32-S3 observer
board, including its flash, PSRAM and partition assumptions. Changing the
identity format alone does not make a different serial-GPS PCB a supported
firmware target. A board port must address pins, components, storage and resource
limits separately.

### 3.2 What this proposal includes

- A concrete identity contract and signed binary layouts.
- Optional RTC identity and its manufacturing/service rules.
- Effects on enrollment, firmware, registry, collector, persistence and UI.
- A GNSS-only capability model and the ordinary software-feeder path.
- Replacement of the prototype formats before first enrollment and acceptance
  criteria for the v1 standards.

### 3.3 What it does not implement or promise

- A new sensor-fusion algorithm or new evidence of RF authenticity.
- A generic NMEA ingestion/analysis feature.
- Completion of ATECC key generation, hardware mTLS enrollment or per-batch
  `SIGNED_DATA`; those paths must retain their actual implementation status.
- Support for arbitrary MCUs, USB bridges or boards without a firmware port.
- An arbitrary-length component certificate system or a new general-purpose
  device identity service.
- Schematics, component selection, eFuse programming or device flashing as part
  of writing or reviewing this document.

## 4. Identity model

### 4.1 Canonical board and observer identity

For a hardware-enrolled observer:

```text
board_eui64 = factory EUI-64 read from the board EEPROM
observer_id = lowercase hex byte pairs of board_eui64, joined by '-'
```

For example, the synthetic board identifier `0004a3aabbccddee` renders as
`00-04-a3-aa-bb-cc-dd-ee`.

Use the factory identifier, not a writable manifest field that claims an EUI-64.
The manifest's self-reference must agree with the factory identifier before the
manifest is accepted. A blank, erased, unreadable or replaced EEPROM must not
cause firmware to invent a hardware identity.

For this contract, replacement of the board's identity EEPROM creates a new
hardware observer identity. Continuity with an earlier installation is a
separate administrative relationship; it does not justify assigning the old
hardware identifier to the replacement.

This is an inventory and authentication convention. A signed identifier does
not prove the physical PCB cannot be modified, that parts cannot be transplanted,
or that an attacker cannot emulate an identifier on an accessible bus.

### 4.2 ATECC identity and operational key

The ATECC serial is mandatory in the permanent hardware attestation. An
operational public key is enrolled separately, with proof of possession and the
hardware/configuration checks required by the hardware enrollment procedure.

Do not derive the observer name from the operational key. Regenerating a key
must not rename the observer. Do not treat a reported ATECC serial or a copied
slot-14 record as proof that the current TLS client holds a non-extractable key.

The core attestation intentionally omits the regenerable operational public
key. Therefore it cannot, by itself, bind an arbitrary enrollment CSR to that
secure element. The enrollment authority must validate that relationship under
its controlled enrollment procedure before issuing a `hardware_mtls` credential.
This proposal does not replace that procedure with a self-reported key field.

### 4.3 RTC presence, model and optional instance identity

An RTC may be:

- absent by design;
- present and equipped with a factory identity included in commissioning;
- present without an identity that this protocol binds;
- expected to provide an identity but unreadable, invalid or mismatched.

The first three are possible hardware configurations. The fourth is a fault,
not a request to change the configuration.

Two signed flags distinguish RTC presence from instance binding. An RTC-present
flag and model code identify the commissioned RTC configuration. A separate
EUI-bound flag means **a particular RTC EUI-64 is bound by this commissioning
record**. Neither flag means RTC time is valid or its battery works.

Models that lack a factory EUI-64, including any selected Maxim/DS-type part with
that limitation, can use the RTC-present flag and a model code without an EUI
binding. A board is not penalized in its MCU trust classification for that model
choice. The hardware port must verify the capabilities of the exact selected
part; this proposal does not assume that every model in a vendor's family has
the same identification features.

### 4.4 A known identifier in code identifies a model, not an individual RTC

A compile-time constant is appropriate for identifying an RTC model. Use a
small, centrally defined `rtc_model_id`, with symbolic constants and a stable
lookup table shared by tooling and firmware. For example, a future model table
can define symbolic entries for MCP79412 and supported DS-family models; their
numeric assignments must be frozen before implementation and never recycled.

Do not populate `rtc_eui64` with a made-up or copied EUI shared by every chip of
one model. That value would identify the model, not the physical instance, and
would defeat uniqueness checks or imply a stronger hardware binding than exists.
In this proposal the equivalent of a "known EUI-64 in code" is the model code in
its own field. It is deliberately not called a factory EUI-64.

If an existing component catalog already uses a shared 64-bit type identifier,
the board integration may map that type identifier to `rtc_model_id`. Keep the
catalog's namespace and semantics explicit; never feed the shared type tag into
the unique-instance field. Prefer reusing a documented catalog mapping over
creating competing model lists.

Model codes are not unique per board and must not have a uniqueness constraint.
Thousands of boards can declare the same model. Each still has its own board
EUI-64, ATECC binding and MCU commissioning evidence.

A model declared in signed commissioning is a manufacturer statement about the
assembly. Firmware checks the expected driver/device behavior where possible,
but a compatible I2C response alone may not distinguish every model. Do not
claim silicon-level model authentication or instance binding from a constant,
I2C address or successful calendar read.

A generic parser also cannot determine whether a syntactically valid EUI was
actually read from a factory-assigned identifier. Model capability checks,
controlled live reads and enrollment uniqueness checks provide that assurance
within their stated limits. Do not describe byte-shape validation as proof of
factory origin.

### 4.5 MCU identity

Keep the current distinction between the MCU's factory MAC label and its
cryptographic identity. For `trusted`, the commissioning record binds the MCU
public-key digest, and the MCU proves possession on the current TLS session.

No-RTC support changes neither the security bit requirements nor the proof
algorithm. In particular, an ATECC attached to a freely programmable host does
not establish the current `trusted` firmware claim.

### 4.6 Software station identity

A software feeder keeps an authority-assigned station ID. It does not need a
synthetic EEPROM ID, an invented ATECC serial or an empty hardware certificate.
Bearer-token admission and any software mTLS requirements remain unchanged.

Hardware enrollment reserves/checks its canonical ID against the authority's
station namespace. Never merge an existing software station with a hardware
station merely because their names happen to match. Ownership and credentials
must be checked explicitly.

### 4.7 Authority scoping

Do not collapse operational admission and hardware provenance into one generic
CA field. An enrollment records two server-owned identifiers:

| Field | Meaning |
|---|---|
| `operational_authority_id` | The authority whose project-specific Issuing intermediate may issue this station's operational certificate |
| `manufacturer_authority_id` | The authority whose slot-5 keys may sign this hardware's core attestation and commissioning record; null for software-only stations |

These values are selected from authenticated enrollment policy and configured
key registrations, never from a device claim, certificate extension,
commissioning field or self-claimed registry field. A signed registry repeats
the expected manufacturer authority for consistency but cannot select it. Each
Issuing-intermediate public key maps to exactly one operational authority. Each
manufacturer public key maps to exactly one manufacturer authority. A CA root
certificate may validate more than one project-specific intermediate and
therefore does not, by itself, identify an operational authority.

The two authority IDs may differ when policy explicitly permits it. For example,
a customer can use its own operational CA for hardware manufactured under the
NavListen manufacturer keys. A customer that manufactures and commissions its
own supported hardware can use its own authority for both roles. Core and
commissioning signers for one hardware record must always map to the same
manufacturer authority, even though the two signatures may use different keys
from that authority's approved root pair.

Interpret a product as `(manufacturer_authority_id, product_u16)`. Different
manufacturer authorities may allocate the same numeric product code without
colliding. Canonical observer IDs remain globally unique across authorities;
authority scoping must not create two live stations with the same board-derived
observer ID.

## 5. Trust and capability semantics

### 5.1 Independent dimensions

| Dimension | Authority/evidence | Examples |
|---|---|---|
| Operational identity and admission | Collector configuration or control plane; token/certificate validation | Station, owner, feed grants, enabled credential |
| Authority provenance | Immutable enrollment snapshot plus configured key-to-authority mappings | Operational authority, manufacturer authority, matching issuer and signer key IDs |
| Manufacturer core attestation | Verified manufacturer signature over board/ATECC binding | Proposed `verified_v1_core` |
| Session hardware trust | Collector-verified commissioning evidence and applicable session proof | `none`, `open`, `test`, `trusted` |
| Declared measurement support | Server-owned enrollment/product information | Supported GNSS signals; separately modeled sensor capabilities |
| Observed health/data availability | Measurements and diagnostics with freshness/validity | Receiver producing frames; pressure sensor failed; RTC time invalid |
| Sharing permissions | Effective owner/organization policy | Private, aggregate use, metadata visibility, raw export |

The existing `DeclaredCapabilities` field is a list of GNSS/signal pairs. Do not
silently put sensor strings into it. If the basic-board feature needs a sensor
capability field, give it an explicit schema and update its producers and
consumers. The binary identity changes do not require that field to be encoded
inside slot 14.

### 5.2 Meaning of hardware trust

- `trusted`: the current commissioned MCU proves possession on this session;
  manufacturer signature, security configuration and configured registry policy
  all validate. This can describe either a basic or a full observer.
- `open`: a valid open commissioning record names this observer. Unlocked
  firmware can be replaced, and a record plus operational credential can be
  copied. This is not the equivalent of the trusted MCU proof.
- `test`: the existing bench/development profile, with its existing key and
  security consistency rules.
- `none`: no accepted commissioning evidence. A correctly authenticated software
  feeder is a normal example and may still contribute useful data.

The update track remains a separate device/build label. The collector never
assigns trust from the claimed track, PCB name, sensor population or UI badge.

Hardware evidence continues to label data rather than grant admission or select
publication policy. Bad credentials still deny admission. A well-framed evidence
message that fails verification leaves an otherwise admitted session at `none`.
An announced evidence frame that never arrives or violates framing remains a
protocol failure under the existing handshake rules.

Report `hardware_trust` with its `manufacturer_authority_id`; `trusted` means the
session satisfies that configured authority's policy. It does not, by itself,
mean NavListen manufactured the board. Preserve the operational authority and
manufacturer authority in receipt-time snapshots without exposing either where
audience policy hides provenance metadata.

### 5.3 Basic measurement behavior

A GNSS-only observer reports the GNSS information its receiver actually exports.
It may support raw navigation words, receiver RF telemetry, signal observations
or other implemented feed data. These capabilities must be checked per receiver
and protocol, not inferred from the USB/serial connector.

Checks that require absent auxiliary inputs remain unavailable. An unavailable
check is neither a passed check nor a failed check. Detectors must not reduce an
independence/quorum requirement merely to produce a result on a smaller board.
They may still perform the checks supported by the available measurements and
compare eligible data across stations.

Current board telemetry does not itself make environmental readings part of
integrity decisions. This proposal must not be presented as enabling a new
sensor-fusion detector. See [observer telemetry](../OBSERVER-TELEMETRY.md) and
[integrity implementation limits](../INTEGRITY.md).

## 6. Permanent manufacturer attestation: v1

### 6.1 Signed core identity

Use this 72-byte slot-14 record representation:

```text
[version U8 = 1][reserved zero bytes 7][ECDSA P-256 R 32][ECDSA P-256 S 32]
```

The manufacturer signs this digest:

```text
SHA-256(
  ASCII("ATECC-MFG-CORE-v1") ||
  product_u16be ||
  board_revision_u16be ||
  board_eui64[8] ||
  atecc_serial[9]
)
```

The domain is 17 bytes and the binary identity suffix is exactly 21 bytes, so
the complete prehash input is 38 bytes. There are no implicit string
terminators, separators, padding bytes or native-structure encodings. EUI and
serial fields are their raw bytes in the established display order, not ASCII
hex. The domain string selects this exact field order and interpretation.

Adding `product` binds the permanent record to the product namespace as well as
the revision. The same core signature cannot be presented as attestation for a
different product by changing only its enrollment metadata.

The signed identity inputs are supplied from the manufacturing record and
verified live reads at enrollment; they do not all reside in the 72-byte slot.
Keep an exact manufacturing record of these inputs and the resulting slot bytes.

### 6.2 Validation

- Require version 1 and seven zero reserved bytes.
- Require a recognized/allowed nonzero product at the relevant admission point.
- Treat board revision as an unsigned 16-bit product-scoped value. Revision zero
  is not globally invalid unless the product definition reserves it.
- Reject an all-zero or all-`0xff` board EUI-64 or ATECC serial.
- Validate the existing P-256 public-key and signature scalar constraints.
- Select the expected `manufacturer_authority_id` from authenticated enrollment
  policy or the enrolled device snapshot. Verify the signature only against that
  authority's configured slot-5 manufacturer-key set, and retain the matching
  signer identity. Do not discover authority by trying every manufacturer key
  configured on the collector.
- Verify live identifiers against the signed manufacturing identity during
  hardware enrollment; a JSON claim alone is insufficient.
- Require the enrolled product and revision to agree with commissioning.
- Do not include the RTC, MCU MAC, operational public key, owner, organization,
  hostname, capabilities or sharing policy in this permanent statement.

Return an explicit attestation result, proposed spelling `verified_v1_core`.
Avoid describing it as a verification of every fitted component. RTC binding
has separate evidence in commissioning.

### 6.3 Signing authorities, root pairs and customer CAs

Manufacturer records and operational X.509 credentials use separate keys and
verification paths:

- The NavListen manufacturer authority uses Root A and Root B. Each holds an
  independent slot-5 manufacturer signing key. Either may sign a v1 core
  attestation or commissioning record.
- A customer-owned CA may use another independent pair, illustrated here as
  Root C and Root D. Register its slot-5 public keys under a distinct
  `manufacturer_authority_id` only if that customer will manufacture and
  commission supported hardware. Do not append C/D to an unscoped A/B key list.
- A 72-byte core attestation has no signer-key-ID field, so its verifier tries
  the keys within the expected manufacturer authority and records which one
  validated. A commissioning record carries the first eight bytes of its
  signer's SubjectPublicKeyInfo SHA-256 as a lookup hint; the signature under a
  key in that same authority remains the proof.
- The core attestation and commissioning record need not be signed by the same
  member of a root pair, but both signers must map to the same
  `manufacturer_authority_id`. The commissioning statement binds the exact core
  record through its SHA-256 fingerprint. Persist the authority and both signer
  identities for audit.
- Operational observer certificates follow the selected root pair to a
  project-specific Issuing intermediate to observer-leaf path. The registration
  authority validates the intermediate under that operational authority's
  approved roots. The collector pins the exact Issuing-intermediate public key,
  including approved rotation keys, so a leaf from another project under the
  same roots cannot authenticate as this authority's observer.
- The operational and manufacturer authorities may differ. A customer can use
  a C/D-backed Issuing intermediate for operational certificates on a board
  whose core and commissioning records remain signed by A/B. If the customer
  also manufactures the board under C/D slot-5 keys, both authority IDs can
  refer to that customer's separately configured roles.
- A Root's slot-0 CA key and slot-5 manufacturer key are distinct. Never accept
  the CA certificate key as a manufacturer pin, a manufacturer signature as an
  operational certificate, or an operational certificate as hardware evidence.

The letters name physical units in an example; they are not protocol values and
must not be hardcoded as a four-root global trust set. Adding a customer CA is an
administrative authority-registration operation. A customer running its own
collector can configure only its chosen authorities. A hosted collector must
enable the customer authority and its permitted manufacturer-authority pairings
explicitly before accepting any customer-issued credential.

The factory host or Factory-role relay holds no manufacturer private key. It
submits the exact typed messages to a Root, which derives each digest and signs
with slot 5 after local approval. The controlled manufacturing workflow must
verify live inputs, target state and record ordering before submission; Root
signing alone does not claim that the Root measured the target board.

### 6.4 Why RTC identity belongs outside slot 14

The permanent record should remain valid across an RTC replacement, MCU
replacement or operational key rotation, provided the bound core identity has
not changed. Those operations have separate authorization and commissioning
requirements.

If slot 14 instead included an optional RTC, replacing that RTC would invalidate
the permanent signature. A permanently locked slot cannot be rewritten merely
because the protocol now permits a new value. Supporting that design would need
a replacement secure element or an additional override certificate system.

This proposal chooses the simpler service model: permanent core attestation plus
replaceable, signed commissioning metadata. It does not weaken the checks for a
record that explicitly binds an RTC.

## 7. Commissioning statement: v1

### 7.1 Concrete binary layout

Use a fixed **153-byte** statement. All multibyte integers are big-endian. Encode
and decode fields explicitly; do not cast a native C struct onto wire bytes.

| Offset | Bytes | Field | Proposed interpretation |
|---:|---:|---|---|
| 0 | 1 | version | `0x01` |
| 1 | 1 | profile | Existing values: trusted=1, open=2, test=3 |
| 2 | 1 | mcu_family | Existing supported value: ESP32-S3=1 |
| 3 | 1 | mcu_key_alg | Existing values: none=0, RSA-3072-PSS/SHA-256=1 |
| 4 | 2 | product | Nonzero manufacturer-assigned product |
| 6 | 2 | board_revision | Must agree with core attestation |
| 8 | 2 | security | Existing five security bits |
| 10 | 2 | identity_flags | Bit 0 = RTC EUI-64 bound; bit 1 = RTC declared present; other bits zero |
| 12 | 2 | rtc_model_id | Registered nonzero model when RTC present; zero when absent |
| 14 | 4 | generation | Starts at 1; increases for each replacement commissioning record |
| 18 | 8 | commissioned_at | Nonzero Unix seconds |
| 26 | 8 | board_eui64 | Mandatory; canonical observer identity |
| 34 | 9 | atecc_serial | Mandatory core binding |
| 43 | 8 | rtc_eui64 | Valid EUI-64 when bit 0 set; all zero otherwise |
| 51 | 6 | mcu_mac | Existing MCU label; mandatory for supported MCU profile |
| 57 | 32 | mcu_key_sha256 | SHA-256 of public-key DER SubjectPublicKeyInfo, or zero |
| 89 | 32 | secure_boot_keys | Existing digest of the three Secure Boot key-digest slots, or zero |
| 121 | 32 | attestation | SHA-256 of the exact 72-byte core slot record, or zero only where test permits |

The signed digest is:

```text
SHA-256(ASCII("MFG-COMMISSION-v1") || statement[153])
```

The domain is 17 bytes, making the complete prehash input 170 bytes.

The complete record is **225 bytes**:

```text
statement[153] || signer_key_id[8] || R[32] || S[32]
```

Keep the signer-key-ID construction described in section 6.3 and the dedicated
manufacturer-key role. The record fingerprint is SHA-256 of all 225 bytes.

### 7.2 Presence and consistency rules

The parser, signer, firmware matcher and collector verifier must agree:

1. Unknown version, wrong size, unknown flags and unknown security bits fail.
2. Board EUI-64 and ATECC serial must be nonblank/non-erased regardless of RTC
   flags. MCU MAC validation remains as in the supported MCU contract.
3. With RTC EUI-bound bit clear, all eight RTC bytes must be zero. Nonzero bytes
   are not an ignored comment; they are an invalid encoding.
4. With RTC EUI-bound bit set, the RTC bytes must be a valid nonblank/non-erased
   identifier, the RTC-present bit must be set, and the model definition must
   support that identifier kind.
5. RTC presence never changes `ObserverID()`, which always renders board EUI-64.
6. Preserve the key-digest/key-algorithm and Secure-Boot-digest/security-bit
   consistency rules.
7. Preserve `trusted` requirements: MCU key algorithm 1 and security `0x001f`.
8. Preserve `open` requirements: MCU key algorithm 0 and security zero.
9. Preserve the existing test-profile flexibility. Only test may omit the
   permanent attestation fingerprint; test still requires the core identifiers
   in this format. Software contributors do not use a fabricated test record.
10. Before submitting a non-test statement for signing, the controlled
    manufacturing workflow must verify the v1 core attestation, confirm
    product/revision/core identifiers, and hash its exact record bytes. The Root
    signing appliance validates the statement encoding but cannot independently
    read the target board. A nonzero caller-supplied digest is not enough to
    authorize submission.
11. With RTC-present clear, model ID must be zero and EUI-bound must be clear.
    With RTC-present set, model ID must be nonzero and recognized by the supported
    product/model policy. Unknown codes are not guessed from an I2C address.

The complete legal RTC combinations are:

| Flags | Model | EUI bytes | Meaning |
|---|---|---|---|
| `0x0000` | 0 | Zero | No RTC declared |
| `0x0002` | Known nonzero code | Zero | RTC model declared, individual RTC not bound |
| `0x0003` | Known nonzero code supporting factory EUI | Valid EUI-64 | RTC model and individual RTC bound |

`0x0001` is invalid because it binds an RTC instance while declaring no RTC.

The collector's commissioning verification relies on the manufacturer's signed
statement about the core attestation; the current evidence exchange does not
transmit every manufacturing input and slot record for separate reconstruction.
Do not claim that the collector independently rereads the EEPROM or ATECC.

### 7.3 Firmware live matching

Before offering commissioning evidence on each new session, firmware must match
the installed record to fresh live core identities and the MCU's relevant key
and security state. The RTC match follows the signed declaration:

| Signed RTC declaration | Current observation | Result |
|---|---|---|
| No RTC | No RTC expected by approved configuration | Core matching can succeed |
| Model only | Expected RTC driver/device checks succeed | Model consistency succeeds; no instance proof claimed |
| Model only | RTC is expected but absent, incompatible or unreadable during pre-session check | Report configuration/component fault; suppress commissioning evidence until resolved |
| Model and EUI | Expected model checks and same EUI-64 read succeed | Model consistency and RTC identity match succeed |
| Model and EUI | Read fails, identifier invalid, part missing or EUI differs | Suppress hardware evidence and report a specific fault |

Also read the actual slot-14 bytes and require their SHA-256 to equal the
commissioning attestation field for non-test records. Matching only the board
and ATECC serials is insufficient to confirm that the installed core record is
the one commissioned. Preserve explicit test-only handling for a zero
attestation fingerprint.

For a no-RTC declaration, an unexpected discovered RTC is diagnostic information,
not permission to rewrite the signed declaration or silently use that RTC as an
attested timing source. Commission the changed assembly before enabling its
declared RTC role. Do not infer every unexpected I2C response is an RTC.

If product policy requires an identifiable RTC to be bound, commissioning must
refuse a record without EUI binding for that configuration. That decision must
use the approved product/assembly definition and bench validation, not an I2C timeout.

A telemetry sample reporting an invalid RTC calendar is not an identity
mismatch. It invalidates time-related measurements as appropriate; it does not
automatically invalidate a matching factory identifier.

Trust is currently assigned per session. This proposal requires fresh matching
before every session and does not claim continuous remote component attestation.
An implementation that detects a confirmed identity mismatch during a live
session should close that session and reconnect without the evidence; it must
not silently carry the previous trust into a new connection. Ordinary transient
sensor measurement failures continue to use measurement health/validity.

Evidence suppression must not disable otherwise permitted GNSS forwarding.
Whether a particular board can start and authenticate without its RTC also
depends on the time-bootstrap checks in section 11.

### 7.4 Extended identifiers versus a general extension format

This revision provides an RTC model descriptor and one explicitly typed optional
instance identity: RTC EUI-64. It does not introduce an open-ended TLV parser
into the signed record. Adding a supported RTC model is a model-table/driver
update, not a new binary format, provided it uses the existing descriptor and
optional EUI semantics.

Unknown identity flags are rejected. A later extension requires an explicitly
defined format revision. This costs another version if additional identity
types become necessary, but keeps the initial firmware parser bounded and makes
the signed meaning easy to test across Go and C.

## 8. Session proof and evidence transport

Use this proof digest construction:

```text
SHA-256(
  ASCII("NAVL-MCU-PROOF-v1") ||
  tls_exported_keying_material[32] ||
  SHA-256(commissioning_record[225])
)
```

The digest commits to the exact v1 record, including its identity flags and
board identity. Keep RSA-3072, exponent 65537, PSS with
SHA-256/MGF1-SHA-256 and 32-byte salt. Keep the current exporter label
`EXPERIMENTAL-navlistener-mcu-proof-v1` and exporter context behavior.

Use evidence-envelope version 1:

```text
[version U8 = 1]
[record_length U16BE][record]
[key_length U16BE][MCU public key DER]
[proof_length U16BE][proof]
```

Its length-delimited record is 225 bytes. The envelope maximum is 2048 bytes;
the individual key/proof bounds are 1024/512 bytes. Even
at those bounds, the new encoded maximum is `7 + 225 + 1024 + 512 = 1768` bytes.

The commissioning parser requires both version 1 and the exact 225-byte record
length. The proof domain, TLS exporter and GNF1 frame type use their v1
definitions because their length-delimited construction and meaning already fit
this record. No parser tries another commissioning layout after v1 validation
fails.

Preserve authentication-before-evidence, the handshake deadline, bounded reads,
key/proof pairing, exact DER/key checks, trailing-byte rejection and separation
of evidence verification from operational authorization.

## 9. Registry, enrollment and persisted identity

### 9.1 Registry v1

Use envelope format `navlistener-registry-v1` and signature domain
`NAVL-REGISTRY-v1`. Keep signing the exact payload bytes and preserving the
current separate registry signing-key role.

Bind each registry stream to one manufacturer authority. Add a required
top-level `manufacturer_authority_id` and require it to equal the authority that
the verifier was configured to expect. Select that authority and its registry
verification keys before parsing the signed payload; never use the payload's
self-claimed ID to select a key from a global set. Scope the persisted sequence
floor to the configured manufacturer authority and do not reset it during
registry-key rotation.

Keep each board record keyed by `board_eui64`. Add a required `rtc_model_id`
integer member, zero for no RTC and a registered nonzero value for a declared
RTC. Change the RTC EUI field to a required JSON member whose value is either
lowercase 16-character hex or `null`. Empty string and missing member are
invalid in this proposed registry schema. A null value means the commissioning
record does not bind an RTC identifier.

Each descriptive column must agree with the embedded commissioning v1 record.
The record is authoritative for flags and other signed fields. An RTC value in
the registry cannot add a binding to an EUI-bound-flag-clear record or remove one
from an EUI-bound-flag-set record.

Validate uniqueness of canonical board identities and ATECC serials across the
registry's current board entries. Validate RTC EUI uniqueness where a non-null
binding exists; null values are not duplicate identities. Model IDs repeat
freely and are never unique-instance keys. Retain historical component
associations in the service ledger, rather than presenting old and
new records as simultaneous current identities for the same board.

Keep sequence rollback protection, persisted sequence floors, signature checks,
record supersession and revocation. The registry can withdraw a manufacturer
claim, not create one. Registry verification and session manufacturer-signature
verification remain distinct checks. A registry signed for one manufacturer
authority cannot list, revoke or supersede another authority's records.

Current code permits configurations without a registry and configurations that
do not require a listed entry. Preserve that distinction in documentation:
without an authoritative registry, a verifier cannot promise that a record is
the current service generation. Deployments relying on supersession/revocation
must configure the signed registry, require entries and persist its sequence
floor. Making those settings mandatory for every deployment is a separate
policy decision, not an implicit format change.

### 9.2 Authority registration and customer CA pairs

Represent trust as configured authority records rather than source-code lists of
all accepted CAs. At minimum, maintain these two record types:

| Record | Required configured data |
|---|---|
| Operational authority | Stable internal ID; enabled state; approved root certificates for Issuing-intermediate installation; exact Issuing-intermediate SPKI pins, including approved rotation pins; allowed manufacturer-authority pairings |
| Manufacturer authority | Stable internal ID; enabled state; slot-5 manufacturer public keys; registry verification keys; allowed product/revision and RTC-binding policy |

The NavListen operational and manufacturer records refer to Root A/B in their
distinct slot-0 and slot-5 roles. Selling a customer its own CA creates separate
customer records backed by its Root C/D pair; it does not add C/D to an
undifferentiated A/B trust bundle. Register only public certificates, public
keys and fingerprints in the service. Customer private keys remain in the
customer's CA hardware and ceremony.

An Issuing-intermediate public key maps to exactly one operational authority,
even when that key has certificates from both members of its root pair. A slot-5
manufacturer key and a registry verification key each map to exactly one
manufacturer authority. Reject ambiguous duplicate registration. A root CA may
validate intermediates for several operational authorities, so a valid chain to
that root is not sufficient application authorization.

If a TLS implementation uses one combined pool of accepted client issuers, that
pool is only the cryptographic handshake input. After verification, map the
actual verified Issuing-intermediate SPKI to its operational authority and
require it to equal the station's enrolled `operational_authority_id`. Do not use
an issuer display name, a device-supplied authority string or the first chain a
peer sends. Unknown authorities, unknown issuer keys and disabled authorities
fail closed. There is no trust-on-first-use or device self-registration path.

An authenticated enrollment policy may allow a customer operational authority
to pair with the NavListen manufacturer authority, the customer's manufacturer
authority, or both. This lets customer C/D credentials operate A/B-manufactured
boards without treating C/D as a manufacturer. The permitted pairing is stored
with the enrollment and evaluated explicitly for every credential and evidence
check.

### 9.3 Enrollment

The proposed hardware enrollment transaction is:

1. Authenticate the enrollment operator and select an enabled
   `operational_authority_id` plus an allowed `manufacturer_authority_id`.
   Software-only stations use a null manufacturer authority.
2. Obtain the board EEPROM factory EUI-64, ATECC serial, product and revision
   through the controlled hardware enrollment process. Interpret product policy
   under the selected manufacturer authority.
3. Verify the v1 core attestation only with that manufacturer authority's key
   set, then enforce global identity uniqueness and ownership constraints.
4. Derive the observer ID from the board EUI-64.
5. For hardware mTLS, validate the operational public key and CSR proof under
   the approved ATECC enrollment procedure, then issue through an exact
   Issuing-intermediate key registered to the selected operational authority.
   Require the certificate's sole DNS SAN to equal the board-derived observer ID.
6. Validate any commissioning record and optional RTC binding independently.
   Its signer may differ from the core signer, but both keys must map to the
   selected manufacturer authority and the commissioning record must bind the
   exact core fingerprint. If registry policy applies, require a current entry
   from that same manufacturer authority.
7. Assign enrollment, owner, feed grants, declared capabilities and sharing
   policy using the existing authority. Do not equate an accepted core
   attestation with a trusted MCU session.
8. Persist both authority IDs, exact signer and issuer identities, evidence
   fingerprints and policy snapshots, and issue the operational credential
   atomically with the enrollment state.

Current ESP32 bearer authentication can still be used alongside MCU session
evidence. A verified MCU proof does not require pretending hardware mTLS is
already implemented. Likewise, hardware mTLS does not replace the MCU proof
needed for the `trusted` firmware claim.

The registration authority validates an installed or renewed Issuing
intermediate under the selected operational authority's root pair. The collector
anchors client-certificate authentication to the exact registered intermediate,
rather than accepting either root as a broad client trust anchor. The
intermediate may keep certificates for the same key from both roots; selecting
or renewing that chain does not change the observer ID, operational authority or
separate manufacturer authority.

### 9.4 Database, configuration and API consequences

- Add operational-authority, manufacturer-authority and allowed-pairing records.
  Enforce unique mappings for Issuing-intermediate SPKI pins, slot-5
  manufacturer keys and registry verification keys. Retain disabled records for
  historical interpretation and audit.
- Replace a flat collector-wide `manufacturer_keys` setting with manufacturer
  authorities and key sets selected by the enrollment's
  `manufacturer_authority_id`. Likewise, map verified client-certificate issuer
  pins to operational authorities instead of treating a shared TLS trust pool as
  authorization.
- Store both authority IDs on each enrollment. Store the exact issuer pin, core
  signer, commissioning signer and registry signer in immutable evidence
  snapshots. Authority display names may change; stored IDs and fingerprints do
  not.
- Key product/revision policy by `(manufacturer_authority_id, product)` and key
  registry sequence floors by manufacturer authority. The same numeric product
  code under two manufacturer authorities is two distinct products.
- Make the RTC identity nullable in device/enrollment/service schemas that
  currently require it, with explicit meaning for null. Add a separate model
  field and preserve the distinction between no RTC and RTC without a bound EUI.
- Index/reserve hardware observer IDs from board identity. Preserve the global
  station namespace checks used for software observers.
- Store permanent core attestation result and fingerprint separately from
  commissioning version, fingerprint, generation, RTC model and optional RTC
  identity.
- Add the v1 core-attestation tier to every validator and database constraint
  before issuing credentials that use it.
- Preserve receipt-time snapshots. Old rows must not change identity or trust
  merely because the current device record changes.
- Inventory affected SQL views, API serializers, UI enums, fixture data and
  enrollment tools. The external control-plane schema is a separate dependency;
  changing Go structs alone does not complete that work.
- Reuse existing authorization-view contracts where their shape remains valid;
  version a view if its schema actually changes. An additional accepted tier
  string still requires a coordinated validation update.

Because there are no enrolled hardware observers, update these schemas and
validators before the first enrollment. Do not create migration rows, RTC-name
aliases or dual identity lookups for prototype records. Local development
samples may be discarded or regenerated; they are not authoritative client
history.

## 10. Basic device mode and software contributions

### 10.1 Basic is a capability description

Use a basic/GNSS-only product or board configuration, not an additional trust
value. Trust still uses `none`, `open`, `test` and `trusted`.

A minimal attested observer needs the core EEPROM and ATECC identity plus a
controller and firmware path that can read them and implement the relevant
transport. The existing commissioning format continues to support ESP32-S3
only. A passive serial/USB bridge with those two parts added does not thereby
gain supported commissioning or session proof.

The firmware board definition should make optional drivers and component
expectations explicit. It must avoid repeated RTC/sensor initialization errors
for components absent by design and preserve error reporting for components
that are expected but fail.

Choose product numbers deliberately. The existing verifier only accepts product
1. If the basic board is a revision of that same defined product, document the
revision's capabilities and identity requirements. If it is a distinct product,
allocate an identifier and update explicit collector/signer product allowlists.
Apply each allowlist within its `manufacturer_authority_id`; the same numeric
code under another manufacturer authority does not inherit support. Do not
accept every nonzero product merely because its signature uses an accepted key.
Final product assignment requires the hardware definition and is not invented
by this proposal.

### 10.2 Ordinary USB/serial GPS contribution

The contributor registers a software station, receives an operational
credential, and runs a feeder on the attached host. The authority grants only
the supported feed types and applies the chosen sharing policy. Sessions carry
`hardware_trust=none` unless separate, valid commissioning evidence is provided.

The current `navfeeder` path uses supported UBX output, especially raw navigation
messages. A USB receiver providing that output can use the existing path. USB
is a transport and says nothing by itself about available measurements.

A receiver exposing only NMEA position/time sentences needs a separately scoped
ingestion feature. Those sentences cannot be converted into the missing raw
navigation words. SBF/RTCM capture support must also retain its documented
capture-only limitations until central decoding is implemented.

Contributors with unverified hardware can still supply useful observations.
Any future detector weighting policy must state what evidence it uses and must
not treat a stronger provenance label as proof of correct RF reception. This
identity proposal does not change detector weights or federation policy.

### 10.3 Telemetry and presentation

Present independent fields such as:

```text
Observer: 00-04-a3-aa-bb-cc-dd-ee
Hardware trust: trusted
Measurements: GNSS only
RTC: not fitted
RTC instance binding: none
```

An RTC-equipped variant can instead show "RTC: <model name>" and "RTC instance
binding: none" when the model has no factory EUI. A code-defined model constant
must never cause the UI to display "unique RTC verified".

For a software contributor, use its assigned station name and a clear phrase
such as "Hardware provenance: unverified". Do not describe a permitted software
contributor as an authentication failure.

Existing ObserverDetails TLVs can omit unsupported components. Missing tags
mean unreported/unsupported, not passed tests. Do not emit synthetic zero
measurements as valid data, and do not create absent-sensor fault alarms from a
full-board default configuration applied to a basic board.

Distinguish "not fitted", "unsupported", "not reported", "failed" and "stale"
where the source data supports those distinctions. Do not infer "not fitted"
solely because a telemetry packet omitted a tag.

## 11. Time bootstrap without an RTC

Removing RTC identity as a requirement is not a complete demonstration that
firmware operates without RTC time. The board port must audit its actual startup
and recovery dependencies:

- obtaining time sufficiently trustworthy for the deployment's TLS certificate
  validation and signed update metadata expiry rules;
- behavior with no GNSS fix, no usable network time and an unavailable collector;
- the distinction between monotonic durations, wall-clock timestamps, GNSS
  receiver time and collector receipt time;
- cold boot and power loss with no retained RTC;
- whether any service assumes an RTC interrupt or pulse input;
- release/update checks that must wait for valid time.

Current observer telemetry documents that RTC time is not used for GNF1
timestamps. That is useful context, but it does not prove every boot dependency
is RTC-independent. Trace the actual time initialization code during the board
port and record the result.

No-RTC support must not disable TLS verification, bypass update expiry checks or
mark unknown time as valid. If time is unavailable, use the existing bounded
waiting/retry behavior and explicit validity states. Timestamp accuracy remains
a measurement issue separate from hardware identity.

## 12. Manufacturing and service lifecycle

### 12.1 New board

1. Read and validate the board EEPROM factory identity and ATECC identity.
2. Select the manufacturer authority and establish its approved product/revision
   and component expectations.
3. Generate and verify the v1 permanent core attestation under the existing
   controlled secure-element provisioning procedure, using one slot-5 key from
   that authority's approved root pair.
4. Configure the selected open/test/trusted MCU profile using the applicable
   commissioning procedure. Open remains unlocked; this format change never
   turns open provisioning into a key-generation or eFuse-writing operation.
5. Read any optional RTC identity required by the assembly definition.
6. Sign and install commissioning v1 with correct flags, core-record fingerprint
   and MCU evidence. Verify the complete record and the live identity match.
7. Publish the current record in that manufacturer authority's registry v1.
   Select an allowed operational authority, then enroll/configure the board-based
   station identity and issue through its exact registered Issuing intermediate.
8. Check the collector's reported result, including a real per-session proof for
   trusted hardware. A successful offline signature check is not that proof.

The exact irreversible provisioning sequence remains governed by the hardware
and commissioning implementation. This proposal supplies no new eFuse values,
ATECC configuration bytes or authority to run such operations.

### 12.2 Service behavior

| Event | Observer ID | Required identity action |
|---|---|---|
| Ordinary firmware update within authorized profile | Unchanged | Existing update verification; no core re-attestation |
| Operational key rotation | Unchanged | Controlled enrollment/key validation; revoke superseded credential |
| Operational authority change | Unchanged | Explicitly authorize the new authority/manufacturer pairing; issue through the new registered intermediate; revoke the old credential; retain both authority snapshots |
| RTC measurement fault, identity still matches | Unchanged | Mark affected measurements invalid; repair as needed |
| Replacement of bound RTC | Unchanged | Validate replacement; new commissioning generation and registry record; permanent core record unchanged |
| Replacement by same RTC model without instance binding | Unchanged | Log service and verify operation; signed fields may remain valid because no individual part was bound |
| Change of RTC model, including to a model without factory EUI | Unchanged | Validate driver/product support; new signed model/flags and commissioning generation; core record unchanged |
| Add/remove an RTC binding | Unchanged | Approved assembly/service change; new signed flags/record and generation |
| MCU replacement | Unchanged | Recommission supported MCU, bind its key/security state and supersede old record |
| ATECC replacement on same board identity | Unchanged if explicitly approved by service authority | New core attestation for replacement ATECC; new commissioning; revoke old operational credentials and supersede old record |
| Identity EEPROM replacement | New board-derived identity | Enroll new hardware identity; retire/revoke old one; record administrative continuity separately |
| PCB replacement | New identity by policy | New enrollment; do not transplant identities to conceal replacement |

Key rotation and ownership transfer remain controlled enrollment operations.
No service event self-authorizes a new organization, collection or publication
policy. A transfer should not change the permanent board identity or its
manufacturer authority. Changing the operational authority is a credential and
policy transition; changing the manufacturer authority requires a new valid core
attestation and commissioning chain under the replacement authority. A locked
core record cannot simply be relabeled and will generally require an approved
secure-element replacement. This is not an effect of ownership transfer.

### 12.3 Replacement ordering and interruption

For a service change, prepare the validated new record, stop the old trusted
session, install the new firmware/record as needed, update the registry and
credentials, and reconnect. The manufacturer/service workflow must document
its exact order and persist progress so interruption is recoverable.

Temporary loss of hardware trust is acceptable during an incomplete transition;
acceptance of both old and new MCU/core bindings as current is not the default.
Where a retired key or component is compromised, revoke its authority before
allowing service to continue. A retained old commissioning record must fail the
configured current-record check after supersession.

Rollback must not decrement registry sequence or reuse a commissioning
generation for a different record. A deliberate restoration is a new signed
service event with a higher generation. Generation overflow is a hard error,
never wraparound.

## 13. V1 standards and development rollout

### 13.1 Initial version set

| Layer | Standard version | Reason |
|---|---|---|
| Permanent slot attestation | v1 | First adopted core board/ATECC contract |
| Commissioning statement | v1 | First adopted MCU and optional-RTC contract |
| Signed registry | v1 | First adopted board-keyed registry contract |
| Bench report | v1 | First adopted report with explicit identifier read states |
| GNF1 evidence envelope | v1 | First adopted length-delimited evidence envelope |
| MCU proof construction/exporter | v1 | First adopted proof bound to the complete record fingerprint |
| ObserverDetails telemetry | v1 | Existing optional TLV shape already represents omitted components |

These labels version separate standards. They all begin at v1 because no earlier
format was deployed or used to enroll a client. A future incompatible change
increments only the affected standard. Every parser still requires the exact
version, size, reserved bits and field rules for its standard; versioning begins
with v1 rather than preserving development-prototype numbering.

### 13.2 Before first enrollment

1. Implement the single v1 contract across firmware, collector, signing tools,
   registry tooling and external enrollment dependencies.
2. Replace prototype records, schemas, constants and fixtures rather than adding
   converters or fallback parsers.
3. Generate synthetic cross-language v1 fixtures; do not use private keys or
   device records as public test fixtures.
4. Confirm that no enrolled client, issued hardware credential or authoritative
   registry entry depends on a prototype format.
5. Inspect any development hardware's slot-lock and MCU state before provisioning
   it under v1; irreversible prototype writes are a bench issue, not a reason to
   add protocol compatibility.
6. Provision test hardware under v1 and verify admission, evidence, telemetry and
   restart/reconnect behavior before the first client enrollment.
7. Remove prototype tooling and inputs, then update the normative documentation
   as one v1 rollout.

An already permanently locked slot cannot be rewritten by software. If a
development part contains a prototype RTC-bound attestation, use an appropriate
uncommitted part or replacement secure element, or resolve that physical
limitation explicitly. Do not add a second parser for it, and do not assume that
development devices have unlocked slots.

No full-flash backup is needed for development flashing. Preserve necessary
settings deliberately; if a partition migration is independently required, read
only its required configuration ranges. This identity proposal itself does not
require changing the partition map.

## 14. Implementation work breakdown

### A. Freeze the protocol contract

- Establish the attestation, commissioning, registry and bench-report contracts
  as v1 and require exact version validation in every parser.
- Accept the core/commissioning split for RTC identity.
- Confirm authority IDs, unique key-to-authority mappings, allowed
  operational/manufacturer pairings, and product/RTC-binding policy scoped to a
  manufacturer authority.
- Freeze the byte layouts, signature domains, flag values, model table and JSON null rules.
- Define negative vectors for unknown versions, malformed v1 records and
  cross-authority substitution.

Deliverable: accepted v1 format tables and common fixtures, with no fallback
interpretation.

### B. Go attestation and commissioning

- Update `go/internal/attestation` identity inputs, digest, version validation and
  returned attestation tier. Add verification within an explicitly selected
  manufacturer-authority key set that returns the matching signer identity.
- Update `go/internal/commissioning` sizes, field encoding, `ObserverID()`,
  validation, RTC model handling and optional instance-identity semantics.
- Update record fingerprint, evidence parser size assumptions and proof tests.
- Update registry schema, signature domain, descriptive-column checks and
  uniqueness rules, including `manufacturer_authority_id`, authority-scoped
  verification and absent RTC handling.
- Update `go/cmd/mfgattest` verification, help and output for the v1 core and
  optional RTC fields. Remove its offline PEM signing command from the production
  workflow, or retain it only as an explicitly development-only fixture tool;
  real manufacturer records go through the hardware Root slot-5 batch ceremony.
- Locate/update every tier allowlist in identity, authorization, config and
  ingest. Keep explicit rejection of unsupported hardware credential claims.

### C. Firmware and bench tooling

- Update the portable C codec, field constants, identity match rules and fixture
  dependencies under `esp32/components/mcu_identity`.
- Update device record storage length handling. A record with the wrong exact
  length must be rejected without reading past a buffer or interpreting a
  truncated record as v1 data.
- Update `esp32/main/board.[ch]`, `commission.c` and station-name checks in
  `main.c` to use board identity and distinct identifier read states.
- Update the report v1 producer and `esp32/tools/commission_report.py`. An
  identifier read failure must remain a diagnosable report that is ineligible
  for the relevant commissioning operation, not an invented valid identity.
- Separate product configuration from runtime bus discovery; lack of an ACK
  alone must never clear an expected identity binding.
- Preserve open-build restrictions against key generation/eFuse writes.
- Update the maintained factory-CA firmware and host batch tooling with the same
  v1 contract. In both the direct-attestation and mixed-batch paths, use the
  `attestation-v1` kind, the `ATECC-MFG-CORE-v1` domain and the 21-byte
  `product || revision || board || ATECC` message, and emit `0x01` as the slot
  record version. Change commissioning statements from 149 to 153 bytes, and
  update every field offset, validator, review display, fixed buffer and fixture.
  Keep the factory console and signing-batch containers at v1; their prototype
  contents were never enrolled.
- Preserve the authority roles in that firmware: Root A/B slot-5 keys sign
  manufacturer batches, a Factory unit may only relay them, and an Issuing
  intermediate signs operational observer certificates. Update mixed-batch,
  relay, counter, replay and two-root tests for the v1 messages.
- Keep authority registration in the host/control plane. The signed v1 core and
  commissioning layouts do not gain hardcoded A/B/C/D labels; verifier-selected
  public-key mappings provide the manufacturer-authority context.
- A customer Root C/D deployment uses the same Root, Factory and Issuing roles
  and message formats. Do not create customer-specific firmware branches; export
  and register its public material under the customer's scoped authority records.
- Continue importing the canonical secure-element slot/profile definitions from
  their maintained owner; do not create a divergent copy in this checkout.
- Add a basic board target only with its independently reviewed hardware and
  firmware resource assumptions.

### D. Control plane, storage and clients

- Update the external device/enrollment schema, authority-registration model,
  allowed-pairing policy and certificate issuance rules.
- Replace the flat manufacturer-key configuration with scoped manufacturer
  authorities. Configure NavListen A/B slot-5 pins as one authority and add a
  synthetic customer C/D authority in tests. Select a key set from the expected
  enrollment authority before verification.
- Register exact project/customer Issuing-intermediate SPKI pins as operational
  authorities. If the TLS layer uses a combined issuer pool, map the verified
  issuer to an authority after the handshake and compare it to the enrollment.
  A chain to an approved root alone does not authorize a station.
- Scope registry keys and persistent sequence floors to manufacturer authority,
  and scope product/revision policy to `(manufacturer_authority_id, product)`.
- Update immutable authority/signer/issuer snapshots, uniqueness constraints and
  operational-authority transition service events.
- Keep the operational authorization SQL contracts aligned with their consumers.
- Check store/API/client handling of new attestation tiers and absent RTC values.
- Ensure UI and export paths distinguish hardware identity, session trust and
  measurement availability.
- Do not expose permanent hardware identifiers in an audience whose metadata
  policy hides them. Board-based naming does not expand public visibility.

### E. Documentation and rollout

Update the normative contracts and relevant implementation guides together:

- [Commissioning](../COMMISSIONING.md).
- [Groups, enrollment and federation](../GROUPS-AND-FEDERATION.md).
- [Hardware observer identity](../HARDWARE-OBSERVER.md).
- [Design](../DESIGN.md).
- [Observer telemetry](../OBSERVER-TELEMETRY.md) and [output](../OUTPUT.md).
- [ESP32 commissioning](../../esp32/docs/COMMISSIONING.md).
- [Attestation package](../../go/internal/attestation/README.md) and
  [manufacturer tool](../../go/cmd/mfgattest/README.md).
- Applicable provisioning, update, README and example identity descriptions.

Remove contradictions such as "RTC is the observer name" and "EEPROM never
replaces the separate RTC identity" from the implemented contract when the v1
rollout is complete. Preserve accurate status disclosures about unfinished
hardware enrollment, on-device validation and decoder support.

## 15. Verification and acceptance criteria

This section describes tests for a future implementation. Writing this proposal
does not imply they have been run or that the formats are implemented.

### 15.1 Cross-language format vectors

Provide deterministic synthetic vectors for:

- Core attestation digest, record and verification with no RTC input.
- Commissioning trusted/open/test records with RTC unbound.
- Commissioning records with RTC bound.
- Commissioning records with a known RTC model and no factory EUI, and more than
  one supported model using the same binary layout.
- Statement bytes, signing digest, complete-record fingerprint, evidence bytes
  and proof digest for each relevant combination.
- Matching Go and C interpretation of every field and flag.
- Bench-report v1 and signed registry v1 with null and populated RTC fields.
- Core and commissioning records signed independently by the Root A and Root B
  slot-5 keys, including a commissioning record that binds a core record signed
  by the other root.
- Equivalent synthetic customer records signed independently by Root C and Root
  D slot-5 keys, registered under a distinct manufacturer authority.
- Operational observer chains through the project Issuing intermediate under
  each approved root certificate, without treating either root as a broad
  collector client trust anchor.
- A customer operational Issuing intermediate under C/D used with both an
  A/B-manufactured board and a C/D-manufactured board when each pairing is
  explicitly permitted.
- Separate A/B and C/D registry streams with authority-scoped signing keys and
  sequence floors.

Retain independent expected bytes/digests, not just encode/decode round trips.
Cryptographic randomized signatures need not be identical on every generation;
fixed fixtures must still verify and their exact record fingerprints must match.

### 15.2 Rejection and mismatch cases

| Case | Required result |
|---|---|
| Version byte other than 1 | Rejection; no alternate interpretation |
| Wrong record length, nonzero reserved bytes, unknown flags | Rejection |
| Missing/blank/erased board or ATECC identity | No valid hardware record |
| RTC EUI-bound flag clear with nonzero RTC bytes | Rejection |
| RTC EUI-bound flag set with zero/erased RTC bytes | Rejection |
| RTC EUI-bound flag set while RTC-present clear | Rejection |
| RTC-present clear with nonzero model, or present with zero/unknown model | Rejection |
| Shared code-defined model value passed as factory EUI | Manufacturing/enrollment procedure rejects the fabricated instance claim |
| Wrong board ID in HELLO or certificate SAN | Operational mismatch or evidence identity rejection as applicable |
| RTC-named HELLO for a board-named record | Evidence rejection; no alternate identity alias |
| Manufacturer signature checked against another product/revision/core identity | Verification failure |
| Root slot-0 CA key offered as a manufacturer key, or slot-5 key offered as an operational CA | Rejection |
| Observer leaf from another project intermediate under an approved root | Operational authentication rejection |
| Valid core and commissioning signatures from different approved Root A/B slot-5 keys | Allowed; exact core fingerprint and both signer identities retained |
| Valid core signed by A and commissioning signed by C | Rejection; signers map to different manufacturer authorities |
| Valid core signed by C and commissioning signed by D in the customer manufacturer authority | Allowed; exact core fingerprint and both signer identities retained |
| Valid customer-issued leaf presented for a station enrolled to the NavListen operational authority | Operational authentication rejection |
| Customer C/D operational authority paired with an A/B-manufactured board by explicit enrollment policy | Allowed; operational and manufacturer authority snapshots remain distinct |
| Valid C/D manufacturer evidence presented for an enrollment expecting A/B | Rejection without trying an unscoped global key set |
| Issuing-intermediate, manufacturer or registry key registered to two authority IDs | Configuration rejection as ambiguous |
| Unknown or device-claimed authority ID | Rejection; no trust-on-first-use |
| Registry payload for one manufacturer authority verified or applied as another | Rejection; no key selection from the self-claimed payload field |
| Same numeric product under two manufacturer authorities | Separate products; each authority's allowlist applies independently |
| Bound RTC missing/unreadable/mismatched at firmware check | No commissioning evidence offered; specific fault |
| RTC calendar invalid while bound EUI matches | Measurement invalidity; not an invented identity mismatch |
| Copied commissioning proof used on another TLS session | Verification failure |
| Valid ATECC claim without commissioned MCU proof | Cannot establish `trusted` |
| Changed RTC flags or values with retained signature | Verification failure |
| New optional RTC registry value contradicts embedded record | Registry rejection |
| Two current entries share board, ATECC or non-null RTC identity | Registry/enrollment rejection |
| Multiple entries have null RTC | Allowed, subject to other uniqueness rules |
| Multiple entries declare the same RTC model | Allowed; model constants identify a type, not an instance |
| Revoked/superseded record after configured registry recheck | Session closes; reconnect reevaluates evidence |
| Registry rollback below persistent floor | Rejection |
| New tier string omitted from a consumer's allowed set | Test exposes mismatch before deployment |

### 15.3 Participation and privacy cases

- A correctly commissioned basic board without RTC/auxiliary sensors can produce
  `trusted` evidence and contribute supported GNSS frames.
- An open basic board is accepted as `open` with the documented weaker meaning.
- An ordinary software feeder with valid credentials is accepted at `none`.
- Invalid operational credentials are rejected irrespective of hardware evidence.
- A customer operational authority can admit explicitly paired
  NavListen-manufactured hardware without acquiring A/B manufacturer-signing
  authority.
- A software station stores a null manufacturer authority and cannot manufacture
  a hardware-trust claim from its operational credential.
- Failed well-framed evidence does not discard otherwise admitted observations.
- Omitted auxiliary data does not fabricate valid measurements or passed checks.
- Capabilities do not lower detector independence requirements.
- Changes in hardware trust do not grant publication or reset unrelated audience
  policy; existing authorization/revocation behavior remains correct.
- A hardware identifier stays hidden wherever metadata policy requires it.
- Historical rows retain receipt-time identity and evidence after a service event.

### 15.4 Service and bench cases

- Replace an RTC binding: observer ID and permanent core record remain unchanged;
  commissioning generation/fingerprint change; old record is superseded.
- Change RTC model to a supported part without factory EUI: board identity and
  core record remain unchanged; signed model/flags change and instance binding
  is explicitly absent. Verify the new driver's time/pulse behavior separately.
- Fit the same model without instance identity to two boards: both enroll
  successfully with distinct board identities and no duplicate-RTC conflict.
- Rotate an operational key: observer ID remains unchanged and old credential is
  rejected after revocation.
- Move a station to another permitted operational authority: observer and
  manufacturer authority remain unchanged; the old credential is revoked and
  both receipt-time authority snapshots remain interpretable.
- Transfer ownership: operational policy may change, but the board is not
  relabeled under the buyer's manufacturer authority.
- Replace MCU: current registry and proof identify only the commissioned MCU.
- Replace ATECC: controlled core re-attestation and credential revocation are
  required; copying the old record is insufficient.
- Replace EEPROM: new identity is required; no automatic reassignment of old ID.
- Boot without RTC, without GNSS time, and with unavailable network/collector:
  security checks remain enabled and retries/validity states remain truthful.
- Exercise cold boot, reconnect, interrupted record install and stale NVS record
  handling on actual hardware before claiming a completed trusted path.

### 15.5 Checks to run during implementation

At minimum, run focused Go tests for attestation, commissioning, identity,
authorization, config, ingest and affected store/output code; the portable C
commissioning tests; the Python report tests; and the affected firmware builds.
The current host-test entry points include:

```sh
make -C esp32/components/mcu_identity/test
make -C esp32 host-test
make -C go check
```

Use the repository's configured toolchain for Go/ESP-IDF and complete the
appropriate open/trusted build checks. Bench verification is a separate required
result, not implied by host test success.

## 16. Questions for review

The v1 starting point is settled for this proposal. The defaults below make the
rest of the proposal actionable; reviewers should challenge specific decisions
or identify missing implementation dependencies.

| Question | Recommended answer |
|---|---|
| Which versions establish the first enrollment contracts? | v1 for attestation, commissioning, registry and bench reporting; future incompatible changes increment the affected standard |
| Put optional RTC in permanent slot or commissioning? | Commissioning, so replacement does not require rewriting a locked slot |
| What is the canonical hardware observer name? | EEPROM factory EUI-64 rendered in the existing lowercase byte-pair style |
| Is RTC identity always required if any RTC exists? | No; enforce per-product binding requirements and distinguish RTC function from identity |
| How are RTCs without a factory EUI represented? | Signed model code and RTC-present flag, with no instance binding |
| Can a shared EUI-like constant in code serve as unique RTC identity? | No; represent it as a model/type identifier in a separate namespace |
| General extension/TLV format now? | No; a fixed model code plus optional EUI and explicit signed flags |
| Does basic hardware need a new trust tier? | No; retain existing trust values and model capabilities separately |
| Can hardware mTLS alone mean trusted firmware? | No; retain the commissioned MCU session proof |
| Is a customer C/D pair added to one global A/B trust list? | No; register scoped operational and, when applicable, manufacturer authorities with exact key mappings and allowed pairings |
| May operational and manufacturer authorities differ? | Yes; explicit policy can pair customer-issued operational credentials with NavListen-manufactured hardware |
| What identifies the operational authority at connection time? | The exact verified Issuing-intermediate SPKI mapped to the enrollment, not merely a valid root chain or issuer name |
| What is the product namespace? | `(manufacturer_authority_id, product_u16)`; numeric product codes may repeat under different manufacturer authorities |
| Should registry presence become mandatory everywhere? | Separate policy decision; require it for deployments claiming current-generation/revocation enforcement |
| How does the serial-GPS board obtain product support? | Confirm its controller and hardware profile; assign product/revision deliberately and update explicit allowlists |
| How are already-locked development attestations handled? | Inspect actual lock state; use replacement parts if needed, without claiming locked records are writable |
| Does ordinary NMEA support belong in this change? | No; separate ingest feature with explicit measurement limits |

Review should also confirm the proposed 153/225-byte layout, evidence size bound,
uniqueness scope, manufacturing verification of regenerable operational keys,
and the dependencies needed to boot securely without RTC time.

## 17. Completion definition

The proposal is ready to implement when the v1 contracts, core/RTC split,
authority model, product policy and binary/schema definitions are accepted with
no unresolved contradictions. The feature is complete only when the collector,
firmware, signing/enrollment tools, registry, control plane and documentation
agree; NavListen A/B and synthetic customer C/D authority cases pass, including
an explicitly permitted mixed operational/manufacturer pairing and all
cross-authority rejection cases; and both a no-RTC hardware observer and an
ordinary software contributor have been verified through their actual supported
paths.

The resulting user-visible behavior is straightforward: a station has a stable
identity, independently verified provenance, explicit measurement capabilities
and deliberate sharing permissions. Adding an RTC or auxiliary sensors expands
what can be measured; it does not determine whether the station may contribute.
