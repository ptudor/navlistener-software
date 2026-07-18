package frame

import (
	"encoding/binary"
	"testing"

	"github.com/ptudor/gnss/clock"
)

func TestBCNAV2ShortFrame(t *testing.T) {
	if _, err := DecodeBeiDouBCNAV2([]uint32{1, 2, 3}); err != ErrShortFrame {
		t.Errorf("err = %v, want ErrShortFrame", err)
	}
}

// TestBCNAV2RejectsBadCRC confirms a frame failing its CRC-24Q is rejected, never
// partially decoded (the real capture's frames all pass — see TestRealBeiDouBCNAV2).
func TestBCNAV2RejectsBadCRC(t *testing.T) {
	words := make([]uint32, 9)
	words[0] = (uint32(30) << 26) | (uint32(10) << 20) // PRN 30, MesType 10, junk CRC
	if _, err := DecodeBeiDouBCNAV2(words); err != ErrBadCRC {
		t.Errorf("err = %v, want ErrBadCRC", err)
	}
}

// TestBCNAV2AcceptsValidCRC builds a CRC-24Q-valid frame and checks the header
// decodes (message type 10).
func TestBCNAV2AcceptsValidCRC(t *testing.T) {
	buf := make([]byte, 36)
	// PRN 30 @0(6), MesType 10 @6(6): top 12 bits = 011110 001010.
	binary.BigEndian.PutUint32(buf[0:], (uint32(30)<<26)|(uint32(10)<<20))
	setBits(buf, 72, 2, 3) // SatType = MEO; 00 is reserved
	// Append a valid CRC-24Q over the leading 264 bits into the trailing 24.
	c := CRC24Q(buf[:33])
	buf[33] = byte(c >> 16)
	buf[34] = byte(c >> 8)
	buf[35] = byte(c)
	words := make([]uint32, 9)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	m, err := DecodeBeiDouBCNAV2(words)
	if err != nil {
		t.Fatalf("valid-CRC frame rejected: %v", err)
	}
	if m.PRN != 30 || m.MesType != 10 {
		t.Errorf("PRN=%d MesType=%d, want 30/10", m.PRN, m.MesType)
	}
}

func TestBCNAV2RejectsReservedSatType(t *testing.T) {
	buf := make([]byte, 36)
	setBits(buf, 0, 6, 30)
	setBits(buf, 6, 6, 10)
	// SatType remains the reserved value 00.
	c := CRC24Q(buf[:33])
	buf[33], buf[34], buf[35] = byte(c>>16), byte(c>>8), byte(c)
	words := make([]uint32, 9)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	if _, err := DecodeBeiDouBCNAV2(words); err != errBadSatType {
		t.Errorf("err = %v, want errBadSatType", err)
	}
}

// TestBCNAV2MT34DecodesBDTUTC guards the MT34 BDT-UTC time offset
// block at bits 143–239 (BDS-SIS-ICD-B2a v1.0 Figure 6-9 placement, Figure 6-16
// layout, Table 7-20 scales/signs) must decode field-for-field, including the
// two's-complement handling of every starred field. Raw values are chosen so a
// ±1-bit offset regression or a signed/unsigned swap changes an asserted value.
func TestBCNAV2MT34DecodesBDTUTC(t *testing.T) {
	buf := make([]byte, 36)
	setBits(buf, 0, 6, 30)     // PRN
	setBits(buf, 6, 6, 34)     // MesType 34
	setBits(buf, 12, 18, 1)    // SOW raw (×3 s)
	setBits(buf, 42, 11, 1234) // SISAItop 
	setBits(buf, 53, 5, 21)    // SISAIocb
	setBits(buf, 58, 3, 5)     // SISAIoc1
	setBits(buf, 61, 3, 2)     // SISAIoc2
	setBits(buf, 64, 11, 100)
	setBits(buf, 133, 10, 7)         // IODC
	setBits(buf, 143, 16, (1<<16)-2) // A0UTC raw −2 (two's complement)
	setBits(buf, 159, 13, (1<<13)-3) // A1UTC raw −3
	setBits(buf, 172, 7, (1<<7)-1)   // A2UTC raw −1
	setBits(buf, 179, 8, 4)          // ΔtLS = 4 s (BDT-UTC, 2017+)
	setBits(buf, 187, 16, 252800/16) // tot raw (×2⁴ s)
	setBits(buf, 203, 13, 932)       // WNot (full BDT week)
	setBits(buf, 216, 13, 933)       // WNLSF
	setBits(buf, 229, 3, 6)          // DN (0–6)
	setBits(buf, 232, 8, (1<<8)-5)   // ΔtLSF raw −5 (signedness probe)
	c := CRC24Q(buf[:33])
	buf[33], buf[34], buf[35] = byte(c>>16), byte(c>>8), byte(c)
	words := make([]uint32, 9)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	m, err := DecodeBeiDouBCNAV2(words)
	if err != nil {
		t.Fatalf("MT34 rejected: %v", err)
	}
	u := m.UTC
	if want := -2.0 / float64(uint64(1)<<35); u.A0 != want {
		t.Errorf("A0UTC = %g, want %g (−2 × 2⁻³⁵)", u.A0, want)
	}
	if want := -3.0 / float64(uint64(1)<<51); u.A1 != want {
		t.Errorf("A1UTC = %g, want %g (−3 × 2⁻⁵¹)", u.A1, want)
	}
	if want := -1.0 / float64(uint64(1)<<34) / float64(uint64(1)<<34); u.A2 != want {
		t.Errorf("A2UTC = %g, want %g (−1 × 2⁻⁶⁸)", u.A2, want)
	}
	if u.DtLS != 4 || u.DtLSF != -5 {
		t.Errorf("ΔtLS/ΔtLSF = %g/%g, want 4/−5", u.DtLS, u.DtLSF)
	}
	if u.Tot != 252800 || u.WNot != 932 {
		t.Errorf("tot/WNot = %g/%d, want 252800/932", u.Tot, u.WNot)
	}
	if u.WNLSF != 933 || u.DN != 6 {
		t.Errorf("WNLSF/DN = %d/%d, want 933/6", u.WNLSF, u.DN)
	}
	// The clock fields sharing the message must be unaffected by the new block.
	if m.clk.Toc != 100*bcnavT0 || m.IODC != 7 {
		t.Errorf("MT34 clock Toc/IODC = %g/%d, want %g/7", m.clk.Toc, m.IODC, 100*bcnavT0)
	}
	// the SISAIoc block at 42–63 (Figure 6-14 internal layout).
	if m.SISAItop != 1234 || m.SISAIocb != 21 || m.SISAIoc1 != 5 || m.SISAIoc2 != 2 {
		t.Errorf("MT34 SISAIoc = top %d ocb %d oc1 %d oc2 %d, want 1234/21/5/2",
			m.SISAItop, m.SISAIocb, m.SISAIoc1, m.SISAIoc2)
	}
}

// TestBCNAV2MT40DecodesSISAI guards MT40 half: SISAIoe at bits 42–46
// then the Figure 6-14 SISAIoc block at 47–68 (Figure 6-10). Raw indices only —
// v1.0 publishes no decode table.
func TestBCNAV2MT40DecodesSISAI(t *testing.T) {
	buf := make([]byte, 36)
	setBits(buf, 0, 6, 30)
	setBits(buf, 6, 6, 40) // MesType 40
	setBits(buf, 12, 18, 1)
	setBits(buf, 30, 2, 1)    // HS
	setBits(buf, 42, 5, 17)   // SISAIoe
	setBits(buf, 47, 11, 999) // SISAItop
	setBits(buf, 58, 5, 21)   // SISAIocb
	setBits(buf, 63, 3, 5)    // SISAIoc1
	setBits(buf, 66, 3, 2)    // SISAIoc2
	c := CRC24Q(buf[:33])
	buf[33], buf[34], buf[35] = byte(c>>16), byte(c>>8), byte(c)
	words := make([]uint32, 9)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	m, err := DecodeBeiDouBCNAV2(words)
	if err != nil {
		t.Fatalf("MT40 rejected: %v", err)
	}
	if m.SISAIoe != 17 || m.SISAItop != 999 || m.SISAIocb != 21 || m.SISAIoc1 != 5 || m.SISAIoc2 != 2 {
		t.Errorf("MT40 SISAI = oe %d top %d ocb %d oc1 %d oc2 %d, want 17/999/21/5/2",
			m.SISAIoe, m.SISAItop, m.SISAIocb, m.SISAIoc1, m.SISAIoc2)
	}
	if m.HS != 1 {
		t.Errorf("MT40 HS = %d, want 1", m.HS)
	}
}

func TestAssembleBeiDouBCNAV2RejectsWrongSlots(t *testing.T) {
	m10 := &BeiDouBCNAV2{MesType: 10, SOW: 100}
	m11 := &BeiDouBCNAV2{MesType: 11, SOW: 103, hasEph2: true}
	clk := &BeiDouBCNAV2{MesType: 30, SOW: 103, hasClk: true}
	if _, _, _, err := AssembleBeiDouBCNAV2(1, m11, m10, nil); err != errWrongMsgType {
		t.Errorf("transposed args error = %v, want errWrongMsgType", err)
	}
	if _, _, _, err := AssembleBeiDouBCNAV2(1, m10, clk, nil); err != errWrongMsgType {
		t.Errorf("clock-as-m11 error = %v, want errWrongMsgType", err)
	}
}

// TestAssembleBCNAV2ClockCarriesDataComponentTGD guards the tracked
// signal is the B2a DATA component (B-CNAV2 rides on B2a-data, u-blox sigId 8),
// so the assembled Model.TGD must be eq. 7-5's TGD_B2ap + ISC_B2ad
// (BDS-SIS-ICD-B2a v1.0 §7.6.2, Table 7-6) — not the pilot-only eq. 7-4 value.
func TestAssembleBCNAV2ClockCarriesDataComponentTGD(t *testing.T) {
	const tgdB2ap, iscB2ad = -137.0 / (1 << 30) / 16, 59.0 / (1 << 30) / 16 // −137·2⁻³⁴, +59·2⁻³⁴ s
	m10 := &BeiDouBCNAV2{MesType: 10, SOW: 100}
	m11 := &BeiDouBCNAV2{MesType: 11, SOW: 103, hasEph2: true}
	m30 := &BeiDouBCNAV2{MesType: 30, SOW: 103, hasClk: true,
		TGDB2ap: tgdB2ap, ISCB2ad: iscB2ad, clk: clock.Model{Af0: 1.5}}
	_, clk, clkOK, err := AssembleBeiDouBCNAV2(28, m10, m11, m30)
	if err != nil || !clkOK {
		t.Fatalf("assembly failed: err=%v clkOK=%v", err, clkOK)
	}
	if want := tgdB2ap + iscB2ad; clk.TGD != want {
		t.Errorf("MT30 clock TGD = %g, want TGD_B2ap+ISC_B2ad = %g (eq. 7-5, data component)", clk.TGD, want)
	}
	// An MT34-sourced clock has no group-delay field at all: TGD must stay the
	// zero value for the caller's provenance machinery to override, never a
	// partial or fabricated correction.
	m34 := &BeiDouBCNAV2{MesType: 34, SOW: 103, hasClk: true, clk: clock.Model{Af0: 1.5}}
	_, clk34, _, err := AssembleBeiDouBCNAV2(28, m10, m11, m34)
	if err != nil {
		t.Fatalf("MT34 assembly failed: %v", err)
	}
	if clk34.TGD != 0 {
		t.Errorf("MT34 clock TGD = %g, want 0 (no group-delay field in MT34)", clk34.TGD)
	}
}

// TestBCNAV2PairAdjacency confirms the type-10/11 assembler rejects a stale pair:
// type 11 carries no IODE, so broadcast adjacency (±3 s) is the only guard against
// stitching elements across an ephemeris changeover.
func TestBCNAV2PairAdjacency(t *testing.T) {
	m10 := &BeiDouBCNAV2{MesType: 10, SOW: 100}
	m11 := &BeiDouBCNAV2{MesType: 11, SOW: 103, hasEph2: true}
	if _, _, _, err := AssembleBeiDouBCNAV2(28, m10, m11, nil); err != nil {
		t.Errorf("adjacent pair rejected: %v", err)
	}
	m11.SOW = 130 // a type 11 from a previous broadcast group
	if _, _, _, err := AssembleBeiDouBCNAV2(28, m10, m11, nil); err != errPairSOW {
		t.Errorf("err = %v, want errPairSOW", err)
	}
}

// TestBCNAV2PairAndClockWeekRollover guards a broadcast-adjacent 10/11
// pair or a current clock straddling the weekly SOW rollover (604797 → 0) must
// not be rejected — the delta wraps mod 604800. A clock a full staleness bound
// behind across the boundary must still be dropped.
func TestBCNAV2PairAndClockWeekRollover(t *testing.T) {
	m10 := &BeiDouBCNAV2{MesType: 10, SOW: 0}
	m11 := &BeiDouBCNAV2{MesType: 11, SOW: 604797, hasEph2: true}
	fresh := &BeiDouBCNAV2{MesType: 30, SOW: 604797, IODC: 3, hasClk: true, clk: clock.Model{Af0: 1.5}}
	_, clk, clkOK, err := AssembleBeiDouBCNAV2(28, m10, m11, fresh)
	if err != nil {
		t.Fatalf("rollover pair (0, 604797) rejected: %v", err)
	}
	if !clkOK || clk.Af0 != 1.5 {
		t.Errorf("rollover clock dropped: Af0=%v clkOK=%v, want fresh clock applied", clk.Af0, clkOK)
	}

	stale := &BeiDouBCNAV2{MesType: 30, SOW: 604800 - bcnavClkStaleSOW - 1, hasClk: true, clk: clock.Model{Af0: 9.9}}
	_, clk2, clkOK2, err := AssembleBeiDouBCNAV2(28, m10, m11, stale)
	if err != nil {
		t.Fatalf("ephemeris must still assemble with a cross-boundary stale clock: %v", err)
	}
	if clkOK2 || clk2.Af0 != 0 {
		t.Errorf("cross-boundary stale clock must be dropped, got Af0=%v clkOK=%v", clk2.Af0, clkOK2)
	}

	// SOW 604800 is outside the transmitted domain [0, 604800): a corrupt
	// field, rejected by sowDelta rather than wrapped into an apparently
	// adjacent pair.
	outOfDomain := &BeiDouBCNAV2{MesType: 11, SOW: 604800, hasEph2: true}
	if _, _, _, err := AssembleBeiDouBCNAV2(28, m10, outOfDomain, fresh); err != errPairSOW {
		t.Errorf("err = %v, want errPairSOW (out-of-domain SOW)", err)
	}
}

// TestBCNAV2StaleClockDropped guards a cached type-30/34 clock message
// whose SOW has drifted far from the current type-10/11 pair (its own type-30
// stopped decoding a long time ago) must not be paired with a fresh ephemeris —
// but the ephemeris itself must still assemble, falling back to the zero clock,
// not fail outright.
func TestBCNAV2StaleClockDropped(t *testing.T) {
	m10 := &BeiDouBCNAV2{MesType: 10, SOW: 100000, IODE: 7}
	m11 := &BeiDouBCNAV2{MesType: 11, SOW: 100002, hasEph2: true}
	fresh := &BeiDouBCNAV2{MesType: 30, SOW: 100005, IODC: 3, hasClk: true, clk: clock.Model{Af0: 1.5}}
	eph, clk, clkOK, err := AssembleBeiDouBCNAV2(28, m10, m11, fresh)
	if err != nil {
		t.Fatalf("fresh clock rejected: %v", err)
	}
	if clk.Af0 != 1.5 || !clkOK {
		t.Errorf("fresh clock not applied: Af0=%v clkOK=%v", clk.Af0, clkOK)
	}

	stale := &BeiDouBCNAV2{MesType: 30, SOW: 100000 - bcnavClkStaleSOW - 1, IODC: 2, hasClk: true, clk: clock.Model{Af0: 9.9}}
	eph2, clk2, clkOK2, err := AssembleBeiDouBCNAV2(28, m10, m11, stale)
	if err != nil {
		t.Fatalf("ephemeris must still assemble when only the clock is stale: %v", err)
	}
	if clk2.Af0 != 0 || clkOK2 {
		t.Errorf("stale clock must be dropped (zero clock, clkOK false), got Af0=%v clkOK=%v", clk2.Af0, clkOK2)
	}
	if eph2.Toe != eph.Toe {
		t.Errorf("ephemeris must be unaffected by clock staleness")
	}
}
