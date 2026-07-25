package frame

import (
	"encoding/binary"
	"errors"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// ErrGalileoAlertPage is returned when a page's Even/Odd or Page Type flag bits
// (OS-SIS-ICD §4.3.1) don't match a nominal even+odd pair -- an alert page or a
// misaligned pair, whose data fields are not a nav word. Exported
//  so the daemon can count it under its own metric label: an alert
// page (Page Type 1) is a deliberate, CRC'd transmission mode whose content
// the ICD reserves (GAL-OS-SIS-ICD-2.2 §4.3.2 Table 39), not input corruption
// — "the constellation is transmitting its attention-worthy page type" must
// be distinguishable from bit-rot.
var ErrGalileoAlertPage = errors.New("frame: Galileo I/NAV alert page or misaligned pair")

// copyGalileoBits copies an MSB-first bit range between byte slices. I/NAV's
// CRC-protected message is split across the even and odd 128-bit page parts,
// so it cannot be checked as one range in the receiver's 256-bit delivery.
func copyGalileoBits(dst []byte, dstOff int, src []byte, srcOff, n int) {
	for i := 0; i < n; i++ {
		sp := srcOff + i
		if src[sp>>3]&(1<<uint(7-(sp&7))) != 0 {
			dp := dstOff + i
			dst[dp>>3] |= 1 << uint(7-(dp&7))
		}
	}
}

// galileoINAVCRCMessage reconstructs the I/NAV CRC message from one nominal
// page. Per OS-SIS-ICD §4.3.1.4, it is the even flags+112 data bits (page
// 0..113), followed by the odd flags+16 data+64 auxiliary/reserved bits (page
// 128..209), followed, when withCRC is true, by the transmitted 24-bit CRC
// (page 210..233). SSP/reserved2 and both tail fields are not protected.
func galileoINAVCRCMessage(page []byte, withCRC bool) []byte {
	bitLen := 196
	oddBits := 82
	if withCRC {
		bitLen += 24
		oddBits += 24
	}
	msg := make([]byte, (bitLen+7)/8)
	copyGalileoBits(msg, 0, page, 0, 114)
	copyGalileoBits(msg, 114, page, 128, oddBits)
	return msg
}

// StampGalileoINAVCRC computes and writes the I/NAV CRC-24Q into an eight-word
// nominal page. It exists for synthetic frame builders; live decoders should
// only call DecodeGalileoINAV, which verifies the transmitted checksum. The
// first eight words are mutated in place; a shorter slice is left unchanged.
func StampGalileoINAVCRC(words []uint32) {
	if len(words) < 8 {
		return
	}
	page := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(page[i*4:], words[i])
	}
	crc := CRC24QBits(galileoINAVCRCMessage(page, false), 0, 196)
	for i := 0; i < 24; i++ {
		p := 210 + i
		mask := byte(1) << uint(7-(p&7))
		if crc&(1<<uint(23-i)) != 0 {
			page[p>>3] |= mask
		} else {
			page[p>>3] &^= mask
		}
	}
	for i := 0; i < 8; i++ {
		words[i] = binary.BigEndian.Uint32(page[i*4:])
	}
}

// Galileo E1-B I/NAV decoding (OS-SIS-ICD Issue 2.2 §4.3). u-blox delivers each
// I/NAV nominal page as one UBX-RXM-SFRBX of eight 32-bit words = a 256-bit page:
// the even page part (words 0–3) then the odd page part (words 4–7), each starting
// with Even/Odd(1)+PageType(1). The 128-bit nav "word" is the even part's 112 data
// bits followed by the odd part's 16 data bits; word type is its first 6 bits.
// Verified against real ZED-F9T frames (radius ≈ 29600 km). Decode still verifies
// the in-frame CRC: push-path frames need not retain a receiver's integrity guarantee.

// Galileo I/NAV scale factors beyond the shared p2mNN set.
const (
	p2m34  = 1.0 / float64(uint64(1)<<34)
	p2m46  = 1.0 / float64(uint64(1)<<46)
	p2m51  = 1.0 / float64(uint64(1)<<51) // GGTO A1G (GAL-OS-SIS-ICD-2.2 Table 76)
	p2m59  = 1.0 / float64(uint64(1)<<59)
	galT0  = 60.0   // t0e / t0c step, seconds
	galT0G = 3600.0 // GGTO t0G step, seconds (Table 76)
)

// GalileoINAV holds the decoded fields of one I/NAV word. Only the fields for this
// word type are populated; a full ephemeris comes from word types 1–4 with a
// matching IODnav (AssembleGalileo).
type GalileoINAV struct {
	Type   int
	IODnav int
	SISA   int
	Health int // E1B health (word 5), when present
	// E5bSHS is the E5b Signal Health Status (word 5, regression fix). Every E1-B word
	// 5 carries per-signal health for BOTH E5b and E1-B/C (GAL-OS-SIS-ICD-2.2
	// Table 46: E5bSHS@67 2 bits, E1BSHS@69 2 bits; semantics Table 83/84), so
	// E5b health visibility needs no E5b-I dispatch  — it is broadcast in
	// a word this decoder already parses. Deliberately a SEPARATE field, never
	// folded into Health: the @0 entry's served health must stay E1-B's own
	// (the regression fix lesson — bit 67 is E5b's, bit 69 is E1-B's). Where E5bSHS
	// lands in the feeds is the regression fix multi-signal keying decision; until then
	// it is decoded-but-unserved, like the DVS bits below.
	E5bSHS int
	// WN/TOW (word 5, regression fix): the 12-bit GST week number and 20-bit GST
	// time-of-week, both plain integer counts (GAL-OS-SIS-ICD-2.2 Table 69 --
	// scale factor 1). GST epoch is 1999-08-22 (gnsstime.go); this 12-bit field
	// must never be confused with GPS's 10-bit WN -- it lives only on this
	// Galileo-specific struct, never a field shared with another constellation.
	// consumed by the state layer's broadcast-vs-receiver GST week
	// cross-check (wn_mismatch), on the GST axis via gnsstime.SysGalileo.
	WN     int
	TOW    float64
	E5bDVS int // E5b Data Validity Status (word 5): 0 valid, 1 working without guarantee
	E1BDVS int // E1B Data Validity Status (word 5)
	// Broadcast group delays (word 5, seconds). I/NAV is the (E1,E5b) clock
	// (OS-SIS-ICD Issue 2.2 Table 71), so an E1 single-frequency user corrects with BGD(E1,E5b);
	// BGD(E1,E5a) belongs to the F/NAV (E1,E5a) clock. Both are decoded so each clock can
	// pick its own pair.
	BGDE1E5a float64
	BGDE1E5b float64
	// GGTO — the GST-GPS conversion parameters (word type 10, regression fix):
	// Δt_systems = t_Galileo − t_GPS = A0G + A1G·[TOW − t0G + 604800·(WN − WN0G)]
	// (GAL-OS-SIS-ICD-2.2 §5.1.8 Eq. 24; positions Table 51, coding Table 76).
	// HasGGTO marks a decoded word 10; GGTOValid is §5.1.8's dissemination rule —
	// all four parameters transmitted all-ones means "GGTO not valid", a
	// deliberate broadcast withdrawal, distinct from the fields never arriving.
	// WN0G is the raw 6-bit truncated week: §5.1.8 guarantees |untruncated
	// WN − WN0G| ≤ 31 at broadcast time, so nearest-cycle (mod-64) disambiguation
	// against wall clock is exact; the consumer disambiguates, not this decoder.
	HasGGTO   bool
	GGTOValid bool
	A0G       float64 // s, two's complement ×2⁻³⁵
	A1G       float64 // s/s, two's complement ×2⁻⁵¹
	T0G       float64 // s, ×3600
	WN0G      int     // weeks, 6-bit truncated
	// OSNMA : the 40-bit OSNMA protocol-data field of the E1-B odd
	// page part — page bits 146..185 (GAL-OS-SIS-ICD-2.2 Table 38: odd part =
	// flags(2) + data 2/2(16) + OSNMA(40) + SAR(22) + spare(2) + CRC + SSP +
	// tail; the field IS CRC-protected, so it arrives integrity-checked here).
	// Structure per GAL-OSNMA-SIS-ICD §2: HKROOT portion (first 8 bits) then
	// MACK portion (32 bits); a satellite not currently distributing OSNMA
	// transmits a 40-bit all-zeros field. HasOSNMA is false for dummy words
	// (type 63): the OSNMA ICD directs that data from the OSNMA field of dummy
	// messages or alert pages be discarded (alert pages never reach here —
	// regression fix rejects them earlier). v1 consumes presence only (live vs zeros,
	// INTEGRITY.md §7); TESLA/Merkle verification is the documented later phase.
	HasOSNMA bool
	OSNMA    uint64 // the 40 raw bits, MSB-first (HKROOT<<32 | MACK)
	eph      kepler.Ephemeris
	clk      clock.Model
	hasClk   bool
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

	// validate the Even/Odd and Page Type flag bits before trusting the
	// page as a nominal nav word. OS-SIS-ICD: even-part bit 0 must be 0, odd-part
	// bit 0 (page bit 128) must be 1, and Page Type (bit 1 of each part) must be 0
	// (Nominal) -- Page Type 1 marks an alert page whose data fields are not a nav
	// word at all. Without this check, an alert page (or a misaligned pair from a
	// non-u-blox source) decodes as a nav word with an arbitrary type 0-63; word
	// type 5 writes health directly into live state, so a single alert page could
	// flip an SV's served health.
	evenFlag, _ := pr.Bits(0, 1)
	evenPageType, _ := pr.Bits(1, 1)
	oddFlag, _ := pr.Bits(128, 1)
	oddPageType, _ := pr.Bits(129, 1)
	if evenFlag != 0 || oddFlag != 1 || evenPageType != 0 || oddPageType != 0 {
		return nil, ErrGalileoAlertPage
	}
	// the nominal-page CRC message is non-contiguous in the delivered
	// even+odd page, so reconstruct its 220 protected bits (including CRC) before
	// applying the same CRC-24Q zero-remainder check used by CNAV and SBAS.
	crcMessage := galileoINAVCRCMessage(page, true)
	if !CheckCRC24QBits(crcMessage, 0, 220) {
		return nil, ErrBadCRC
	}

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
	// the OSNMA field rides EVERY nominal page's odd part (page bits
	// 146..185), independent of word type — except dummy messages (word type
	// 63), whose OSNMA field the OSNMA ICD directs receivers to discard.
	if wt != 63 {
		osnma, _ := pr.Bits(146, 40)
		w.HasOSNMA, w.OSNMA = true, osnma
	}
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
		// Ionosphere, BGD, health, DVS, GST (OS-SIS-ICD Issue 2.2 Table 46). Layout
		// after BGD_E1E5a(47-56): BGD_E1E5b(57-66), E5b_HS(67-68), E1B_HS(69-70),
		// E5bDVS(71), E1BDVS(72), WN(73-84, 12 bits), TOW(85-104, 20 bits), Spare
		// (105-127) — bit 67 is E5b health, not E1B health. WN/TOW are plain integer
		// counts (Table 69: scale factor 1), never scaled like GPS's ×6 TOW.
		bgdA, _ := r.Signed(47, 10) // BGD(E1,E5a), 2^-32 s — the F/NAV (E1,E5a) clock's pair
		bgdB, _ := r.Signed(57, 10) // BGD(E1,E5b), 2^-32 s — the I/NAV (E1,E5b) clock's pair
		e5bHealth, _ := r.Bits(67, 2)
		e1bHealth, _ := r.Bits(69, 2)
		e5bDVS, _ := r.Bits(71, 1)
		e1bDVS, _ := r.Bits(72, 1)
		wn, _ := r.Bits(73, 12)
		tow, _ := r.Bits(85, 20)
		w.eph.ID = gnss.Galileo
		w.Health = int(e1bHealth)
		w.E5bSHS = int(e5bHealth) // separate field, never Health (see struct doc)
		w.E5bDVS = int(e5bDVS)
		w.E1BDVS = int(e1bDVS)
		w.WN = int(wn)
		w.TOW = float64(tow)
		const bgdScale = 1.0 / float64(uint64(1)<<32)
		w.BGDE1E5a = float64(bgdA) * bgdScale
		w.BGDE1E5b = float64(bgdB) * bgdScale
		// the I/NAV clock is (E1,E5b) type (OS-SIS-ICD Issue 2.2 Table 71), so its group
		// delay for an E1 single-frequency user is BGD(E1,E5b) — not BGD(E1,E5a), which the
		// old code applied. clock.Model.TGD is "group delay for the tracked signal".
		w.clk.TGD = w.BGDE1E5b
	case 10:
		// Almanac SVID3 (2/2) + GST-GPS conversion. Table 51 layout:
		// Type(6) IODa(4) Ω0(16) Ω̇(11) M0(16) af0(16) af1(13) E5bSHS(2)
		// E1BSHS(2) A0G(16) A1G(12) t0G(8) WN0G(6) = 128 → cumulative offsets
		// A0G@86, A1G@102, t0G@114, WN0G@122. Only the GGTO half is decoded:
		// the leading almanac fields (and their E5bSHS/E1BSHS at bits 82/84)
		// describe the ALMANAC SUBJECT satellite SVID3, not the transmitter —
		// folding those health bits into the transmitting SV's state would be
		// the cross-SV chimera the regression fix family guards against.
		a0gRaw, _ := r.Bits(86, 16)
		a1gRaw, _ := r.Bits(102, 12)
		t0gRaw, _ := r.Bits(114, 8)
		wn0gRaw, _ := r.Bits(122, 6)
		a0g, _ := r.Signed(86, 16)
		a1g, _ := r.Signed(102, 12)
		w.HasGGTO = true
		// §5.1.8: all four parameters all-ones = "the GGTO is considered as not
		// valid" — a broadcast withdrawal, checked on the raw patterns before
		// scaling (A0G/A1G are two's complement; all-ones is a legal −1 value
		// for either alone, so the sentinel is only the four-field conjunction).
		w.GGTOValid = !(a0gRaw == 0xFFFF && a1gRaw == 0xFFF && t0gRaw == 0xFF && wn0gRaw == 0x3F)
		w.A0G = float64(a0g) * p2m35
		w.A1G = float64(a1g) * p2m51
		w.T0G = float64(t0gRaw) * galT0G
		w.WN0G = int(wn0gRaw)
	}
	return w, nil
}

// AssembleGalileo combines I/NAV word types 1–4 for one SV (matching IODnav) into
// the kepler ephemeris and clock model. svid tags the constellation. w5 (word type
// 5: BGD/health) is nil-tolerant and not part of the IODnav-matched set — when
// present, its BGD(E1,E5b) (the I/NAV clock's group-delay pair, regression fix) is folded into the
// clock model's TGD.
func AssembleGalileo(svid int, w1, w2, w3, w4, w5 *GalileoINAV) (kepler.Ephemeris, clock.Model, error) {
	if w1 == nil || w2 == nil || w3 == nil || w4 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	if w1.Type != 1 || w2.Type != 2 || w3.Type != 3 || w4.Type != 4 || (w5 != nil && w5.Type != 5) {
		return kepler.Ephemeris{}, clock.Model{}, errWrongMsgType
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
