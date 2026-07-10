package frame

import (
	"encoding/binary"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// Galileo E1-B I/NAV decoding (OS-SIS-ICD Issue 2.1 §4.3). u-blox delivers each
// I/NAV nominal page as one UBX-RXM-SFRBX of eight 32-bit words = a 256-bit page:
// the even page part (words 0–3) then the odd page part (words 4–7), each starting
// with Even/Odd(1)+PageType(1). The 128-bit nav "word" is the even part's 112 data
// bits followed by the odd part's 16 data bits; word type is its first 6 bits.
// Verified against real ZED-F9T frames (radius ≈ 29600 km). The receiver already
// validated the CRC, so — as with GPS LNAV — we extract fields directly.

// Galileo I/NAV scale factors beyond the shared p2mNN set.
const (
	p2m34 = 1.0 / float64(uint64(1)<<34)
	p2m46 = 1.0 / float64(uint64(1)<<46)
	p2m59 = 1.0 / float64(uint64(1)<<59)
	galT0 = 60.0 // t0e / t0c step, seconds
)

// GalileoINAV holds the decoded fields of one I/NAV word. Only the fields for this
// word type are populated; a full ephemeris comes from word types 1–4 with a
// matching IODnav (AssembleGalileo).
type GalileoINAV struct {
	Type   int
	IODnav int
	SISA   int
	Health int // E1B health (word 5), when present
	eph    kepler.Ephemeris
	clk    clock.Model
	hasClk bool
}

// DecodeGalileoINAV decodes one I/NAV page (eight words) into its word fields.
func DecodeGalileoINAV(words []uint32) (*GalileoINAV, error) {
	if len(words) < 8 {
		return nil, ErrShortFrame
	}
	page := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(page[i*4:], words[i])
	}
	pr := NewBitReader(page)

	// Reconstruct the contiguous 128-bit nav word: even data (page bits 2..114)
	// then odd data (page bits 130..146). Some fields (e.g. √A) straddle the join,
	// so the content must be contiguous.
	content := make([]byte, 16)
	put := func(off int, v uint64, n int) {
		for i := 0; i < n; i++ {
			if v&(1<<uint(n-1-i)) != 0 {
				p := off + i
				content[p>>3] |= 1 << uint(7-(p&7))
			}
		}
	}
	e0, _ := pr.Bits(2, 56)
	e1, _ := pr.Bits(58, 56)
	od, _ := pr.Bits(130, 16)
	put(0, e0, 56)
	put(56, e1, 56)
	put(112, od, 16)

	r := NewBitReaderN(content, 128)
	wt, _ := r.Bits(0, 6)
	w := &GalileoINAV{Type: int(wt)}
	semi := physconst.Pi
	switch wt {
	case 1:
		iod, _ := r.Bits(6, 10)
		t0e, _ := r.Bits(16, 14)
		m0, _ := r.Signed(30, 32)
		ecc, _ := r.Bits(62, 32)
		sqrtA, _ := r.Bits(94, 32)
		w.IODnav = int(iod)
		w.eph.Toe = float64(t0e) * galT0
		w.eph.M0 = float64(m0) * p2m31 * semi
		w.eph.Ecc = float64(ecc) * p2m33
		w.eph.SqrtA = float64(sqrtA) * p2m19
	case 2:
		iod, _ := r.Bits(6, 10)
		omg0, _ := r.Signed(16, 32)
		i0, _ := r.Signed(48, 32)
		omega, _ := r.Signed(80, 32)
		idot, _ := r.Signed(112, 14)
		w.IODnav = int(iod)
		w.eph.Omega0 = float64(omg0) * p2m31 * semi
		w.eph.I0 = float64(i0) * p2m31 * semi
		w.eph.Omega = float64(omega) * p2m31 * semi
		w.eph.IDot = float64(idot) * p2m43 * semi
	case 3:
		iod, _ := r.Bits(6, 10)
		omgDot, _ := r.Signed(16, 24)
		dn, _ := r.Signed(40, 16)
		cuc, _ := r.Signed(56, 16)
		cus, _ := r.Signed(72, 16)
		crc, _ := r.Signed(88, 16)
		crs, _ := r.Signed(104, 16)
		sisa, _ := r.Bits(120, 8)
		w.IODnav = int(iod)
		w.SISA = int(sisa)
		w.eph.OmegaDot = float64(omgDot) * p2m43 * semi
		w.eph.DeltaN = float64(dn) * p2m43 * semi
		w.eph.Cuc = float64(cuc) * p2m29
		w.eph.Cus = float64(cus) * p2m29
		w.eph.Crc = float64(crc) * p2m5
		w.eph.Crs = float64(crs) * p2m5
	case 4:
		// Offsets: type(6) IODnav(10) SVID(6) Cic(16) Cis(16) t0c(14) af0(31)
		// af1(21) af2(6).
		iod, _ := r.Bits(6, 10)
		cic, _ := r.Signed(22, 16)
		cis, _ := r.Signed(38, 16)
		t0c, _ := r.Bits(54, 14)
		af0, _ := r.Signed(68, 31)
		af1, _ := r.Signed(99, 21)
		af2, _ := r.Signed(120, 6)
		w.IODnav = int(iod)
		w.eph.Cic = float64(cic) * p2m29
		w.eph.Cis = float64(cis) * p2m29
		w.clk = clock.Model{
			ID:  gnss.Galileo,
			Toc: float64(t0c) * galT0,
			Af0: float64(af0) * p2m34,
			Af1: float64(af1) * p2m46,
			Af2: float64(af2) * p2m59,
		}
		w.hasClk = true
	case 5:
		// Ionosphere, BGD, health. Layout after BGD_E1E5a(47-56): BGD_E1E5b(57-66),
		// E5b_HS(67-68), E1B_HS(69-70) per ICD — bit 67 is E5b health, not E1B health.
		bgd, _ := r.Signed(47, 10) // BGD(E1,E5a), 2^-32 s
		e1bHealth, _ := r.Bits(69, 2)
		w.eph.ID = gnss.Galileo
		w.Health = int(e1bHealth)
		w.clk.TGD = float64(bgd) * float64(1.0/float64(uint64(1)<<32))
	}
	return w, nil
}

// AssembleGalileo combines I/NAV word types 1–4 for one SV (matching IODnav) into
// the kepler ephemeris and clock model. svid tags the constellation. w5 (word type
// 5: BGD/health) is nil-tolerant and not part of the IODnav-matched set — when
// present, its BGD(E1,E5a) is folded into the clock model's TGD.
func AssembleGalileo(svid int, w1, w2, w3, w4, w5 *GalileoINAV) (kepler.Ephemeris, clock.Model, error) {
	if w1 == nil || w2 == nil || w3 == nil || w4 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	if w1.IODnav != w2.IODnav || w1.IODnav != w3.IODnav || w1.IODnav != w4.IODnav {
		return kepler.Ephemeris{}, clock.Model{}, errIODMismatch
	}
	eph := w1.eph
	eph.Omega0, eph.I0, eph.Omega, eph.IDot = w2.eph.Omega0, w2.eph.I0, w2.eph.Omega, w2.eph.IDot
	eph.OmegaDot, eph.DeltaN = w3.eph.OmegaDot, w3.eph.DeltaN
	eph.Cuc, eph.Cus, eph.Crc, eph.Crs = w3.eph.Cuc, w3.eph.Cus, w3.eph.Crc, w3.eph.Crs
	eph.Cic, eph.Cis = w4.eph.Cic, w4.eph.Cis
	eph.ID = gnss.Galileo
	eph.SVID = svid
	clk := w4.clk
	clk.ID = gnss.Galileo
	if w5 != nil {
		clk.TGD = w5.clk.TGD
	}
	return eph, clk, nil
}
