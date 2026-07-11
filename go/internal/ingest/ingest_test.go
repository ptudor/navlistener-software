package ingest

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
)

var fixedTime = func() time.Time { return time.Unix(1_700_000_000, 0) }

// collect runs a scanner over data and returns the emitted frames and error kinds.
func collect(t *testing.T, sc scanner, data []byte) ([]*RawFrame, []string) {
	t.Helper()
	var frames []*RawFrame
	var errs []string
	err := sc(bytes.NewReader(data), "test", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) },
		func(kind string) { errs = append(errs, kind) })
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Fatalf("scanner error = %v", err)
	}
	return frames, errs
}

func buildUBX(cls, id byte, payload []byte) []byte {
	msg := []byte{ubxSync1, ubxSync2, cls, id, byte(len(payload)), byte(len(payload) >> 8)}
	msg = append(msg, payload...)
	ckA, ckB := fletcher8(cls, id, byte(len(payload)), byte(len(payload)>>8), payload)
	return append(msg, ckA, ckB)
}

func buildSFRBXPayload(gnssID gnss.GNSSID, svID, sigID, freqID byte, words []uint32) []byte {
	p := []byte{byte(gnssID), svID, sigID, freqID, byte(len(words)), 0, 2, 0}
	for _, w := range words {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], w)
		p = append(p, b[:]...)
	}
	return p
}

func TestScanUBXSFRBX(t *testing.T) {
	words := make([]uint32, 10)
	for i := range words {
		words[i] = uint32(0x11000000 + i)
	}
	payload := buildSFRBXPayload(gnss.GPS, 5, 0, 0, words)
	msg := buildUBX(ubxClassRXM, ubxIDSFRBX, payload)

	frames, errs := collect(t, scanUBX, msg)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(frames) != 1 {
		t.Fatalf("got %d frames, want 1", len(frames))
	}
	f := frames[0]
	if f.GnssID != gnss.GPS || f.SvID != 5 || f.SigID != 0 {
		t.Errorf("frame ids = %+v", f)
	}
	if len(f.Words) != 10 || f.Words[0] != 0x11000000 {
		t.Errorf("words wrong: %x", f.Words)
	}
	if !f.Recv.Equal(fixedTime()) {
		t.Errorf("recv time = %v", f.Recv)
	}
}

func TestScanUBXChecksumFailDropped(t *testing.T) {
	payload := buildSFRBXPayload(gnss.GPS, 5, 0, 0, make([]uint32, 10))
	msg := buildUBX(ubxClassRXM, ubxIDSFRBX, payload)
	msg[len(msg)-1] ^= 0xFF // corrupt checksum

	frames, errs := collect(t, scanUBX, msg)
	if len(frames) != 0 {
		t.Errorf("corrupt frame should be dropped, got %d", len(frames))
	}
	if len(errs) == 0 || errs[0] != "ubx_checksum" {
		t.Errorf("want ubx_checksum error, got %v", errs)
	}
}

func TestScanUBXResyncsToNextMessage(t *testing.T) {
	// Junk, then a valid SFRBX: the scanner must resync and emit the good one.
	payload := buildSFRBXPayload(gnss.Galileo, 14, 1, 0, make([]uint32, 8))
	good := buildUBX(ubxClassRXM, ubxIDSFRBX, payload)
	data := append([]byte{0x00, 0xB5, 0x11, 0xFF, 0xB5}, good...)

	frames, _ := collect(t, scanUBX, data)
	if len(frames) != 1 || frames[0].GnssID != gnss.Galileo {
		t.Fatalf("expected one Galileo frame after junk, got %+v", frames)
	}
}

// TestScanUBXResyncWithinFalseSyncExtent guards on a checksum failure,
// the scanner must resume scanning from the byte after the false sync, not skip
// the whole claimed (bogus) extent -- a real, complete frame B embedded within a
// corrupt frame A's claimed payload must still be found and emitted.
func TestScanUBXResyncWithinFalseSyncExtent(t *testing.T) {
	payloadB := buildSFRBXPayload(gnss.Galileo, 14, 1, 0, make([]uint32, 8))
	frameB := buildUBX(ubxClassRXM, ubxIDSFRBX, payloadB)

	frameA := buildUBX(ubxClassRXM, ubxIDSFRBX, frameB) // A's payload is exactly frame B
	frameA[len(frameA)-1] ^= 0xFF                       // corrupt A's checksum

	frames, errs := collect(t, scanUBX, frameA)
	if len(errs) == 0 || errs[0] != "ubx_checksum" {
		t.Fatalf("want a leading ubx_checksum error for A, got %v", errs)
	}
	if len(frames) != 1 || frames[0].GnssID != gnss.Galileo {
		t.Fatalf("want frame B still found and emitted despite A's checksum failure, got %+v", frames)
	}
}

func TestScanRTCM(t *testing.T) {
	// A message whose first 12 bits are 1019.
	payload := make([]byte, 20)
	payload[0] = 0x3F
	payload[1] = 0xB0
	full := []byte{rtcmPreamble, byte(len(payload) >> 8), byte(len(payload))}
	full = append(full, payload...)
	c := frame.CRC24Q(full)
	full = append(full, byte(c>>16), byte(c>>8), byte(c))

	frames, errs := collect(t, scanRTCM, full)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(frames) != 1 || frames[0].MsgType != 1019 {
		t.Fatalf("want one RTCM 1019, got %+v", frames)
	}
}

func TestScanRTCMBadCRC(t *testing.T) {
	payload := make([]byte, 10)
	full := []byte{rtcmPreamble, 0x00, byte(len(payload))}
	full = append(full, payload...)
	full = append(full, 0xDE, 0xAD, 0xBE) // wrong CRC
	frames, errs := collect(t, scanRTCM, full)
	if len(frames) != 0 {
		t.Errorf("bad-CRC RTCM should be dropped")
	}
	if len(errs) == 0 || errs[0] != "rtcm_crc" {
		t.Errorf("want rtcm_crc, got %v", errs)
	}
}

// TestScanRTCMShortLength guards a length==1 RTCM message (a valid CRC-24Q
// over an undersized payload is trivial to construct — no message-number bits
// exist) must be rejected before the message-number extraction, not panic on an
// out-of-bounds payload[1] read.
func TestScanRTCMShortLength(t *testing.T) {
	payload := make([]byte, 1)
	full := []byte{rtcmPreamble, 0x00, byte(len(payload))}
	full = append(full, payload...)
	c := frame.CRC24Q(full)
	full = append(full, byte(c>>16), byte(c>>8), byte(c))

	frames, errs := collect(t, scanRTCM, full)
	if len(frames) != 0 {
		t.Errorf("length-1 RTCM message should be dropped, got %+v", frames)
	}
	if len(errs) == 0 || errs[0] != "rtcm_length" {
		t.Errorf("want rtcm_length, got %v", errs)
	}
}

// TestScanRTCMZeroLengthFiller guards length 0 is a legal RTCM3
// filler/keepalive message (no payload, no message number) and must not be
// counted as an rtcm_length error -- the fix spec's exact verification: a
// D3 00 00 filler with a valid CRC, followed by a real 1019, must yield exactly
// one emitted frame and zero rtcm_length errors.
func TestScanRTCMZeroLengthFiller(t *testing.T) {
	filler := []byte{rtcmPreamble, 0x00, 0x00}
	c := frame.CRC24Q(filler)
	filler = append(filler, byte(c>>16), byte(c>>8), byte(c))

	payload := make([]byte, 20)
	payload[0] = 0x3F
	payload[1] = 0xB0
	msg := []byte{rtcmPreamble, byte(len(payload) >> 8), byte(len(payload))}
	msg = append(msg, payload...)
	mc := frame.CRC24Q(msg)
	msg = append(msg, byte(mc>>16), byte(mc>>8), byte(mc))

	full := append(filler, msg...)
	frames, errs := collect(t, scanRTCM, full)
	if len(frames) != 1 || frames[0].MsgType != 1019 {
		t.Fatalf("want exactly one RTCM 1019 frame, got %+v", frames)
	}
	for _, e := range errs {
		if e == "rtcm_length" {
			t.Errorf("zero-length filler counted as rtcm_length error, want none: %v", errs)
		}
	}
}

// TestScanRTCMZeroLengthBadCRCIsError confirms a corrupt zero-length filler (not
// a genuine keepalive, but a false preamble whose length field happens to read
// as 0) is still flagged -- regression fix only exempts a *verified* filler from the
// error path, not any length-0 candidate.
func TestScanRTCMZeroLengthBadCRCIsError(t *testing.T) {
	full := []byte{rtcmPreamble, 0x00, 0x00, 0xDE, 0xAD, 0xBE} // wrong CRC
	frames, errs := collect(t, scanRTCM, full)
	if len(frames) != 0 {
		t.Errorf("bad-CRC zero-length candidate should not emit, got %+v", frames)
	}
	if len(errs) == 0 || errs[0] != "rtcm_crc" {
		t.Errorf("want rtcm_crc, got %v", errs)
	}
}

// TestScanRTCMResyncWithinFalseSyncExtent guards on a checksum failure,
// the scanner must resume scanning from the byte after the false preamble, not
// skip the whole claimed (bogus) extent -- the fix spec's exact scenario: valid
// frame A whose payload contains a false preamble overlapping real frame B;
// corrupt A's CRC, and B must still be found and emitted.
func TestScanRTCMResyncWithinFalseSyncExtent(t *testing.T) {
	// Frame B: a real, valid RTCM 1019 message.
	bPayload := make([]byte, 20)
	bPayload[0] = 0x3F
	bPayload[1] = 0xB0
	frameB := []byte{rtcmPreamble, byte(len(bPayload) >> 8), byte(len(bPayload))}
	frameB = append(frameB, bPayload...)
	bc := frame.CRC24Q(frameB)
	frameB = append(frameB, byte(bc>>16), byte(bc>>8), byte(bc))

	// Frame A: a "valid-length" RTCM message whose payload is exactly frame B
	// (so B is fully contained within A's claimed extent), but A's own trailing
	// CRC is deliberately wrong -- a checksum failure on a false/corrupt sync.
	frameA := []byte{rtcmPreamble, byte(len(frameB) >> 8), byte(len(frameB))}
	frameA = append(frameA, frameB...)
	frameA = append(frameA, 0xDE, 0xAD, 0xBE) // wrong CRC for A

	frames, errs := collect(t, scanRTCM, frameA)
	if len(errs) == 0 || errs[0] != "rtcm_crc" {
		t.Fatalf("want a leading rtcm_crc error for A, got %v", errs)
	}
	if len(frames) != 1 || frames[0].MsgType != 1019 {
		t.Fatalf("want frame B still found and emitted despite A's CRC failure, got %+v", frames)
	}
}

func TestScanSBF(t *testing.T) {
	blockNum := uint16(4017) // GPSRawCA
	body := make([]byte, 16)
	for i := range body {
		body[i] = byte(i)
	}
	length := 8 + len(body)
	hdr := make([]byte, 4) // ID(2), Length(2), LE
	binary.LittleEndian.PutUint16(hdr[0:], blockNum)
	binary.LittleEndian.PutUint16(hdr[2:], uint16(length))
	crc := crc16ccitt(hdr, body)
	msg := []byte{sbfSync1, sbfSync2, byte(crc), byte(crc >> 8)}
	msg = append(msg, hdr...)
	msg = append(msg, body...)

	frames, errs := collect(t, scanSBF, msg)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if len(frames) != 1 || frames[0].MsgType != 4017 {
		t.Fatalf("want one SBF 4017, got %+v", frames)
	}
	if !bytes.Equal(frames[0].Bytes, body) {
		t.Errorf("SBF body mismatch")
	}
}

// TestScanSBFResyncWithinFalseSyncExtent guards on a CRC failure, the
// scanner must resume scanning from the byte after the false sync, not skip the
// whole claimed (bogus) extent -- a real, complete block B embedded within a
// corrupt block A's claimed body must still be found and emitted.
func TestScanSBFResyncWithinFalseSyncExtent(t *testing.T) {
	// Frame B: a valid SBF block (same construction as TestScanSBF).
	blockNum := uint16(4017)
	bodyB := make([]byte, 16)
	for i := range bodyB {
		bodyB[i] = byte(i)
	}
	lengthB := 8 + len(bodyB)
	hdrB := make([]byte, 4)
	binary.LittleEndian.PutUint16(hdrB[0:], blockNum)
	binary.LittleEndian.PutUint16(hdrB[2:], uint16(lengthB))
	crcB := crc16ccitt(hdrB, bodyB)
	frameB := []byte{sbfSync1, sbfSync2, byte(crcB), byte(crcB >> 8)}
	frameB = append(frameB, hdrB...)
	frameB = append(frameB, bodyB...)

	// Frame A: a "valid-length" SBF block whose body is exactly frame B (so B
	// is fully contained within A's claimed extent), but A's CRC is
	// deliberately wrong -- a checksum failure on a false/corrupt sync.
	bodyA := append([]byte(nil), frameB...)
	for (8+len(bodyA))%4 != 0 { // length must stay a multiple of 4
		bodyA = append(bodyA, 0)
	}
	lengthA := 8 + len(bodyA)
	hdrA := make([]byte, 4)
	binary.LittleEndian.PutUint16(hdrA[0:], 9999)
	binary.LittleEndian.PutUint16(hdrA[2:], uint16(lengthA))
	crcA := crc16ccitt(hdrA, bodyA)
	frameA := []byte{sbfSync1, sbfSync2, byte(crcA), byte(crcA >> 8)}
	frameA = append(frameA, hdrA...)
	frameA = append(frameA, bodyA...)
	frameA[3] ^= 0xFF // corrupt A's CRC high byte

	frames, errs := collect(t, scanSBF, frameA)
	if len(errs) == 0 || errs[0] != "sbf_crc" {
		t.Fatalf("want a leading sbf_crc error for A, got %v", errs)
	}
	if len(frames) != 1 || frames[0].MsgType != 4017 {
		t.Fatalf("want frame B still found and emitted despite A's CRC failure, got %+v", frames)
	}
}

// TestScanUBXRAWX drives a synthetic UBX-RXM-RAWX message through the scanner and
// checks the per-measurement observation frames (u-blox interface description
// layout: 16-byte header + 32 bytes per measurement). The current fleet captures
// carry SFRBX only, so this parser is validated synthetically until a
// RAWX-enabled capture exists.
func TestScanUBXRAWX(t *testing.T) {
	body := make([]byte, 16+2*32)
	binary.LittleEndian.PutUint64(body[0:], math.Float64bits(345601.25)) // rcvTow
	binary.LittleEndian.PutUint16(body[8:], 2372)                        // week
	body[11] = 2                                                         // numMeas
	m0 := body[16:]
	binary.LittleEndian.PutUint64(m0[0:], math.Float64bits(2.2e7))    // prMes
	binary.LittleEndian.PutUint64(m0[8:], math.Float64bits(1.15e8))   // cpMes cycles
	binary.LittleEndian.PutUint32(m0[16:], math.Float32bits(-1234.5)) // doMes
	m0[20], m0[21], m0[22], m0[23] = 0, 7, 0, 0                       // GPS G07 L1
	binary.LittleEndian.PutUint16(m0[24:], 60000)                     // locktime
	m0[26] = 47                                                       // cno
	m0[30] = 0x03                                                     // pr+cp valid
	m1 := body[48:]
	binary.LittleEndian.PutUint64(m1[0:], math.Float64bits(2.2e7+5)) // prMes
	m1[20], m1[21], m1[22] = 0, 7, 6                                 // GPS G07 L5-I
	m1[30] = 0x01                                                    // pr valid, cp invalid

	msg := buildUBX(0x02, 0x15, body)
	var got []*RawFrame
	err := scanUBX(bytes.NewReader(msg), "test", fixedTime,
		func(f *RawFrame) { got = append(got, f) }, func(string) {})
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Fatalf("scan err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("emitted %d observation frames, want 2", len(got))
	}
	o := got[0]
	if o.Obs == nil || o.GnssID != gnss.GPS || o.SvID != 7 || o.SigID != 0 {
		t.Fatalf("first obs frame = %+v", o)
	}
	if o.Obs.RcvTow != 345601.25 || o.Obs.Week != 2372 || o.Obs.PrM != 2.2e7 ||
		o.Obs.Cn0 != 47 || o.Obs.LockTimeMs != 60000 || !o.Obs.CpValid {
		t.Fatalf("first obs fields = %+v", o.Obs)
	}
	if got[1].SigID != 6 || got[1].Obs.CpValid {
		t.Fatalf("second obs frame = sig %d cpValid %v, want 6/false", got[1].SigID, got[1].Obs.CpValid)
	}
}

func TestParseRAWXRejectsNonFiniteFieldsIndependently(t *testing.T) {
	base := func() []byte {
		body := make([]byte, 16+2*32)
		binary.LittleEndian.PutUint64(body, math.Float64bits(100000))
		binary.LittleEndian.PutUint16(body[8:], 2372)
		body[11] = 2
		for i := 0; i < 2; i++ {
			m := body[16+i*32:]
			binary.LittleEndian.PutUint64(m, math.Float64bits(2.2e7+float64(i)))
			binary.LittleEndian.PutUint64(m[8:], math.Float64bits(1.1e8))
			binary.LittleEndian.PutUint32(m[16:], math.Float32bits(-100))
			m[20], m[21], m[22], m[30] = 0, 7, byte(i*6), 0x03
		}
		return body
	}
	for _, tc := range []struct {
		name      string
		mutate    func([]byte)
		want      int
		wantBreak bool
	}{
		{"nan tow rejects epoch", func(b []byte) { binary.LittleEndian.PutUint64(b, math.Float64bits(math.NaN())) }, 0, false},
		{"inf pseudorange keeps sibling", func(b []byte) { binary.LittleEndian.PutUint64(b[16:], math.Float64bits(math.Inf(1))) }, 1, false},
		{"nan doppler keeps sibling", func(b []byte) { binary.LittleEndian.PutUint32(b[32:], math.Float32bits(float32(math.NaN()))) }, 1, false},
		{"nan carrier emits arc break", func(b []byte) { binary.LittleEndian.PutUint64(b[24:], math.Float64bits(math.NaN())) }, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := base()
			tc.mutate(body)
			var got []*RawFrame
			n := parseRAWX(body, "test", fixedTime(), func(f *RawFrame) { got = append(got, f) })
			if n != tc.want || len(got) != tc.want {
				t.Fatalf("emitted %d/%d, want %d", n, len(got), tc.want)
			}
			if tc.wantBreak && (!got[0].Obs.ArcBreak || got[0].Obs.CpValid) {
				t.Fatalf("invalid carrier did not produce a non-mutating arc break: %+v", got[0].Obs)
			}
		})
	}
}
