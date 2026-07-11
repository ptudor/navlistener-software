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
