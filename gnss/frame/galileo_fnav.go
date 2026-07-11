package frame

import (
	"encoding/binary"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// Galileo E5a F/NAV decoding (OS-SIS-ICD Issue 2.1 §4.2). u-blox delivers each
// F/NAV page as one 8-word SFRBX (256-bit block); the page type is the first 6
// bits and the clock (page 1) + ephemeris (pages 2/3/4) fields follow at fixed
// offsets in that block. F/NAV carries the same ephemeris as E1-B I/NAV, on the
// E5a signal — decoding it enables the cross-signal integrity check. The receiver
// has validated the page CRC, so fields are read directly.
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
	eph      kepler.Ephemeris
	clk      clock.Model
	hasClk   bool
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
	r := NewBitReaderN(buf, 256)
	u := func(p, n int) uint64 { v, _ := r.Bits(p, n); return v }
	s := func(p, n int) int64 { v, _ := r.Signed(p, n); return v }

	pt := int(u(0, 6))
	w := &GalileoFNAV{PageType: pt, IODnav: int(u(6, 10))}
	semi := physconst.Pi
	switch pt {
	case 1:
		// SVID(6) IODnav(10) t0c(14) af0(31) af1(21) af2(6) SISA(8) ai0(11) ai1(11)
		// ai2(14) Region1-5(5) BGD(E1,E5a)(10) E5aHS(2) WN(12) TOW(20) E5aDVS(1)
		// Spare(26) CRC(24) Tail(6) — OS-SIS-ICD Issue 2.1 Table 28 (SISA
		// and E5aHS added; ionospheric/GST/DVS fields are out of this fix's scope).
		w.IODnav = int(u(12, 10))
		w.SISA = int(u(94, 8))
		w.E5aHS = int(u(153, 2))
		w.clk = clock.Model{
			ID:  gnss.Galileo,
			Toc: float64(u(22, 14)) * galT0,
			Af0: float64(s(36, 31)) * p2m34,
			Af1: float64(s(67, 21)) * p2m46,
			Af2: float64(s(88, 6)) * p2m59,
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
	case 4: // Cic, Cis (+ GST, spare)
		w.eph.Cic = float64(s(16, 16)) * p2m29
		w.eph.Cis = float64(s(32, 16)) * p2m29
	}
	return w, nil
}

// AssembleGalileoFNAV combines pages 1–4 (matching IODnav) into the ephemeris and
// clock model. svid tags the constellation.
func AssembleGalileoFNAV(svid int, p1, p2, p3, p4 *GalileoFNAV) (kepler.Ephemeris, clock.Model, error) {
	if p1 == nil || p2 == nil || p3 == nil || p4 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	if p2.IODnav != p3.IODnav || p2.IODnav != p4.IODnav {
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
