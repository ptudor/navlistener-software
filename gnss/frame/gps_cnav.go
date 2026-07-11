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

// GPS/QZSS L2C·L5 CNAV decoding (IS-GPS-200 §30.3.3; IS-QZSS mirrors it). u-blox
// delivers each 300-bit CNAV message as one 10-word SFRBX, packed MSB-first across
// the full 32-bit words (preamble 0x8B in word 0's top byte). Ephemeris is split
// across message types 10 and 11; the clock is in message types 30–37. Unlike
// LNAV, CNAV uses the ΔA parameterization: A = A_ref + ΔA. Fields and offsets are
// the documented IS-GPS-200 Table 30-I/II/III layout, confirmed against real
// ZED-F9P frames (a≈26560 km, i₀≈55°, and CNAV position agrees with LNAV to <5 m).
//
// Unlike LNAV/I-NAV, no documented u-blox guarantee was found (regression fix, investigated
// rather than assumed) that RXM-SFRBX delivers only CRC-validated CNAV words —
// LNAV's "receiver already validated parity" claim above is specific to that
// format's D30*-corrected word delivery, and no equivalent statement exists for
// L2C/L5 CNAV in this codebase's history or any consulted reference. Push-path
// frames also arrive from remote feeders, not just a directly-dialed receiver.
// DecodeGPSCNAV therefore validates the preamble and CRC-24Q itself rather than
// trusting the source, matching BeiDou B-CNAV2's ErrBadCRC-rejection precedent.

// ErrBadPreamble is returned when a CNAV message's leading byte isn't 0x8B.
var ErrBadPreamble = errors.New("frame: CNAV preamble mismatch")

// CNAV scale factors and reference constants beyond the shared set.
const (
	p2m8  = 1.0 / (1 << 8)
	p2m9  = 1.0 / (1 << 9)
	p2m21 = 1.0 / (1 << 21)
	p2m30 = 1.0 / (1 << 30)
	p2m32 = 1.0 / float64(uint64(1)<<32)
	p2m35 = 1.0 / float64(uint64(1)<<35)
	p2m44 = 1.0 / float64(uint64(1)<<44)
	p2m48 = 1.0 / float64(uint64(1)<<48)
	p2m57 = 1.0 / float64(uint64(1)<<57)
	p2m60 = 1.0 / float64(uint64(1)<<60)

	cnavAref = 26559710.0 // GPS/QZSS reference semi-major axis, metres
	cnavOmgD = -2.6e-9    // reference rate of right ascension, semicircles/s
	cnavT0   = 300.0      // toe/toc step, seconds
)

// GPSCNAV holds the decoded fields of one CNAV message.
type GPSCNAV struct {
	MsgType int
	PRN     int
	TOW     float64
	eph     kepler.Ephemeris
	clk     clock.Model
	hasEph2 bool // message 11 present (has i0/Ω0)
	hasClk  bool

	// Message-10-only integrity fields : WN (bits 39-51), the 3-bit L1/L2/L5
	// health flags (52-54), and URA_ED (66-70) — captured for LNAV (GPSSubframe.WN/
	// Health/URAIndex) but previously dropped for CNAV. Purely additive; ephemeris/
	// clock field offsets above are unchanged. Populated only when MsgType == 10.
	WN     int
	Health int // raw 3-bit L1/L2/L5 signal-health field (IS-GPS-200 §30.3.3.1.1.2), unmasked
	URAED  int

	// Message-30-only group delay differential correction terms : T_GD is set
	// directly into clk.TGD (IS-GPS-200 Table 30-IV; 13 bits, 2⁻³⁵ — wider than LNAV's
	// 8-bit T_GD, so it is NOT the same field width/scale), packed contiguously right
	// after Af2 (bits 117-126), so T_GD starts at bit 127 — confirmed against this
	// codebase's existing, already-verified Toc/Af0/Af1/Af2 offsets, each of which
	// starts exactly where the previous field ends. The four ISCs immediately follow
	// T_GD and are captured here, additive: which ISC applies is signal-pair-specific
	// (IS-GPS-200 §30.3.3.3.1.1) and folding one into the generic clock polynomial
	// would be wrong for every signal that doesn't use it, so none is applied
	// automatically. Populated only when MsgType == 30.
	ISCL1CA float64
	ISCL2C  float64
	ISCL5I5 float64
	ISCL5Q5 float64
}

// DecodeGPSCNAV decodes one CNAV message (ten words). id is GPS or QZSS (they
// share the format and constants).
func DecodeGPSCNAV(id gnss.GNSSID, words []uint32) (*GPSCNAV, error) {
	if len(words) < 10 {
		return nil, ErrShortFrame
	}
	buf := make([]byte, 40) // 10 words × 32 bits = 320 bits (300 used)
	for i := 0; i < 10; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	if buf[0] != 0x8B { // preamble check
		return nil, ErrBadPreamble
	}
	if !CheckCRC24QBits(buf, 0, 300) { // CRC-24Q over the full 300-bit message
		return nil, ErrBadCRC
	}
	r := NewBitReaderN(buf, 320)
	u := func(p, n int) uint64 { v, _ := r.Bits(p, n); return v }
	s := func(p, n int) int64 { v, _ := r.Signed(p, n); return v }

	m := &GPSCNAV{
		MsgType: int(u(14, 6)),
		PRN:     int(u(8, 6)),
		TOW:     float64(u(20, 17)) * 6,
	}
	semi := physconst.Pi
	switch {
	case m.MsgType == 10: // Ephemeris 1
		// WN/Health/URA_ED : IS-GPS-200 Table 30-I cites these as 1-indexed bit
		// numbers 39-51/52-54/66-70; BitReader.Bits uses 0-indexed absolute offsets (one
		// less, the same translation PRN/MsgType/TOW above already use — e.g. PRN's
		// ICD "bits 9-14" is coded as u(8,6)). URA_ED's 0-indexed span (65-69) ends
		// exactly where the existing, already-verified Toe field begins (u(70, 11)),
		// which is the cross-check that the offsets below are right.
		m.WN = int(u(38, 13))
		m.Health = int(u(51, 3))
		// URA_ED is a SIGNED two's-complement integer (+15..−16), IS-GPS-200N
		// §30.3.3.1.1.2 — not unsigned. A negative index (URA < 2.4 m, routine for modern
		// SVs) was mis-decoded (e.g. bits 11111 = −1 read as 31) over half the domain.
		m.URAED = int(s(65, 5))
		m.eph.Toe = float64(u(70, 11)) * cnavT0
		m.eph.SqrtA = math.Sqrt(cnavAref + float64(s(81, 26))*p2m9)
		m.eph.ADot = float64(s(107, 25)) * p2m21 // Ȧ, m/s (Table 30-I)
		m.eph.DeltaN = float64(s(132, 17)) * p2m44 * semi
		m.eph.DeltaNDot = float64(s(149, 23)) * p2m57 * semi // Δṅ₀, rad/s²
		m.eph.M0 = float64(s(172, 33)) * p2m32 * semi
		m.eph.Ecc = float64(u(205, 33)) * p2m34
		m.eph.Omega = float64(s(238, 33)) * p2m32 * semi
	case m.MsgType == 11: // Ephemeris 2
		m.eph.Toe = float64(u(38, 11)) * cnavT0
		m.eph.Omega0 = float64(s(49, 33)) * p2m32 * semi
		m.eph.I0 = float64(s(82, 33)) * p2m32 * semi
		m.eph.OmegaDot = (cnavOmgD + float64(s(115, 17))*p2m44) * semi
		m.eph.IDot = float64(s(132, 15)) * p2m44 * semi
		m.eph.Cis = float64(s(147, 16)) * p2m30
		m.eph.Cic = float64(s(163, 16)) * p2m30
		m.eph.Crs = float64(s(179, 24)) * p2m8
		m.eph.Crc = float64(s(203, 24)) * p2m8
		m.eph.Cus = float64(s(227, 21)) * p2m30
		m.eph.Cuc = float64(s(248, 21)) * p2m30
		m.hasEph2 = true
	case m.MsgType >= 30 && m.MsgType <= 37: // clock block (common to 30–37)
		m.clk = clock.Model{
			ID:  id,
			Toc: float64(u(60, 11)) * cnavT0,
			Af0: float64(s(71, 26)) * p2m35,
			Af1: float64(s(97, 20)) * p2m48,
			Af2: float64(s(117, 10)) * p2m60,
		}
		m.hasClk = true
		if m.MsgType == 30 { // group delay + ISCs : MT30-only, not common to 31-37
			// Contiguous with the clock block above (Af2 occupies bits 117-126, so
			// T_GD starts at 127) — confirmed against the existing, already-verified
			// Toc/Af0/Af1/Af2 offsets, each of which starts exactly where the
			// previous field ends.
			m.clk.TGD = float64(s(127, 13)) * p2m35
			m.ISCL1CA = float64(s(140, 13)) * p2m35
			m.ISCL2C = float64(s(153, 13)) * p2m35
			m.ISCL5I5 = float64(s(166, 13)) * p2m35
			m.ISCL5Q5 = float64(s(179, 13)) * p2m35
		}
	}
	return m, nil
}

// AssembleGPSCNAV combines ephemeris messages 10 and 11 (and, if present, a clock
// message 30–37) into the ephemeris and clock model. m10/m11 must share a toe.
func AssembleGPSCNAV(id gnss.GNSSID, svid int, m10, m11, mClk *GPSCNAV) (kepler.Ephemeris, clock.Model, error) {
	if m10 == nil || m11 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	if m10.eph.Toe != m11.eph.Toe {
		return kepler.Ephemeris{}, clock.Model{}, errIODMismatch
	}
	eph := m10.eph
	eph.Omega0, eph.I0, eph.OmegaDot, eph.IDot = m11.eph.Omega0, m11.eph.I0, m11.eph.OmegaDot, m11.eph.IDot
	eph.Cis, eph.Cic, eph.Crs, eph.Crc, eph.Cus, eph.Cuc = m11.eph.Cis, m11.eph.Cic, m11.eph.Crs, m11.eph.Crc, m11.eph.Cus, m11.eph.Cuc
	eph.ID = id
	eph.SVID = svid
	var clk clock.Model
	if mClk != nil {
		clk = mClk.clk
	}
	clk.ID = id
	return eph, clk, nil
}
