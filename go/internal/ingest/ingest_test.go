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

// TestScanUBXRejectsOutOfDomainGnssID guards the SFRBX gnssId byte is
// receiver metadata outside any nav CRC, and nothing downstream re-checks it —
// an out-of-domain id (IMES=4, 8..255) must be rejected at the boundary under
// its own error label, before it can mint a FramesTotal series or persist a
// nav_frames row outside the schema's documented 0..7 domain.
func TestScanUBXRejectsOutOfDomainGnssID(t *testing.T) {
	words := make([]uint32, 10)
	for _, bad := range []gnss.GNSSID{4, 8, 42, 255} { // IMES and out-of-range
		msg := buildUBX(ubxClassRXM, ubxIDSFRBX, buildSFRBXPayload(bad, 5, 0, 0, words))
		frames, errs := collect(t, scanUBX, msg)
		if len(frames) != 0 {
			t.Errorf("gnssId %d emitted %d frames, want 0", bad, len(frames))
		}
		if len(errs) != 1 || errs[0] != "ubx_gnssid" {
			t.Errorf("gnssId %d errors = %v, want [ubx_gnssid]", bad, errs)
		}
	}
	// Boundary: NavIC (7) is the top of the valid domain and must pass.
	msg := buildUBX(ubxClassRXM, ubxIDSFRBX, buildSFRBXPayload(gnss.NavIC, 5, 0, 0, words))
	frames, errs := collect(t, scanUBX, msg)
	if len(frames) != 1 || len(errs) != 0 {
		t.Errorf("gnssId 7 frames/errs = %d/%v, want 1/none", len(frames), errs)
	}
}

// TestScanUBXRAWXRejectsOutOfDomainGnssID : a RAWX measurement whose
// gnssId byte is out of domain is dropped per-field (like the other invalid
// measurement fields) while its well-formed siblings still emit.
func TestScanUBXRAWXRejectsOutOfDomainGnssID(t *testing.T) {
	body := make([]byte, 16+2*32)
	binary.LittleEndian.PutUint64(body[0:], math.Float64bits(345601.25))
	binary.LittleEndian.PutUint16(body[8:], 2372)
	body[11] = 2
	m0 := body[16:]
	binary.LittleEndian.PutUint64(m0[0:], math.Float64bits(2.2e7))
	m0[20], m0[21] = 42, 7 // out-of-domain gnssId
	m0[30] = 0x01
	m1 := body[48:]
	binary.LittleEndian.PutUint64(m1[0:], math.Float64bits(2.2e7+5))
	m1[20], m1[21] = 0, 7 // valid GPS
	m1[30] = 0x01

	msg := buildUBX(0x02, 0x15, body)
	var got []*RawFrame
	err := scanUBX(bytes.NewReader(msg), "test", fixedTime,
		func(f *RawFrame) { got = append(got, f) }, func(string) {})
	if err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Fatalf("scan err = %v", err)
	}
	if len(got) != 1 || got[0].GnssID != gnss.GPS {
		t.Fatalf("emitted %d frames (want just the valid GPS measurement): %+v", len(got), got)
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

// TestCRC16CCITTGoldenVectors guards the SBF framing tests build their
// expected CRCs by calling crc16ccitt itself, so alone they verify resync but
// could never catch a wrong polynomial, init value, or shift direction — every
// real SBF block would then be silently dropped as sbf_crc with SourceUp=1. These
// vectors come from an INDEPENDENTLY-AUTHORED implementation of the same named
// algorithm (CRC-16-CCITT, poly 0x1021, init 0, unreflected): CPython's
// binascii.crc_hqx. Reproduce with:
//
//	python3 -c "import binascii; print(hex(binascii.crc_hqx(b'123456789', 0)))"
//	  -> 0x31c3  (the standard CRC-catalog check string)
//	python3 -c "import binascii; print(hex(binascii.crc_hqx(
//	    (4017).to_bytes(2,'little') + (24).to_bytes(2,'little') + bytes(range(16)), 0)))"
//	  -> 0x1a31  (the exact ID+Length+body sequence TestScanSBF frames)
//
// Not invented from memory: both values were machine-computed from binascii at
// fix time (regression fix fix log). Remaining gap, deliberately left open: a vector
// from a REAL captured SBF block (needs Septentrio hardware or an SBF-REF
// worked example — the guide is cite-only, reference/REFERENCES.md §3c) would
// additionally pin that Septentrio's on-wire CRC is this exact algorithm.
func TestCRC16CCITTGoldenVectors(t *testing.T) {
	if got := crc16ccitt([]byte("123456789")); got != 0x31C3 {
		t.Errorf("crc16ccitt(123456789) = %#04x, want 0x31c3 (CRC-16/XMODEM catalog check via binascii.crc_hqx)", got)
	}
	hdr := []byte{0xB1, 0x0F, 0x18, 0x00} // ID=4017 LE, Length=24 LE
	body := make([]byte, 16)
	for i := range body {
		body[i] = byte(i)
	}
	if got := crc16ccitt(hdr, body); got != 0x1A31 {
		t.Errorf("crc16ccitt(sbf hdr+body) = %#04x, want 0x1a31 (binascii.crc_hqx)", got)
	}
	// Group splitting must not change the digest (the scanner passes ID+Length and
	// body as separate groups).
	if crc16ccitt(hdr, body) != crc16ccitt(append(append([]byte(nil), hdr...), body...)) {
		t.Error("crc16ccitt digest differs across group boundaries")
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

// TestScanUBXRAWXColdStartNotAParseError guards a checksum-valid RAWX
// that is structurally fine but carries nothing usable — cold-start week==0, or
// every measurement's PR-valid bit clear — must NOT count as a ubx_rawx ingest
// error (it previously did, inflating the error metric through every receiver
// cold/warm start and double-counting the week==0 case, which RawObsInvalidTotal
// already accounts). A truncated body (numMeas overrunning the payload) must
// still count: the checksum passed, so the corruption is real.
func TestScanUBXRAWXColdStartNotAParseError(t *testing.T) {
	measurement := func(body []byte, i int, trkStat byte) {
		m := body[16+i*32:]
		binary.LittleEndian.PutUint64(m, math.Float64bits(2.2e7))
		binary.LittleEndian.PutUint32(m[16:], math.Float32bits(-100))
		m[20], m[21], m[22], m[30] = 0, 7, 0, trkStat
	}
	t.Run("cold start week zero", func(t *testing.T) {
		body := make([]byte, 16+1*32)
		binary.LittleEndian.PutUint64(body, math.Float64bits(100000)) // valid rcvTow
		// week stays 0: the receiver has no time solution yet
		body[11] = 1
		measurement(body, 0, 0x03)
		frames, errs := collect(t, scanUBX, buildUBX(ubxClassRXM, ubxIDRAWX, body))
		if len(frames) != 0 || len(errs) != 0 {
			t.Fatalf("cold-start RAWX: frames=%d errs=%v, want 0 frames and NO ubx_rawx error", len(frames), errs)
		}
	})
	t.Run("all pseudoranges invalid", func(t *testing.T) {
		body := make([]byte, 16+2*32)
		binary.LittleEndian.PutUint64(body, math.Float64bits(100000))
		binary.LittleEndian.PutUint16(body[8:], 2372)
		body[11] = 2
		measurement(body, 0, 0x00) // PR-valid bit clear
		measurement(body, 1, 0x00)
		frames, errs := collect(t, scanUBX, buildUBX(ubxClassRXM, ubxIDRAWX, body))
		if len(frames) != 0 || len(errs) != 0 {
			t.Fatalf("no-PR-valid RAWX: frames=%d errs=%v, want 0 frames and NO ubx_rawx error", len(frames), errs)
		}
	})
	t.Run("truncated body still errors", func(t *testing.T) {
		body := make([]byte, 16+1*32)
		binary.LittleEndian.PutUint64(body, math.Float64bits(100000))
		binary.LittleEndian.PutUint16(body[8:], 2372)
		body[11] = 3 // claims 3 measurements; payload carries 1
		measurement(body, 0, 0x03)
		frames, errs := collect(t, scanUBX, buildUBX(ubxClassRXM, ubxIDRAWX, body))
		if len(frames) != 0 {
			t.Fatalf("truncated RAWX emitted %d frames, want 0", len(frames))
		}
		if len(errs) != 1 || errs[0] != "ubx_rawx" {
			t.Fatalf("truncated RAWX errs=%v, want exactly [ubx_rawx]", errs)
		}
	})
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
			n, ok := parseRAWX(body, "test", fixedTime(), func(f *RawFrame) { got = append(got, f) })
			if !ok {
				t.Fatal("structurally-valid payload reported malformed")
			}
			if n != tc.want || len(got) != tc.want {
				t.Fatalf("emitted %d/%d, want %d", n, len(got), tc.want)
			}
			if tc.wantBreak && (!got[0].Obs.ArcBreak || got[0].Obs.CpValid) {
				t.Fatalf("invalid carrier did not produce a non-mutating arc break: %+v", got[0].Obs)
			}
		})
	}
}
