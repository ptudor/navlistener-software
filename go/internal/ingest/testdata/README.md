# `internal/ingest/testdata` — real captured UBX streams

**Headline:** three raw byte captures from live u-blox receivers. They are the only test inputs
in the daemon that no one in this repo authored — real broadcasts, with real timing, real gaps,
and real receiver quirks. Synthetic frames prove a decoder is self-consistent; these prove it
matches what a satellite actually transmits.

---

## Index — what's in this folder

| File | Receiver | Size | What it's for |
|---|---|---|---|
| `f9t_capture.ubx` | u-blox ZED-F9T (timing receiver) | ~204 KB | The workhorse. ~90 s of live UBX. GPS LNAV, Galileo I/NAV and F/NAV, BeiDou D1 and B-CNAV2. Also the payload for the C↔Go feeder end-to-end tests. |
| `f9p_capture.ubx` | u-blox ZED-F9P | ~383 KB | Carries **both** GPS L1 C/A LNAV **and** L2C CNAV for the same SVs, which is what makes the CNAV-vs-LNAV agreement test possible. Also the GLONASS almanac source. |
| `glo_superframe_capture.ubx` | u-blox | ~585 KB | Long enough to contain a complete GLONASS superframe — the 2.5-minute cycle needed to assemble almanac string pairs and the frame `NA` day number. |
| `README.md` | — | — | This file. |

These are raw UBX byte streams: `0xB5 0x62` sync, class/id, little-endian length, payload, and a
two-byte 8-bit Fletcher checksum. They are fed to `scanUBX` exactly as a live TCP stream would be.

---

## Summary

The tests that use these fixtures live in `../realframes_test.go`, `../glonass_almanac_test.go`,
and the `feeder_*_e2e_test.go` files. They fall into three groups.

### 1. "Does it decode at all, and does the result make physical sense?"

`TestRealF9TCapture` runs the capture through the real UBX scanner and the real GPS LNAV decoder,
and requires every assembled ephemeris to propagate to a GPS-shell radius. **This is the test that
caught the u-blox parity/inversion convention** — that the receiver pre-validates SFRBX words and
resolves the D30* inversion before delivering them, which is why `gnss/frame` extracts LNAV data
bits directly instead of re-running the broadcast parity. That behavior is not something you can
discover from an ICD; you discover it from a capture that refuses to decode until you stop
re-checking parity.

### 2. Cross-signal agreement — the highest-value tests here

The same satellite, decoded twice through independent code paths, must produce the same position:

| Test | Compares | Catches |
|---|---|---|
| `TestRealGPSCNAVAgreesWithLNAV` | GPS L2C CNAV vs L1 C/A LNAV (F9P) | A wrong CNAV field offset or a ΔA-vs-√A parameterization error. Agreement to within a few metres. |
| `TestRealBeiDouD1AgreesWithBCNAV2` | BeiDou B1I D1 vs B2a B-CNAV2 | A wrong D1 offset — and D1's field order is genuinely surprising (IDOT sits between Cis and Ω0). |
| `TestRealGalileoFNAVAgreesWithINAV` | Galileo E5a F/NAV vs E1-B I/NAV | Every F/NAV offset, which was originally pinned *by* this comparison. |

This is the pattern worth understanding: a decoder bug that produces a plausible-looking number
survives every unit test written against the same (mistaken) reading of the ICD. It does not
survive being checked against a second, independently-specified encoding of the identical
broadcast ephemeris.

### 3. Integrity must-not-regress

`TestRealGalileoINAVIntegrityAllCaptures` is guard: **every** I/NAV page across **all
three** captures must pass the decoder's in-frame CRC check. The ephemeris tests prove that
useful pages still assemble, but they would silently skip individual CRC failures — this one
won't. It reads all three fixtures with `t.Fatalf` on a missing file rather than skipping,
because the whole point is total coverage.

`TestRealGalileoFNAVIntegrityAllCaptures` is the F/NAV twin of the same guard, and
`TestRealGalileoGSTAgreesWithGPS` cross-checks the decoded GST week/TOW against the GPS axis.
`TestRealSBAS`, `TestRealGLONASS`, `TestRealBeiDouD1`, `TestRealBeiDouBCNAV2`, and
`TestRealGalileoINAV` each cover their own constellation's decode-and-assemble path against these
same bytes.

---

## Details

### Why the fixtures are committed rather than generated

Three reasons, in order of importance:

1. **They encode receiver behavior no document states.** The parity/inversion convention above is
   the clearest example. A synthetic frame builder implements what we *believe* the receiver
   does; a capture shows what it *does*.
2. **They make the cross-signal tests possible at all.** You cannot synthesize "the same
   ephemeris, independently encoded on two signals" without already assuming both encodings are
   right — which is precisely the thing under test.
3. **They are the end-to-end payload.** The C feeder tests replay `f9t_capture.ubx` through the
   real feeder binary over a real TLS socket into this collector. Nothing else exercises the
   handshake, sequencing, and ack-driven spool pruning against a non-Go implementation.

### On skip-vs-fail

Most fixture tests `t.Skipf` when the file is missing, so `go test ./...` still runs on a tree
where a fixture wasn't fetched. The all-captures CRC integrity test deliberately does **not** —
it `t.Fatalf`s, because "the CRC regression guard silently didn't run" is the failure mode it
exists to prevent.

### A note on attribution vs reproducibility

A separate 2026-07 live dev capture verified 3737/3737 BeiDou B-CNAV2 frames passing CRC-24Q.
**That capture is not committed**, so the figure is attributed rather than reproducible from this
tree. What *is* committed and reproducible is the 270-frame regression in
`f9t_capture.ubx` — 270/270 decode clean, exercised by `TestRealBeiDouBCNAV2`. When you see a
frame count cited in a decoder comment, check which of the two it refers to.

### Maintaining these files

- **Don't regenerate them with this codebase.** Their value is that they came from silicon.
- **Don't trim them without checking what breaks.** The GLONASS superframe capture is long
  specifically because the almanac cycle is 2.5 minutes; shortening it would silently drop
  almanac coverage.
- **New captures should record their provenance** — receiver model, firmware, duration, antenna
  location class, date — in the test that consumes them, the way the existing ones do.
- They are byte streams of public broadcast signals: no credentials, no station coordinates, no
  network topology. Safe for a public repo.

---

## Running

```sh
go test ./internal/ingest/ -run Real -v     # the real-frame suite, with per-test counts logged
make check-e2e                              # from go/ — the C feeder replay tests
```

The `-v` output logs how many SVs assembled per constellation, which is the number to watch. Those
counts sit above the tests' floors (the Galileo I/NAV case asserts only `>= 4`), so a real
regression can shrink the count substantially while the test still passes. Compare against the
previous run's log, not against the assertion.

---

## See also

- `../README.md` — the ingest package.
- `../../../../gnss/frame/README.md` — the decoders these captures exercise.
- `../../../../gnss/testdata/README.md` — the *other* external fixtures, which validate the
  propagators against a precise orbit rather than the decoders against real bits.
