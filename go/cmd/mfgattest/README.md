# `mfgattest` — manufacturer-record verifier and fixture tool

Production manufacturer records are issued by the hardware Root ceremony, not
by this program. `mfgattest verify` accepts one or more pinned public keys or
certificates, requires exactly one signer to match, and emits its key id, the
verified tier, and statement/record fingerprints for the enrollment row. The
selected manufacturer authority is explicit in its output.

```sh
mfgattest verify -manufacturer-authority example-manufacturing \
  -key manufacturer-public.pem -record RECORD_HEX -product 1 \
  -atecc-serial 0123456789abcdef11 \
  -board-eui 0004a3aabbccddee -board-rev 1
```

`fixture-sign` exists only for deterministic development fixtures. It requires
the explicit `-development-fixture` acknowledgement and a mode-0600 key; there
is deliberately no generic `sign` command.

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
mfgattest commission-verify -manufacturer-authority example-manufacturing \
  -key manufacturer-public.pem -record RECORD_HEX
```

`registry-verify` checks a signed registry file against the pinned registry keys and prints
its sequence, issue time and board count.

```sh
mfgattest registry-verify -manufacturer-authority example-manufacturing \
  -key registry-public.pem -file registry.json
```

`-key` may be given more than once in all three verification commands. Verifiers pin sets of keys: a replaced
signer adds a key, and what the earlier key signed stays valid for as long as it stays pinned.
The manufacturer keys and the registry keys are separate sets, and neither command accepts
the other's.

A verified record shows that the manufacturer commissioned that board with that
microcontroller under that profile. It does not show that a given connection comes from that
microcontroller: that is the per-session proof a collector checks during the GNF1 handshake,
and it cannot be checked offline. Both commands print nothing and exit non-zero when
verification fails.
