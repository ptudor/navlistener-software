# `mfgattest` — offline manufacturer record tool

`mfgattest sign` creates the exact 72-byte ATECC slot-14 record from an offline P-256
manufacturer key. The private-key file must be mode 0600. `mfgattest verify` accepts a public
key or certificate and emits the verified tier plus statement/record fingerprints for the
enrollment row.

```sh
mfgattest sign -version 2 -key manufacturer.pem \
  -atecc-serial 0123456789abcdef11 \
  -rtc-eui 0004a31234567890 -board-eui 0004a3aabbccddee -board-rev 1

mfgattest verify -key manufacturer-public.pem -record RECORD_HEX \
  -atecc-serial 0123456789abcdef11 \
  -rtc-eui 0004a31234567890 -board-eui 0004a3aabbccddee -board-rev 1
```

This tool never issues an operational CA certificate and never assigns an organization,
collection, or publication policy.

## Verifying commissioning records and the registry

Two further commands need only public keys, so a buyer, a federation peer or an auditor can
run them offline. The formats are normative in
[`docs/COMMISSIONING.md`](../../../docs/COMMISSIONING.md).

`commission-verify` checks a commissioning record against the pinned manufacturer keys and
prints what the manufacturer signed: the observer id, profile, product, generation, the board,
secure-element and microcontroller identifiers, the lock state recorded at the bench, and the
record fingerprint.

```sh
mfgattest commission-verify -key manufacturer-public.pem -record RECORD_HEX
```

`registry-verify` checks a signed registry file against the pinned registry keys and prints
its sequence, issue time and board count.

```sh
mfgattest registry-verify -key registry-public.pem -file registry.json
```

`-key` may be given more than once in both commands. Verifiers pin sets of keys: a replaced
signer adds a key, and what the earlier key signed stays valid for as long as it stays pinned.
The manufacturer keys and the registry keys are separate sets, and neither command accepts
the other's.

A verified record shows that the manufacturer commissioned that board with that
microcontroller under that profile. It does not show that a given connection comes from that
microcontroller: that is the per-session proof a collector checks during the GNF1 handshake,
and it cannot be checked offline. Both commands print nothing and exit non-zero when
verification fails.
