# navlistener reference library

Constants, bitfield layouts, scale factors, and sign conventions in `gnss/` and
`go/` must trace to a cite-key and section listed here. Mark uncited values as
unverified rather than filling them in from memory.

- **Cite-keys are stable.** Code comments cite `<cite-key> §<section>` / `Table <n>` (e.g.
  `IS-GPS-200N §30.3.3.1.1.4`, `GAL-OS-SIS-ICD-2.2 Table 71`). The cite-key is the PDF's
  basename in `reference/icd/` (without `.pdf`).
- **Documents live in `reference/icd/`.** Redistribution-permitted ICDs are committed to git
  (present after `git clone`). The rest are git-ignored — their terms grant no redistribution
  (or forbid it) — and are fetched on demand.
- **Reproducing the library:** `make -C reference update` re-fetches every ICD from its
  official source and verifies each against the SHA-256 pinned in `SOURCES.tsv`; on a fresh
  clone, `make -C reference missing` pulls just the local-only set. `make -C reference verify`
  checks the files already on disk. The fetch User-Agent is set (and overridable) in the
  Makefile — several issuers reject curl's default UA.
- **The checks are fail-closed**. Every target reports all rows, then exits nonzero
  if any row failed: a SHA-256 mismatch or a missing *committed* ICD always fails, a fetch
  failure always fails, and a missing *local-only* ICD fails under `STRICT=1` (it is a normal
  state on a fresh clone, so not by default). `make -C go check` runs the default `verify`, so
  a corrupted or substituted committed ICD breaks the build gate; to prove the whole library,
  `make -C reference missing && make -C reference verify STRICT=1`.

**Retrieved:** 2026-07-17. **Editions are the current in-force ones as of that date;** a new
upstream edition means a new row + SHA-256, not an in-place overwrite (that is what `verify`'s
mismatch detection is for).

---

## 1. Vendored ICDs — committed to git (redistribution permitted by the document's terms)

| cite-key | title | authority | edition | date | redistribution terms | SHA-256 |
|---|---|---|---|---|---|---|
| **IS-GPS-200N** | NAVSTAR GPS Space Segment / Navigation User Interfaces (L1/L2, LNAV+CNAV) | US Space Force (SSC) | Rev N | 2022-08-01 | Public domain — "Distribution Statement A. Approved for public release. Distribution is unlimited." (US Gov work, 17 U.S.C. §105) | `54ec544bfe7e6acd97daaa1de0ca248e5abec6b418f23c1a69991e5c7bcf749a` |
| **IS-GPS-705J** | GPS L5 Space Segment / User Interfaces (L5, CNAV) | US Space Force (SSC) | Rev J | 2022-08-01 | Public domain — Distribution Statement A | `532fc3b89061dff84e34406b9a5b41e34b83bdc8cc194b7b7f75a70b15391885` |
| **IS-GPS-800J** | GPS L1C Space Segment / User Interfaces (L1C, CNAV-2) | US Space Force (SSC) | Rev J | 2022-08-01 | Public domain — Distribution Statement A | `c4aad71058a0eba1ba90672e9511566d2d124b49801cfaa42a51c21241815d0a` |
| **ICD-GPS-240D** | GPS Control Segment to User Support (SEM/YUMA almanac formats) | US Space Force (SSC) | Rev D | 2021-03-04 | Public domain — Distribution Statement A | `f458baf07aedac518c807fd21897539257183ba563bde6921ca54d8cdc7d77a5` |
| **WAAS-PS** | FAA WAAS Performance Standard | FAA | 1st Ed | 2008-10-31 | Public domain — "Approved for public release; distribution is unlimited." (no © asserted) | `745bbfddc975c17b3797da5a7d4802cb82c2ceb24c8bdd42d2d8b3da1383463f` |
| **GAL-OS-SIS-ICD-2.2** | Galileo Open Service SIS ICD (E1/E5/E6, I/NAV+F/NAV) | EUSPA / EU | Issue 2.2 | 2025-11 | Conditional © grant — "may only be partly or wholly reproduced and/or transmitted to a third party… [if] the 'Terms of Use and Disclaimers'… are accepted, reproduced and transmitted entirely and unmodified… the copyright notice '© European Union 2025' is not removed." | `1ca51b26c970140d2a5fd932fdf6086e5dcf945cbe216ccb081c300136504671` |
| **GAL-OSNMA-SIS-ICD** | Galileo OSNMA (nav-message authentication) SIS ICD | EUSPA / EU | Issue 1.1 | 2023-10 | Conditional © grant (same shape as OS SIS ICD; Terms of Use + Annex E travel unmodified; "© European Union 2023" preserved) | `0f8e385af744924cd64cea476684d399f63af0d65384d368b0765ea94d6176c3` |
| **GAL-OSNMA-RXG** | Galileo OSNMA Receiver Guidelines | EUSPA / EU | Issue 1.3 | 2024-01 | Conditional © grant (Terms of Use unmodified; "© European Union 2024" preserved) | `4b005862f5d1fbb53e9f51ae3f182cee1c7b890ea8236578669b56abc7d3b651` |
| **GAL-HAS-SIS-ICD** | Galileo High Accuracy Service SIS ICD (E6-B) | EUSPA / EU | Issue 1.0 | 2022-05 | Conditional © grant (Terms of Use + Annex E unmodified; "© European Union 2022" preserved) | `7b1058520175cf4d135df0fc17c4fb339a95d645804599242204405dd2a88873` |
| **EGNOS-SDD-OS** | EGNOS Open Service — Service Definition Document | EUSPA / ESSP | Issue 3.0 | 2024 | Conditional © grant — "may be excerpted, copied, printed, republished… only under the conditions that the… 'Terms and Conditions of Use' are… reproduced and transmitted entirely and unmodified… source: 'EGNOS OS SDD, © European Union, 2024'." (use restricted to non-safety-critical) | `3a6ddeb601c9c0acdba5c1c6a71eca54f4f513e26107e70b61f444f91267b5d8` |
| **QZSS-DCR** | IS-QZSS-DCR (Disaster/Crisis Report / DC Report) | Cabinet Office, Japan | DCR-016 | 2026-04-03 | Explicit redistribution — "you may redistribute the information… to the redistribution-destination users… [if you] display the… Disclaimer of Liability… and this Additional Terms of Use." | `07cc143ea7d03b486392b16f348ca291adf758f9d81ae4c457f3e89a165fa516` |

### 1a. Data-exchange format standards (committed)

Not ICDs, but the formats the validation harness consumes. `gnss/truth_test.go` propagates a
real broadcast ephemeris (RINEX 3.05 nav) per constellation and compares it against a precise
orbit (SP3-d) — so the field-by-column layout the test parses comes from these specs.

| cite-key | title | authority | edition | date | redistribution terms | SHA-256 |
|---|---|---|---|---|---|---|
| **RINEX-3.05** | The Receiver Independent Exchange Format, v3.05 (the version gnss/truth_test.go's BKG BRDC fixture and field parser follow) | IGS / RTCM RINEX WG | 3.05 | 2020-12 | Open community standard — published openly at files.igs.org for universal interchange; no © asserted (authored by the IGS/RTCM RINEX WG). Treated redistributable on the same basis as WAAS-PS's no-©-asserted public standard | `05de6c898e1cf4102d6d0ef8b2710a98a16a5105c49a6d972757d647c7593918` |
| **RINEX-4.02** | The Receiver Independent Exchange Format, v4.02 (current edition) | IGS / RTCM RINEX Committee | 4.02 | 2024 | Open community standard (as above) | `7d5bb16d50e6010138bcc6ee7f2e8c5652f8819461523651a8e180b617d537db` |
| **SP3-D** | The Extended Standard Product 3 Orbit Format (SP3-d) — the ESA/ESOC MGEX precise-orbit truth fixture's format | IGS | SP3-d | 2016 | Open community standard (as above) | `0809fe6571816a9b8394b46b8851b9bfbab956b46f7acab5af6eb62acee5ebbe` |

Local path for all of the above: `reference/icd/<cite-key>.pdf`. Official URLs are in `SOURCES.tsv`.

---

## 2. Local-only ICDs — present on disk, **not redistributed** (git-ignored)

Downloaded from the official issuer and pinned by SHA-256, but **not committed**: each
document's own terms grant no redistribution, forbid it, or are uncleared. A fresh clone
does not contain these — run `make -C reference missing` to fetch them. They are fully
citable; only the *bytes* are not redistributed by us.

| cite-key | title | authority | edition | date | why local-only | SHA-256 |
|---|---|---|---|---|---|---|
| **QZSS-PNT-006** | IS-QZSS-PNT (L1C/A, L1C, L2C, L5) | Cabinet Office, Japan | PNT-006 | 2024-07-11 | Grants free *use* of the Services/Document; no explicit redistribution clause (unlike DCR) | `ed8097704f2335180aba8cbf5d4a5240518674eeb4d8ba7e752856a3cfbc49fb` |
| **QZSS-L1S** | IS-QZSS-L1S (SLAS sub-metre augmentation) | Cabinet Office, Japan | L1S-009 | 2026-02-20 | Use-only disclaimer, no redistribution clause | `26acfb4b803990a1f72beab6e6a3c0fda161cff2255aa3277738206724877b73` |
| **QZSS-L6** | IS-QZSS-L6 (CLAS centimetre augmentation) | Cabinet Office, Japan | L6-008 | 2026-03-18 | Use-only disclaimer, no redistribution clause | `c7de5e845bdac6815350a717cd486d04ddddd25c09edcc4099a3bef48140a1ff` |
| **NAVIC-SPS-L5S** | IRNSS/NavIC SPS ICD (L5 + S band) | ISRO | v1.1 | 2017-08 | **Forbids reproduction** — "shall not be copied in whole, in part or otherwise reproduced… without prior written consent of ISRO" | `321573a6903ebc5d7392a1baa26795ccd6997100449cb79836f849498ea8216f` |
| **NAVIC-SPS-L1** | NavIC SPS ICD, L1 frequency | ISRO | v1.0 | 2023-08 | Conditional non-profit grant only; held local-only by decision (public-repo redistribution borderline) | `ad534a52f5eda610c11c7fbb4af6721680ee1f103f8df121b8d5a0dbd7c523c6` |
| **GLO-ICD-5.1** | GLONASS ICD, FDMA navigational signal L1/L2 | Russian Institute of Space Device Engineering | Ed 5.1 | 2008 | No redistribution grant stated | `5c6226de34720656f8d195b7132a3778a7c377d263bb50954c0c581ff08cd3e0` |
| **GLO-OS-PS-2.2** | GLONASS Open Service Performance Standard | PNT IAC / TsNIIMash | Ed 2.2 | 2020-06 | No grant and no prohibition stated (uncleared) | `f0def7be0890508cd7502d7a8d6e1e9e0a69ab1f7dd1d20165fdd2c7c40a3410` |
| **BDS-SIS-B1I-3.0** | BeiDou B1I Open Service SIS ICD | China Satellite Navigation Office | v3.0 | 2019-02 | CSNO reserves interpretation; no redistribution grant (uncleared) | `7f2de6f69af3eb970627d43e5ad6645e76486e1cbaedfdf5f697210ede9ce2e8` |
| **BDS-SIS-B2a-1.0** | BeiDou B2a Open Service SIS ICD | China Satellite Navigation Office | v1.0 | 2017-12 | Uncleared (as above) | `50e1b2b80181dc1d3fe4f8a9b154953d32a8e157316c09fb90e02d2954b11880` |
| **BDS-SIS-B1C-1.0** | BeiDou B1C Open Service SIS ICD | China Satellite Navigation Office | v1.0 | 2017-12 | Uncleared (as above) | `a297befc287fe4598735ccc99246c58e299375d79770a788538123ed4a20d137` |
| **BDS-SIS-B2b-1.0** | BeiDou B2b Open Service SIS ICD | China Satellite Navigation Office | v1.0 | 2020-07 | Uncleared (as above) | `7c99ad37a7153fbeeaadfa395700b54dfc284dc81b296c09bda9aec3853f6a74` |
| **BDS-SIS-B3I-1.0** | BeiDou B3I Open Service SIS ICD | China Satellite Navigation Office | v1.0 | 2018-02 | Uncleared (as above) | `b924b861c79d1e70048dc13ec41c833c3bc231fd92e50b1089f055ce3a7be4a8` |
| **BDS-SIS-B2I-2.1** | BeiDou (BDS-2) B1I+B2I SIS ICD (`BDS-SIS-ICD-2.1`) | China Satellite Navigation Office | v2.1 | 2016-11 | Uncleared. NB: legacy combined B1I/B2I doc — no standalone BDS-3-era "B2I" ICD is published | `e59e5e225524eb1b2ef2288d2dbf97a846b377e777327182cbf4afeffe8ac345` |
| **BDS-PPP-B2b-1.0** | BeiDou PPP-B2b Precise Point Positioning SIS ICD | China Satellite Navigation Office | v1.0 | 2020-07 | Uncleared (as above) | `a70c08357b420f0fb662e5bc02a15df6a66ff6ab5c40686bbb7d5a763791d742` |
| **BDS-BDSBAS-B1C-1.0** | BeiDou SBAS (BDSBAS-B1C) SIS ICD | China Satellite Navigation Office | v1.0 | 2020-07 | Uncleared (as above) | `22ae917b179c55ab460489c895f538cc6996ef4870eb8eb0532737feb07328ca` |

`GLO-ICD-5.1` is fetched from the University of New Brunswick GNSS mirror
(`gauss.gge.unb.ca/GLONASS.ICD.pdf`), byte-identical to the RISDE 2008 original; the issuer's
own host (`russianspacesystems.ru`) serves it behind a Russian-government CA the default trust
store rejects.

---

## 3. Reference-by-citation only — **not obtained / not redistributable**

Cited by code where relevant, but the document itself is never downloaded or committed.

### 3a. Paid / copyrighted standards (obtain from the issuer)

| cite-key | title | issuer | where to obtain |
|---|---|---|---|
| **RTCA-DO-229** | SBAS MOPS — the definitive WAAS/SBAS L1 message format | RTCA | rtca.org (paid) |
| **RTCA-DO-401** | DFMC SBAS MOPS (L5 dual-frequency) | RTCA | rtca.org (paid) |
| **ICAO-ANNEX-10** | Aeronautical Telecommunications — SARPs (SBAS definition) | ICAO | icao.int (paid) |
| **RTCM-10403** | RTCM differential/RTK message format (used by the rtcm ingest connector) | RTCM | rtcm.org (paid) |
| **NMEA-0183** | NMEA sentence format | NMEA | nmea.org (paid) |

### 3b. Could not be fetched from an authoritative source (record only)

| cite-key | title | issuer | note |
|---|---|---|---|
| **GLO-CDMA-GENDESC** | GLONASS CDMA "General Description of Code Division Multiple Access Signal System" (Ed 1.0, 2016) | JSC Russian Space Systems | Carries a worked numeric orbit-integration example useful for a GLONASS propagator truth vector (see regression fix). **Not fetchable non-interactively — obtain manually.** The issuer (`russianspacesystems.ru`, direct path `/wp-content/uploads/2016/08/ICD-GLONASS-CDMA-General.-Edition-1.0-2016.pdf`) sits behind a Russian-government CA the default trust store rejects *and*, even with the cert bypassed (`curl -k`), returns a hard reverse proxy **403 Forbidden** to non-Russian requests (geo/WAF block — reproduced with a browser UA + Referer, 2026-07-17: everyone off a Russian network gets the 403). The Scribd mirror (doc 785928725) is gated behind a JavaScript "Client Challenge" bot-wall, so curl only sees the challenge stub. Acquire from a Russian-network vantage or a JS-capable browser session. |
| **GLO-CDMA-L1OC** | GLONASS CDMA open-service L1OC signal ICD (Ed 1.0, 2016) | JSC Russian Space Systems | Same host/CA/403 and Scribd-JS-wall limitations as above (Scribd doc 785928728). |

### 3c. Vendor / receiver protocol documents (proprietary — cite, don't redistribute)

Match the doc to the receiver generation; pull from the vendor.

| cite-key | title | vendor |
|---|---|---|
| **UBX-PROTOCOL** | u-blox UBX protocol / interface description (UBX-RXM-SFRBX, -RAWX, MON-RF, etc.) | u-blox |
| **SBF-REF** | Septentrio Binary Format (SBF) reference guide | Septentrio |
| **NOVATEL-OEM7** | NovAtel OEM7 commands & logs reference | NovAtel |
| **TRIMBLE-*** | Trimble receiver protocol docs | Trimble |

---

## 4. Findings → citation status

This table records specification checks against the cited documents. It is a
citation record; implementation status is documented in the component guides.

| finding | spec value | source now backing it | status |
|---|---|---|---|
| **regression fix** | GPS/QZSS CNAV `URA_ED` is signed two's-complement (+15..−16) | IS-GPS-200N **§30.3.3.1.1.4** (verbatim confirmed). Code comment cited §30.3.3.1.1.2 (Signal Health) — **corrected** to §30.3.3.1.1.4 (Elevation-Dependent Accuracy) | ✅ backed + citation fixed |
| **regression fix** | Galileo I/NAV clock is (E1,E5b) → uses BGD(E1,E5b) | GAL-OS-SIS-ICD-2.2 **Table 71** + Word Type 5 **Table 46** (confirmed: I/NAV clock model = (E1,E5b); BGD_E1E5a@47, BGD_E1E5b@57). Citations moved from Issue 2.1 to the vendored 2.2 | ✅ backed + citation fixed |
| **regression fix** | GPS LNAV HOW TOW = 17 MSBs at start of **next** subframe | IS-GPS-200N §20.3.3.2 (verbatim confirmed). Code comment already correct | ✅ backed |
| **regression fix** | GPS CNAV requires `toe == toc` in a data set | IS-GPS-200N §30.3.4.4 (LNAV analog §… confirmed verbatim: "The toe shall be equal to the toc of the same LNAV CEI data set") | ✅ backed |
| **regression fix** | Klobuchar azimuth `A` evaluated in **radians** (not semicircles) | IS-GPS-200N §20.3.3.5.2.5. Code (iono.go) cites this and documents radians; **docs/MATH.md §7.1**'s input list now labels `A` in radians with the regression fix note — the "(semicircles)" mislabel is gone | ✅ backed (code and docs both correct) |
| **regression fix** | GLONASS frame-5 strings 14/15 carry **B1/B2/KP** (UT1/leap), not almanac | GLO-ICD-5.1 §4.5, **Figure 4.2b** (5th-frame structure: string 14 = B1 B2 KP) + §4.3.1 ("The 5th frame contains remainder of almanac for 4 satellites") | ✅ ICD fact confirmed (logic fix deferred) |
| **regression fix** | GLONASS string Hamming check + Galileo I/NAV CRC-24Q | GLO-ICD-5.1 §4.7 **Table 4.13**; GAL-OS-SIS-ICD-2.2 §4.3 (CRC-24Q) | ✅ backed (logic fix deferred) |
| **regression fix (QZSS A_REF)** | QZSS CNAV reference semi-major axis 42164200.0 m | QZSS-PNT-006 **§5.6.2 (CNAV2(L1C) and CNAV(L2C,L5)), Table 5.6.2-2** (verbatim confirmed: `AREF = 42164200` m; the same table lists Ω̇_REF = −2.6×10⁻⁹ semicircles/s — identical to GPS MT11, so `cnavOmgD` is intentionally shared). Code comment cited the stale IS-QZSS-PNT-005 Table 4.3.2-16 — **corrected** to the vendored PNT-006 table | ✅ backed + citation fixed |
| **regression fix** | External truth vectors for the Kepler / GLONASS propagators | RINEX BRDC + SP3 harness (`gnss/truth_test.go`) is implemented; the formats are now vendored (**RINEX-3.05**, **SP3-D**, §1a). Klobuchar 40°N/260°E vector: IS-GPS-200N §20.3.3.5.2.5. GLONASS worked example: **GLO-CDMA-GENDESC** (could not fetch — see §3b) | ◐ mostly backed; only the GLONASS worked-example doc still open |
| **regression fix** | SBAS PRN → provider assignments (120/124 = EGNOS; 122 = SouthPAN) | Not an ICD constant — the live **gps.gov / EUSPA SBAS PRN assignment table** (a web reference, not redistributed). Code comment already records the April-2024 PRN-124 reassignment | ✅ source identified (web table) |
| **regression fix (IRNWT epoch)** | NavIC IRNWT epoch = GST(0,0) = 1999-08-21T23:59:47 UTC; `gnsstime.SysNavIC` = `SysGalileo` | NAVIC-SPS-L5S **§5.7 "IRNSS System Time"** (verbatim confirmed: "start epoch shall be 00:00 UT on Sunday August 22nd 1999", "ahead of UTC by 13 leap seconds. (i.e. IRNSS time, August 22nd 1999, 00:00:00 corresponds to UTC time August 21st 1999, 23:59:47)" — the GST(0,0) instant, 13-s subtlety included). Constants: **App. B** prints the WGS-84 `μ = 3.986005e14` and `ωe = 7.2921151467e-5`, **App. A** prints `F = 4.442807633e-10` — the exact `physconst` NavIC row | ✅ backed + code comment now cites §5.7 |
| **regression fix (NavIC URA)** | NavIC independently specifies the GPS URA nominal-value formula, the N = 15 no-prediction sentinel, and the 2.8/5.7/11.3 m rounding advice | NAVIC-SPS-L5S **§6.2.1.4, Table 23** (verbatim confirmed: "If the value of N is 6 or less, X = 2(1 + N/2)", "If the value of N is 6 or more, but less than 15, X = 2(N - 2)", "For N = 1, 3, and 5, X should be rounded to 2.8, 5.7, and 11.3 meters, respectively", and 15 = "no accuracy prediction is available") — exactly as `accuracy.go`'s regression fix comment claims | ✅ backed |

