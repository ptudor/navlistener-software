# `gnss/testdata` — external truth fixtures

These fixtures compare five broadcast ephemerides—GPS, Galileo, BeiDou,
QZSS, and GLONASS—with independent precise-orbit positions. They check the
selected records and epochs within the tolerances below. They do not prove
correctness for other inputs, signal families, or constellations.

---

## Index — what's in this folder

| File | Format | What it is |
|---|---|---|
| `BRDC00WRD_R_20240100000_truth.rnx` | RINEX 3.05 mixed navigation | Five real broadcast ephemeris records — one each for GPS, Galileo, BeiDou, QZSS, GLONASS — trimmed verbatim from a BKG merged BRDC file. **The input.** |
| `ESA0MGNFIN_20240100000_truth.sp3` | SP3-d | The ESA/ESOC MGEX final precise orbit for the same five satellites at two epochs. **The answer.** |
| `README.md` | — | This file. |

Consumed by `gnss/truth_test.go` (package `gnss_test`), which reads both, propagates the
broadcast records with this library, and requires agreement with the precise orbit.

---

## Summary

There are two kinds of test in this module. Property tests and analytic tests check that the code
is *self-consistent* — the orbit has a sane radius, the integrator reverses cleanly, geodetic
conversion round-trips, nothing goes NaN. Those are valuable and they catch a lot. What they
structurally **cannot** catch is a systematic error that's consistent with itself:

- Cus swapped with Cuc, or Crc with Crs — the orbit still has the right radius.
- Φ used where 2Φ belongs in the harmonic corrections — the orbit is still smooth and periodic.
- A δi sign flip — the orbit still reverses perfectly.
- A GLONASS Coriolis sign flip — the state still integrates and un-integrates to itself.

Every one of those produces a wrong position by hundreds of metres to kilometres while sailing
through every internal check. Independent reference data can reveal errors that self-consistency checks miss. That's what this folder is.

These fixtures implement the independent orbit comparison described in `docs/MATH.md §12`.

---

## Details

### The broadcast fixture — `BRDC00WRD_R_20240100000_truth.rnx`

Trimmed from `BRDC00WRD_R_20240100000_01D_MN.rnx.gz` (BKG Frankfurt, via
`igs.bkg.bund.de/root_ftp/IGS/BRDC/2024/010/`), fetched 2026-07-12. Five records, **verbatim** —
no re-derivation, no reformatting of the numbers:

| Record | Epoch | Why this one |
|---|---|---|
| `G05` | 2024-01-10 12:00:00 | Baseline GPS. |
| `E21` | 2024-01-10 12:10:00 | Galileo I/NAV (ds = 517). Note the different toe — 12:10, not 12:00. |
| `C11` | 2024-01-10 12:00:00 BDT | A BeiDou **MEO**, exercising the standard (non-GEO) path with BDS-specific constants. |
| `J02` | 2024-01-10 09:00:00 | A QZSS **QZO** with e ≈ 0.075 — the highest eccentricity any live constellation broadcasts, so it stresses the Kepler solve hardest. |
| `R09` | 2024-01-10 12:15:00 UTC | GLONASS: a PZ-90 Cartesian state, freq channel −2, for the RK4 path. |

The provenance is recorded in the file's own RINEX `COMMENT` header lines, so it travels with the
data rather than living only here.

**NavIC has no independent fixture in this test set.** It shares the Kepler
propagation code path with GPS-compatible constants (see `gnss/physconst`),
but that does not independently validate its decoder or signal-specific behavior.
The NavIC decoder remains deferred.

**SBAS is absent for a different reason** — the library decodes SBAS L1 message headers but has no
SBAS orbit propagator at all (`physconst.For` returns `ok=false` for it), so there is nothing here
to validate. `loadNavRecords` still recognises an `S` record's 3-continuation-line layout, which is
why the parser notes below mention SBAS.

### The truth fixture — `ESA0MGNFIN_20240100000_truth.sp3`

Trimmed from `ESA0MGNFIN_20240100000_01D_05M_ORB.SP3.gz` (ESA/ESOC MGEX **final** product,
`navigation-office.esa.int/products/gnss-products/2296/`), fetched 2026-07-12. Positions are in
**kilometres, ITRF, satellite centre of mass** — the parser multiplies by 1000.

Two epochs, both GPST:

```
*  2024  1 10  9 30   →  J02
*  2024  1 10 12 30   →  G05, R09, E21, C11
```

### The reference-point caveat — why the tolerances are metres, not centimetres

This is the part worth understanding before anyone "tightens" the tolerances.

Broadcast ephemerides describe the **antenna phase centre**. SP3 describes the **centre of mass**.
Those are up to about 1–3 m apart, mostly radially. Add each constellation's own signal-in-space
error — sub-metre for Galileo, roughly a metre for GPS, a few metres for BeiDou, QZSS, and GLONASS
— and the *honest, expected* disagreement between a correct broadcast propagation and a precise
orbit is single-digit metres.

So the per-case tolerances are set to absorb that while staying roughly two orders of magnitude
below many large propagation errors; smaller or unexercised errors can still pass:

| Case | Tolerance | Note |
|---|---|---|
| GPS (G05) | 8 m | tk = +1800 s from toe |
| Galileo (E21) | 8 m | tk = +1200 s |
| BeiDou (C11) | 10 m | target in BDT (−14 s from GPST) |
| QZSS (J02) | 12 m | tk = +1800 s, the high-eccentricity case |
| GLONASS (R09) | 12 m | tk = +882 s, RK4 from a UTC-day-relative tb |

A test that failed at 50 cm would be testing the antenna phase-centre offset, not our math. A test
that passes at 8 m still fails loudly at the hundreds-of-metres scale any of the swap/sign errors
above would produce. `docs/MATH.md §12 item 2` records this reasoning.

### How the test uses them

`gnss/truth_test.go` does its own minimal parsing, deliberately:

- **`rinexFields`** slices fixed-width RINEX 3 float fields (format `4X,4D19.12`) by column.
  `strings.Fields` is **not safe** here — adjacent fields abut with no space when a value is
  negative. Base column is 23 on the SV/epoch line, 4 on continuation lines; `D`/`d` exponents are
  translated to `e`; a blank field parses as 0.
- **`loadNavRecords`** keys each record by its first 23 columns (`"G05 2024 01 10 12 00 00"`) and
  reads 7 continuation lines for the Kepler constellations, 3 for GLONASS/SBAS.
- **`sp3Position`** finds the epoch line by prefix, then the `P<sv>` line, and converts km → m.

Time handling in the test is worth reading once: 2024-01-10 is a Wednesday, so GPS day-of-week is
3; BDT is GPST − 14 s (a constant); GPS − UTC was 18 s in January 2024. The GLONASS case converts
the 12:30:00 GPST SP3 epoch to 12:29:42 UTC and forms tk relative to the UTC day, and the test
asserts tk = 882 s exactly so a silent timing error can't hide inside a passing position check.

### Maintaining these files

- **Do not regenerate the numbers by hand or with this library.** The entire value of the fixture
  is that it came from somewhere else. If a record needs replacing, re-fetch from BKG and ESA/ESOC
  and trim verbatim.
- **Keep the provenance comments.** Both files carry their source URL and fetch date in their own
  header comment syntax. That's what lets someone in 2029 re-derive where these came from.
- **They are intentionally tiny** — a few kilobytes total — because they're committed to a public
  repo. Both formats are open community standards (RINEX-3.05 and SP3-D are catalogued in
  `reference/REFERENCES.md`), and both source products are openly published by their agencies.

---

## Running the tests

From `gnss/`:

```sh
go test .                        # includes TestKeplerTruthVectors and TestGLONASSTruthVector
go test . -run Truth -v          # just these, with the per-case miss distances logged
```

The verbose form logs the actual `|broadcast − precise|` for each case, which is the number to
watch: it should sit in the low single-digit metres. A jump to tens of metres means something
changed in the propagator even if the test still passes, and that's worth chasing before it
becomes a failure.

---

## Sources

- `docs/MATH.md §12` — the validation strategy, and this fixture as "oracle 2."
- **RINEX-3.05** and **SP3-D** — the two formats, both catalogued in `reference/REFERENCES.md` as
  open community standards.
- BKG Frankfurt (broadcast merge) and ESA/ESOC (MGEX final orbits) — the upstream products, both
  openly published.
