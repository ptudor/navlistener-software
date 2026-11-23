package ingest

import (
	"encoding/binary"
	"testing"
)

func TestRFDetailFields(t *testing.T) {
	frames := scanRF(t, syntheticRFCapture())
	if len(frames) != 8 {
		t.Fatal(len(frames))
	}
	s := frames[3].RF.SignalSecurity
	if s == nil || !s.JamEnabled || s.JamState != 2 || !s.SpoofEnabled || s.SpoofState != 1 ||
		len(s.Centers) != 2 || s.Centers[0] != (RFCenter{1575420, true}) || s.Centers[1] != (RFCenter{1176450, false}) {
		t.Fatalf("wrong SEC-SIG %+v", s)
	}
	log := frames[4].RF.SecurityLog
	if log == nil || len(*log) != 1 || (*log)[0] != (SecurityEvent{42, 4, 0}) {
		t.Fatalf("wrong SIGLOG %+v", log)
	}
	for path := range 2 {
		s := frames[5+path].RF.Spectrum
		if s == nil || s.Path != byte(path) || s.Paths != 2 || s.PGADB != 3 || s.ResolutionHz != 500000 ||
			s.SpanHz != 128000000 || s.PeakHz != int64(1586000000-400000000*path+500000*path) ||
			len(s.Bins) != 256 || s.Bins[127+path] != 80 || s.PeakMedianDB != 20 {
			t.Fatalf("wrong spectrum path %d: %+v", path, s)
		}
	}
	if h := frames[7].RF.HighSensitivity; h == nil || *h {
		t.Fatal("present and disabled high sensitivity was lost")
	}
}

func TestRFDetailBounds(t *testing.T) {
	for _, b := range [][]byte{{2}, {2, 2, 5, 1}, {2, 3, 5, 0, 0}, {2, 0x51, 2}, {2, 0x51},
		{2, 0x10, 1, 1, 0, 0, 0, 0, 0, 0}, {2, 0x31, 0, 1}, {2, 0xff},
		append([]byte{2, 0x10, 1, 17}, make([]byte, 6+17*8)...),
		append([]byte{2, 0x31, 2, 2}, make([]byte, 272)...)} {
		if _, err := decodeRFStats(b); err == nil {
			t.Errorf("accepted invalid RF detail header %x", b[:min(4, len(b))])
		}
	}
	empty, err := decodeRFStats([]byte{2, 0x10, 1, 0, 0, 0, 0, 0, 0, 0})
	if err != nil || empty.SecurityLog == nil || *empty.SecurityLog == nil || len(*empty.SecurityLog) != 0 {
		t.Fatal("empty event log", err)
	}
	b := make([]byte, 1024)
	b[0], b[1], b[3] = 2, 3, 255
	binary.LittleEndian.PutUint32(b[1020:], 0xfe000000|1602000)
	r, err := decodeRFStats(b)
	if err != nil || r.SignalSecurity.JamEnabled || len(r.SignalSecurity.Centers) != 255 ||
		r.SignalSecurity.Centers[254] != (RFCenter{1602000, false}) {
		t.Fatal("maximum frequency list with reserved bits", err)
	}
}
