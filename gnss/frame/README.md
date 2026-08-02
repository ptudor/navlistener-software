# `gnss/frame` — raw broadcast navigation-frame decoders

**Headline:** this is the package that touches untrusted bytes. Everything else in `gnss` operates
on typed, already-validated structs; `frame` is where a stream of bits that arrived over the air
(or over the network, from a feeder we don't physically control) becomes those structs. Every
length is bounds-checked, every broadcast integrity check is verified rather than assumed, and
every decoder is fuzzed. It starts hardened, on purpose.

---

## Index — what's in this folder

### Shared primitives

| File | What it is |
|---|---|
| `bitreader.go` | `BitReader` — MSB-first, absolute-bit-offset, fully bounds-checked field extraction. `Bits`, `Signed`, `SignMag`, `Concat`, `ConcatSigned`. |
| `crc.go` | CRC-24Q (polynomial 0x1864CFB) in byte-aligned and arbitrary-bit-range forms. Used by GPS/QZSS CNAV, Galileo, BeiDou B-CNAV, NavIC, SBAS, RTCM3. |
| `parity.go` | GPS/QZSS LNAV Hamming parity (IS-GPS-200N §20.3.5), including the D30* data-complement rule. |
| `sow.go` | `sowDelta` — the domain-checked week-ring difference used by every broadcast-adjacency rule. |

### Per-constellation decoders

| File | Signal | Delivery | Integrity check |
|---|---|---|---|
| `gps_lnav.go` | GPS L1 C/A + QZSS L1 C/A LNAV | 10 × 30-bit words | TLM preamble, TOW range, subframe id (parity pre-validated by the receiver) |
| `gps_cnav.go` | GPS/QZSS L2C·L5 CNAV | 10 × 32-bit words (300 bits) | preamble + CRC-24Q over all 300 bits |
| `galileo_inav.go` | Galileo E1-B I/NAV | 8 × 32-bit words (256-bit page) | page-type flags + CRC-24Q over the reconstructed 196 protected bits + their CRC |
| `galileo_fnav.go` | Galileo E5a F/NAV | 8 × 32-bit words | CRC-24Q over 238 bits |
| `beidou_d1.go` | BeiDou B1I D1 NAV (MEO/IGSO) | 10 × 30-bit words | BCH(15,11,1) on every code block |
| `beidou_bcnav2.go` | BeiDou B2a B-CNAV2 | 9 × 32-bit words (288 bits) | CRC-24Q |
| `glonass_string.go` | GLONASS L1OF/L2OF strings + almanac | 4 × 32-bit words (85-bit string) | ICD §4.7 Hamming (detect-only) + field range gates |
| `sbas_l1.go` | SBAS L1 C/A | 8 × 32-bit words (250 bits) | CRC-24Q over 250 bits |
| `navic.go` | NavIC (IRNSS) SPS | — | **deferred stub** — see below |

### Tests

`primitives_test.go`, `bitreader_test.go`, `bitreader_signed_test.go`, `crc_test.go`,
`gps_lnav_test.go`, `gps_cnav_test.go`, `galileo_inav_test.go`, `beidou_d1_test.go`,
`beidou_bcnav2_test.go`, `glonass_string_test.go`, `glonass_almanac_guard_test.go`,
`sbas_l1_test.go`, `navic_test.go`, and `fuzz_test.go` — fourteen fuzz targets covering every
exported decoder except the constant-error NavIC stub (which parses nothing), plus the
primitives.

Imports: the root `gnss` package, `clock`, `kepler`, `glonass`, `physconst`, and `gnsstime`. It is
the top of the module's dependency graph — everything else imports *into* here.

---

## Summary

The job is narrow and the threat model is not. A navigation frame arrives as a handful of 32-bit
words from a receiver, or from a remote feeder over GNF1, or from a federation peer. It may be
truncated, corrupted, mis-tagged, replayed, or deliberately fabricated. The decoders turn it into
a `kepler.Ephemeris` + `clock.Model` (or a `glonass.Ephemeris`), or they return an error. They
never panic, never read out of bounds, and never pair a nil result with a nil error; a frame that
fails its integrity check is rejected outright rather than partially decoded.

Three principles run through every file:

**1. Verify the broadcast integrity check yourself.** Where the ICD defines a CRC, BCH, or
Hamming code, we compute it. The one documented exception is GPS/QZSS LNAV parity, explained
below — and even there, the exception is narrow, deliberate, and written down rather than assumed
by convenience.

**2. Reject out-of-range structural fields at the boundary.** A length-valid frame carrying a
subframe id of 6, a GLONASS string number of 0, a tb index of 120, or an FDMA channel of +9 is
mis-tagged or corrupt. It gets an error, so the caller counts a decode *error* rather than a
decode *success*, and no durable capability evidence is ever recorded off garbage. (This is the
regression fix idiom, applied in five places.)

**3. Never pair frames that don't belong together.** Each constellation has a different way of
saying "these parts are one data set" — IODE/IODC, IODnav, a shared toe, or nothing at all — and
each assembler enforces its own. Splicing across an ephemeris changeover fabricates an orbit that
belongs to no satellite, which then trips a *critical* integrity alert. Fake alerts are as
damaging as missed ones.

The per-constellation field *meanings* are `docs/MATH.md`; the frame *layouts* and the coverage
matrix are `docs/CONSTELLATIONS.md`. This README is about the code.

---

## Details — the shared primitives

### `BitReader`

```go
func NewBitReader(data []byte) *BitReader
func NewBitReaderN(data []byte, nbits int) *BitReader  // nbits clamped to the slice
func (r *BitReader) Len() int
func (r *BitReader) Bits(start, n int) (uint64, error)         // MSB-first, right-aligned
func (r *BitReader) Signed(start, n int) (int64, error)        // two's complement
func (r *BitReader) SignMag(start, n int) (int64, error)       // top bit = sign
func (r *BitReader) Concat(hiStart, hiN, loStart, loN int) (uint64, error)
func (r *BitReader) ConcatSigned(hiStart, hiN, loStart, loN int) (int64, error)
```

MSB-first bit order, because that's what the ICDs specify. Reads are by **absolute bit offset**,
because ICD field maps are naturally expressed that way and translating to a cursor model is just
an extra place to make an off-by-one.

`SignMag` exists because several GLONASS and BeiDou fields are sign-magnitude rather than two's
complement. Decoding a sign-magnitude field as two's complement produces garbage for exactly half
the domain, and it's the kind of error whose output still looks like a number.

**Three hardening fixes worth knowing about, because each one is a class of bug:**

- **regression fix — integer overflow in the bound check.** `start > r.nbits` is checked *before*
  `start + n` is ever computed, and it short-circuits. Without that, an attacker-supplied `start`
  near `math.MaxInt` overflows the signed addition, wraps negative, slips past the bound check,
  and indexes `r.data` out of bounds. Once `start <= r.nbits` is established, `start + n` cannot
  overflow (nbits is a real frame's bit length and n ≤ 64).
- **regression fix — sign extension at n = 63/64.** Sign-extending via `int64(u) - (1<<n)` overflows at
  n = 63, because `1<<63` is `math.MinInt64` and subtracting it from a positive value wraps. The
  shift form `int64(u<<(64-n)) >> (64-n)` is exact for every 1 ≤ n ≤ 64.
- **regression fix — silent truncation in `Concat`.** Go defines a shift by ≥ the operand width as
  yielding 0, not an error. So `hi << 64` silently becomes 0 and the result is just `lo`, with
  `hiN` bits dropped and no signal. `Concat` now rejects any total width > 64 up front, matching
  `Bits`'s own `n > 64` rejection.

### CRC-24Q

```go
func CRC24Q(data []byte) uint32
func CheckCRC24Q(data []byte) bool
func CRC24QBits(data []byte, bitOffset, bitLen int) uint32
func CheckCRC24QBits(data []byte, bitOffset, bitLen int) bool
```

Polynomial 0x1864CFB, init 0, no reflection, MSB-first. Used by GPS/QZSS CNAV, Galileo
I/NAV·F/NAV·C/NAV, BeiDou B-CNAV, NavIC SPS, SBAS, and RTCM3.

The `Check*` functions use the zero-remainder idiom: run the CRC over the message *including* its
own trailing checksum and the result is zero exactly when the checksum is right.

The bit-range variants exist because several ICDs don't put their CRC coverage on a byte
boundary. GPS/QZSS CNAV's 300-bit message has no byte-aligned split at all; SBAS's is 250
bits; Galileo I/NAV's protected region is non-contiguous in the delivered page and has to
be *reconstructed* before checking. `CRC24QBits` deliberately does **not** repeat
`BitReader`'s bounds checks — it's a low-level primitive called only with format constants, after
the frame length has been checked.

### GPS LNAV parity, and the one place we trust the receiver

```go
func GPSParity(word uint32, d29star, d30star uint32) (data uint32, ok bool)
```

Implements IS-GPS-200N §20.3.5 in full: each 30-bit word carries 24 data bits and 6 parity bits
computed over those data bits plus the previous word's last two parity bits (D29*, D30*), and
**when D30* is set the 24 data bits are transmitted complemented** and must be inverted first.

Here is the honest situation: **`DecodeGPSLNAV` does not call this function.** u-blox
UBX-RXM-SFRBX (and, verified against real ZED-F9T frames, SBF GPSRawCA) delivers LNAV words the
receiver has *already* parity-checked and D30*-normalized. Re-running the broadcast parity on
receiver-normalized words fails, because they aren't the raw broadcast words any more — this is
the same reason RTKLIB trusts u-blox SFRBX. So the decoder extracts the 24 data bits directly and
validates what it *can* validate: the TLM preamble, the HOW TOW count range, and the subframe id.

`GPSParity` is retained, tested, and fuzzed for the future raw-signal path (a software-defined
receiver, or a source that delivers unnormalized words). Related: **`ErrParity` is declared and
exported but no decoder returns it today.** That's deliberate API surface for the same future
path, not a dead branch — noted here so nobody spends an afternoon hunting for its call site.

Note the contrast with every other format: LNAV's "the receiver already checked it" claim is
specific to that format's normalized delivery, and no equivalent guarantee was found for CNAV,
I/NAV, F/NAV, D1, B-CNAV2, GLONASS strings, or SBAS. Those all verify their own checks (regression fix
investigated this rather than assuming it) — which matters doubly because push-path frames arrive
from remote feeders, not just a directly-dialed receiver.

### `sowDelta`

```go
func sowDelta(a, b int) (int, bool)  // unexported
```

Wraps `gnsstime.SOWDelta` with a domain check: both inputs must be in [0, 604800). The decoders
don't range-check raw SOW fields, so an out-of-domain value from a corrupt frame must **fail
here** rather than be normalized into an apparently broadcast-adjacent delta. Every adjacency
rule in the package goes through it.

---

## Details — the decoders

### GPS/QZSS L1 C/A LNAV (`gps_lnav.go`)

QZSS L1 C/A shares this path entirely — IS-QZSS defers to IS-GPS-200 for the format, so gnssId 5
decodes here with PRN = svId + 192.

A subframe is 10 words × 30 bits. The 24 data bits of each word are packed contiguously into a
240-bit buffer, then fields are read at `field(word, bit)` offsets translated straight from the
ICD's 1-indexed word/bit notation.

**Validated at decode:** the 0x8B TLM preamble (`ErrBadTLMPreamble`); the HOW TOW count against
its §20.3.3.2 maximum of 100,799 (`errBadTOWCount`, regression fix — a larger count would scale past the
604,800 s week); and the subframe id ∈ 1..5 (`errBadSubframe`, regression fix).

**Decoded:** subframe 1 (WN, URA index, health, IODC, toc, af0/af1/af2, TGD), subframes 2 and 3
(the ephemeris elements, IODE, toe), and from *every* subframe's HOW the **Alert** and
**AntiSpoof** flags. Alert deserves attention: raised means "the signal URA may be
worse than indicated in subframe 1 and the SPS user shall use that SV at his own risk" — one of
the ICD's three §6.4.6.3 marginal conditions and a first-class integrity input, present in all
five subframes.

Also decoded from subframe 2 word 10 : **FitIntervalFlag** (0 = the nominal 4 h
curve-fit interval, 1 = greater — 6–26 h per the Table 20-XII IODC ranges; QZSS redefines the
values, 0 = 2 h, at the same bit position) and **AODO** (5 bits × 900 s; 27900 means "NMCT
unavailable," which is the value QZSS fixes it at).

Subframes 4 and 5 are almanac and iono pages — structurally valid, accepted, not decoded into
ephemeris fields.

**A documented trap for library users :** `GPSSubframe.TOW` is the HOW's truncated count ×
6, and per the ICD that is the seconds-of-week at the start of the **next** subframe, not this
one. Timing this frame's epoch means using TOW − 6. No internal consumer reads it today; it's
documented so an external one doesn't mis-time frames by 6 s.

**`AssembleGPS(id, svid, sf1, sf2, sf3)`** requires the IODE of subframes 2 and 3 to agree *and*
to equal the low 8 bits of subframe 1's IODC — the LNAV data-set consistency rule
(§20.3.4.4). It also asserts the arguments are actually in slots 1/2/3 (`ErrWrongMsgType`). The
IODE/IODC failure returns a locally-constructed error, not the shared `errIODMismatch` sentinel
the other assemblers use.

### GPS/QZSS L2C·L5 CNAV (`gps_cnav.go`)

Each 300-bit CNAV message arrives as one 10-word SFRBX packed MSB-first across full 32-bit words.
Both the preamble (`ErrBadPreamble`) and CRC-24Q over all 300 bits (`ErrBadCRC`) are verified
here.

Unlike LNAV, CNAV uses the **ΔA parameterization**: A = A_ref + ΔA, plus rate terms Ȧ and Δṅ₀.
A_ref differs by constellation — 26,559,710 m for GPS, 42,164,200 m for QZSS
(IS-QZSS-PNT-005 Table 4.3.2-16). The MT11 reference nodal rate is *not* redefined by IS-QZSS, so
GPS and QZSS intentionally share `cnavOmgD = −2.6e-9 semicircles/s`.

Ephemeris is split across **MT10** (Ephemeris 1) and **MT11** (Ephemeris 2); the clock is in
**MT30–37**.

**Message-10-only integrity fields** : WN (13 bits — note this is *not* LNAV's 10-bit
field), the 3-bit L1/L2/L5 health, and URA_ED. **URA_ED is signed** two's complement, +15..−16
(§30.3.3.1.1.4) — decoding it unsigned mis-reads over half the domain, e.g. `11111` as 31 instead
of −1, and negative URA_ED (URA < 2.4 m) is routine for modern SVs. That was regression fix.

**Message-30-only** : TGD — 13 bits at 2⁻³⁵, **wider and differently scaled than LNAV's
8-bit TGD**, so do not assume they're the same field — plus the four ISCs (L1CA, L2C, L5I5,
L5Q5). None of the ISCs is applied automatically: which one applies is signal-pair-specific
(§30.3.3.3.1.1), so folding one into the generic clock polynomial would be wrong for every signal
that doesn't use it.

Settled and recorded so it isn't re-litigated : **neither IS-GPS-200N nor IS-GPS-705J
defines any "not available" sentinel bit-string for TGD or the ISCs.** The remembered
`1000000000000` (−4096) sentinel does not exist in the in-force editions. The raw two's-complement
decode is correct as-is and performs no sentinel screening.

The **Alert** flag  is ICD bit 38 — the single bit between TOW (ending at 37) and MT10's
WN (starting at 39) — and rides every CNAV message type.

**`AssembleGPSCNAV(id, svid, m10, m11, mClk)`** enforces three things:

- **PRN match**. The header PRN is the SV discriminator; **toe equality deliberately is
  not.** The control segment routinely uploads batches of SVs sharing one toe, so SV A's MT10
  would pair with SV B's MT11 through a toe-only gate and assemble a cross-SV chimera. For GPS
  the 6-bit field is the PRN; for QZSS it carries the 6 LSBs of PRN 193–202, i.e. 1–10 — exactly
  this library's svid convention.
- **Shared toe** between MT10 and MT11.
- **Clock coherence**. §30.3.4.4 says "the toe shall be equal to the toc of the same CNAV
  CEI data set," so the clock attaches only when the cached MT30–37 carries this SV's PRN *and*
  its Toc matches. A returned `clkOK bool` distinguishes "no, wrong-SV, or stale clock" from a
  real one — necessary because a zero `clock.Model` (af0 = 0, Toc = 0) is otherwise
  indistinguishable from a decoded one. A wrong-SV or stale clock is dropped, not an assembly
  error: the ephemeris pair itself is still coherent.

### Galileo E1-B I/NAV (`galileo_inav.go`)

Each I/NAV nominal page arrives as 8 words = 256 bits: the even page part (words 0–3) then the
odd part (words 4–7), each beginning with Even/Odd(1) + PageType(1). The 128-bit nav word is the
even part's 112 data bits followed by the odd part's 16 — and since fields like √A straddle that
join, the content must be made contiguous before reading.

**Two checks before any field is trusted:**

- **regression fix — page-type flags.** Even-part bit 0 must be 0, odd-part bit 0 must be 1, and both Page
  Type bits must be 0 (Nominal). Page Type 1 marks an **alert page**, whose data fields are not a
  nav word at all. Without this, an alert page decodes as a nav word with an arbitrary type 0–63
  — and word type 5 writes health straight into live state, so a single alert page could flip an
  SV's served health. `ErrGalileoAlertPage` is **exported**  so the daemon can count it
  under its own metric label: an alert page is a deliberate, CRC'd transmission mode whose
  content the ICD reserves, and "the constellation is transmitting its attention-worthy page
  type" must be distinguishable from bit-rot.
- **regression fix — CRC-24Q.** The protected message is non-contiguous in the delivered page
  (even flags + 112 data, then odd flags + 16 data + 64 auxiliary/reserved, then the 24-bit CRC),
  so `galileoINAVCRCMessage` reconstructs the 196 protected bits followed by the transmitted CRC
  and the standard zero-remainder check runs over that 220-bit message.

**Word types decoded:** 1–3 (ephemeris elements + SISA), 4 (Cic/Cis + the clock polynomial), 5
(BGDs, health, DVS, GST week/TOW), and 10 (the GST-GPS conversion parameters).

Word 5 carries subtleties worth naming:

- **Bit 67 is E5b health; bit 69 is E1-B health.** Confusing them was regression fix. `E5bSHS` is a
  *separate field*, never folded into `Health`, so the `@0` entry's served health stays E1-B's
  own. A nice consequence: E5b health visibility needs no E5b-I dispatch at all.
- **WN is 12 bits and TOW is 20, both plain integer counts** (Table 69, scale factor 1) — never
  scaled like GPS's ×6 TOW. The 12-bit GST week must never be confused with GPS's 10-bit
  WN; it lives only on this Galileo-specific struct.
- **The clock's TGD is BGD(E1,E5b), not BGD(E1,E5a)**. I/NAV is the (E1,E5b) clock
  (Table 71). Both BGDs are decoded so each clock can pick its own pair.

**Word 10's GGTO**  carries A0G/A1G/t0G/WN0G for Δt = t_Galileo − t_GPS. §5.1.8's
withdrawal sentinel — all four parameters all-ones — is checked on the **raw** patterns before
scaling, because all-ones is a legal −1 for A0G or A1G *alone*; only the four-field conjunction
is the sentinel. Only the GGTO half of word 10 is decoded: the leading almanac fields (and their
health bits at 82/84) describe the almanac *subject* satellite SVID3, not the transmitter, and
folding those into the transmitting SV's state would be exactly the cross-SV chimera the regression fix
family guards against.

**OSNMA** : the 40-bit protocol-data field rides every nominal page's odd part (bits
146..185) regardless of word type, and it *is* CRC-protected so it arrives integrity-checked.
Dummy words (type 63) are excluded — the OSNMA ICD directs that their OSNMA field be discarded —
and alert pages never get here because regression fix rejects them first. Today's consumer uses presence
only (live vs. all-zeros); TESLA/Merkle verification is a documented later phase.

**`AssembleGalileo(svid, w1, w2, w3, w4, w5)`** requires matching IODnav across words 1–4. `w5`
is nil-tolerant and deliberately *not* part of the IODnav-matched set — BGD lives outside the
covered data set — and when present it contributes the clock's TGD.

**A deliberate non-check, recorded so it isn't "fixed"** : t0G is 8 bits × 3600 s, so it
reaches 918,000 s and raw values 168–255 name an epoch beyond the 604,800 s week. That is **not**
rejected or clamped, because Table 76 specifies t0G by bits/scale/unit alone — no range column,
no stated restriction — and §5.1.8 defines exactly one GGTO sentinel. Inventing a tighter bound
would be precisely the "spec value from memory" this repo forbids. The numeric exposure is
negligible: worst case ~0.28 µs on the served `gps_offset_ns`.

### Galileo E5a F/NAV (`galileo_fnav.go`)

The same ephemeris as I/NAV, on a different signal — which is exactly why it's worth decoding:
comparing the two independently-decoded positions is a cross-signal integrity check.

Delivered as 8 words; CRC-24Q covers bits 0–237 (214 protected bits + the 24-bit checksum,
§4.2.2.3). Page 1 is the clock, SISA, E5a health/DVS, GST week/TOW, and BGD(E1,E5a); pages 2/3
are the ephemeris; page 4 is Cic/Cis plus the GST-UTC and GST-GPS blocks.

Every field offset here was pinned by decoding the same SV through the already-validated I/NAV
path and matching raw values bit-for-bit — and `TestRealGalileoFNAVAgreesWithINAV` (in the
daemon's ingest tests) requires the F/NAV position to agree with I/NAV to within 5 m, across at
least three SVs of the capture.

**The group-delay scaling is applied here, and only here**. `clock.Model.TGD`'s contract
is "group delay for the tracked signal, already scaled." The tracked signal on this path is E5a —
the **f2** user of the (E1,E5a) clock pair — so Eq. 19 applies and the assembled clock carries
BGD × `clock.E5aGroupDelayFactor`. (The I/NAV path's tracked E1 is the f1 user, Eq. 18, unscaled.)
The struct's `BGDE1E5a` field keeps the *unscaled* broadcast value for consumers who need the raw
parameter. Before this was fixed the field shipped 0, silently biasing the `@3` clock by the whole
group delay and arming the I/NAV-vs-F/NAV comparison with a built-in false offset.

`ClockTGD()` exposes the already-scaled value so the state layer can do a freshest-wins TGD fold
without re-assembling and without re-deriving the Eq. 19 policy that belongs in this decoder.

**Note the field-order difference from I/NAV:** F/NAV's Table 33 transmits **t0G before A0G**,
where I/NAV's Table 51 does the opposite. Same GGTO semantics, different layout.

Still undecoded and honestly so: the NeQuick ai0/ai1/ai2 coefficients (no NeQuick model exists in
`gnss/iono` yet) and the GST-UTC block (the standing regression fix broadcast-UTC work).

### BeiDou B1I D1 NAV (`beidou_d1.go`)

For MEO/IGSO satellites. Each 300-bit subframe arrives as 10 × 30-bit words with the BCH(15,11,1)
parity in the low bits — four bits in word 1, eight bits (two de-interleaved blocks) in words
2–10. **Every code block is verified** (`ErrBadBCH`) before the 224-bit information stream is
assembled.

FraID must be 1..5 (`errBadFraID`, regression fix). FraID 1 is clock + Klobuchar; 2 and 3 are the
ephemeris halves; 4 and 5 are almanac/integrity pages, structurally valid but not decoded.

Three layout quirks that will trip you up if you assume GPS shapes:

- **Crc/Crs use 2⁻⁶, not GPS's 2⁻⁵.**
- **Subframe 1 broadcasts a2 *before* a0 and a1** (a2@162, a0@173, a1@197), and the **Klobuchar
  α/β set rides subframe 1** (@98–161) rather than subframe 4 as in GPS.
- In subframe 3, **IDOT sits between Cis and Ω0** — the word-split fields are contiguous once
  parity is stripped, which is not the order a GPS reader expects.

`toe` is 17 bits split across subframes 2 (2 MSB) and 3 (15 LSB).

**`AssembleBeiDou(svid, sf1, sf2, sf3)`** and the adjacency rule : D1 carries **no
IOD-style pairing tag at all** — no IODE, no IODC, no IODnav. Broadcast adjacency is the only
valid rule, so sf1/sf2/sf3 must each be exactly 6 s apart within one 30 s D1 frame. Without it, a
stale sf2 (say, from before an hourly changeover after a subframe-2 loss) pairs with a fresh
sf1/sf3 — and since toe is *split across sf2 and sf3*, that splices a toe belonging to neither,
fabricating a garbage ephemeris that then sails through the toe-based IOD gate downstream and
fires a false **critical** orbit-disco event. The delta wraps mod 604800  so the one
legitimate frame per week straddling the BDT rollover (604794 → 0 → 6) still assembles.

### BeiDou B2a B-CNAV2 (`beidou_bcnav2.go`)

The satellite LDPC(96,48)-encodes a 288-bit message to 576 symbols; the u-blox receiver decodes
the LDPC and delivers exactly the 288 information bits as one 9-word SFRBX. CRC-24Q over the
whole block; a failing frame is rejected, never partially decoded.

Like GPS CNAV it uses ΔA with rate terms: A(tk) = A_ref + ΔA + Ȧ·tk, n = n₀ + Δn₀ + ½Δṅ₀·tk.
A_ref depends on the broadcast **SatType** (Table 7-8): 27,906,100 m for MEO, 42,162,200 m for
IGSO/GEO. SatType 0 is reserved and rejected (`errBadSatType`).

Ephemeris splits across **MT10/MT11**; **MT30** carries clock + group delays + the nine BDGIM
ionosphere coefficients; **MT34** carries clock + the BDT-UTC parameter set; **MT40** carries the
midi almanac (not consumed) plus accuracy indices.

- **MT34's BDT-UTC block**  decodes into a `clock.UTCParams` including the A2 drift-rate
  term. ΔtLS/ΔtLSF are the BDT-**UTC** leap counts; BDT itself is leap-free.
- **SISAI indices**  are decoded **raw and deliberately never converted to metres**,
  because ICD v1.0 defines only the bit layout — "the specific definitions … will be published in
  a future update of this ICD," verbatim. There is no index→metres table to transcribe, so
  inventing one would be a fabricated constant.
- **BDGIM α5 carries a negative scale** (−2⁻³) while the others are +2⁻³. That's the ICD's
  Table 7-10, not a typo.

**`AssembleBeiDouBCNAV2(svid, m10, m11, mClk)`**: MT11 carries no IODE, so pairing is validated by
broadcast adjacency (within one 3 s frame, wrapped mod 604800). The clock is treated differently
on purpose : a **stale mClk does not fail the assembly**, it's just dropped — an ephemeris
update must not be blocked forever because this SV's MT30/34 stopped decoding. `clkOK` reports
whether a fresh clock actually attached, and when it's false the caller must not serve the
returned zero model, difference it for a time-disco, or latch its IODC as applied.

**The TGD is TGD_B2ap + ISC_B2ad**, per §7.6.2 eq. 7-5, because the tracked signal is
the B2a **data** component (B-CNAV2 is itself carried on B2a-data; u-blox delivers it as sigId 8).
TGD_B2ap alone is eq. 7-4, the *pilot* component's correction — wrong for this stream by
ISC_B2ad, which is ns-scale, the same order as the 2.5 ns time-disco threshold.

Group delays ride MT30 only. MT34 is a complete clock polynomial with no group-delay field.

### GLONASS strings and almanac (`glonass_string.go`)

Each 85-bit string arrives as 4 words = a 128-bit block. The mapping is **block bit = 85 − (ICD
bit number)** — so an ICD field spanning bits [lo..hi] is read at block offset 85−hi with width
hi−lo+1. Every offset in the file is pre-computed that way, and the arithmetic is shown in the
comment beside each one.

**GLONASS fields are sign-magnitude, not two's complement** — `SignMag`, not `Signed`.

**The §4.7 Hamming check is verified** (regression fix, `ErrGLONASSHamming`) before any field is trusted:
seven check equations C1..C7 plus the whole-string parity CΣ, all of which must be zero.

**It is detection-only, by deliberate decision**. §4.7 rule (b) additionally defines
single-bit-error *correction* from the syndrome, and the machinery here computes everything
correction would need. It was considered and declined: an (8,4)-class check cannot distinguish a
correctable single-bit error from a miscorrectable multi-bit one, and for an integrity monitor a
silently miscorrected string trusted into live state is strictly worse than a dropped one that
re-broadcasts within 30 s (immediate data) or 2.5 min (almanac). The cost is a slightly elevated
reject rate on noisy links, visible per source via the `nav_crc_fail` metric. Implement rule (b)
only if a real feeder shows meaningful loss — and then only with a corrected-vs-rejected split.

Because the Hamming code is 8-bit detect-only rather than a strong CRC, three **range gates** back
it up:

| Gate | Rule | Why |
|---|---|---|
| `errBadStringNum` | string number ∈ 1..15 | regression fix — a mis-tagged block. |
| `errBadTb` | tb index ∈ 1..95 | regression fix — the 7-bit codespace (0..127 ≈ 31.75 h) exceeds a day, but Table 4.5 bounds the effective range to 15…1425 min. An out-of-range tb decodes to a garbage epoch that `EphAgeDay`'s single ±43,200 s wrap then aliases *back into* an in-domain RK4 interval, defeating the regression fix domain guard and silently mis-epoching the served position. |
| `errBadFreqCh` | FDMA channel k ∈ [−7, +6] | regression fix — the post-2005 frequency plan. HnA values 7..24 encode no valid channel at all. Corrupt identity metadata would otherwise ride into FDMA frequency math and federation wire records. |

**Positional field gating :** strings 1–3 share coordinate/velocity/acceleration field
positions, but for strings 4–15 those same bit spans hold entirely different fields. The reads are
gated on m ∈ 1..3, so an external library consumer decoding a string 4 doesn't get
plausible-looking garbage in `Coord`/`Vel`/`Accel` with no error. In-repo callers were already
safe; the exported struct doc promises those are the axis component, so honor it.

**Health is not a boolean, and the raw field over-flags**. Only the MSB of string 2's
3-bit Bn word is the malfunction flag — the ICD says user equipment "does not consider both second
and third bits of this word." So the natural `Health != 0` test flags a healthy SV on a benign low
bit. **Call `Unhealthy()`**, which checks Bn's MSB and the ℓn fast flag together.

**ℓn**  is the GLONASS-M low-latency self-flag, present on strings 3, 5, 7, 9, 11, 13, 15
— 7 of the 15 strings refresh it. It exists precisely to cut the onboard malfunction-to-flag delay
from ≤1 min (Bn) to ≤10 s. On a legacy-GLONASS message the position is reserved; callers should
still treat a set bit as a malfunction, because the conservative failure is a spurious not-ok
worth investigating, never a silent wrong OK.

**Almanac decoding** (`DecodeGLONASSAlmanac`) assembles one satellite from its two-string pair —
first ∈ {6,8,10,12,14}, second must be first+1 — plus the frame's NA day number from string 5
(`DecodeGLONASSFrameNA`). Angles convert from semicircles to radians; the result is a
`glonass.Almanac` ready for the analytic propagator.

**`AssembleGLONASS(slot, freqID, s1, s2, s3, s4)`** asserts the string numbers are really 1/2/3
(regression fix, defense-in-depth against a mis-wired caller silently combining x/y/z from the wrong
strings), validates freqID ∈ 0..13, and takes `s4` nil-tolerantly : when present it must
really be a string 4, and contributes τn/Δτn and `ClockKnown = true`; without it, the ephemeris
assembles clockless, which is fine because position math needs none of those terms (γn rides
string 3 either way).

regression fix also *decoded* two long-ignored words that had a consumer waiting: string 1's **P1** (the
raw 2-bit tb-update-interval flag, the broadcast input to any adaptive validity window over tb)
and string 4's **En** (the SV-declared age of the immediate data in whole days — a large En at a
fresh tb is an upload anomaly).

**Deliberate deferrals, documented rather than forgotten** : tk (string 1 — a tk-vs-tb
plausibility gate), P2, P4, M (satellite type), n (the broadcast slot number, which would enable
svId-vs-n cross-checks), and FT (accuracy, tracked as regression fix). Each is broadcast in a string this
decoder already parses and each awaits a concrete consumer.

### SBAS L1 (`sbas_l1.go`)

250-bit messages delivered as 8 words: an 8-bit preamble (one of 0x53/0x9A/0xC6, cycling), a 6-bit
message type, a 212-bit body, and a 24-bit CRC.

**The CRC is checked**  over the 250 bits — not byte-aligned, hence `CRC24QBits`. That fix
mattered more than it sounds: a single corrupted bit flipping the 6-bit type field to 0 fabricated
a "do not use for safety applications" alarm — the exact event this feed exists to report.
`TestDecodeSBASL1FabricatedDoNotUseNowRejected` is that regression.

Note the preamble is reported as a `PreambleOK` **flag**, not an error — it cycles across three
values and a single message can't tell you where in the cycle it is.

Correction bodies (fast/long/iono) aren't decoded; they're augmentation payloads, and this feed
tracks message type per GEO and flags MT 0.

`SBASProvider(prn)` maps PRN to augmentation system. Two assignments carry explicit warnings
: **PRN 124 was reassigned from EGNOS to SouthPAN in April 2024** — do not move it back —
and PRN 120 is EGNOS's last holder of that code.

### NavIC (`navic.go`) — a deferred stub, on purpose

`DecodeNavICSPS` exists and always returns `ErrNavICDeferred`. It is **not** wired into the
collector's dispatch; every NavIC SFRBX is counted under the `navic_deferred` metric label and
dropped.

This is a tracked deferral, not an oversight, for two reasons stated in the file:

1. **It's a from-scratch signal format.** Unlike QZSS (whose L1 C/A and L2C/L5 reuse the GPS
   decoders verbatim) or Galileo F/NAV (which existed and just needed dispatch), the IRNSS SPS
   L5/S NAV frame has its own subframe layout and word set — real multi-day ICD-citation
   engineering.
2. **We cannot test it.** NavIC's satellites are below the horizon from our stations, so no
   receiver in the fleet produces NavIC SFRBX. Implementing an ICD decoder with no real frames to
   validate against would be untestable guesswork — exactly what the clean-room discipline forbids
   shipping unmarked.

The stub exists so the future decoder has a named home and so callers can detect the deferral
explicitly rather than by a bare "unsupported." NavIC coverage is also the motivating case for
`docs/FEDERATION.md`.

---

## The assembler pairing rules, side by side

Every constellation says "these parts are one data set" differently. This table is the summary;
each rule's failure mode is described in its section above.

| Format | Pairing rule | Error |
|---|---|---|
| GPS/QZSS LNAV | IODE(sf2) = IODE(sf3) = IODC(sf1) & 0xFF | IODE/IODC mismatch |
| GPS/QZSS CNAV | header PRN match **and** shared toe; clock attaches only if Toc = toe | `errPRNMismatch`, `errIODMismatch` |
| Galileo I/NAV | matching IODnav across words 1–4 (word 5 excluded) | `errIODMismatch` |
| Galileo F/NAV | matching IODnav across pages 1–4 | `errIODMismatch` |
| BeiDou D1 | **broadcast adjacency** — sf1/sf2/sf3 exactly 6 s apart (no IOD tag exists) | `errBeiDouSOWGap` |
| BeiDou B-CNAV2 | **broadcast adjacency** — MT10/MT11 within 3 s; clock within 10 × 300 s or dropped | `errPairSOW` |
| GLONASS | string numbers 1/2/3 asserted; same-frame window enforced by the caller | `errGLONASSStringOrder` |

Every adjacency delta wraps mod 604800 through `sowDelta`, so the once-a-week rollover straddle
still assembles  while an out-of-domain SOW fails rather than being normalized.

---

## The error taxonomy

**Exported** — callers can `errors.Is` these to distinguish causes and count them under separate
metric labels:

| Error | Meaning |
|---|---|
| `ErrOutOfRange` | A `BitReader` read fell outside the frame. |
| `ErrShortFrame` | Too few words/bytes handed to a decoder or assembler. |
| `ErrWrongMsgType` | A valid object in the wrong positional slot — an argument transposition, distinguishable from an IOD mismatch. |
| `ErrBadTLMPreamble` | LNAV word 1 lacks the 0x8B preamble. |
| `ErrBadPreamble` | A CNAV message's leading byte isn't 0x8B. |
| `ErrBadCRC` | CRC-24Q failed (CNAV, I/NAV, F/NAV, B-CNAV2, SBAS). |
| `ErrBadBCH` | A BeiDou D1 BCH(15,11,1) block failed. |
| `ErrGLONASSHamming` | A GLONASS string failed the §4.7 check. |
| `ErrGalileoAlertPage` | An alert page or misaligned pair — a *transmission mode*, not corruption. |
| `ErrNavICDeferred` | The NavIC decoder is an explicit deferral. |
| `ErrParity` | Declared for the future raw-signal path; **no decoder returns it today.** |

**Unexported** — internal precision, surfaced to callers as decode errors:
`errIODMismatch`, `errBadSubframe`, `errBadTOWCount`, `errPRNMismatch`, `errBeiDouSOWGap`,
`errBadFraID`, `errBadSatType`, `errPairSOW`, `errGLONASSStringOrder`, `errBadStringNum`,
`errBadTb`, `errBadFreqCh`.

## The `Stamp*` helpers

```go
func StampGalileoINAVCRC(words []uint32)
func StampGalileoFNAVCRC(words []uint32)
func StampBeiDouD1BCH(words []uint32)
func StampGLONASSHamming(words []uint32)
```

These compute and write the correct check bits into a **synthetic** frame, so tests can build one
that a hardened decoder will actually accept. They exist because verifying integrity checks means
every test fixture must carry a valid one. Live decode paths should never call them — they're used
by this package's tests and by the daemon's `internal/state` and `internal/serve` tests.

One asymmetry to know: the other three stampers no-op on a short slice, while
`StampGLONASSHamming` **requires at least four words and will panic on a shorter one.**
Production ingest never synthesizes strings.

---

## Tests and fuzzing

Every exported decoder has a fuzz target covering unchecked lengths, out-of-bounds
reads, and arithmetic underflows. The contract is: **arbitrary input never panics;
malformed input returns an error; a nil result never accompanies a nil error.**

```
FuzzBitReader   FuzzCRC24Q   FuzzGPSParity   FuzzSignedAgainstReference
FuzzGPSLNAV     FuzzDecodeGPSCNAV
FuzzDecodeGalileoINAV   FuzzDecodeGalileoFNAV
FuzzDecodeBeiDouD1      FuzzDecodeBeiDouBCNAV2
FuzzDecodeGLONASSString FuzzDecodeGLONASSFrameNA  FuzzGLONASSAlmanac
FuzzDecodeSBASL1
```

`FuzzSignedAgainstReference` is the differential one: `Signed` is checked against an independent
reference implementation across the whole width range, which is how the regression fix n = 63 overflow was
pinned shut.

Unit coverage highlights, by theme rather than exhaustively:

- **Primitives:** MSB-first ordering, cross-byte reads, sign-magnitude, `Concat`, bounds,
  the regression fix overflow (`TestBitsRejectsOverflowingStart`), the regression fix over-wide `Concat`, CRC
  known-answer and round-trip, and `TestCRC24QBitsMatchesByteWiseCRC24Q` (the bit and byte forms
  agree on byte-aligned input).
- **Integrity rejection:** `TestBCNAV2RejectsBadCRC`, `TestDecodeGPSCNAVRejectsBadPreambleAndCRC`,
  `TestDecodeGalileoINAVRejectsAlertPage`, `TestDecodeGLONASSStringHammingReject`,
  `TestDecodeSBASL1RejectsBadCRC`, `TestCheckCRC24QBitsRejectsFlippedBit`.
- **Range gates:** `TestGPSLNAVRejectsOutOfRangeSubframe`, `TestGPSLNAVRejectsTOWOverflow`,
  `TestDecodeGLONASSStringRejectsZeroNumber`, `TestDecodeGLONASSStringTbRange`,
  `TestGLONASSFreqChValidation`, `TestBCNAV2RejectsReservedSatType`.
- **Pairing:** `TestGPSLNAVIODEMismatch`, `TestAssembleGPSCNAVRejectsCrossSVPair` (the regression fix
  chimera), `TestAssembleBeiDouSOWAdjacency` + `TestAssembleBeiDouSOWWeekRollover`,
  `TestBCNAV2PairAdjacency` + `TestBCNAV2PairAndClockWeekRollover`, and the stale-clock drops on
  both CNAV families (`TestAssembleGPSCNAVStaleClockDropped`, `TestBCNAV2StaleClockDropped`).
  Four wrong-slot tests cover `ErrWrongMsgType` argument transposition (LNAV, CNAV, Galileo,
  B-CNAV2); GLONASS's string-order equivalent rides `TestAssembleGLONASSClock`.
- **Field-level regressions:** `TestDecodeGPSCNAVMsg10Integrity` (signed URA_ED),
  `TestDecodeGalileoINAVWord5Health` (bit 67 vs 69), `TestAssembleGalileoBGD` /
  `TestAssembleGalileoFNAVBGD` (the right BGD on the right clock),
  `TestAssembleBCNAV2ClockCarriesDataComponentTGD` (eq. 7-5),
  `TestDecodeGLONASSStringNonPositionalZeroed`,
  `TestDecodeSBASL1FabricatedDoNotUseNowRejected`.

```sh
go test ./frame/                                   # unit tests
go test ./frame/ -run Fuzz -fuzz FuzzDecodeGPSCNAV # one target, extended
```

**Real-frame validation lives outside this package**, in the daemon:
`go/internal/ingest/realframes_test.go` replays the committed live-receiver UBX captures
(`f9t_capture.ubx`, `f9p_capture.ubx`, `glo_superframe_capture.ubx`) through these decoders and
cross-validates constellations against themselves —
`TestRealBeiDouD1AgreesWithBCNAV2` (B1I vs B2a), `TestRealGalileoFNAVAgreesWithINAV` (E5a vs
E1-B), and `TestRealGPSCNAVAgreesWithLNAV` (L2C vs L1 C/A) are the three that would catch a
wrong offset that still produces a plausible number.

---

## Sources

`docs/CONSTELLATIONS.md` (frame layouts and the coverage matrix), `docs/MATH.md` (field
meanings), `docs/INTEGRITY.md §9` (the untrusted-input contract). Primary ICDs:

- **IS-GPS-200N** §20.3.2/§20.3.3 (LNAV), §20.3.5 (parity), §30.3.3 (CNAV), Tables 20-XII/XIV,
  30-I/II/III/IV.
- **IS-GPS-705J** — L5 CNAV.
- **GAL-OS-SIS-ICD-2.2** §4.2 (F/NAV), §4.3 (I/NAV), §5.1.5/§5.1.8/§5.1.12, Tables 30, 33, 38, 39,
  46, 51, 69, 71, 72, 76, 79, 81, 83, 84, 91.
- **GAL-OSNMA-SIS-ICD** §2 — the OSNMA field structure.
- **BDS-SIS-B1I-3.0** §5.1.3 (BCH), §5.2 / §5.2.4 with Figures 5-8…5-10 and Tables 5-5…5-10.
- **BDS-SIS-B2a-1.0** §6.2 Figures 6-1…6-16, §7 Tables 7-2…7-20, §7.6.2, §7.16.
- **GLO-ICD-5.1** §3.3.1.1 (the FDMA plan), §4.1/§4.4/§4.5 (the message), §4.7 + Table 4.13
  (Hamming), Tables 4.3, 4.5, 4.6, 4.9, 4.10, 4.11, 5.1.
- **QZSS-PNT-006** §4.1.2.4, §4.3.1 — the QZSS redefinitions at GPS bit positions.
- **RTCA-DO-229 / ICAO-ANNEX-10** — SBAS L1 (cited, not vendored; both are paid standards).
- **UBX-PROTOCOL** — the RXM-SFRBX delivery shapes.

See `reference/REFERENCES.md` for what's vendored, what's local-only, and the redistribution terms
on each.
