package frame

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// BeiDou B2a B-CNAV2 decoding (BDS-SIS-ICD-B2a v1.0 §6.2 Figures 6-1…6-15, §7
// Tables 7-5…7-10). Each frame carries a 288-bit message — PRN(6) MesType(6)
// SOW(18) data(234) CRC-24Q(24) — that the satellite LDPC(96,48)-encodes to 576
// symbols; the u-blox receiver decodes the LDPC and delivers exactly the 288
// information bits as one 9-word SFRBX (verified: 3737/3737 captured frames pass
// CRC-24Q as a plain 9-word big-endian bit stream). The ephemeris is split across
// message types 10 (Ephemeris I) and 11 (Ephemeris II); type 30 carries the
// clock, group delays, and the BDGIM ionosphere coefficients. Like GPS CNAV the
// ephemeris uses the ΔA parameterization with rate terms: A(tk) = A_ref + ΔA +
// Ȧ·tk, n = n₀ + Δn₀ + ½Δṅ₀·tk. All offsets and scales are ICD-cited; positions
// cross-validate against the B1I D1 decode of the same SVs (frame test).

// B-CNAV2 semi-major-axis reference values (ICD Table 7-8), metres.
const (
	bcnavArefMEO  = 27906100.0 // MEO
	bcnavArefIGSO = 42162200.0 // IGSO/GEO

	bcnavT0 = 300.0 // toe / toc step, seconds
)

// ErrBadCRC is returned when a frame fails its CRC-24Q check.
var ErrBadCRC = errors.New("frame: CRC-24Q check failed")

// errPairSOW is returned when message types 10 and 11 are not broadcast-adjacent
// (the ICD requires them broadcast continuously together; stitching a stale pair
// could mix elements across an ephemeris changeover).
var errPairSOW = errors.New("frame: B-CNAV2 type 10/11 not broadcast-adjacent")

// bcnavClkStaleSOW bounds |m10.SOW − mClk.SOW|. Types 10/11/30/31/32/33/34
// interleave across a repeating cycle of B-CNAV2 message types (one every 3 s
// frame), so the clock message is never more than one full cycle away from the
// ephemeris pair in normal operation; this is generous margin over that cycle
// (a small multiple of the Toe/Toc step) while still catching a clock message
// left over from well before the last changeover.
const bcnavClkStaleSOW = 10 * bcnavT0

// BeiDouBCNAV2 holds the decoded fields of one B-CNAV2 message.
type BeiDouBCNAV2 struct {
	PRN     int
	MesType int
	SOW     int

	// Message type 10.
	WN      int // BDT week number
	SatType int // 1 GEO, 2 IGSO, 3 MEO (ICD Table 7-8)
	IODE    int

	// Message types 11/30/34/40 health & integrity flags.
	HS     int // health status (2 bits)
	DIF    bool
	SIF    bool
	AIF    bool
	SISMAI int

	// Message type 30: clock, group delays, BDGIM.
	IODC    int
	clk     clock.Model
	TGDB2ap float64    // group delay, B2a pilot vs B3I reference, seconds
	ISCB2ad float64    // B2a data vs pilot component, seconds
	TGDB1Cp float64    // B1C pilot vs B3I, seconds
	BDGIM   [9]float64 // α1..α9, TECu (ICD Table 7-10; α5 carries the −2⁻³ scale)

	eph     kepler.Ephemeris
	hasEph2 bool
	hasClk  bool
}

// DecodeBeiDouBCNAV2 decodes one B-CNAV2 message (nine words = the 288-bit
// information block). The CRC-24Q over the leading 264 bits must match the
// trailing 24; a failing frame is rejected, never partially decoded.
func DecodeBeiDouBCNAV2(words []uint32) (*BeiDouBCNAV2, error) {
	if len(words) < 9 {
		return nil, ErrShortFrame
	}
	buf := make([]byte, 36) // 9 words × 32 bits = 288 bits
	for i := 0; i < 9; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	if !CheckCRC24Q(buf) {
		return nil, ErrBadCRC
	}
	r := NewBitReaderN(buf, 288)
	u := func(p, n int) uint64 { v, _ := r.Bits(p, n); return v }
	s := func(p, n int) int64 { v, _ := r.Signed(p, n); return v }

	m := &BeiDouBCNAV2{
		PRN:     int(u(0, 6)),
		MesType: int(u(6, 6)),
		// SOW is transmitted in 3-second units — one B-CNAV2 frame — and
		// denotes the current frame's preamble edge (ICD Table 7-2, §7.3).
		SOW: int(u(12, 18)) * 3,
	}
	// The per-signal integrity flags follow the header in every defined type:
	// type 10 has WN first (Fig 6-3); types 11/30/31/32/33/34/40 have HS first
	// (Figs 6-4…6-10). SISMAI sits between the B2a and B1C flag triplets; we
	// keep the B2a triplet (our signal) and SISMAI.
	flags := func(p int) int { // decode DIF/SIF/AIF(B2a)+SISMAI at p; return next offset
		m.DIF = u(p, 1) != 0
		m.SIF = u(p+1, 1) != 0
		m.AIF = u(p+2, 1) != 0
		m.SISMAI = int(u(p+3, 4))
		return p + 7 + 3 // skip the B1C DIF/SIF/AIF triplet
	}

	semi := physconst.Pi
	switch m.MesType {
	case 10: // Fig 6-3: WN(13) flags IODE(8) Ephemeris I (203)
		m.WN = int(u(30, 13))
		flags(43)
		m.IODE = int(u(53, 8))
		m.SatType = int(u(72, 2))
		aref := bcnavArefMEO
		if m.SatType != 3 {
			aref = bcnavArefIGSO
		}
		m.eph.Toe = float64(u(61, 11)) * bcnavT0
		m.eph.SqrtA = math.Sqrt(aref + float64(s(74, 26))*p2m9)
		m.eph.ADot = float64(s(100, 25)) * p2m21 // Ȧ, m/s
		m.eph.DeltaN = float64(s(125, 17)) * p2m44 * semi
		m.eph.DeltaNDot = float64(s(142, 23)) * p2m57 * semi // Δṅ₀, rad/s²
		m.eph.M0 = float64(s(165, 33)) * p2m32 * semi
		m.eph.Ecc = float64(u(198, 33)) * p2m34
		m.eph.Omega = float64(s(231, 33)) * p2m32 * semi
	case 11: // Fig 6-4: HS(2) flags Ephemeris II (222)
		m.HS = int(u(30, 2))
		flags(32)
		m.eph.Omega0 = float64(s(42, 33)) * p2m32 * semi
		m.eph.I0 = float64(s(75, 33)) * p2m32 * semi
		m.eph.OmegaDot = float64(s(108, 19)) * p2m44 * semi
		m.eph.IDot = float64(s(127, 15)) * p2m44 * semi
		m.eph.Cis = float64(s(142, 16)) * p2m30
		m.eph.Cic = float64(s(158, 16)) * p2m30
		m.eph.Crs = float64(s(174, 24)) * p2m8
		m.eph.Crc = float64(s(198, 24)) * p2m8
		m.eph.Cus = float64(s(222, 21)) * p2m30
		m.eph.Cuc = float64(s(243, 21)) * p2m30
		m.hasEph2 = true
	case 30: // Fig 6-5: HS(2) flags clock(69) IODC(10) TGD/ISC BDGIM(74) TGD_B1Cp
		m.HS = int(u(30, 2))
		flags(32)
		m.clk = clock.Model{
			ID:  gnss.BeiDou,
			Toc: float64(u(42, 11)) * bcnavT0,
			Af0: float64(s(53, 25)) * p2m34,
			Af1: float64(s(78, 22)) * p2m50,
			Af2: float64(s(100, 11)) * p2m66,
		}
		m.IODC = int(u(111, 10))
		m.TGDB2ap = float64(s(121, 12)) * p2m34
		m.ISCB2ad = float64(s(133, 12)) * p2m34
		// BDGIM α1..α9 (Table 7-10): α1 10 bits unsigned; α2 8 signed; α3, α4
		// 8 unsigned; α5 8 unsigned with the −2⁻³ scale; α6..α9 8 signed. All
		// in TECu at 2⁻³ resolution.
		const p2m3 = 1.0 / 8.0
		m.BDGIM[0] = float64(u(145, 10)) * p2m3
		m.BDGIM[1] = float64(s(155, 8)) * p2m3
		m.BDGIM[2] = float64(u(163, 8)) * p2m3
		m.BDGIM[3] = float64(u(171, 8)) * p2m3
		m.BDGIM[4] = float64(u(179, 8)) * -p2m3
		for i := 0; i < 4; i++ {
			m.BDGIM[5+i] = float64(s(187+8*i, 8)) * p2m3
		}
		m.TGDB1Cp = float64(s(219, 12)) * p2m34
		m.hasClk = true
	case 34: // Fig 6-9: HS(2) flags SISAI_oc(22) clock(69) IODC(10) BDT-UTC(97)
		m.HS = int(u(30, 2))
		flags(32)
		m.clk = clock.Model{
			ID:  gnss.BeiDou,
			Toc: float64(u(64, 11)) * bcnavT0,
			Af0: float64(s(75, 25)) * p2m34,
			Af1: float64(s(100, 22)) * p2m50,
			Af2: float64(s(122, 11)) * p2m66,
		}
		m.IODC = int(u(133, 10))
		m.hasClk = true
	case 40: // Fig 6-10: HS(2) flags SISAI_oe(5) SISAI_oc(22) midi almanac(156)
		m.HS = int(u(30, 2))
		flags(32)
	}
	return m, nil
}

// AssembleBeiDouBCNAV2 combines message types 10 and 11 (Ephemeris I + II) and,
// if present, a clock-bearing message (type 30 or 34) into the ephemeris and
// clock model. Types 10 and 11 are broadcast continuously together (ICD §6.2.3)
// and type 11 carries no IODE, so pairing is validated by broadcast adjacency:
// the two SOWs must be within one frame (3 s) of each other. svid tags the
// constellation; kepler applies the GEO rotation for C01–C05/C59–C63.
//
// mClk freshness : unlike the m10/m11 pairing check above, a stale mClk
// does not fail the whole assembly — an ephemeris update must not be blocked
// forever just because this SV's type-30/34 stopped decoding. Instead a mClk
// whose SOW has drifted too far from m10's is treated as if it were absent
// (falls back to the zero clock model below), and the caller keeps trying with
// whatever type-30/34 arrives next.
func AssembleBeiDouBCNAV2(svid int, m10, m11, mClk *BeiDouBCNAV2) (kepler.Ephemeris, clock.Model, error) {
	if m10 == nil || m11 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	if d := m10.SOW - m11.SOW; d < -3 || d > 3 {
		return kepler.Ephemeris{}, clock.Model{}, errPairSOW
	}
	if mClk != nil && mClk.hasClk {
		if d := m10.SOW - mClk.SOW; d < -bcnavClkStaleSOW || d > bcnavClkStaleSOW {
			mClk = nil // stale: don't pair a cached-old clock with this ephemeris
		}
	}
	eph := m10.eph
	eph.Omega0, eph.I0, eph.OmegaDot, eph.IDot = m11.eph.Omega0, m11.eph.I0, m11.eph.OmegaDot, m11.eph.IDot
	eph.Cis, eph.Cic, eph.Crs, eph.Crc, eph.Cus, eph.Cuc = m11.eph.Cis, m11.eph.Cic, m11.eph.Crs, m11.eph.Crc, m11.eph.Cus, m11.eph.Cuc
	eph.ID = gnss.BeiDou
	eph.SVID = svid
	clk := clock.Model{ID: gnss.BeiDou}
	if mClk != nil && mClk.hasClk {
		clk = mClk.clk
		// The B2a pilot user's group delay (ICD §7.6.2 eq. 7-4); the data
		// component additionally applies ISC_B2ad (eq. 7-5), kept on the struct.
		clk.TGD = mClk.TGDB2ap
	}
	return eph, clk, nil
}
