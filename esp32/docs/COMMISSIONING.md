# Commissioning an ESP32-S3 observer

The firmware's side of [commissioning and hardware trust](../../docs/COMMISSIONING.md):
the microcontroller's own key, the record the manufacturer signs for the board,
and the evidence the feeder presents to a collector. That document is normative
for every format named here. This one describes what the firmware does at the
bench, what is irreversible, and how each step fails.

**Status.** The portable formats are host-tested against the shared fixtures
(`make -C esp32 host-test`). The target code builds. Nothing here has yet been
exercised on hardware: no key has been generated, no eFuse burned and no proof
verified by a collector from a real board. Work through
[Before the first production burn](#before-the-first-production-burn) on a scrap
module first.

## What the firmware holds

| Item | Where | Notes |
|---|---|---|
| HMAC key for the Digital Signature peripheral | one eFuse key block, purpose `HMAC_DOWN_DIGITAL_SIGNATURE` | read-protected and write-protected in hardware; software never sees it again |
| RSA-3072 private key | NVS, as ciphertext only the peripheral can use | namespace `hwtrust` in the `update_meta` partition |
| RSA-3072 public key | same namespace, DER SubjectPublicKeyInfo | its SHA-256 names the microcontroller in the commissioning statement |
| commissioning record | same namespace, 225 bytes | signed by the manufacturer; installed at the bench |

`update_meta` is the updater's NVS partition. It is encrypted on a production
build, and no configuration-reset or provisioning path erases it: the BOOT
gesture removes individual provisioning keys from the default `nvs` partition
only. A whole-chip erase does remove it. The ciphertext is useless without the
eFuse key of the chip that made it, so the factory keeps the copy that
`commission report` prints and can give it back with `commission restore`.

Commissioning an open board reads its identifiers and installs its record;
nothing else changes. In an open build `keygen` and `seal` compile to refusals,
and the built `mcu_identity` component references no eFuse write function and no
key generation (checked with `nm` on the open-profile build).

## Bench commands

The commands exist only on the USB Serial/JTAG console. No HTTP, BLE, SoftAP or
GNF1 path reaches them. A locked unit cannot be reflashed over USB, so the
release firmware carries them (`CONFIG_NVF_COMMISSION_CONSOLE`, required by
`tools/production_profile.py` for both release tracks).

Send one line per command. Each answers with exactly one line beginning
`NVF-COMMISSION-`, printed independently of the log level.

| Command | Effect |
|---|---|
| `commission report` | print `NVF-COMMISSION-REPORT {json}` |
| `commission keygen` | **irreversible**: generate the key, burn one eFuse key block, then seal eFuse read protection |
| `commission install <base64 record>` | store the manufacturer-signed record |
| `commission restore <base64 ds context> <base64 public key>` | give a wiped unit its ciphertext and public key back |
| `commission seal` | **irreversible**: the seal alone, for a board whose `keygen` could not finish it |

`tools/commission_report.py console.log -o report.json` extracts the newest
report from a captured console log and checks its shape. It reads files only; it
never opens a serial port.

### The report

```json
{"v":1,"product":1,"identity_flags":3,"rtc_model_id":1,
 "rtc_expected":true,"rtc_present":true,"identity_complete":true,
 "atecc_serial":"…","rtc_eui64":"…","board_eui64":"…","board_rev":1,
 "mcu_family":1,"mcu_mac":"…","security":31,"secure_boot_keys_sha256":"…",
 "attestation_record":"…","mcu_key_alg":1,"mcu_public_key_der":"<base64>",
 "mcu_key_sha256":"…","ds_context":"<base64>","key_state":"ready","key_block":4,
 "rd_dis_sealed":true,"record":"…","firmware":"…","trust_profile":"trusted"}
```

- Identifiers are lowercase hex and are read live from the parts: the ATECC
  serial from its configuration zone, the RTC EUI-64 from the MCP79412's
  protected EEPROM block, the board EUI-64 from the manifest EEPROM, and the
  factory base MAC from eFuse. **An identifier that could not be read is JSON
  `null`, as is an unreadable `board_rev`; nothing is guessed.**
- `identity_flags` and `rtc_model_id` are the statement values for this product.
  `rtc_expected`, `rtc_present`, and `identity_complete` make the difference
  between “not part of this product,” “expected but absent,” and “successfully
  read” explicit.
- `attestation_record` is the 72-byte slot-14 record. The ATECC refuses that
  read until its data zone is locked, so an unprovisioned part reports it empty.
- `security` is the statement's security bit field as this chip reports it now.
  **Bit 4 is set only when the key is ready, its block is read- and
  write-protected, and eFuse read protection is sealed.** A keyed but unsealed
  chip reports `key_state: ready` with bit 4 clear (`security` 15, not 31), so no
  trusted statement can be prepared for it; `rd_dis_sealed` says which it is.
  `tools/commission_report.py` rejects a report that sets bit 4 any other way.
- `mcu_key_alg`, `mcu_public_key_der`, `mcu_key_sha256` and `ds_context` are
  populated only when `key_state` is `ready`.
- `record` is the installed record in hex, or empty.

The factory should treat the report as the firmware's account of the board.
Identifiers read independently, with the microcontroller held in reset, are the
check on it.

### `keygen`

Refused unless all of the following hold:

1. Secure Boot is enabled and flash encryption is in release mode. A
   test-profile build may opt out with `CONFIG_NVF_MCU_KEY_UNLOCKED_TEST`; that
   still burns a key block permanently, and the resulting key says nothing about
   what firmware runs on the board. Both release tracks forbid the option.
2. The chip has no usable or restorable key: no key that is ready, no burned
   Digital Signature block awaiting its ciphertext (`orphaned`; use `restore`),
   and no burned block whose self-test has yet to complete. A block recorded as a
   **fault** does not count: it is spent, and another free block may be tried.
3. A free eFuse key block exists.
4. The eFuse read-protection field is still writable. The bootloader
   write-protects it when it enables Secure Boot unless it was built with
   `CONFIG_SECURE_BOOT_V2_ALLOW_EFUSE_RD_DIS=y`, which the production profile now
   requires. Without it a new key block could not be read-protected, so the
   firmware refuses before anything is burned. A chip that is already sealed and
   has no key can therefore never be given one.

The rules are `nvf_mcu_keygen_refusal()` in the portable core and are host-tested;
every one of them is decided before any eFuse is written.

Then, in order:

1. Generate a 3072-bit RSA key (`e = 65537`) and a 256-bit HMAC key from a DRBG
   seeded by the hardware random number generator. The radio is the entropy
   source whenever Wi-Fi is up, which is the normal state in both setup and
   station mode; otherwise the SAR ADC source is enabled for the duration.
   Expect tens of seconds to a few minutes. The console task drops to the idle
   priority while it works so the task watchdog stays fed.
2. Derive the peripheral's operands and encrypt them under the HMAC key.
3. **Store** the ciphertext and the public key, marked *staged*.
4. **Burn** the HMAC key into the first free key block. The eFuse API writes the
   key, its purpose and its read and write protection as one batch.
5. Confirm the block's purpose and protection, sign a test message through the
   peripheral, and verify it with mbedTLS under the public key using the same
   RSASSA-PSS parameters a collector uses. Mark the key *ready* and store that.
6. **Seal**: write-protect the eFuse read-protection field, the state the
   bootloader would have left by default. This happens only after steps 4 and 5
   have fully succeeded, and it is not optional: until it holds, the chip does
   not report security bit 4.

Every plaintext buffer is erased as soon as the burn has been attempted,
whichever way the attempt ends.

All key material stays in internal RAM: this board's configuration gives mbedTLS and
ordinary allocations internal memory only, and the PSRAM is used solely by explicit
requests such as the spool.

| Interrupted or failed at | Result |
|---|---|
| steps 1–3 | nothing burned; retry |
| step 4, refused while the batch is prepared | the batch is cancelled and the block is still unused; the staged ciphertext is discarded; retry after fixing the cause |
| step 4, error while the batch is committed | the block may be partly burned and is treated as spent; step 5 decides, and normally ends in the hard fault below |
| reset between 3 and 4 | next boot finds a staged key and an unused block, discards the staged data; retry |
| reset between 4 and 5 | next boot finds a staged key and a burned block and runs step 5 |
| step 5 | **fault**: the block is spent and unusable. The firmware reports the block number and records it as a fault. **Nothing is sealed**, so `keygen` may be run again while a free key block remains — a production module has exactly one. A failed self-test is more likely a defect in the firmware or the procedure than in the silicon: find the cause before spending the last block. |
| reset or failure between 5 and 6 | the key is ready and the field unsealed: `security` stays 15 and `rd_dis_sealed` false. Run `commission seal`. The firmware also says so in the log at every boot until it is done. |

A production module uses five of its six key blocks: three Secure Boot digests,
one flash-encryption key (XTS-AES-128, the configured size), and this key. One
remains.

### `seal`

`keygen` seals by itself. This command is the recovery path for a board that
lost power between the self-test and the seal, or whose ready state could not be
stored. It burns the write-protect bit of the eFuse read-protection field; after
it, no further block on the chip can ever be read-protected.

It is refused unless a committed key exists **and passes its self-test at that
moment**: the key is signed with and verified again, its ready state is stored,
and only then is the field sealed. It is a no-op on a chip that is already
sealed and keyed. A chip with a recorded fault and no good key is never sealed,
because sealing it would end any chance of a second key.

An unsealed chip leaves signed firmware able to read-protect a Secure Boot
digest block, which would stop the chip booting. That is why the seal is tied to
the one bit a trusted statement cannot do without.

### `install`

Accepts a 225-byte record and stores it only if its statement:

- is for the observer product;
- exactly names this product, board revision, board EUI-64, ATECC serial,
  declared RTC presence/model/EUI-64, this chip's factory MAC, and the live
  72-byte slot-14 record digest, **all of which must have been read
  successfully when the statement binds them**;
- names the key this chip holds, when it names a key at all; and
- claims no lock state the chip does not have.

**The manufacturer signature is not checked on the device.** The firmware holds
no trust root for it, and a collector verifies it on every session. The checks
above exist to catch the wrong board's record at the bench, not to establish
trust.

Installing a new record replaces the old one; this is how a re-commissioned
board receives its next generation.

### `restore`

For a unit whose flash was erased. It requires a burned, fully protected Digital
Signature key block, and stores the supplied ciphertext and public key only
after a signature made with them verifies on this chip. Every protected Digital
Signature block is tried, so a recorded fault in a lower block does not hide the
good key above it. A context from another chip, or a mismatched public key,
fails that test and changes nothing. A restored key on a chip that was never
sealed still needs `commission seal`.

### What the commands can do to a unit in the field

Someone with physical access to the USB port can read the report, which holds
only public values and ciphertext. They cannot generate a second key, cannot
install a record for another board, and can at worst replace the record with an
older genuine one for the same board, which a collector holding the registry
answers with `superseded`. They already have the board.

## Presenting evidence

When a record is installed, every connection to a collector carries evidence
([§6](../../docs/COMMISSIONING.md#6-gnf1-evidence-exchange)):

1. The firmware re-reads the permanent and declared replaceable identities,
   slot-14 record, MCU security state, Secure Boot key digest, and MCU key, then
   compares all of them with the installed statement. Any mismatch suppresses
   the entire evidence exchange for that session.
2. After the TLS handshake the pusher exports 32 bytes of keying material with
   `mbedtls_ssl_export_keying_material` under the label
   `EXPERIMENTAL-navlistener-mcu-proof-v1`
   (`CONFIG_MBEDTLS_SSL_KEYING_MATERIAL_EXPORT`). The pusher pins TLS 1.2, where
   export depends on the extended master secret; mbedTLS offers it.
3. For a record that names a key, the firmware signs the proof digest through
   the peripheral with RSASSA-PSS (SHA-256, MGF1-SHA-256, 32-byte salt).
4. The HELLO carries `"evidence":true` and one EVIDENCE frame (`0x0A`) follows
   it immediately, before anything is read.

An existing build directory keeps its generated `sdkconfig`, so it does not pick up
the export option by itself; the build wrapper warns about the difference. Build in a
fresh directory. A build without the option still runs, and behaves as below.

If the keying material cannot be exported, the key is not ready, or the
peripheral does not sign:

- an **open or test** record is presented alone, without a key or a proof;
- a **trusted** record is not presented at all. A trusted record without its
  proof would only be rejected, so the HELLO carries no flag and the collector
  answers `none`.

Either case is logged, and a withheld trusted record is journaled.

The collector's answer arrives in WELCOME as `hardware_trust` and, when the
evidence was rejected, `evidence_error`. They are informational: the session
continues whatever they say. The firmware logs a change of verdict, writes it to
the [journal](JOURNAL.md#hardware-trust-and-commissioning-events), and reports
the latest values as `hardware_trust`, `evidence_error` and
`commissioning_record` in `tools/ota.py status`.

A collector accepts evidence only for the observer it names, so the station name
must be the record's board EUI-64 as lowercase hyphen-separated byte pairs
(`00-04-a3-12-34-56-78-90`). The firmware warns at startup when the configured
station differs.

## Bench sequence

Trusted board, after the attested secure element is fitted and the board has
passed test:

1. Flash the signed production bootloader, partition table and application, and
   let the first boots enable Secure Boot and flash encryption. This is the
   separately attended locking procedure in
   [Update operations](UPDATE-OPERATIONS.md#signing-adapters-and-the-dangerous-parts).
2. `commission report` — confirm `security` is 15 and every identifier is present.
3. `commission keygen` — it seals as its last step and prints a fresh report
   whichever way it went.
4. Check that report: `security` is 31, `key_state` is `ready`, `rd_dis_sealed`
   is true. If the key is ready but `security` is 15, run `commission seal` and
   `commission report`. Capture the log and keep the extracted JSON, including
   `ds_context`, with the unit's record.
5. The manufacturer signs the commissioning statement built from that report. A
   trusted statement needs `security` 31; nothing less validates.
6. `commission install <record>`. The firmware refuses a record that claims a
   lock state the chip does not report, so a trusted record cannot be installed
   on an unsealed chip either.
7. Provision the station with the observer id as its station name and confirm
   the collector answers `trusted`.

Open board: steps 2, 5 (an open statement), 6 and 7, on an open build. No eFuse
changes at any point.

## Before the first production burn

Bench validation still owed, on a scrap ESP32-S3 module that can be locked and
discarded:

1. **Locking with `SECURE_BOOT_V2_ALLOW_EFUSE_RD_DIS`.** Confirm the first boots
   leave the read-protection field writable and everything else as the
   production profile intends.
2. **Key generation.** Time it; confirm the watchdog stays quiet; confirm the
   chosen block is the expected free one, that `keygen` alone takes the report's
   `security` from 15 to 31, and that `rd_dis_sealed` turns true only after the
   self-test has passed.
3. **The signature itself.** Verify a proof independently of the firmware's own
   self-test: capture the public key and have the Go collector accept the
   session (`hardware_trust: trusted`). This is the first time the byte order of
   the peripheral's operands and result is checked against an outside verifier.
4. **Interrupted generation.** Reset between the store and the burn, between the
   burn and the self-test, and between the self-test and the seal, and confirm
   the documented recovery. For the last: the report must show `key_state: ready`
   with `security` 15 and `rd_dis_sealed` false, a trusted record must be refused
   by `install`, and `commission seal` must then take `security` to 31.
5. **Restore.** Erase flash, reflash, confirm `key_state` is `orphaned`, restore
   from the saved report, and confirm proofs verify again. Confirm a context from
   a different module is refused.
6. **Seal and faults.** Confirm `commission seal` is refused with no key and is a
   no-op once sealed; that `keygen` on an already-sealed chip with no key is
   refused before any burn; and, if a module can be spared for it, that a forced
   self-test failure records a fault, leaves the field unsealed, and lets a
   second `keygen` use the remaining free block.
7. **Identifier reads.** Compare `atecc_serial`, `rtc_eui64` and `board_eui64`
   with values read by an independent I²C master, and `attestation_record` with
   the slot-14 contents of a part whose data zone is locked.
8. **USB console.** Confirm commands are accepted on a locked chip, that lines of
   the `restore` length arrive intact, and that logging does not stall with no
   USB host attached.
9. **TLS export.** Confirm both the direct and the tunnelled connection export
   keying material, and that a collector requiring the extended master secret
   agrees on it.
