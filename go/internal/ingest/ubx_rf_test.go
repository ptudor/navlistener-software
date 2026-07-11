package ingest

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// ubxMsg wraps a class/id/payload in a full UBX frame with the sync bytes and the 8-bit
// Fletcher checksum, so it round-trips through the real scanUBX parser.
func ubxMsg(cls, id byte, payload []byte) []byte {
	n := len(payload)
	msg := []byte{ubxSync1, ubxSync2, cls, id, byte(n), byte(n >> 8)}
	msg = append(msg, payload...)
	var a, b byte
	for _, x := range msg[2:] {
		a += x
		b += a
	}
	return append(msg, a, b)
}

func scanRF(t *testing.T, msg []byte) []*RawFrame {
	t.Helper()
	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(msg), "stn", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) }, func(string) {})
	return frames
}

// TestParseMONRF builds a one-block MON-RF frame with known AGC/jamming values and checks
// the parsed RF band matches the ICD field offsets (noisePerMS/agcCnt/jamInd are at
// 12/14/16 — blockId(1) flags(1) antStatus(1) antPower(1) postStatus(4) reserved(4) = 12
// bytes before noisePerMS, not 14/16/20 as a prior version of this fixture assumed).
func TestParseMONRF(t *testing.T) {
	block := make([]byte, 24)
	block[0] = 3    // blockId
	block[1] = 0x02 // flags: jammingState = warning
	block[2] = 2    // antStatus = ok
	// block[3] = antPower, block[4:8] = postStatus, block[8:12] = reserved (unused here)
	binary.LittleEndian.PutUint16(block[12:], 120)  // noisePerMS
	binary.LittleEndian.PutUint16(block[14:], 3000) // agcCnt
	block[16] = 200                                 // jamInd (CW)
	payload := append([]byte{0, 1, 0, 0}, block...) // version, nBlocks=1, reserved[2]

	frames := scanRF(t, ubxMsg(ubxClassMON, ubxIDMONRF, payload))
	if len(frames) != 1 || frames[0].RF == nil || len(frames[0].RF.Bands) != 1 {
		t.Fatalf("frames = %+v", frames)
	}
	b := frames[0].RF.Bands[0]
	if b.Block != 3 || b.AGC != 3000 || b.CWSuppress != 200 || b.JamState != 2 || b.AntStatus != 2 || b.NoiseLevel != 120 {
		t.Errorf("band = %+v", b)
	}
	if frames[0].Source != "stn" {
		t.Errorf("source = %q", frames[0].Source)
	}
}

// TestParseMONHW checks the legacy MON-HW single-band extraction (agcCnt @18, aStatus @20,
// jammingState in flags bits 2-3 @22, jamInd @45).
func TestParseMONHW(t *testing.T) {
	p := make([]byte, 60)
	binary.LittleEndian.PutUint16(p[16:], 90)   // noisePerMS
	binary.LittleEndian.PutUint16(p[18:], 4096) // agcCnt
	p[20] = 4                                   // aStatus = open
	p[22] = 0x0C                                // flags: jammingState=3 (bits 2-3) → critical
	p[45] = 150                                 // jamInd

	frames := scanRF(t, ubxMsg(ubxClassMON, ubxIDMONHW, p))
	if len(frames) != 1 || len(frames[0].RF.Bands) != 1 {
		t.Fatalf("frames = %+v", frames)
	}
	b := frames[0].RF.Bands[0]
	if b.AGC != 4096 || b.AntStatus != 4 || b.JamState != 3 || b.CWSuppress != 150 || b.NoiseLevel != 90 {
		t.Errorf("band = %+v", b)
	}
}

// TestParseNAVSAT checks per-SV C/N₀ + elevation extraction and the svUsed flag.
func TestParseNAVSAT(t *testing.T) {
	hdr := make([]byte, 8)
	hdr[5] = 2 // numSvs
	sv := func(gnss, id, cno byte, elev int8, used bool) []byte {
		blk := make([]byte, 12)
		blk[0], blk[1], blk[2], blk[3] = gnss, id, cno, byte(elev)
		var flags uint32
		if used {
			flags |= 0x08
		}
		binary.LittleEndian.PutUint32(blk[8:], flags)
		return blk
	}
	payload := append(hdr, sv(0, 5, 47, 61, true)...)
	payload = append(payload, sv(2, 14, 33, 12, false)...)

	frames := scanRF(t, ubxMsg(ubxClassNAV, ubxIDNAVSAT, payload))
	if len(frames) != 1 || frames[0].RF == nil || len(frames[0].RF.Sats) != 2 {
		t.Fatalf("frames = %+v", frames)
	}
	s0, s1 := frames[0].RF.Sats[0], frames[0].RF.Sats[1]
	if s0.GnssID != 0 || s0.SvID != 5 || s0.Cn0 != 47 || s0.ElevDeg != 61 || !s0.Used {
		t.Errorf("sat0 = %+v", s0)
	}
	if s1.GnssID != 2 || s1.Cn0 != 33 || s1.ElevDeg != 12 || s1.Used {
		t.Errorf("sat1 = %+v", s1)
	}
}

// TestParseRFShortFrames confirms truncated telemetry is dropped (bounds-checked), never
// read past — the untrusted-input discipline (docs/INTEGRITY.md §9).
func TestParseRFShortFrames(t *testing.T) {
	if parseMONRF([]byte{0, 1, 0, 0, 1, 2}, "s", fixedTime()) != nil { // nBlocks=1 but no block
		t.Error("short MON-RF not rejected")
	}
	if parseMONHW(make([]byte, 40), "s", fixedTime()) != nil {
		t.Error("short MON-HW not rejected")
	}
	if parseNAVSAT([]byte{0, 0, 0, 0, 0, 3, 0, 0}, "s", fixedTime()) != nil { // numSvs=3, no blocks
		t.Error("short NAV-SAT not rejected")
	}
}

// FuzzParseRF asserts the RF telemetry parsers never panic or read out of bounds on
// arbitrary receiver bytes — MON-RF/MON-HW/NAV-SAT all parse untrusted input.
func FuzzParseRF(f *testing.F) {
	f.Add([]byte{0, 1, 0, 0})
	f.Add(make([]byte, 60))
	f.Fuzz(func(t *testing.T, p []byte) {
		_ = parseMONRF(p, "s", fixedTime())
		_ = parseMONHW(p, "s", fixedTime())
		_ = parseNAVSAT(p, "s", fixedTime())
	})
}
