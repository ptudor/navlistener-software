package ingest

import (
	"bytes"
	"encoding/binary"
	"io"
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
