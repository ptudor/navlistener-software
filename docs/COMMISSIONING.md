# Commissioning and hardware trust

How a collector knows what kind of hardware is on a connection: an original
board that was locked and commissioned, an original board that was never
locked, a bench unit, or something it knows nothing about.

This document is normative for the commissioning statement, the session proof,
the GNF1 evidence exchange and the registry. The Go reference implementation is
[`go/internal/commissioning`](../go/internal/commissioning/); the shared test
vectors are in [`common/fixtures`](../common/fixtures/) (`commissioning-*`).

## 1. Why a device's own report is not enough

A device reports the update track it was built for as `trust_profile`
([update operations](../esp32/docs/UPDATE-OPERATIONS.md)). That report is a
label. Firmware on unlocked hardware can report anything.

The slot-14 manufacturer attestation
([hardware contract §4.1b](HARDWARE-OBSERVER.md)) proves that a board core and
its secure element are original. It binds the manufacturer product, board
revision, typed board UID and ATECC serial. Replaceable parts deliberately do not
belong in that permanent core. It does not mention the microcontroller,
and whether a unit is locked is entirely a microcontroller property: Secure
Boot and flash encryption are ESP32-S3 eFuses. A board whose locked module was
replaced with a blank one keeps the attested board core unchanged, and its secure
element signs for whichever microcontroller drives the I²C bus.

Two additions close that gap:

1. a **commissioning record**, signed by the manufacturer after the board is
   locked, that binds the board's identifiers to one microcontroller and one
   profile; and
2. a **session proof**, signed by a key that only that microcontroller can use,
   over a value that exists only inside the current TLS session.

## 2. Identities on one board

| Part | Identity | Role |
|---|---|---|
| Typed factory UID (24CS128 preferred) | public: board and observer | the PCB's permanent identity and the network name: certificate SAN and observer id. A replaced PCB is a new unit. |
| ATECC608C | private: secure element | holds the operational key; its serial and the slot-14 attestation bind it to the board. |
| ESP32-S3 key | microcontroller | the part that runs the firmware. Named by the SHA-256 of its public key; its factory MAC is a label only. |

The microcontroller is a replaceable part. Replacing it is a recorded service
event that produces a new commissioning record with the next **generation**
number (§8); the board, observer and secure-element identities do not change.

The statement also records the RTC fitted at commissioning: its model, and its
factory EUI-64 when the part has one (only the MCP79412 does). That is history
for the unit record, not identity. No verifier compares it with the live board
or with a product policy, so a replaced or missing RTC leaves the record valid.

## 3. What a collector concludes

The collector derives one `hardware_trust` value per session, from evidence it
verified itself. It is never taken from a device's report or from configuration.

| Value | Meaning |
|---|---|
| `trusted` | A pinned manufacturer key signed a trusted record for this observer, the record is current and not revoked, and the commissioned microcontroller proved possession of its key on this session. |
| `open` | A pinned manufacturer key signed an open record for this observer: original hardware that is never locked. |
| `test` | A pinned manufacturer key signed a test record: a bench or development unit. |
| `none` | Everything else: software feeders, unknown or copied boards, revoked boards, and any evidence that failed verification. |

`trusted` means the manufacturer built and locked this unit and it is running
firmware the manufacturer signed. It does not mean the observations are true: a
genuine, locked receiver still reports whatever reaches its antenna. Detecting
that is the job of the [RF monitor](DEFENSE-PNT.md) and cross-station
comparison, not of this mechanism.

Evidence labels data; it does not gate admission. A session whose evidence is
rejected continues with `hardware_trust = none`, because discarding a station's
observations over a labelling fault would lose data that an unattended station
cannot send again. Admission remains the operational credential's job.

## 4. The commissioning statement (v1)

This pre-launch v1 contract uses a fixed 180-byte big-endian layout. The typed
board UID is defined in [BOARD-IDENTITY.md](BOARD-IDENTITY.md). After launch, a
layout change requires a new version and domain string.

| Offset | Size | Field | Notes |
|---:|---:|---|---|
| 0 | 1 | version | `0x01` |
| 1 | 1 | profile | 1 trusted, 2 open, 3 test |
| 2 | 1 | mcu_family | 1 = ESP32-S3 |
| 3 | 1 | mcu_key_alg | 0 none; 1 = RSA-3072, RSASSA-PSS, SHA-256 |
| 4 | 2 | product | the product line the board was built as; 1 = NavListen GNSS observer, 0 reserved |
| 6 | 2 | board_rev | as in the slot-14 attestation |
| 8 | 2 | security | bit field below |
| 10 | 2 | identity_flags | bit 0: RTC EUI-64 recorded; bit 1: RTC recorded; all other bits reserved |
| 12 | 2 | rtc_model_id | recorded RTC: 0 = none; 1 = MCP79412; 2 = DS3231; 3 = MAX31328 |
| 14 | 4 | generation | 1 at first commissioning; +1 each time the board is commissioned again |
| 18 | 8 | commissioned_at | Unix seconds, UTC |
| 26 | 35 | board_uid | kind (2), length (1), value with zero padding (32); permanent board identity |
| 61 | 9 | atecc_serial | |
| 70 | 8 | rtc_eui64 | recorded MCP79412 EUI-64; zero unless identity flag bit 0 is set |
| 78 | 6 | mcu_mac | factory base MAC; a label, never a proof |
| 84 | 32 | mcu_key_sha256 | SHA-256 of the key's DER SubjectPublicKeyInfo; zero when `mcu_key_alg` is 0 |
| 116 | 32 | secure_boot_keys | SHA-256 over the three 32-byte Secure Boot key digests in slot order, an empty or revoked slot as 32 zero bytes; zero when Secure Boot is off |
| 148 | 32 | attestation | SHA-256 of the board's 72-byte slot-14 record |

Security bits, as observed at the bench when the statement was prepared:

| Bit | Meaning |
|---:|---|
| 0 | Secure Boot v2 enabled |
| 1 | flash encryption in release mode |
| 2 | pad JTAG and USB JTAG permanently disabled |
| 3 | ROM download mode is secure, or disabled |
| 4 | the microcontroller key's eFuse block is burned, read-protected and write-protected, **and** the eFuse read-protection field has been sealed again |

All other bits are reserved and must be zero.

Every signer and every verifier enforces the same consistency rules, so a
statement that carries a valid signature is also one that makes sense:

- `product ≠ 0`, `generation ≥ 1`, `commissioned_at ≠ 0`, and no identifier is all-zero or all-`0xff`.
- A recorded RTC EUI-64 requires the RTC flag and model 1, the only listed part
  with a factory EUI-64. With no RTC, the model and EUI-64 are zero; a recorded RTC
  uses a listed model; an unrecorded EUI-64 is zero.
- `mcu_key_sha256` is nonzero exactly when `mcu_key_alg ≠ 0`; bit 4 requires a named key.
- `secure_boot_keys` is nonzero exactly when bit 0 is set.
- **trusted** requires `mcu_key_alg = 1` and security bits `0x001f` — all five.
- **open** requires `mcu_key_alg = 0` and security `0`: an open board is never locked.
- **test** may be locked or unlocked, with or without a key.
- `attestation` is nonzero for trusted and open. Only a test board may be unattested.

One manufacturer key signs for every product line its owner makes, which is
why the statement names the product: a record for some other product proves
nothing to an observer collector, whatever its profile. Product values other
than 1 are assigned by the manufacturer and are opaque here.

The manufacturer key signs

```text
SHA-256( "MFG-COMMISSION-v1" || statement[180] )
```

with ECDSA P-256. The ASCII prefix is domain separation: the same key also signs
slot-14 core attestations (`"ATECC-MFG-CORE-v1"`), and neither signature can be
presented as the other.

The **record** is what is stored, carried and published — 252 bytes:

```text
statement[180] || signer_key_id[8] || R[32] || S[32]
```

`signer_key_id` is the first eight bytes of the SHA-256 of the signer's DER
SubjectPublicKeyInfo. It is a lookup hint and is not signed. A record's
**fingerprint** is the SHA-256 of all 252 bytes.

## 5. The microcontroller key and the session proof

### 5.1 The key

The ESP32-S3 Digital Signature peripheral performs RSA private-key operations
with a key that software can never read. The private parameters are stored as
ciphertext, encrypted under a key derived from a 256-bit HMAC key in an eFuse
block (purpose `HMAC_DOWN_DIGITAL_SIGNATURE`) that is read-protected in
hardware. Any firmware running on the chip can ask the peripheral to sign;
Secure Boot restricts what runs on the chip to signed firmware. The key is
therefore evidence of "this locked microcontroller, running signed firmware"
only on a chip whose Secure Boot is enabled — which is why the statement
records the lock state beside the key, and why a trusted statement requires
both.

Commissioning generates the key **on the device**, after Secure Boot and flash
encryption are in force:

1. Signed firmware generates a 3072-bit RSA key (`e = 65537`) and a 256-bit
   HMAC key from the hardware random number generator with an entropy source
   enabled.
2. It encrypts the private parameters for the peripheral, stores the ciphertext
   and the public key, burns the HMAC key into a free eFuse key block with read
   and write protection, signs and verifies a test message through the
   peripheral, and erases the plaintext from memory.
3. It seals the eFuse read-protection field, so that no further block can be
   read-protected.
4. It reports the public key. The private key has never existed outside the chip.

Step 3 exists because of step 2. A Secure Boot chip normally seals that field
on its first boot: read-protecting a Secure Boot key digest would make it read
as zeros. Protecting a key *after* Secure Boot is enabled therefore needs the
production build to leave the field open
(`CONFIG_SECURE_BOOT_V2_ALLOW_EFUSE_RD_DIS`) and the commissioning step to close
it. A chip that holds a key but has not been sealed is not locked: security
bit 4 stays clear, and no trusted statement can be made for it.

A failed self-test after the burn spends that eFuse block and seals nothing, so
a second free block can still be tried. A production module has one spare. A
failure here is more likely a firmware or procedure defect than bad silicon;
find the cause before spending the last block.

The ciphertext is useless without the eFuse key, so the factory keeps a copy of
it with the unit's record. A unit whose flash was wiped can have its ciphertext
and record restored; it cannot be given a different identity.

Open boards receive no microcontroller key: commissioning an open board changes
no eFuse.

### 5.2 The proof

Both ends derive 32 bytes of exported keying material from the TLS session
(RFC 5705; RFC 8446 §7.5) with

```text
label   = "EXPERIMENTAL-navlistener-mcu-proof-v1"
context = none
length  = 32
```

The device signs, through the peripheral,

```text
SHA-256( "NAVL-MCU-PROOF-v1" || exported[32] || record_fingerprint[32] )
```

with RSASSA-PSS, SHA-256, MGF1-SHA-256 and a 32-byte salt.

The exported value is never transmitted and differs for every TLS session, so a
proof cannot be replayed, and cannot be obtained from a genuine unit over one
connection and presented on another. No challenge round trip is needed.
A TLS 1.2 session must have negotiated the extended master secret; without it
no keying material is exported and no proof can be verified.

## 6. GNF1 evidence exchange

A feeder that holds a commissioning record sets `"evidence": true` in its HELLO
and sends one **EVIDENCE** frame (type `0x0A`) immediately after it, without
waiting:

```text
MAGIC, HELLO{..., "evidence": true}, EVIDENCE  →
                                               ←  WELCOME{..., "hardware_trust": "trusted"}
```

EVIDENCE payload, at most 2048 bytes:

```text
[1B version = 0x01]
[2B BE length][record, 252 bytes]
[2B BE length][microcontroller public key, DER SubjectPublicKeyInfo — or empty]
[2B BE length][session proof — or empty]
```

The key and the proof are present together or absent together, and nothing
follows the proof.

The collector authenticates the HELLO first. Evidence from a peer that fails
authentication is never parsed, so an unauthenticated connection cannot make
the collector perform a signature verification. After authentication it reads
exactly one further frame, under the handshake deadline and the 2048-byte cap;
any other frame type is a protocol error that ends the connection.

WELCOME then carries `hardware_trust` and, when evidence was rejected,
`evidence_error` with one of the reasons below. Both are informational for the
device's journal. The `"ok":true` spelling that feeders match is unchanged, and
a feeder that sets no `evidence` flag sees no difference at all.

Evaluation order, with the rejection reason for each step:

| Step | Check | Reason |
|---:|---|---|
| 1 | the payload and its statement are well formed | `malformed` |
| 2 | the record verifies under a pinned manufacturer key | `signature` |
| 3 | the record is for the observer product | `product` |
| 4 | the record's typed board UID, rendered as an observer id, equals the authenticated observer | `identity` |
| 5 | with a registry: the board is listed, when the collector requires it | `unlisted` |
| 6 | with a registry: the board is not revoked | `revoked` |
| 7 | with a registry: the record is the board's current one | `superseded` |
| 8 | a trusted record comes with a proof | `proof_missing` |
| 9 | the presented key is the one the record names, is RSA-3072 with `e = 65537`, and the proof verifies for this session | `proof` |

A collector with no pinned manufacturer keys still reads the frame, to keep the
handshake in step, and answers `none` with `unconfigured`.

Step 4 is what ties the evidence to the operational credential. A credential
for one observer cannot borrow another board's record, and a record is useless
without a credential for the observer it names.

## 7. The registry

The registry is the manufacturer's published list of commissioned boards. It
says which commissioning record is current for each board and which boards have
been withdrawn.

**It can only take trust away.** A board is trusted because of its
manufacturer-signed record and its session proof; nothing in the registry can
substitute for either. The registry is therefore signed by a separate
**operations key** that can be kept online, and the worst a stolen operations
key can do is withdraw boards or restore a withdrawn one. It cannot create a
trusted board.

Envelope:

```json
{
  "format": "navlistener-registry-v1",
  "payload": "<base64 of the payload bytes>",
  "signatures": [{"key_id": "<16 hex>", "signature": "<base64 R||S>"}]
}
```

Each signature is ECDSA P-256 over
`SHA-256( "NAVL-REGISTRY-v1" || payload bytes )`. Signing the exact bytes avoids
any dependence on JSON canonicalisation. One valid signature from a pinned key
is sufficient; signatures from keys a verifier does not pin are ignored, so a
new key can be introduced before every verifier pins it.

Payload:

```json
{
  "manufacturer_authority_id": "example-manufacturing",
  "sequence": 42,
  "issued_at": "2026-09-17T12:00:00Z",
  "ledger_head": "<64 hex>",
  "boards": [{
    "board_uid_kind": "eui64",
    "board_uid": "0004a3aabbccddee",
    "rtc_model_id": 1,
    "rtc_eui64": "0004a31234567890",
    "atecc_serial": "0123456789abcdef11",
    "status": "active",
    "reason": "",
    "profile": "trusted",
    "generation": 1,
    "record": "<base64 of the 252-byte record>"
  }]
}
```

- `status` is `active` or `revoked`. The manufacturer's finer-grained lifecycle
  (in stock, shipped, returned, scrapped) stays in its own records.
- `manufacturer_authority_id` must equal the authority selected by authenticated
  deployment configuration before the registry is adopted. Product values and
  rollback state are interpreted only within that authority.
- `rtc_eui64` is required but nullable: it is a string exactly when the record
  records an RTC EUI-64, and `null` otherwise.
- The descriptive columns must agree with the embedded record. A typed board UID
  or ATECC serial may appear only once. An RTC EUI-64 may repeat: a part moved to
  another board is recorded there too. A registry that breaks either rule is
  rejected whole.
- `sequence` only rises. A verifier refuses a registry older than the one it
  holds, and records the highest sequence it has accepted so that a restart
  cannot be used to load an older copy — a restored backup must not quietly
  restore a withdrawn board.
- `ledger_head` names the manufacturer's record-keeping state the export was
  made from; it is opaque to verifiers.

A collector reloads the registry when the file changes. A reload that fails
verification leaves the registry already in force untouched.

## 8. Commissioning a board again

`generation` rises by one every time a board is commissioned again, for any
reason, so a board never has two records with the same generation and "the
current record" is never ambiguous. The usual reason is a replaced
microcontroller.

A replaced module is a new part with a new key. The board is commissioned again:
the replacement generates its own key, and the manufacturer signs a statement
with the same board identifiers, the new key and `generation + 1`. The registry
then lists the new record, and a collector holding that registry answers
`superseded` to the old one. The removed module's key names a part that is no
longer fitted to anything.

Only the manufacturer can do this, because only the manufacturer key can sign
the new statement. A module swapped by anyone else leaves a board whose record
names a key the new module does not have, and its sessions prove nothing.

## 9. Keys and custody

| Key | Signs | Custody |
|---|---|---|
| manufacturer key | slot-14 attestations and commissioning statements | offline hardware signer; reached only through a signing ceremony. Never a file in production. |
| operations key | registry exports | the manufacturer's record-keeping host |
| microcontroller key | session proofs | inside each ESP32-S3; never leaves it |
| operational key | the mTLS handshake | inside each ATECC608C; never leaves it |

Verifiers pin **authority-scoped sets** of manufacturer keys and of registry keys. A signer
that is replaced adds a key to the set, and records signed by the earlier key
remain valid for as long as that key stays pinned. Removing a key withdraws
every record it signed.

Test hierarchies use file keys and are kept entirely separate: a test
manufacturer key is never pinned by a production collector.

## 10. Collector configuration

```toml
[[operational_authority]]
id = "example-operations"
enabled = true
roots = ["/usr/local/etc/navlistener/root-a.crt", "/usr/local/etc/navlistener/root-b.crt"]
issuers = ["/usr/local/etc/navlistener/issuing-a.crt", "/usr/local/etc/navlistener/issuing-b.crt"]
manufacturer_authorities = ["example-manufacturing"]

[[manufacturer_authority]]
enabled = true
manufacturer_authority_id = "example-manufacturing"
manufacturer_keys = ["/usr/local/etc/navlistener/root-a-slot5.pem", "/usr/local/etc/navlistener/root-b-slot5.pem"]

# Optional. Without a registry every valid record is honoured and nothing can
# be withdrawn.
registry = "/var/db/navlistener/registry.json"
registry_keys = ["/usr/local/etc/navlistener/registry-1.pem"]
registry_reload = "30s"
# Where the highest accepted registry sequence is recorded, so a restart cannot
# accept an older registry. Absolute path; written atomically, mode 0600.
registry_state = "/var/db/navlistener/registry.state"
# Withhold trust from a board the registry does not list. Leave false where
# the registry copy may lag behind newly commissioned boards.
require_registry_entry = false

[[manufacturer_authority.product_policy]]
product = 1
revision = 258
```

Manufacturer and registry key files hold P-256 `PUBLIC KEY` PEM, never CA
certificates. Root slot-0 CA keys, Issuing keys, manufacturer slot-5 keys and
registry keys are separate roles; ambiguous ownership or role reuse is rejected.
Operational roots and issuers are certificates. Cross-signed certificates for the
same issuing SPKI name one operational authority. `[push].require_client_certificate`
requires mTLS with the registered issuers. The SQL enrollment supplies both expected
authority IDs; neither the leaf name nor the record selects a manufacturer.

For customer-issued credentials on A/B hardware, register a separate operational
authority and explicitly list the A/B manufacturer in its allowed pairings. A C/D
manufacturer, if present, has its own slot-5 pins, product policies, registry keys,
registry file and persistent floor. Never combine A/B/C/D manufacturer keys in one
set. Core and commissioning may use different keys of the same pair; enrollment
retains both exact signer SPKIs and the core-record fingerprint.

A configured registry that is
missing, does not verify, or is older than the recorded sequence stops the
collector at startup. A registry configured without `registry_state` starts
with a warning, because its sequence is then remembered only until the next
restart.

A registry that is withheld rather than rolled back cannot be detected from
the file. The collector exports the loaded registry's sequence and issue time
as metrics labeled by `manufacturer_authority_id` so that each stream's staleness
can be alerted on. Enrollment, issuance and service procedures are documented in
[CONTROL-PLANE.md](CONTROL-PLANE.md).

RTC model 2 is the [DS3231](https://www.analog.com/media/en/technical-documentation/data-sheets/DS3231.pdf)
and model 3 the [MAX31328](https://www.analog.com/media/en/technical-documentation/data-sheets/max31328.pdf).
Neither has a factory EUI, so a record names only the model. A model code
names a part, not an individual chip. Listing a model does not implement a
board driver.

## 11. Limits

- **`open` and `test` are labels, not proofs of origin for the data.** Those
  boards hold no key, so whoever holds the observer's operational credential
  and a copy of its record is labelled the same way. That is inherent in an
  unlocked board, which runs whatever firmware its owner chooses. Only
  `trusted` carries a per-session proof, and only `trusted` should ever be
  given weight by policy.
- The mechanism identifies hardware and firmware provenance. It makes no claim
  about the truth of an observation (§3).
- An attacker holding a genuine, trusted, powered unit can let it run. They can
  also drive its antenna input. Neither is addressed here.
- Fault injection against, or decapsulation of, the ESP32-S3 or the ATECC608C is
  outside this design; the security of the eFuse read protection and of the
  secure element are their manufacturers' claims.
- Whoever holds a Secure Boot signing key can ship firmware that signs proofs.
  Those keys need the same custody as the manufacturer key.
