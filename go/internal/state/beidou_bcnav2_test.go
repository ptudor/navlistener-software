package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// bcnav2Frame builds a synthetic, CRC-valid 288-bit B-CNAV2 message (9-word
// buffer) with the given PRN/MesType/SOW (seconds, must be a multiple of 3 —
// the wire field is SOW/3), letting fill set any additional fields (via the
// shared setAbsBits helper from beidou_d1_test.go) before the CRC-24Q is
// stamped over the leading 264 bits.
func bcnav2Frame(prn, mesType, sow int, fill func(buf []byte)) []uint32 {
	buf := make([]byte, 36) // 9 words = 288 bits
	setAbsBits(buf, 0, 6, uint64(prn))
	setAbsBits(buf, 6, 6, uint64(mesType))
	setAbsBits(buf, 12, 18, uint64(sow/3))
	if fill != nil {
		fill(buf)
	}
	crc := frame.CRC24Q(buf[:33])
	buf[33] = byte(crc >> 16)
	buf[34] = byte(crc >> 8)
	buf[35] = byte(crc)

	words := make([]uint32, 9)
	for i := 0; i < 9; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestApplyBeiDouBCNAV2ClockOnlyRefreshUpdatesSameIODE guards before the
// fix, applyBeiDouBCNAV2 gated the entire update (including the clock) on the
// type-10/11 IODE, so a same-IODE type-30 clock refresh (a new af0 under an
// unchanged ephemeris) was silently dropped forever. It must now update the
// served clock while leaving the ephemeris IODE untouched.
func TestApplyBeiDouBCNAV2ClockOnlyRefreshUpdatesSameIODE(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 20

	m10 := bcnav2Frame(prn, 10, 100002, func(buf []byte) {
		setAbsBits(buf, 30, 13, 1)  // WN (arbitrary)
		setAbsBits(buf, 53, 8, 7)   // IODE = 7
		setAbsBits(buf, 61, 11, 10) // Toe (arbitrary)
		setAbsBits(buf, 72, 2, 3)   // SatType = MEO
	})
	m11 := bcnav2Frame(prn, 11, 100002, nil)
	clk1 := bcnav2Frame(prn, 30, 100005, func(buf []byte) {
		setAbsBits(buf, 111, 10, 3)                  // IODC = 3
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw)
	})

	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m10})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m11})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: clk1})

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph {
		t.Fatalf("ephemeris not assembled: %+v", st)
	}
	af0First := st.clk.Af0
	if af0First == 0 {
		t.Fatalf("test setup: initial af0 must be nonzero to detect a later change")
	}

	// A same-IODE, new-af0/new-IODC type-30 refresh must still update st.clk.
	clk2 := bcnav2Frame(prn, 30, 100008, func(buf []byte) {
		setAbsBits(buf, 111, 10, 4)                  // IODC 3 -> 4
		setAbsBits(buf, 53, 25, uint64(int64(2000))) // Af0 changed
	})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: clk2})

	if st.clk.Af0 == af0First {
		t.Errorf("same-IODE clock refresh did not update served af0: still %v", st.clk.Af0)
	}
	if st.iod != 7 {
		t.Errorf("ephemeris IODE must be unchanged by a clock-only refresh: %d", st.iod)
	}
}

// TestApplyBeiDouBCNAV2StaleClockAtIODEChangeover guards the regression fix follow-up: an
// IODE changeover assembled while the cached type-30/34 is stale (dropped by the
// assembler) must keep serving the previously applied clock — not install the
// zero model over it — and must not report a time-disco (there is no fresh clock
// to difference against; absent, not zero).
func TestApplyBeiDouBCNAV2StaleClockAtIODEChangeover(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 21

	apply := func(words []uint32, at time.Time) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: at, Words: words})
	}
	apply(bcnav2Frame(prn, 10, 100002, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)   // IODE = 7
		setAbsBits(buf, 61, 11, 10) // Toe (raw)
		setAbsBits(buf, 72, 2, 3)   // SatType = MEO
	}), now)
	apply(bcnav2Frame(prn, 11, 100002, nil), now)
	apply(bcnav2Frame(prn, 30, 100005, func(buf []byte) {
		setAbsBits(buf, 111, 10, 3)                  // IODC = 3
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw, nonzero)
	}), now)

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph || st.clk.Af0 == 0 {
		t.Fatalf("setup: ephemeris+clock not applied: %+v", st)
	}
	af0 := st.clk.Af0

	// One hour later the IODE changes; the cached type-30's SOW is now well past
	// bcnavClkStaleSOW, so the assembler drops it and returns the zero model.
	later := now.Add(time.Hour)
	apply(bcnav2Frame(prn, 10, 103602, func(buf []byte) {
		setAbsBits(buf, 53, 8, 8)   // IODE 7 -> 8
		setAbsBits(buf, 61, 11, 11) // Toe advances one step
		setAbsBits(buf, 72, 2, 3)
	}), later)
	apply(bcnav2Frame(prn, 11, 103602, nil), later)

	if st.iod != 8 {
		t.Fatalf("IODE changeover not applied: iod=%d, want 8", st.iod)
	}
	if st.clk.Af0 != af0 {
		t.Errorf("stale clock zeroed the served clock at IODE changeover: Af0=%v, want %v kept", st.clk.Af0, af0)
	}
	if st.timeDiscoValid {
		t.Errorf("time-disco reported with no fresh clock to difference against: %v ns", st.timeDiscoNs)
	}
}

// TestApplyBeiDouBCNAV2StaleClockIODCNotLatched guards the regression fix follow-up's
// second half: when the assembler drops the cached type-30/34 as stale, its IODC
// was never applied and must not be latched — otherwise a later fresh type-30
// carrying the same IODC reads as "unchanged" and its clock is never served.
func TestApplyBeiDouBCNAV2StaleClockIODCNotLatched(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 22

	apply := func(words []uint32, at time.Time) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: at, Words: words})
	}
	// The type-30 arrives first (IODC=7), before any 10/11 pair completes.
	apply(bcnav2Frame(prn, 30, 100005, func(buf []byte) {
		setAbsBits(buf, 111, 10, 7)                  // IODC = 7
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw, nonzero)
	}), now)

	// An hour later the 10/11 pair assembles; the cached type-30 is stale and
	// dropped, so its IODC=7 must not be latched as applied.
	later := now.Add(time.Hour)
	apply(bcnav2Frame(prn, 10, 103602, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)   // IODE = 7
		setAbsBits(buf, 61, 11, 11) // Toe (raw)
		setAbsBits(buf, 72, 2, 3)   // SatType = MEO
	}), later)
	apply(bcnav2Frame(prn, 11, 103602, nil), later)

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph {
		t.Fatalf("setup: ephemeris not assembled: %+v", st)
	}
	if st.clk.Af0 != 0 {
		t.Fatalf("setup: stale clock should not have been applied, Af0=%v", st.clk.Af0)
	}

	// A fresh type-30 rebroadcasting the same IODC=7 must now be recognized and
	// applied — the stale message's IODC was never latched.
	apply(bcnav2Frame(prn, 30, 103605, func(buf []byte) {
		setAbsBits(buf, 111, 10, 7)                  // same IODC = 7
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw, nonzero)
	}), later)

	if st.clk.Af0 == 0 {
		t.Errorf("fresh type-30 with the stale message's IODC was never applied: served clock still zero")
	}
}

func TestApplyBeiDouBCNAV2MT34CarriesMT30TGD(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 23
	apply := func(words []uint32, at time.Time) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: at, Words: words})
	}
	m10 := func(iode, toe, sow int) []uint32 {
		return bcnav2Frame(prn, 10, sow, func(buf []byte) {
			setAbsBits(buf, 53, 8, uint64(iode))
			setAbsBits(buf, 61, 11, uint64(toe))
			setAbsBits(buf, 72, 2, 3) // MEO
		})
	}
	m11 := func(sow int) []uint32 { return bcnav2Frame(prn, 11, sow, nil) }
	m30 := func(iodc, sow int) []uint32 {
		return bcnav2Frame(prn, 30, sow, func(buf []byte) {
			setAbsBits(buf, 111, 10, uint64(iodc))
			setAbsBits(buf, 121, 12, (1<<12)-137) // about -8 ns, 12-bit two's complement
		})
	}
	m34 := func(iodc, sow int) []uint32 {
		return bcnav2Frame(prn, 34, sow, func(buf []byte) {
			setAbsBits(buf, 133, 10, uint64(iodc))
		})
	}

	apply(m10(1, 10, 100002), now)
	apply(m11(100002), now)
	apply(m30(1, 100005), now)

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.clkHasBcTGD || st.clk.TGD == 0 {
		t.Fatalf("MT30 TGD not applied: %+v", st)
	}
	wantTGD := st.clk.TGD

	// MT34 wins the next IODC race. Its clock polynomial must carry the last
	// decoded MT30 TGD rather than install a decoded-looking zero.
	second := now.Add(5 * time.Minute)
	apply(m34(2, 100299), second)
	apply(m10(2, 11, 100302), second)
	apply(m11(100302), second)
	if st.clk.TGD != wantTGD || !st.clkHasBcTGD {
		t.Fatalf("MT34 lost MT30 TGD: got %g known=%v, want %g", st.clk.TGD, st.clkHasBcTGD, wantTGD)
	}
	// A same-IODC MT30 supplies the authoritative field without creating a
	// separate ephemeris changeover.
	apply(m30(2, 100305), second)
	if st.clk.TGD != wantTGD || !st.clkHasBcTGD {
		t.Fatalf("same-IODC MT30 did not preserve TGD: got %g known=%v", st.clk.TGD, st.clkHasBcTGD)
	}

	third := now.Add(10 * time.Minute)
	apply(m34(3, 100599), third)
	apply(m10(3, 12, 100602), third)
	apply(m11(100602), third)
	if !st.timeDiscoValid || st.timeDiscoNs >= 2.5 {
		t.Errorf("continuous MT34 changeover time disco = %g ns valid=%v, want <2.5 ns", st.timeDiscoNs, st.timeDiscoValid)
	}
}
