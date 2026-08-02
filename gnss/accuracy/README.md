# `gnss/accuracy` — decoding the broadcast signal-in-space accuracy indices

**Headline:** every constellation broadcasts a small integer meaning "here's how much you should
trust my ranging signal right now." This package turns those indices into metres — and, just as
importantly, tells you when the index is a *sentinel* rather than a value.

---

## Index — what's in this folder

| File | What it is |
|---|---|
| `accuracy.go` | The whole package: `URAMeters`, `URAEDMeters`, `GalileoSISA`, `GlonassFT`. |
| `accuracy_test.go` | Per-constellation known-value checks, the band boundaries, sentinel handling, and a monotonicity property test. |
| `README.md` | This file. |

**No module-internal dependencies** — not even the root `gnss` package; stdlib `math` is the only
import. Pure integer-in, float-out.

---

## Summary

Four decoders, one shape:

```go
func URAMeters(n int) (float64, bool)     // GPS / QZSS / NavIC / BeiDou B1I URA index
func URAEDMeters(n int) (float64, bool)   // GPS/QZSS CNAV elevation-dependent URA_ED (SIGNED)
func GalileoSISA(n int) (float64, bool)   // Galileo SISA index 0–255
func GlonassFT(n int) (float64, bool)     // GLONASS F_T index 0–15
```

Each returns metres and a `valid` flag. These feed the `sisa_m` / `sisa_valid` feed fields and
the SISA-change integrity alert (`docs/OUTPUT.md`, `docs/INTEGRITY.md`).

**The single most important thing in this package is what `valid = false` means.** It is *not*
"nothing to report." Every one of these sentinels is a deliberate broadcast statement — the
satellite is actively telling you it cannot vouch for its own accuracy — and that is a
first-class integrity signal. The caller **must** surface the raw index itself (the feed's
`acc_index`, the detector's `no_accuracy` state) rather than folding the sentinel into absence.
Fold it in, and the one broadcast field whose entire job is to disclaim the SV's accuracy
silently vanishes. That rule is recorded per-function as regression fix (URA) and regression fix (SISA).

---

## Details

### `URAMeters` — GPS, QZSS, NavIC, and BeiDou B1I

IS-GPS-200N §20.3.3.3.1.3's nominal-value formula:

- N ≤ 6 ⇒ X = 2^(1 + N/2)
- N ≥ 7 ⇒ X = 2^(N − 2)

| N | metres | | N | metres |
|---|---|---|---|---|
| 0 | 2.00 | | 8 | 64 |
| 1 | 2.83 | | 9 | 128 |
| 2 | 4.00 | | 10 | 256 |
| 3 | 5.66 | | 11 | 512 |
| 4 | 8.00 | | 12 | 1024 |
| 5 | 11.31 | | 13 | 2048 |
| 6 | 16.00 | | 14 | 4096 |
| 7 | 32.00 | | **15** | **sentinel** |

**Index 15 is the sentinel.** Per the same ICD section it "shall indicate the absence of an
accuracy prediction and shall advise the standard positioning service user to use that SV at his
own risk." Returns `valid = false`.

**The NavIC claim is not a GPS-shape assumption**. The IRNSS SPS ICD independently
specifies the identical formula and the identical N = 15 sentinel
(NAVIC-SPS-L5S §6.2.1.4, Table 23) — it was checked against the document, not inferred from
family resemblance.

**Rounding is deliberately not applied**. Both ICDs print advice that N = 1/3/5 "should
be rounded to 2.8, 5.7, and 11.3 meters." That advice is advisory, and the value served here is
the exact nominal 2^(1+N/2). So a consumer comparing against the printed tables must expect
2.828 / 5.657 / 11.313, not 2.8 / 5.7 / 11.3. Same decision for `URAEDMeters`. Written down so no
future pass re-litigates it.

### `URAEDMeters` — the CNAV elevation-dependent index, and its signedness

CNAV's URA_ED is a **signed two's-complement integer in the range +15 to −16**
(IS-GPS-200N §30.3.3.1.1.4). The formula extends the LNAV shape below zero:

- −16 < N ≤ 6 ⇒ 2^(1 + N/2) — so N = −1 gives ≈1.41 m, N = −4 gives 0.5 m
- 6 ≤ N < 15 ⇒ 2^(N − 2)

**Both N = 15 and N = −16 are "no accuracy prediction" sentinels** and return `valid = false`.

The signedness is not a footnote. Modern SVs routinely broadcast negative URA_ED — index 0's
tabulated band is 1.70 < URA_ED ≤ 2.40 m, so any better prediction indexes below zero — and
reading the field as unsigned mis-decodes *half the domain* (16 of the 32 values): bits `11111`
read as 31 instead of −1. That was a real bug in the frame decoder, closed as regression fix; this
package's contract is the other half of the fix.

**On the citation** (regression fix, re-verified and deliberately unchanged): a review pass proposed
re-attributing this formula to LNAV §20.3.3.3.1.3 on the premise that §30.3.3.1.1.4 "prints no
formula." It does print it, immediately below its band table — including the signed guard "but
more than -16," which is that section's own wording. Re-pointing the citation at LNAV would be a
citation regression. What regression fix *did* correctly observe: the nominal values don't all sit
inside their tabulated bands — N = −15 nominally yields 2^−6.5 ≈ 0.011 m against that row's
"≤ 0.01" ceiling. That's an ICD-internal rounding artefact at the most-accurate index,
sub-centimetre, and no consumer of `sisa_m` distinguishes 0.010 from 0.011 m. The nominal formula
— which the ICD instructs users to apply — stays authoritative.

### `GalileoSISA` — four linear bands

GAL-OS-SIS-ICD-2.2 §5.1.12 Table 91:

| Index | Range | Step |
|---|---|---|
| 0–49 | 0 – 0.49 m | 1 cm |
| 50–74 | 0.5 – 0.98 m | 2 cm |
| 75–99 | 1 – 1.96 m | 4 cm |
| 100–125 | 2 – 6 m | 16 cm |
| 126–254 | spare | — |
| **255** | **NAPA** — No Accuracy Prediction Available | — |

Both the spare range and NAPA return `valid = false`. And §5.1.12 is explicit that SISA = NAPA
"is an indicator of a potential anomalous SIS" — which is precisely why the raw index has to be
surfaced. A NAPA broadcast is an event; "no SISA decoded yet" is a non-event; if both collapse to
the same absence, the monitor cannot tell them apart.

### `GlonassFT` — a table, not a formula

GLONASS ICD Ed. 5.1's F_T accuracy table (GLO-ICD-5.1 **Table 4.4**, "Word F_T"), in metres:

```
index:  0   1   2    3  4  5   6   7   8   9  10  11   12   13   14   15
value:  1   2   2.5  4  5  7  10  12  14  16  32  64  128  256  512   —
```

Index 15 is "not used" and returns `valid = false`, as does any out-of-range input.

---

## Tests

| Test | What it pins |
|---|---|
| `TestURAMeters` | Known values across both formula branches, plus the N = 15 sentinel. |
| `TestURAEDMeters` | The signed domain including negative indices, plus both sentinels (15 and −16). |
| `TestGalileoSISABands` | All four band boundaries — the off-by-one-prone spots — plus the spare range and NAPA. |
| `TestGlonassFT` | Table values and the index-15 "not used" case. |
| `TestURAMonotonic` | A property test over `URAMeters` N = 0–14: decoded accuracy is non-decreasing in the index, so the two formula branches cannot invert at their N = 6/7 join. `URAMeters` only — the literal `glonassFT` table is unswept. |

Run with `go test ./accuracy/` from `gnss/`.

---

## Sources

`docs/MATH.md §6`. Primary ICDs:

- **IS-GPS-200N §20.3.3.3.1.3** (LNAV URA) and **§30.3.3.1.1.4** (CNAV URA_ED, signed).
- **GAL-OS-SIS-ICD-2.2 §5.1.12, Table 91** (SISA bands and NAPA).
- **GLO-ICD-5.1 §4.4, Table 4.4** (the F_T word).
- **NAVIC-SPS-L5S §6.2.1.4, Table 23** (NavIC's independent statement of the URA formula).

See `reference/REFERENCES.md`.
