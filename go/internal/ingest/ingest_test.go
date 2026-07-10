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
