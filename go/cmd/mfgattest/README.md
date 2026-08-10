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
