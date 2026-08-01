# Constellations, Signals & Nav-Frame Decoding

**Status: design (2026-07-07).** The per-constellation coverage specification for
`navlistener`, the GNSS collector daemon serving the Integrity Constellation Map.
It records signal formats and implementation plans, including QZSS and NavIC.
The coverage matrix distinguishes implemented, capture-only, and planned paths.

> **Authorship and validation.** Decoders are written from the public ICDs cited
> in §1. Independent implementations, including galmon, can provide numerical
> cross-checks over the same captured frames. Source citations and external test
> fixtures document the provenance of the implementation and its validation.

**Division of labour with the other docs.** This document specifies **what** is decoded and
**how the raw frames are plumbed** from receiver to collector. It does **not** reproduce the
orbit/clock equations — those live in `docs/MATH.md`. Health/URA/integrity interpretation is
`docs/INTEGRITY.md`; the emitted feed shape is `docs/OUTPUT.md`; the pipeline and wire
transport are `docs/DESIGN.md`.

**The edge does not decode.** `navfeeder` (C/ESP32) forwards *raw broadcast nav frames* —
UBX-RXM-SFRBX words, Septentrio SBF raw-nav blocks, RTCM3 — inside `GNF1` DATA frames. All
de-interleaving, parity/CRC checking, ephemeris assembly, and orbit propagation happen
centrally in `navlistener`, exactly as galmon centralizes in `navparse`. This keeps the edge
dumb, tiny, and robust, and keeps the security-sensitive parser in one auditable place.

---

## 0. gnssId conventions (read this first)

**One numbering, everywhere** — the u-blox `gnssId` order, which UBX-RXM-SFRBX carries
natively (no remap on ingest) and which our feeds emit (`docs/OUTPUT.md §0`):

| GPS | SBAS | Galileo | BeiDou | IMES | QZSS | GLONASS | NavIC |
|---|---|---|---|---|---|---|---|
| 0 | 1 | 2 | 3 | 4 *(never emitted)* | **5** | 6 | **7** |

- **SV-name letters:** `G` GPS, `S` SBAS, `E` Galileo, `C` BeiDou, `R` GLONASS, **`J` QZSS**,
  **`I` NavIC** — the RINEX 3.x / IGS letters. No consumer parses letters (verified: clients
  key on the numeric `gnssid` and treat SV names as opaque map keys), so `J`/`I` names carry
  zero client work.
- **Consumer conformance:** mapintsat's `Gnss.cs`/`GNSS.swift` currently mislabel `4 = NavIC`
  and `7 = KASS` (KASS is an SBAS *provider* on PRN 134 under gnssid 1, not a constellation);
  that table is fixed per `docs/OUTPUT.md §6.1` before NavIC SVs appear in feeds.

---

## 1. Coverage matrix

Decoder status: **★ core** (v1, must ship) · **▲ extended** (modern civil signals, v2) ·
**◇ carried-raw** (stored + re-decodable, not yet interpreted).

| Constellation | gnssId | Letter | Nav messages we decode | Signals / freqs | Source ICD (doc # / edition) | Status |
|---|---|---|---|---|---|---|
| **GPS** | 0 | G | L1 C/A **LNAV** (subframes 1–5); L2C/L5 **CNAV** (msg 10/11/30–37); L1C **CNAV-2** | L1 1575.42, L2 1227.60, L5 1176.45 MHz | IS-GPS-200 (Rev N, 2022); IS-GPS-705 (L5, Rev J); IS-GPS-800 (L1C, Rev J) | ★ LNAV; ▲ CNAV/CNAV-2 |
| **Galileo** | 2 | E | E1-B **I/NAV** (word types 0–10, 16 reduced-CED, 17–20 FEC2, 63 dummy); E5a **F/NAV**; E5b **I/NAV**; E6-B **C/NAV** | E1 1575.42, E5a 1176.45, E5b 1207.14, E6 1278.75 MHz | Galileo OS SIS ICD Issue 2.1 (Nov 2023); Galileo HAS SIS ICD 1.0 (E6) | ★ I/NAV, F/NAV; ▲ E6 C/NAV |
| **BeiDou** | 3 | C | **D1** NAV (MEO/IGSO, subframes 1–5); **D2** NAV (GEO); **B-CNAV1** (B1C); **B-CNAV2** (B2a); **B-CNAV3** (B2b) | B1I 1561.098, B1C 1575.42, B2a 1176.45, B2b 1207.14, B3I 1268.52 MHz | BDS-SIS-ICD-B1I 3.0 (2019); -B1C 1.0 (2017); -B2a 1.0 (2017); -B2b 1.0 (2020) | D1 (B1I) + B-CNAV2 (B2a) shipped; **D2/B-CNAV1/B-CNAV3 planned** |
| **GLONASS** | 6 | R | L1OF/L2OF **strings 1–15** (eph strings 1–4, time string 5, almanac 6–15) | L1 ~1602+k·0.5625, L2 ~1246+k·0.4375 MHz (FDMA, k=−7..+6); L3OC 1202.025 (CDMA, future) | GLONASS ICD Ed. 5.1 (2008, FDMA); GLONASS ICD CDMA Gen. Desc. Ed. 1.0 (L3OC) | ★ L1OF/L2OF; ◇ L3OC |
| **QZSS** 🇯🇵 | 5 | J | L1 C/A **LNAV** (GPS-compatible); L2C/L5 **CNAV**; L1C **CNAV-2**; **L1S** (SLAS + DC Report); **L6** (L6D/L6E CLAS/MADOCA) | L1 1575.42, L2 1227.60, L5 1176.45, L1S 1575.42, L6 1278.75 MHz | IS-QZSS-PNT-005 (2023); IS-QZSS-L1S-005; IS-QZSS-L6-005 | LNAV/CNAV validation shipped; **CNAV-2/L1S/L6 planned** |
| **NavIC/IRNSS** 🇮🇳 | 7 | I | L5/S **SPS NAV** (master frame, subframes 1–4); **L1 SPS** NAV (NVS-01 onward) | L5 1176.45, S 2492.028, **L1 1575.42** (NVS-01+) MHz | IRNSS SPS ICD Version 1.1 (Aug 2017); NavIC L1 SPS ICD 1.0 (2023) | **planned / captured raw only; no live decoder** |
| **SBAS** | 1 | S | L1 C/A **MT 0–63** (integrity, fast/long corrections, iono grid, almanac) | L1 1575.42, L5 1176.45 (DFMC, future) MHz | RTCA DO-229 (MOPS, D/E); ICAO Annex 10 SARPs; SBAS L5 DFMC ICD (L5) | ★ L1 MT; ◇ L5 DFMC |

SBAS providers we name and geo-fence for coverage (the `sbas` feed, `docs/OUTPUT.md §1.5`): **WAAS**
(US, PRN 131/133/135/138), **EGNOS** (EU, 120/121/123/126/136 — PRN 120 = Inmarsat-3F2 AOR-E,
EGNOS's last holder of that code), **MSAS** (Japan, 129/137), **GAGAN** (India, 127/128/132),
**SDCM** (Russia, 125/140/141), **BDSBAS** (China, 130/143/144), **KASS** (Korea, 134),
**SouthPAN** (AU/NZ, 122/124 — PRN 124 reassigned from EGNOS to SouthPAN April 2024, regression fix; do
NOT move it back). Unknown PRNs degrade to the generic `"SBAS"` provider rather than being
dropped. **The shipped map + test are authoritative** (`gnss/frame/sbas_l1.go
SBASProvider`, locked by `TestSBASProviderAssignments`) — PRN assignments are an operational
registry, not an ICD constant, and this prose is synced to the code, not the other
way around. QZSS also broadcasts an SBAS-like service on L1S; see §3.

---

## 2. Nav-frame ingestion path

Three raw-frame sources are supported. Each is forwarded verbatim by `navfeeder` and decoded
in `gnss/frame/<constellation>.go` (the top-level `gnss` module). The parser's contract: **untrusted input**
— every length, index, and bit-read is bounds-checked. A frame that fails parity/CRC is
counted (`navlistener_nav_crc_fail_total{gnssid,sigid,source}` — the `source` label is
per-link noise visibility) and dropped, never assembled.

### 2.1 u-blox — UBX-RXM-SFRBX

`navfeeder --source-mode ubx` reads the UBX stream from the receiver and forwards each
**UBX-RXM-SFRBX** (class 0x02, id 0x13) message. Its header carries `gnssId, svId, sigId,
freqId` (GLONASS FDMA channel, `k = freqId − 7`), `numWords`, and `numWords` little-endian
32-bit `dwrd[]`. The collector's `sfrbx.go`:

1. **De-interleaves** each word: u-blox stores every 32-bit word little-endian; the ICD bit
   order is MSB-first within the constellation's native word length. We reassemble the native
   bit stream (GPS/QZSS: 10×30-bit words per subframe; Galileo I/NAV: 8 words → a 240-bit
   page; F/NAV: a 244-bit page; GLONASS: a 85-bit string in 3 words; BeiDou D1/D2: 10×30-bit;
   SBAS: a 250-bit block in 8 words) into a big-endian `bitreader`.
2. **Checks integrity** using the coding actually present in SFRBX: GPS/QZSS CNAV, Galileo
   I/NAV and F/NAV, BeiDou B-CNAV2, and SBAS use central **CRC-24Q** checks (polynomial
   `0x1864CFB`); GLONASS uses its central ICD Hamming check; BeiDou D1 centrally verifies the
   delivered, de-interleaved **BCH(15,11,1)** blocks. GPS/QZSS LNAV is the exception: u-blox
   delivers D30*-resolved data words, so broadcast word parity cannot be recomputed; the
   collector checks the fixed TLM preamble structurally and relies on receiver validation for
   parity. Thus a GNF1 push still has a central bit-level integrity gate for every shipped
   decoder except LNAV, whose parity trust terminates at the receiver/feeder. BeiDou D2 and
   NavIC decoding remain unsupported/planned rather than claiming an integrity check.
3. **Dispatches** on `(gnssId, sigId)` to the constellation decoder, which extracts the ICD
   parameter set (§below) and hands it to the ephemeris store.

**u-blox `(gnssId, sigId)` → our GNF1 nav type** (F9/F10 generation; `sfrbx.go` dispatch
table):

| gnssId | sigId | Signal | GNF1 type (§6) |
|---|---|---|---|
| 0 GPS | 0 | L1 C/A | `GpsLnav` |
| 0 GPS | 3,4 | L2 CL/CM | `GpsCnav` |
| 0 GPS | 6,7 | L5 I/Q | `GpsCnav` |
| 2 Gal | 0,1 | E1 C/B | `GalInav` |
| 2 Gal | 3,4 | E5a I/Q | `GalFnav` |
| 2 Gal | 5,6 | E5b I/Q | `GalInav` |
| 3 BDS | 0 | B1I D1 | `BdsD1` |
| 3 BDS | 1,3 | B1I/B2I D2 | planned/unsupported (`BdsD2` reserved) |
| 3 BDS | 2 | B2I D1 | planned/unsupported (needs capture-verified ID mapping) |
| 3 BDS | 5,6 | B1 Cp/Cd (B1C) | planned/unsupported (`BdsCnav1` reserved) |
| 3 BDS | 7 | B2 ap (B2a companion) | planned/unsupported (needs capture-verified ID mapping) |
| 3 BDS | 8 | B2 ad (B2a) | `BdsCnav2` |
| **5 QZSS** | 0 | L1 C/A | `QzsLnav` |
| **5 QZSS** | 1 | **L1S** | planned/unsupported (`QzsL1s` reserved) |
| **5 QZSS** | 4,5 | L2 CM/CL | `QzsCnav` |
| **5 QZSS** | 8,9 | L5 I/Q | `QzsCnav` |
| 6 GLO | 0 | L1 OF | `GloNav` |
| 6 GLO | 2 | L2 OF | `GloNav` |
| **7 NavIC** | 0 | L5 A | `NavicNav` |
| **7 NavIC** | — | L1 (NVS-01+; no u-blox support yet — SBF only) | `NavicL1Nav` |
| 1 SBAS | 0 | L1 C/A | `SbasL1` |

QZSS uses gnssId 5 and NavIC uses gnssId 7. Each needs an explicit dispatch
policy and validation against the corresponding signal specification.

### 2.2 Septentrio — SBF raw-nav blocks

`navfeeder --source-mode sbf` reads the SBF stream and forwards the raw-nav blocks. SBF wraps
the *already-de-interleaved* ICD nav bits (Septentrio does the word unpacking on the
receiver), so a future central decoder skips step 1 above and goes straight to parity/CRC +
parameter extraction. The current `sbf.go` validates framing/CRC and persists raw blocks only
(capture-only, like RTCM below); the block-specific live decoders in this table are planned
(the block ID's low 13 bits, ignoring the 3-bit revision):

| SBF block | ID | Yields |
|---|---|---|
| `GPSRawCA` | 4017 | GPS L1 C/A LNAV subframe → `GpsLnav` |
| `GPSRawL2C` | 4018 | GPS L2C CNAV message → `GpsCnav` |
| `GPSRawL5` | 4019 | GPS L5 CNAV message → `GpsCnav` |
| `GPSRawL1C` | 4221 | GPS L1C CNAV-2 → `GpsCnav2` |
| `GLORawCA` | 4026 | GLONASS L1/L2 OF string → `GloNav` |
| `GALRawFNAV` | 4022 | Galileo E5a F/NAV page → `GalFnav` |
| `GALRawINAV` | 4023 | Galileo E1-B/E5b I/NAV page → `GalInav` |
| `GALRawCNAV` | 4024 | Galileo E6-B C/NAV page → `GalCnav` |
| `BDSRaw` | 4047 | BeiDou B1I/B3I D1/D2 → `BdsD1`/`BdsD2` |
| `BDSRawB1C` | 4218 | BeiDou B-CNAV1 → `BdsCnav1` |
| `BDSRawB2a` | 4219 | BeiDou B-CNAV2 → `BdsCnav2` |
| `BDSRawB2b` | 4242 | BeiDou B-CNAV3 → `BdsCnav3` |
| `QZSRawL1CA` | 4066 | **QZSS L1 C/A LNAV** → `QzsLnav` |
| `QZSRawL2C` | 4067 | QZSS L2C CNAV → `QzsCnav` |
| `QZSRawL5` | 4068 | QZSS L5 CNAV → `QzsCnav` |
| `QZSRawL6` | 4069 | **QZSS L6D/L6E** → `QzsL6` |
| `QZSRawL1S` | 4228 | **QZSS L1S** SLAS/DC-report → `QzsL1s` |
| `QZSRawL1C` | 4227 | QZSS L1C CNAV-2 → `QzsCnav2` |
| `NAVICRaw` | **4093** | **NavIC L5/S SPS NAV** → `NavicNav` |
| `NAVICLNAVRaw` | 4259 | NavIC L1 SPS NAV → `NavicL1Nav` |
| `GEORawL1` | 4020 | SBAS L1 C/A block → `SbasL1` |
| `GEORawL5` | 4021 | SBAS L5 DFMC block → `SbasL5` ◇ |

Septentrio block 4093 carries NavIC raw navigation data. Its central decoder
is part of the planned NavIC support.

> **Block-ID provenance.** The established IDs (4017–4026, 4047, 4066–4068, 4093, 4218/4219,
> 4024) are confirmed against published SBF reference guides. The newest-generation entries —
> `QZSRawL1S`/`QZSRawL1C`, `QZSRawL6`, `BDSRawB2b`, `GPSRawL1C`, and especially `NAVICLNAVRaw`
> (Septentrio may not yet emit a NavIC-L1 raw block at all) — must be re-verified against the
> receiver's *shipping* firmware SBF Reference Guide when `sbf.go` is written in P3; Septentrio
> adds raw-nav blocks per firmware release. The dispatch table is config-data, not logic, so a
> corrected ID is a one-line change.

### 2.3 RTCM3 — ephemeris & SSR

`navfeeder --source-mode rtcm` forwards RTCM3 framed messages (used where a receiver or an
NTRIP caster exposes RTCM rather than raw frames, and as the source of *precise* orbits for
the future broadcast-vs-precise integrity check in `docs/INTEGRITY.md`). The current `rtcm.go`
validates framing/CRC and persists raw messages only; the following live decoders are planned:

| RTCM3 msg | Content |
|---|---|
| 1019 | GPS ephemeris (full Kepler set + clock + health + URA + IODE/IODC + TGD) |
| 1020 | GLONASS ephemeris (Cartesian state + freq channel + τ/γ + health) |
| 1041 | **NavIC/IRNSS** ephemeris |
| 1042 | BeiDou ephemeris |
| 1044 | **QZSS** ephemeris |
| 1045 | Galileo **F/NAV** ephemeris |
| 1046 | Galileo **I/NAV** ephemeris |
| 1057–1062 | GPS SSR orbit/clock/code-bias corrections |
| 1063–1068 | GLONASS SSR orbit/clock/code-bias |
| (SSR-4076) | Multi-GNSS SSR (Galileo/BeiDou/QZSS orbit/clock) |

The SSR radial/along/cross orbit deltas feed the *broadcast-vs-precise* discontinuity metric
(`rtcm_eph_delta_cm`, `docs/INTEGRITY.md`). Ephemeris messages 1041/1044 give a second,
receiver-independent path to NavIC/QZSS orbits.

---

## 3. QZSS decoder spec (Japan — first-class) 🇯🇵

QZSS is the flagship gap-closure. Four decode paths:

### 3.1 L1 C/A LNAV — reuse the GPS LNAV decoder
QZSS L1 C/A uses the **identical LNAV frame structure** as GPS (IS-QZSS-PNT-005 §5.2 defers to
IS-GPS-200): 5 subframes × 300 bits, subframe 1 clock/health, 2–3 ephemeris, 4–5
almanac/iono/UTC. `QzsLnav` therefore **shares the `GpsLnav` bit-field extractor**; only the
framing differs:

- **PRN mapping.** QZSS SVIDs are **193–202** (u-blox `svId` for gnssId 5 is 1–10; add 192 to
  get PRN). SV-name is `J01..J10` (`svId`, not PRN). The decoder tags `gnssId=5`.
- **QZSS-specific ephemeris/clock quirks** vs GPS (`docs/MATH.md` uses the same Kepler set):
  - **Larger eccentricity & inclination.** QZO (quasi-zenith, 3 of the constellation) fly a
    highly-elliptical, ~45° inclined, geosynchronous orbit: `e ≈ 0.075`, `i₀ ≈ 0.7 rad`,
    `√A` ≈ 6493 (a ≈ 42,164 km). The GEO member (QZS-3, PRN 199) sits at ~127°E. The math is
    unchanged — but range-checks tuned to GPS's near-circular MEO (`e < 0.03`) would wrongly
    reject valid QZO ephemerides; our validity gates (`docs/INTEGRITY.md`) use
    per-constellation bounds.
  - **Fit interval.** The `fitInterval` flag + `IODC` gate ephemeris validity exactly as GPS.
  - **Time base.** **QZSST = GPST** (same epoch, same leap-second handling; QZSS week number is
    the GPS week). No separate time-system offset for QZSS clock/orbit — but QZSS *does*
    broadcast GPS/Galileo/GLONASS inter-system offsets we surface in `global.json`.
  - **Health.** QZSS health is signalled in subframe 1 (like GPS) *and* per-signal in the
    almanac pages; `docs/INTEGRITY.md` details the composite.

### 3.2 L1S — SLAS + DC Report (disaster) — new message type
QZSS L1S (1575.42 MHz, distinct signal, u-blox sigId 1) carries **250-bit messages at 1 Hz**
with a 6-bit preamble + 8-bit message-type + 212-bit body + 24-bit CRC-24Q. Two services we
decode into `QzsL1s`:

- **SLAS** (Sub-meter Level Augmentation Service) — SBAS-like fast/long corrections + iono for
  the Japan service area. Message types mirror the SBAS MT taxonomy; decoded like an SBAS
  block but tagged QZSS/L1S so it does not pollute the SBAS layer.
- **DC Report** (Disaster and Crisis Report / "Sasudachi") — JMA earthquake/tsunami/volcano
  and J-ALERT bulletins, MT 43/44. **We decode, timestamp, and store these but treat them as
  an *enrichment/messages* stream** (like ACARS in `radiolistener`), not a positioning input —
  `docs/OUTPUT.md` `/gnss/api/qzss-dcr`. This is a distinctive QZSS capability worth surfacing.

### 3.3 L2C/L5 CNAV & L1C CNAV-2
Structurally identical to the GPS CNAV/CNAV-2 decoders (`QzsCnav`/`QzsCnav2` share the
`GpsCnav`/`GpsCnav2` extractors), PRN-mapped to 193–202. The shared L2C/L5 CNAV
extractor selects the constellation-specific MT10 reference semi-major axis: QZSS uses
`A_REF = 42,164,200 m` (IS-QZSS-PNT-005 Table 4.3.2-16), not GPS's 26,559,710 m.

### 3.4 L6 (CLAS/MADOCA) — carried, enrichment
L6D/L6E (1278.75 MHz) carry the **CLAS** (Centimeter Level Augmentation, compact SSR) and
**MADOCA** PPP corrections at 2000 bits/frame. Decoded into `QzsL6` and stored raw + framed;
the compact-SSR interpretation is a v2 enrichment (feeds the same broadcast-vs-precise check as
RTCM SSR). Not on the ★ critical path.

**New GNF1 types for QZSS:** `QzsLnav`, `QzsCnav`, `QzsCnav2`, `QzsL1s`, `QzsL6` (§6).

---

## 4. NavIC/IRNSS decoder spec (India — first-class) 🇮🇳

NavIC's nav message is **structurally distinct from GPS** — do not assume GPS reuse here.
Source: **IRNSS SPS ICD Version 1.1 (Aug 2017)** for L5/S; **NavIC L1 SPS ICD 1.0 (2023)** for
the new L1 signal on NVS-01 and later spacecraft.

### 4.1 L5/S SPS NAV frame structure
- **Master frame = 2400 symbols**, split into **4 subframes of 600 symbols each**: a
  **16-symbol sync word** followed by **584 symbols** of rate-1/2 convolutionally coded +
  interleaved data carrying a **292-bit subframe payload** (which includes the 6 tail bits);
  after Viterbi decode + de-interleave the collector works on the 292-bit payload.
  (Septentrio SBF 4093 / u-blox deliver post-FEC bits; the collector CRC-checks.)
- Each subframe: **TLM (8b) + TOWC (17b) + Alert/Autonav/Subframe-ID (…) + data (233b) +
  CRC-24Q (24b) + tail (6b)**.
- **Subframe 1 & 2 are fixed:** primary **ephemeris + clock** (the full Kepler-like set —
  `√A, e, i₀, Ω₀, ω, M₀, Δn, IDOT, Ω̇, Cuc/Cus, Crc/Crs, Cic/Cis, t₀e`, and clock
  `a_f0/a_f1/a_f2, t₀c, T_GD`). `docs/MATH.md` propagates this with the GPS-family Kepler
  algorithm and the NavIC constants (`μ = 3.986005e14`, `Ω̇_e = 7.2921151467e-5`).
- **Subframes 3 & 4 are switchable** via a **Message ID** (first 6 bits of the data body),
  carrying: iono grid coefficients (α/β Klobuchar-style **and** the NavIC iono grid), UTC & GPS
  time offset, almanac, **special messages / regional text** (NavIC broadcasts short text
  messages — decoded to an enrichment stream, not positioning), and differential corrections.
  The decoder is a **message-ID dispatch**, not a fixed subframe map — this is the key
  structural difference from GPS the developer must implement.

### 4.2 System time & geometry
- **IRNWT** (IRNSS Network Time): steered to UTC; the broadcast gives IRNWT↔UTC and IRNWT↔GPST
  offsets (surfaced in `global.json`). Week number is a NavIC week (10-bit, own rollover epoch
  22-Aug-1999).
- **Orbits:** 3 GEO (~34/83/132°E) + 4 GSO (inclined ~29°, ground tracks over the Indian
  region). Like QZSS QZO these are high-eccentricity/high-inclination relative to GPS MEO —
  the same per-constellation validity bounds apply.

### 4.3 L1 SPS (NVS-01 onward)
The modernized NavIC L1 signal (1575.42 MHz) uses an interoperable CNAV-2-like frame. Decoded
into `NavicL1Nav`; ▲ extended-tier since only NVS-01+ spacecraft transmit it. Cite NavIC L1 SPS
ICD 1.0.

**New GNF1 types for NavIC:** `NavicNav` (L5/S), `NavicL1Nav` (L1) (§6).

---

## 5. GLONASS specifics

GLONASS uses **FDMA, Cartesian state vectors, numerical integration, and the
PZ-90.11 datum**. Source: **GLONASS ICD Edition 5.1 (2008)**.

- **FDMA channel, not code.** Satellites share codes and separate by frequency: L1 ≈
  `1602 + k·0.5625` MHz, L2 ≈ `1246 + k·0.4375` MHz, `k ∈ [−7,+6]`. UBX-RXM-SFRBX carries the
  channel as `freqId` (`k = freqId − 7`); it is **the** identifier — GLONASS nav strings carry
  **no week number and no full TOW**, only a time-of-day. `GloNav` records `k` and the slot
  number `n`.
- **Datum: PZ-90.11** (not WGS84). **As-built policy :** GLONASS positions are served
  **raw in PZ-90.11**, in the same `x_m/y_m/z_m` fields as the WGS-84-datum constellations,
  with **no datum field and no Helmert transform anywhere in the pipeline** — matching the
  `docs/OUTPUT.md` contract, which defines no datum marker. This is deliberate: PZ-90.11 has
  been aligned with ITRF to ~cm since 2014, far below broadcast-ephemeris accuracy, so a
  transform would move nothing a feed consumer can resolve. The per-constellation ellipsoid
  *is* correctly selected for geodetic (lat/lon) conversion. The ~cm shrug stops being valid
  exactly one place: the planned RTCM precise-vs-broadcast comparison (`docs/INTEGRITY.md
  §5`), where cm-level matters — a fixed 7-parameter Helmert belongs **there, when that
  lands**, not in the feeds. (An earlier revision of this bullet promised datum notation in
  the output; the contract decided otherwise — this text now records what the code does.)
- **Strings 1–15 (85 bits each, 2 s each; a frame is 15 strings = 30 s; a superframe is 5
  frames = 2.5 min):**
  - **Strings 1–4** — immediate **ephemeris**: broadcast **Cartesian position, velocity, and
    luni-solar acceleration** `(x,y,z, ẋ,ẏ,ż, ẍ,ÿ,z̈)` in PZ-90 at reference time `t_b`, plus
    clock `τ_n, γ_n, Δτ_n`, health `B_n`/`l_n`, `F_T`, `E_n`, `P1–P4`, `M`.
  - **String 5** — time: `τ_c` (GLONASST↔UTC(SU)), `τ_GPS` (GLONASST↔GPST), `N4` (4-year
    interval), `NA`. The `todKnown` gate (only trust the time-of-day once string 5 anchors it)
    is a correctness requirement.
  - **Strings 6–15** — **almanac** (5 satellites × 2 strings): Keplerian-style reduced
    almanac parameters `λ, t_λ, Δi, ΔT, ΔT′, ε, ω, τ, C, M, n` per slot.

- **Rotating-frame coordinates.** An inertial position mislabeled as PZ-90 ECEF
  introduces an Earth-rotation-sized longitude error. A single-instant test can
  miss this, so validation must compare positions at multiple times of day.
  The almanac propagator expresses the ascending node longitude directly in the
  rotating Greenwich frame, cancelling sidereal terms (`docs/MATH.md §3.1`).
  Immediate ephemerides use RK4 integration of the PZ-90 equations of motion,
  including the J₂/C₂₀ oblateness term, and reject zero or timeless states
  (`docs/MATH.md §3`).

- **GLONASS positions.** The svs feed carries `x_m/y_m/z_m` from RK4 ephemeris
  propagation. The almanac feed (`docs/OUTPUT.md §1.4`, metres) carries the
  longer-lived all-SV view. Consumer integration is described in
  `docs/OUTPUT.md §6.1`.

- **L3OC (CDMA, future).** New GLONASS-K satellites add a CDMA L3OC signal (1202.025 MHz) with
  a modern message. Carried-raw (◇) for now; a decoder is a later deliverable.

---

## 6. GNF1 nav message type registry

The `GNF1` wire (transport in `docs/DESIGN.md`) carries typed raw frames.
Its registry covers the constellation-specific navigation messages, augmentation
payloads, and receiver telemetry described below.

**The `(gnssId, sigId) → frame_type` mapping has exactly one authority: `testdata/gnf1_frame_type.tsv`**,
the golden matrix generated from Go's canonical `RawFrame.NavType` (`go/internal/ingest/rawframe.go`).
The tables below are the human-readable registry; the fixture is what the three
implementations are tested against — Go (`TestNavTypeMatchesGolden`), the C feeder
end-to-end through the real binary (`TestNavfeederFrameTypeMatrix`), and the ESP32
(`TestESP32Gnf1FrameTypeMatchesGolden`, also runnable as
`make -C esp32/components/gnf1/test`). They had drifted pairwise before the fixture existed
. **A pair with no shipped, capture-verified decoder maps to `0`, never to its
constellation's family byte** — an unverified label persists a frame that replay will
re-decode through the wrong layout. `frame_type` is forensic
metadata: the collector dispatches decode on `(gnssId, sigId)`, so `0` still decodes.

### 6.1 Raw-nav frame types

| # | Name | Constellation / signal | Raw payload | Source |
|---|---|---|---|---|
| 0x10 | `GpsLnav` | GPS L1 C/A LNAV | 300-bit subframe (10×30b) | UBX (0,0) / SBF 4017 |
| 0x11 | `GpsCnav` | GPS L2C/L5 CNAV | 300-bit message | UBX (0,3/4/6/7) / SBF 4018,4019 |
| 0x12 | `GpsCnav2` | GPS L1C CNAV-2 | subframe 2 (600b / 1200 symbols) + TOI + sf3 | SBF 4221 |
| 0x20 | `GalInav` | Galileo E1-B / E5b I/NAV | 240-bit page (2×120b half-pages) | UBX (2,0/1/5/6) / SBF 4023 |
| 0x21 | `GalFnav` | Galileo E5a F/NAV | 244-bit page | UBX (2,3/4) / SBF 4022 |
| 0x22 | `GalCnav` | Galileo E6-B C/NAV | 486-bit page | SBF 4024 |
| 0x30 | `BdsD1` | BeiDou B1I D1 (MEO/IGSO) | 300-bit subframe | UBX (3,0) / SBF 4047 |
| 0x31 | `BdsD2` (reserved/planned) | BeiDou B1I D2 (GEO) | 300-bit subframe | raw capture only |
| 0x32 | `BdsCnav1` (reserved/planned) | BeiDou B1C B-CNAV1 | frame (1800 symbols → payload) | raw capture only |
| 0x33 | `BdsCnav2` | BeiDou B2a B-CNAV2 | 600-symbol frame (288-bit message) | UBX (3,8) / SBF 4219 |
| 0x34 | `BdsCnav3` | BeiDou B2b B-CNAV3 | 1000-symbol frame (486-bit message) | SBF 4242 |
| 0x40 | `GloNav` | GLONASS L1OF/L2OF | 85-bit string (+ `k`, slot) | UBX (6,0/2) / SBF 4026 |
| **0x50** | **`QzsLnav`** | **QZSS L1 C/A LNAV** | 300-bit subframe | UBX (5,0) / SBF 4066 |
| **0x51** | **`QzsCnav`** | **QZSS L2C/L5 CNAV** | 300-bit message | UBX (5,4/5/8/9) / SBF 4067,4068 |
| **0x52** | **`QzsCnav2` (reserved/planned)** | **QZSS L1C CNAV-2** | subframe 2 + TOI | raw capture only |
| **0x53** | **`QzsL1s` (reserved/planned)** | **QZSS L1S SLAS / DC-report** | 250-bit message | raw capture only |
| **0x54** | **`QzsL6` (reserved/planned)** | **QZSS L6D/L6E CLAS/MADOCA** | 2000-bit frame | raw capture only |
| **0x60** | **`NavicNav` (reserved/planned)** | **NavIC L5/S SPS NAV** | 292-bit subframe (post-FEC) | raw capture only; not emitted as supported |
| **0x61** | **`NavicL1Nav` (reserved/planned)** | **NavIC L1 SPS NAV** | frame (post-FEC) | raw capture only; not emitted as supported |
| 0x70 | `SbasL1` | SBAS L1 C/A | 250-bit block | UBX (1,0) / SBF 4020 |
| 0x71 | `SbasL5` | SBAS L5 DFMC ◇ | 250-bit block | SBF 4021 |

### 6.2 Telemetry types (receiver-side, not decoded centrally)

| # | Name | Content |
|---|---|---|
| 0x01 | `ReceptionData` | per-SV C/N₀, elevation, azimuth, pseudorange-residual, quality-ind, used-in-solution (UBX-NAV-SAT/NAV-SIG) |
| 0x02 | `RFData` | raw observables: pseudorange, carrier phase, Doppler, lock-time, cno, validity (UBX-RXM-RAWX). Dual-frequency observable pairs feed the measured-ionosphere cross-check (`docs/MATH.md §7.4`) |
| 0x03 | `ObserverPosition` | receiver ECEF x/y/z, accuracy, ground-speed (UBX-NAV-HPPOSECEF/PVT) |
| 0x04 | `ObserverDetails` | vendor, hw/sw version, git hash, serial, clock offset/drift, owner, remark, uptime |
| 0x05 | `JammingStats` | u-blox MON-HW/MON-RF jamming/AGC/spoofing indicators (MON-RF on F9+; RF-integrity input, `docs/INTEGRITY.md`) |
| 0x06 | `TimeOffset` | per-GNSS inter-system offsets (GGTO, BGTO, GPS-UTC, …) → `global.json` |
| 0x07 | `RtcmMessage` | forwarded RTCM3 message (ephemeris 1019/1020/1041/1042/1044/1045/1046; SSR 1057-1068) |
| 0x08 | `QzssDcr` | QZSS L1S DC-report (disaster/crisis) — enrichment stream (derived from `QzsL1s`) |
| 0x09 | `NavicText` | NavIC special/regional text message — enrichment stream (derived from `NavicNav`) |

Types `0x08`/`0x09` are *derived* enrichment records the collector synthesizes from the raw
L1S/NavIC frames; they are not sent by the feeder. They give Japan (disaster alerts) and India
(regional messaging) a visible, distinctive product surface beyond raw positioning — see
`docs/OUTPUT.md`.

---

## 7. Receiver capability awareness (tie-in)

Each observer node is a real receiver with real silicon limits (the `tudorgps` project exists
precisely because a `MON-VER` string does not determine capability — e.g. ZED-F9T-00B is L1+L2,
-10B is L1+L5, same `MOD`). `navlistener` records each node's **capability fingerprint** (which
`(gnssId,sigId)` it actually produces, from `UBX-NAV-SIG` / observed frame types) so the
integrity layer knows **what a node *should* be reporting**: a node whose silicon supports E5a
but which suddenly stops delivering `GalFnav` is a signal (jamming, spoofing, or fault), not
just "quiet." This is the GNSS analogue of `radiolistener`'s per-observer expectations and is
detailed in `docs/INTEGRITY.md §6` (capability plausibility). The fingerprint schema reuses
the `tudorgps` capability-tuple shape (`constellation_support`, `signal_support`) so the two
systems share vocabulary.
