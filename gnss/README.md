# `gnss` — the clean-room GNSS math and decode library

**Headline:** a self-contained, I/O-free, Apache-2.0 Go module that turns raw broadcast
navigation frames and a timestamp into satellite positions, clock corrections, and geometry —
for supported GPS, GLONASS, Galileo, BeiDou, and QZSS signals. It also decodes SBAS L1
headers and the MT0 health indication; NavIC decoding remains deferred. Every line of math in it is
authored from the public Interface Control Documents. It provides the numerical core of `navlistener` and
it is deliberately its own module, so anything can link it.

---

## Index — what's in this folder

### Packages

| Package | Responsibility | Depends on |
|---|---|---|
| **`.` (root)** | `ECEF` and `GNSSID` — the two value types shared by everything else. | — |
| **`accuracy/`** | URA / URA_ED / SISA / F_T index → metres, and the "no prediction" sentinels. | — |
| **`clock/`** | SV clock polynomial, relativistic term, group delay, broadcast GNSS→UTC. | root, gnsstime, kepler, physconst |
| **`frame/`** | Raw-frame bit decoders for every broadcast signal we decode. **The untrusted-input boundary.** | root, clock, glonass, gnsstime, kepler, physconst |
| **`geo/`** | ECEF ↔ geodetic, topocentric azimuth/elevation. | root, physconst |
| **`glonass/`** | PZ-90 RK4 numerical propagation + the analytic almanac model. | root, physconst |
| **`gnsstime/`** | Time systems, week/TOW arithmetic, the half-week wrap, rollover disambiguation. | — |
| **`iono/`** | Klobuchar and the coefficient carriers; dual-frequency geometry-free measurement. | — |
| **`kepler/`** | The generic Keplerian ECEF propagator + BeiDou GEO + velocity + Doppler. | root, gnsstime, physconst |
| **`physconst/`** | Per-constellation physical constants and reference ellipsoids. | root |
| **`testdata/`** | External truth fixtures — five real broadcast ephemerides (GPS, Galileo, BeiDou, QZSS, GLONASS) plus the precise orbit. | — |

Every package has its own README with the full detail; this one is the map.

### Files in this directory

| File | What it is |
|---|---|
| `gnss.go` | The root package: `ECEF` (with `Add`/`Sub`/`Scale`/`Dot`/`Norm`) and `GNSSID` (with `Letter`/`String`/`Valid`). |
| `gnss_test.go` | Vector arithmetic and the constellation-id mapping. |
| `truth_test.go` | The external validation suite — `TestKeplerTruthVectors` and `TestGLONASSTruthVector`. See `testdata/README.md`. |
| `go.mod` | `module github.com/ptudor/gnss`, Go 1.24. **No third-party dependencies.** |
| `LICENSE` | Apache License 2.0. |

---

## Summary

### What it does

Give it bits and a time; get back physics.

```go
import (
    "github.com/ptudor/gnss"
    "github.com/ptudor/gnss/clock"
    "github.com/ptudor/gnss/frame"
    "github.com/ptudor/gnss/geo"
    "github.com/ptudor/gnss/kepler"
    "github.com/ptudor/gnss/physconst"
)

// 1. Decode raw broadcast subframes (untrusted input — every call can error).
sf1, err := frame.DecodeGPSLNAV(words1)
sf2, err := frame.DecodeGPSLNAV(words2)
sf3, err := frame.DecodeGPSLNAV(words3)

// 2. Assemble a coherent data set (IODE/IODC must agree — see frame/README.md).
eph, clk, err := frame.AssembleGPS(gnss.GPS, 5, sf1, sf2, sf3)

// 3. Propagate. Solve returns the eccentric anomaly so the clock can reuse it.
sol, err := kepler.Solve(eph, tow)

// 4. Clock correction at the same epoch.
dtsv := clock.Offset(clk, tow, eph.Ecc, eph.SqrtA, sol.E)

// 5. Geometry as seen from a ground station.
az, el := geo.AzEl(sol.Pos, receiver, physconst.WGS84)
```

### The contract

Four promises, enforced package-wide:

1. **No I/O.** No files, no network, no logging, no globals, no `init()` side effects. This is
   pure computation, which is what makes it testable, fuzzable, deterministic, and linkable from
   anywhere.
2. **Errors, never NaN.** Degenerate input returns a typed error. A NaN doesn't crash anything —
   it silently propagates into a feed, freezes a map marker, and poisons an integrity threshold
   three layers away with no stack trace. Every propagation checks finiteness before returning.
3. **Never panic on untrusted input.** `frame` is fuzzed for exactly this. A malformed frame is an
   error value, not a crash.
4. **No third-party dependencies.** The `go.mod` has no `require` block. A vendored math library
   is a supply-chain surface and a provenance question we don't need.

### Why it's a separate module

`gnss/` can be used independently of the `navlistener` daemon. It provides
ICD-cited decoding, propagation, clocks, and geometry without collector, database,
or network dependencies.

The repo's `go.work` joins `./gnss` and `./go` for local development, so a change here is
immediately visible to the daemon without a version bump.

---

## Details

### The root package

Two types, and they're deliberately tiny.

**`ECEF`** — an Earth-Centered, Earth-Fixed position or vector in metres, with `Sub`, `Add`,
`Scale`, `Dot`, and `Norm`. The **datum is carried by the caller**, not embedded in the type. The
WGS-84 / PZ-90.11 / CGCS2000 differences are centimetre-level (`docs/MATH.md §5.1`) and only
matter where a common frame is required — but every function that needs one takes it explicitly
rather than assuming.

**`GNSSID`** — the constellation identifier, in the one numbering used everywhere.

### The gnssId numbering — read this before touching ids

**One numbering, everywhere: the u-blox `gnssId` order.**

| id | Constellation | RINEX letter |
|---|---|---|
| 0 | GPS | `G` |
| 1 | SBAS | `S` |
| 2 | Galileo | `E` |
| 3 | BeiDou | `C` |
| 4 | IMES | — (never emitted) |
| 5 | QZSS | `J` |
| 6 | GLONASS | `R` |
| 7 | NavIC | `I` |

This is what UBX-RXM-SFRBX carries, so ingest needs no remap and the feeds emit the same ids
(`docs/CONSTELLATIONS.md §0`, `docs/OUTPUT.md §0`).

- `Letter()` returns the RINEX 3.x / IGS letter, or `'?'` for IMES and unknown ids.
- `String()` returns the lowercase name used for metric labels and per-constellation feed counts.
- `Valid()` is true for every well-formed constellation id — everything except IMES (never
  emitted) and out-of-range values. It is id-plausibility, not decode support: NavIC is `Valid`
  so its frames pass ingest and get counted (`navic_deferred`) even though its decoder is a
  deferred stub.

**SV keys in the feeds are `name@sigid`** — `E14@0`, `J03@0`, `E14@3` for the same Galileo SV on
E5a. Consumers key on the **numeric id** and never parse the letter.

**Known consumer bug to fix before NavIC ships** (`docs/OUTPUT.md §6.1`): mapintsat's `Gnss.cs`
and `GNSS.swift` mislabel `4 = NavIC, 7 = KASS`. KASS is an SBAS *provider*, not a constellation.

### The dependency layering

```
                    frame  ────────────────┐  (the untrusted-input boundary)
                      │                    │
        ┌─────────────┼──────────┬─────────┤
        ▼             ▼          ▼         ▼
      clock        kepler     glonass   physconst
        │             │          │         │
        └──────┬──────┘          │         │
               ▼                 │         │
           gnsstime              │         │
                                 ▼         ▼
                              physconst   root (gnss)

     geo → root, physconst        accuracy → (nothing)      iono → (nothing)
```

Three packages have **zero dependencies** — `gnsstime`, `accuracy`, and `iono`. `physconst`
depends only on the root package. `frame` sits at the top and imports nearly everything, which is
correct: it's the layer that turns bytes into the types everyone else consumes. There are no
cycles and no back-edges from a lower layer to a higher one.

### Constellation coverage

| Constellation | Signals decoded | Propagation | Notes |
|---|---|---|---|
| **GPS** | L1 C/A LNAV, L2C·L5 CNAV | Kepler | Full clock, TGD, ISCs, iono, UTC. |
| **QZSS** | L1 C/A LNAV, L2C·L5 CNAV | Kepler | Reuses the GPS decoders verbatim — QZSS's signal structure is identical for these. Different A_REF for CNAV; a few redefined fields at the same bit positions. |
| **Galileo** | E1-B I/NAV, E5a F/NAV | Kepler | Both signals decoded independently, which enables the cross-signal integrity check. OSNMA field captured (presence only in v1). GGTO decoded. |
| **BeiDou** | B1I D1 NAV, B2a B-CNAV2 | Kepler + GEO branch | Cross-validated B1I against B2a on real frames. BDT-UTC and BDGIM coefficients decoded. |
| **GLONASS** | L1OF/L2OF strings + almanac | RK4 (PZ-90) + analytic almanac | Cartesian state, not Kepler. Hamming-checked. |
| **SBAS** | L1 C/A headers | — | Message type per GEO and the MT 0 "do not use" flag; correction bodies are augmentation payloads, not decoded. |
| **NavIC** | — | Kepler constants present | **Deferred stub.** From-scratch signal format, and no receiver in the fleet can see NavIC to validate against. See `frame/navic.go`. |

Per-signal detail and the "decoded but not yet served" fields are in `frame/README.md` and
`docs/CONSTELLATIONS.md`.

### Clean-room provenance — the rule that governs this module

This library is developed from the public GNSS Interface Control Documents.
The following rules preserve traceable authorship and reproducible validation:

- **Every line of GNSS math here is authored from the public ICDs** — IS-GPS-200, Galileo
  OS-SIS-ICD, BDS-SIS-ICD, GLONASS ICD, IS-QZSS, IRNSS SPS ICD. No galmon code is copied,
  pasted, or transliterated.
- **Do not open `third_party/galmon` while writing code in this module.** The code lineage is
  ICD-only, and that lineage is the product.
- **Cite the ICD section in the code comment** for any non-obvious constant, bitfield layout, sign
  convention, unit, or rollover — by cite-key and section, e.g.
  `// GPS week number is 13-bit in CNAV, not the 10-bit LNAV field — see IS-GPS-200N §30.3.3.1.1.1`.
- **Never invent a spec value from memory.** If a constant can't be traced to a citation in
  `reference/REFERENCES.md`, it gets a TODO and a conversation, not a guess. This is the exact
  failure an early review surfaced, and it's why the reference library exists.

galmon remains a legitimate **differential-test oracle** — running both over the same raw frames
and comparing numbers cross-validates both implementations. That's reading a reference
implementation's *results*, which is standard practice and keeps provenance clean. Any actual GPL
binary reuse is quarantined out-of-process in `galmon-bridge` and never links this module.

### Comments are load-bearing here

This is a hard domain, and the in-code comments carry decisions that would otherwise be
re-litigated every few weeks. You will find long comments explaining why a line that looks wrong
is right — the BeiDou GEO node formula that *doesn't* use (Ω̇ − ωe)·tk, GLONASS's positive-J₂ sign,
the ICD's mandated π rather than `math.Pi`, `URA_ED`'s signedness, the deliberate non-checks
(t0G, rounding). Several also record what was *considered and declined*
(Hamming correction) or *checked and left alone* (citation, non-existent sentinel).

**Don't strip those during a refactor.** If a line looks weird but is correct, the fix is a
comment, not a rewrite.

---

## Testing

```sh
cd gnss
go test ./...                                    # everything
go test .                                        # the external truth vectors
go test ./frame/ -run Fuzz -fuzz FuzzDecodeGPSCNAV
```

Three layers, and they catch different things:

1. **Analytic and property tests, per package.** Closed-form base cases, radius windows,
   reversibility, round-trips, determinism, and a full degenerate-input matrix asserting errors
   rather than NaN. Fast, and they catch most mistakes.
2. **Fuzzing, in `frame`.** Fourteen targets, one per exported decoder plus the primitives. The
   contract: arbitrary input never panics, malformed input errors, a nil result never accompanies
   a nil error. Fuzzing exercises unchecked lengths and out-of-bounds reads.
3. **External truth vectors, in `truth_test.go`.** Real broadcast ephemerides propagated against
   the ESA/ESOC precise orbit. This is the layer that catches self-consistent errors — a swapped
   harmonic coefficient, a Φ-vs-2Φ mistake, a sign flip — which layers 1 and 2 structurally
   cannot. See `testdata/README.md` for why the tolerances are metres and not centimetres.

**Real-frame regression lives in the daemon**, not here, because it needs captured UBX:
`go/internal/ingest/realframes_test.go` runs committed live u-blox captures through these decoders
— `f9t_capture.ubx` alone carries 1464 SFRBX frames, of which the 270 BeiDou B-CNAV2 ones decode
clean (that 270 is the B-CNAV2 subset, not the fixture's size — the same figure is cited in
`frame/beidou_bcnav2.go`). Three of those tests are cross-signal validations that would catch a
wrong-but-plausible offset: `TestRealBeiDouD1AgreesWithBCNAV2` (B1I vs B2a),
`TestRealGalileoFNAVAgreesWithINAV` (E5a vs E1-B), and `TestRealGPSCNAVAgreesWithLNAV` (L2C vs
L1 C/A).

---

## License

**Apache-2.0** (see `LICENSE`). Permissive, with an explicit patent grant — which matters for the
OSNMA and crypto-adjacent work
(H3, Plus Codes, geohash). It's what lets the Swift/Kotlin/.NET clients and `tudorgps` link this
directly.

The vendored ICDs under `reference/icd/` have their own, quite varied terms; several are
local-only and must not be redistributed. `reference/REFERENCES.md` records the terms per
document, with SHA-256 pins.

---

## Where to read next

| If you want… | Read |
|---|---|
| Every equation, with its ICD citation and constants | `docs/MATH.md` |
| Per-constellation frame layouts and the coverage matrix | `docs/CONSTELLATIONS.md` |
| How the daemon uses all this | `docs/DESIGN.md` |
| What the integrity monitor computes from it | `docs/INTEGRITY.md` |
| The served feed shapes and enums | `docs/OUTPUT.md` |
| The ICD library itself, with terms and hashes | `reference/REFERENCES.md` |
| A specific package | that package's own `README.md` |
