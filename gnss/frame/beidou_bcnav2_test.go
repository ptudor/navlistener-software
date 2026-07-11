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
