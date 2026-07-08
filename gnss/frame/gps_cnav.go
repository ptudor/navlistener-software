package frame

import (
	"encoding/binary"
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
