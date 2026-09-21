# Board identity

The preferred EEPROM for new assemblies is Microchip **24CS128**, with its main
array strapped to I2C address **0x50**. Its factory-programmed serial supplies the
board identity. Assemblies using 24AA025E64 remain
supported hardware choices under the same contract.

A board UID is a **kind and an opaque byte string**. Never truncate, hash, convert
to a floating-point JSON number, or reinterpret it as a MAC address. Part model,
I2C address, reader and time belong in the manufacturing observation. They do not
change the identity namespace.

| Code | JSON kind | Value length | Interpretation |
|---|---|---|---|
| 1 | `microchip_eui64` | 8 bytes | Microchip factory EUI-64, including 24AA025E64 |
| 3 | `microchip_cs128` | 16 bytes | Complete Microchip CS-series factory serial, in read order |

The 24CS128, AT24CS01 and AT24CS02 share the 128-bit identity namespace. Their
hardware read procedures differ. Supporting a namespace does not automatically
qualify every member's driver or its writable memory layout.

The wire field is 35 bytes: big-endian uint16 code, uint8 value length, and 32
value bytes padded on the right with zeros. The listed lengths are mandatory.
Zero, erased, unknown-kind, wrong-length and nonzero-padding values are
rejected. Future sources get explicitly registered codes, lengths and validation;
a verifier that does not understand a code rejects it.

JSON uses `board_uid_kind` and `board_uid` (lowercase hex, without separators).
The observer identifier is `board-<four lowercase hex code digits>-<value hex>`.
For example, `board-0003-00112233445566778899aabbccddeeff` retains all 128 bits.
Identical value bytes under different kinds are different identities.

The permanent core signs product, revision, the complete typed UID and ATECC
serial. Commissioning also binds the typed UID. Neither an MCU replacement nor
an optional RTC replacement changes the board UID. An identity-chip replacement
requires a new board identity; record the relationship in the service history.
Once an identity is adopted, a missing or changed source fails validation rather
than silently selecting a different chip. Existing signatures must match live
identity before authorization.

## Physical reads

24CS128 discovery uses its I2C Manufacturer ID (0x00d0b8), then reads all 16 serial
bytes from Security Register offset 0x0800. At main-array address 0x50, that register
is addressed at 0x58. These are separate address spaces; the serial is not in the
writable main array. Main-array address 0x51 is also supported; the address does
not enter the signed UID.

A 24AA025E64 uses its factory EUI at offset 0xf8, at 0x50 or 0x51. An ACK alone
does not identify a model. Discovery must avoid storage writes and reject
ambiguous devices and bus faults. The EEPROM identity is recorded separately
from its role as board UID; both values match for the supported assemblies.

These are identification devices, not proof of possession. The signed core,
manufacturer authority and operational key policy supply authentication. The
A/B root and C/D intermediate architecture is independent of UID chip selection.

References: [24CS128 datasheet, sections 10–11](https://ww1.microchip.com/downloads/aemDocuments/documents/MPD/ProductDocuments/DataSheets/24CS128-128-Kbit-3.4-MHz-I2C-Serial-EEPROM-DS20006913.pdf),
[AT24CS01/02 datasheet](https://ww1.microchip.com/downloads/en/DeviceDoc/20006330A.pdf).
