package frame

import (
	"encoding/binary"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// Galileo E5a F/NAV decoding (GAL-OS-SIS-ICD-2.2 §4.2). u-blox delivers each
// F/NAV page as one 8-word SFRBX (256-bit block); the page type is the first 6
// bits and the clock (page 1) + ephemeris (pages 2/3/4) fields follow at fixed
// offsets in that block. F/NAV carries the same ephemeris as E1-B I/NAV, on the
// E5a signal — decoding it enables the cross-signal integrity check. The
// transmitted CRC remains present in SFRBX and is verified centrally.
//
// Every field offset here was pinned by decoding the same SV via the (validated)
// I/NAV path and matching the raw values bit-for-bit — the F/NAV position agrees
// with I/NAV to the metre (TestRealGalileoFNAVAgreesWithINAV).

// GalileoFNAV holds the decoded fields of one F/NAV page.
type GalileoFNAV struct {
	PageType int
	IODnav   int
	SISA     int // page 1 only, SISA(E1,E5a)
	E5aHS    int // page 1 only, E5a Signal Health Status
	// E5aDVS is the E5a Data Validity Status (page 1 only, regression fix):
	// 0 = navigation data valid, 1 = "Working without guarantee"
	// (GAL-OS-SIS-ICD-2.2 Table 79/81). The ICD gives E5a TWO per-signal
	// integrity flags — SHS and DVS — and neither alone suffices: an SV in a
	// maintenance window can broadcast E5aHS=0 with E5aDVS=1. Decoded so the
	// state layer has both halves when the regression fix health-contract decision
	// (how DVS maps into the frozen health enum) lands; until then it is
	// deliberately decoded-but-unserved, like I/NAV's E1B/E5b DVS bits.
	E5aDVS int
	// WN/TOW (page 1 only, regression fix): the live broadcast GST week (12 bits) and
	// time-of-week (20 bits), plain integer counts (GAL-OS-SIS-ICD-2.2 Table
	// 69 via Table 30 — WN@155, TOW@167). Same semantics and caveats as
	// GalileoINAV's word-5 pair; consumed by the state layer's
	// broadcast-vs-receiver GST week cross-check on the @3 entry.
	WN  int
	TOW float64
	// BGDE1E5a is the raw broadcast E1-E5a group delay (page 1 only, regression fix),
	// seconds — 10-bit two's complement × 2⁻³² (GAL-OS-SIS-ICD-2.2 Table 30
	// position, Table 72 coding). The F/NAV clock is the (E1,E5a) pair (Table
	// 71) and its single-frequency service is E5a — the f2 user — so the value
	// APPLIED to the assembled clock's TGD is this × E5aGroupDelayFactor (Eq.
	// 19); this field keeps the unscaled broadcast value, mirroring
	// GalileoINAV.BGDE1E5a, for consumers that need the raw parameter (an E1
	// user of the F/NAV clock, Eq. 18, would apply it unscaled).
	BGDE1E5a float64
	// GGTO — the GST-GPS conversion parameters (page 4 only, regression fix), the
	// F/NAV twin of I/NAV word 10's set; same Eq. 24 / Table 76 semantics and
	// the same all-ones "not valid" rule (§5.1.8). See GalileoINAV's GGTO doc.
	// NB Table 33's field ORDER differs from I/NAV Table 51: F/NAV transmits
	// t0G BEFORE A0G (…ΔtLSF(8) t0G@147(8) A0G@155(16) A1G@171(12) WN0G@183(6)…).
	HasGGTO   bool
	GGTOValid bool
	A0G       float64 // s, two's complement ×2⁻³⁵
	A1G       float64 // s/s, two's complement ×2⁻⁵¹
	T0G       float64 // s, ×3600
	WN0G      int     // weeks, 6-bit truncated
	eph       kepler.Ephemeris
	clk       clock.Model
	hasClk    bool
}

// StampGalileoFNAVCRC computes and writes the F/NAV CRC-24Q into a synthetic
// eight-word page. OS-SIS-ICD 2.2 §4.2.2.3 protects the first 214 bits and
// transmits the checksum in bits 214..237. The first eight words are mutated in
// place; a shorter slice is left unchanged.
func StampGalileoFNAVCRC(words []uint32) {
	if len(words) < 8 {
		return
	}
	buf := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	crc := CRC24QBits(buf, 0, 214)
	for i := 0; i < 24; i++ {
		p := 214 + i
		mask := byte(1) << uint(7-(p&7))
		if crc&(1<<uint(23-i)) != 0 {
			buf[p>>3] |= mask
		} else {
			buf[p>>3] &^= mask
		}
	}
	for i := 0; i < 8; i++ {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
}

// DecodeGalileoFNAV decodes one F/NAV page (eight words).
func DecodeGalileoFNAV(words []uint32) (*GalileoFNAV, error) {
	if len(words) < 8 {
		return nil, ErrShortFrame
	}
	buf := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	// OS-SIS-ICD 2.2 §4.2.2.3: PageType+NavData (214 bits) followed
	// by the transmitted 24-bit CRC. Tail/spare bits after bit 237 are excluded.
	if !CheckCRC24QBits(buf, 0, 238) {
		return nil, ErrBadCRC
	}
	r := NewBitReaderN(buf, 256)
	u := func(p, n int) uint64 { v, _ := r.Bits(p, n); return v }
	s := func(p, n int) int64 { v, _ := r.Signed(p, n); return v }

	pt := int(u(0, 6))
	w := &GalileoFNAV{PageType: pt, IODnav: int(u(6, 10))}
	semi := physconst.Pi
	switch pt {
	case 1:
		// Type(6) SVID(6) IODnav(10) t0c(14) af0(31) af1(21) af2(6) SISA(8) ai0(11)
		// ai1(11) ai2(14) Region1-5(5) BGD(E1,E5a)(10) E5aHS(2) WN(12) TOW(20)
		// E5aDVS(1) Spare(26) CRC(24) Tail(6) — GAL-OS-SIS-ICD-2.2 Table 30.
		// the leading 6-bit page Type was missing from this named list,
		// so it summed to 238 against the 244 ledger below it. Cumulative
		// offsets from that ledger: BGD@143, E5aHS@153, WN@155, TOW@167, E5aDVS@187
		// (6+6+10+14+31+21+6+8+11+11+14+5 = 143; +10+2 = 155; +12 = 167; +20 = 187).
		// regression fix added SISA/E5aHS; regression fix added E5aDVS; the ionospheric (NeQuick
		// ai0/ai1/ai2) fields remain undecoded — no NeQuick model exists in
		// gnss/iono yet (additional constellation models are planned).
		w.IODnav = int(u(12, 10))
		w.SISA = int(u(94, 8))
		w.E5aHS = int(u(153, 2))
		w.E5aDVS = int(u(187, 1)) // Table 81 — 0 valid, 1 working without guarantee
		w.WN = int(u(155, 12))    // GST week/TOW, unscaled integer counts (Table 69)
		w.TOW = float64(u(167, 20))
		w.BGDE1E5a = float64(s(143, 10)) * p2m32
		w.clk = clock.Model{
			ID:  gnss.Galileo,
			Toc: float64(u(22, 14)) * galT0,
			Af0: float64(s(36, 31)) * p2m34,
			Af1: float64(s(67, 21)) * p2m46,
			Af2: float64(s(88, 6)) * p2m59,
			// Model.TGD's contract is "group delay for the tracked
			// signal, ALREADY SCALED". The tracked signal here is E5a (this
			// decoder feeds the daemon's E##@3 entries), the f2 of the (E1,E5a)
			// clock pair, so Eq. 19 applies: (f_E1/f_E5a)²·BGD(E1,E5a) — unlike
			// the I/NAV path, whose tracked E1 is the f1 user (Eq. 18, unscaled).
			// Previously this field shipped 0, silently biasing the assembled
			// @3 clock by the whole group delay (metre-scale in range) and
			// arming the future I/NAV-vs-F/NAV comparison with a built-in
			// ≈BGD false clock offset — exactly as latent note predicted.
			TGD: float64(s(143, 10)) * p2m32 * clock.E5aGroupDelayFactor,
		}
		w.hasClk = true
	case 2: // M0, Ω̇, e, √A, Ω0, IDOT
		w.eph.M0 = float64(s(16, 32)) * p2m31 * semi
		w.eph.OmegaDot = float64(s(48, 24)) * p2m43 * semi
		w.eph.Ecc = float64(u(72, 32)) * p2m33
		w.eph.SqrtA = float64(u(104, 32)) * p2m19
		w.eph.Omega0 = float64(s(136, 32)) * p2m31 * semi
		w.eph.IDot = float64(s(168, 14)) * p2m43 * semi
	case 3: // i0, ω, Δn, Cuc, Cus, Crc, Crs, t0e
		w.eph.I0 = float64(s(16, 32)) * p2m31 * semi
		w.eph.Omega = float64(s(48, 32)) * p2m31 * semi
		w.eph.DeltaN = float64(s(80, 16)) * p2m43 * semi
		w.eph.Cuc = float64(s(96, 16)) * p2m29
		w.eph.Cus = float64(s(112, 16)) * p2m29
		w.eph.Crc = float64(s(128, 16)) * p2m5
		w.eph.Crs = float64(s(144, 16)) * p2m5
		w.eph.Toe = float64(u(160, 14)) * galT0
	case 4:
		// Cic, Cis + GST-UTC + GST-GPS conversion + TOW — Table 33: Type(6)
		// IODnav(10) Cic(16) Cis(16) A0(32) A1(24) ΔtLS(8) t0t(8) WN0t(8)
		// WNLSF(8) DN(3) ΔtLSF(8) t0G(8) A0G(16) A1G(12) WN0G(6) TOW(20)
		// Spare(5) CRC(24) Tail(6) = 244 → cumulative GGTO offsets t0G@147,
		// A0G@155, A1G@171, WN0G@183 (6+10+16+16+32+24+8+8+8+8+3+8 = 147;
		// +8 = 155; +16 = 171; +12 = 183 — regression fix; the GST-UTC block remains
		// undecoded, the standing regression fix broadcast-UTC work).
		w.eph.Cic = float64(s(16, 16)) * p2m29
		w.eph.Cis = float64(s(32, 16)) * p2m29
		a0gRaw := u(155, 16)
		a1gRaw := u(171, 12)
		t0gRaw := u(147, 8)
		wn0gRaw := u(183, 6)
		w.HasGGTO = true
		// §5.1.8's four-field all-ones withdrawal sentinel — see the I/NAV twin.
		w.GGTOValid = !(a0gRaw == 0xFFFF && a1gRaw == 0xFFF && t0gRaw == 0xFF && wn0gRaw == 0x3F)
		w.A0G = float64(s(155, 16)) * p2m35
		w.A1G = float64(s(171, 12)) * p2m51
		// regression fix (t0G plausibility — a deliberate non-check, see the I/NAV twin
		// for the full rationale): 8 bits × 3600 s reaches 918 000 s, so raws
		// 168–255 name an epoch past the 604 800 s week. Table 76 gives t0G only
		// bits/scale/unit — it prints no range column and no restriction — and
		// §5.1.8's only defined sentinel is the four-field all-ones withdrawal
		// above, so we accept and serve the raw value by contract rather than
		// invent a bound. Worst case via Eq. 24's A1G·dt term: 313 200 s of
		// excess × |A1G|max (2¹¹×2⁻⁵¹ s/s) ≈ 0.28 µs on gps_offset_ns.
		w.T0G = float64(t0gRaw) * galT0G
		w.WN0G = int(wn0gRaw)
	}
	return w, nil
}

// ClockTGD returns the already-scaled (Eq. 19) group delay carried by this
// page's clock model — meaningful for page 1 only, zero otherwise. Exposed for
// the state layer's freshest-wins TGD fold : BGD is outside the
// IODnav-covered data set, so a BGD revision must reach an already-assembled
// clock without re-assembly, and the caller must not re-derive the Eq. 19
// scaling policy that lives in DecodeGalileoFNAV.
func (w *GalileoFNAV) ClockTGD() float64 { return w.clk.TGD }

// AssembleGalileoFNAV combines pages 1–4 (matching IODnav) into the ephemeris and
// clock model. svid tags the constellation.
func AssembleGalileoFNAV(svid int, p1, p2, p3, p4 *GalileoFNAV) (kepler.Ephemeris, clock.Model, error) {
	if p1 == nil || p2 == nil || p3 == nil || p4 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	if p1.PageType != 1 || p2.PageType != 2 || p3.PageType != 3 || p4.PageType != 4 {
		return kepler.Ephemeris{}, clock.Model{}, ErrWrongMsgType
	}
	if p1.IODnav != p2.IODnav || p2.IODnav != p3.IODnav || p2.IODnav != p4.IODnav {
		return kepler.Ephemeris{}, clock.Model{}, errIODMismatch
	}
	eph := p2.eph
	eph.I0, eph.Omega, eph.DeltaN = p3.eph.I0, p3.eph.Omega, p3.eph.DeltaN
	eph.Cuc, eph.Cus, eph.Crc, eph.Crs, eph.Toe = p3.eph.Cuc, p3.eph.Cus, p3.eph.Crc, p3.eph.Crs, p3.eph.Toe
	eph.Cic, eph.Cis = p4.eph.Cic, p4.eph.Cis
	eph.ID = gnss.Galileo
	eph.SVID = svid
	clk := p1.clk
	clk.ID = gnss.Galileo
	return eph, clk, nil
}
