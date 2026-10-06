package ingest

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/wire"
)

func samplePVT(tow uint32) *SolutionPVT {
	return &SolutionPVT{
		TOWMS: tow, Year: 2026, Month: 10, Day: 5, Hour: 21, Minute: 14, Second: 30,
		UTCValid: UTCValidDate | UTCValidTime | UTCValidFullyResolved, TAccNS: 25, NanoNS: -1234,
		FixType: 3, FixFlags: FixFlagOK | FixFlagDifferential, NumSV: 17,
		LonE7: -1220841000, LatE7: 374219000, HeightMM: 12500, HAccMM: 1500, VAccMM: 2500,
		VelNMMS: 12, VelEMMS: -7, VelDMMS: 3, SAccMMS: 90, PDOPx100: 135,
	}
}

func sampleSolution(tow uint32) *ReceiverSolution {
	return &ReceiverSolution{
		PVT:    samplePVT(tow),
		Clock:  &SolutionClock{TOWMS: tow, BiasNS: -412_345, DriftNSS: 87, TAccNS: 25, FAccPSS: 410},
		Status: &SolutionStatus{TOWMS: tow, FixType: 3, Flags: 0x0d, FixStatus: 0x01, SpoofState: 1, TTFFMS: 28_000, SinceStart: 3_600_000},
	}
}

func TestReceiverSolutionRoundTrip(t *testing.T) {
	for name, s := range map[string]*ReceiverSolution{
		"all blocks":  sampleSolution(345_600_000),
		"pvt only":    {PVT: samplePVT(1000)},
		"clock only":  {Clock: &SolutionClock{TOWMS: 1000, BiasNS: 5}},
		"status only": {Status: &SolutionStatus{TOWMS: 1000, SpoofState: 3}},
	} {
		b := EncodeReceiverSolution(s)
		got, err := decodeReceiverSolution(b)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !reflect.DeepEqual(got, s) {
			t.Fatalf("%s: round trip\n got %+v\nwant %+v", name, got, s)
		}
	}
	if n := len(EncodeReceiverSolution(sampleSolution(0))); n != 2+64+20+16 {
		t.Fatalf("full body is %d bytes, want 102", n)
	}
}

func TestReceiverSolutionRejects(t *testing.T) {
	good := EncodeReceiverSolution(sampleSolution(1000))
	mutate := func(f func(b []byte) []byte) []byte { return f(append([]byte(nil), good...)) }
	cases := map[string][]byte{
		"empty":           {},
		"version":         mutate(func(b []byte) []byte { b[0] = 2; return b }),
		"no blocks":       {1, 0},
		"unknown block":   mutate(func(b []byte) []byte { b[1] |= 0x08; return b }),
		"short":           good[:len(good)-1],
		"trailing":        append(append([]byte(nil), good...), 0),
		"week overflow":   mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[2:], solutionWeekMS); return b }),
		"utc bit":         mutate(func(b []byte) []byte { b[2+11] |= 0x08; return b }),
		"bad month":       mutate(func(b []byte) []byte { b[2+6] = 13; return b }),
		"bad hour":        mutate(func(b []byte) []byte { b[2+8] = 24; return b }),
		"fix type":        mutate(func(b []byte) []byte { b[2+20] = 6; return b }),
		"fix flag":        mutate(func(b []byte) []byte { b[2+21] |= 0x04; return b }),
		"carrier both":    mutate(func(b []byte) []byte { b[2+21] |= 0xC0; return b }),
		"reserved byte":   mutate(func(b []byte) []byte { b[2+23] = 1; return b }),
		"latitude":        mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[2+28:], 900_000_001); return b }),
		"position flag":   mutate(func(b []byte) []byte { b[2+62] = 0x80; return b }),
		"nano":            mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[2+16:], 1_000_000_000); return b }),
		"clock week":      mutate(func(b []byte) []byte { binary.BigEndian.PutUint32(b[66:], solutionWeekMS); return b }),
		"status spoofing": mutate(func(b []byte) []byte { b[86+7] = 4; return b }),
		"status fix type": mutate(func(b []byte) []byte { b[86+4] = 6; return b }),
	}
	for name, b := range cases {
		if _, err := decodeReceiverSolution(b); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// Calendar fields are unchecked while the receiver says they are not valid.
	loose := sampleSolution(1000)
	loose.PVT.UTCValid, loose.PVT.Month, loose.PVT.Hour = 0, 0, 99
	if _, err := decodeReceiverSolution(EncodeReceiverSolution(loose)); err != nil {
		t.Errorf("invalid-flagged calendar rejected: %v", err)
	}
}

func TestSolutionPVTUTC(t *testing.T) {
	p := samplePVT(0)
	got, ok := p.UTC()
	want := time.Date(2026, 10, 5, 21, 14, 30, 0, time.UTC).Add(-1234 * time.Nanosecond)
	if !ok || !got.Equal(want) {
		t.Fatalf("UTC = %v %v, want %v", got, ok, want)
	}
	p.UTCValid &^= UTCValidFullyResolved
	if _, ok := p.UTC(); ok {
		t.Fatal("UTC valid without full resolution")
	}
}

// signed returns the two's-complement bits of a signed field.
func signed(v int32) uint32 { return uint32(v) }

// ubxPVT builds a NAV-PVT payload at the u-blox interface description offsets.
func ubxPVT(tow uint32, latE7, lonE7 int32) []byte {
	p := make([]byte, ubxNAVPVTLen)
	le := binary.LittleEndian
	le.PutUint32(p, tow)
	le.PutUint16(p[4:], 2026)
	p[6], p[7], p[8], p[9], p[10] = 10, 5, 21, 14, 30
	p[11] = 0x07 | 0x08 // date, time, fully resolved, and validMag (dropped)
	le.PutUint32(p[12:], 25)
	le.PutUint32(p[16:], signed(-1234))
	p[20], p[21], p[22], p[23] = 3, 0x01|0x02|0x0c|0x20, 0xe0, 17 // psmState and headVehValid bits are dropped
	le.PutUint32(p[24:], uint32(lonE7))
	le.PutUint32(p[28:], uint32(latE7))
	le.PutUint32(p[32:], 12500)
	le.PutUint32(p[36:], 45000) // hMSL, not carried
	le.PutUint32(p[40:], 1500)
	le.PutUint32(p[44:], 2500)
	le.PutUint32(p[48:], 12)
	le.PutUint32(p[52:], signed(-7))
	le.PutUint32(p[56:], 3)
	le.PutUint32(p[68:], 90)
	le.PutUint16(p[76:], 135)
	le.PutUint16(p[78:], 0x0001|0x001e) // invalidLlh plus lastCorrectionAge (dropped)
	return p
}

func ubxClock(tow uint32, bias, drift int32) []byte {
	p := make([]byte, ubxNAVCLOCKLen)
	le := binary.LittleEndian
	le.PutUint32(p, tow)
	le.PutUint32(p[4:], uint32(bias))
	le.PutUint32(p[8:], uint32(drift))
	le.PutUint32(p[12:], 25)
	le.PutUint32(p[16:], 410)
	return p
}

func ubxStatus(tow uint32, spoof byte) []byte {
	p := make([]byte, ubxNAVSTATUSLen)
	le := binary.LittleEndian
	le.PutUint32(p, tow)
	p[4], p[5], p[6], p[7] = 3, 0x0d, 0x01, spoof<<3|0x02
	le.PutUint32(p[8:], 28_000)
	le.PutUint32(p[12:], 3_600_000)
	return p
}

func ubxEOE(tow uint32) []byte {
	p := make([]byte, ubxNAVEOELen)
	binary.LittleEndian.PutUint32(p, tow)
	return p
}

func scanSolutions(t *testing.T, stream []byte) ([]*RawFrame, []string) {
	t.Helper()
	var frames []*RawFrame
	var errs []string
	_ = scanUBX(bytes.NewReader(stream), "stn", fixedTime, func(f *RawFrame) { frames = append(frames, f) }, func(k string) { errs = append(errs, k) })
	return frames, errs
}

func TestScanUBXAssemblesEpochs(t *testing.T) {
	var stream []byte
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVPVT, ubxPVT(1000, 374219000, -1220841000))...)
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVCLOCK, ubxClock(1000, -412_345, 87))...)
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVSTATUS, ubxStatus(1000, 2))...)
	// The next epoch's first block completes epoch 1000; EOE completes epoch 2000.
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVCLOCK, ubxClock(2000, -412_258, 87))...)
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVEOE, ubxEOE(2000))...)
	// A stale EOE for an epoch that is not pending does nothing.
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVEOE, ubxEOE(1000))...)
	frames, errs := scanSolutions(t, stream)
	if len(errs) != 0 {
		t.Fatalf("errors: %v", errs)
	}
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}
	first := frames[0]
	if first.Solution == nil || first.MsgType != TelemReceiverSolution || first.Source != "stn" {
		t.Fatalf("first frame = %+v", first)
	}
	s := first.Solution
	want := samplePVT(1000)
	want.PosFlags = PosFlagInvalidLLH
	if !reflect.DeepEqual(s.PVT, want) {
		t.Fatalf("PVT parsed at the wrong offsets:\n got %+v\nwant %+v", s.PVT, want)
	}
	if *s.Clock != (SolutionClock{TOWMS: 1000, BiasNS: -412_345, DriftNSS: 87, TAccNS: 25, FAccPSS: 410}) {
		t.Fatalf("clock = %+v", s.Clock)
	}
	if *s.Status != (SolutionStatus{TOWMS: 1000, FixType: 3, Flags: 0x0d, FixStatus: 0x01, SpoofState: 2, TTFFMS: 28_000, SinceStart: 3_600_000}) {
		t.Fatalf("status = %+v", s.Status)
	}
	if second := frames[1].Solution; second.PVT != nil || second.Status != nil || second.Clock.TOWMS != 2000 {
		t.Fatalf("second epoch = %+v", second)
	}
}

func TestScanUBXRejectsMalformedSolutionBlocks(t *testing.T) {
	stream := ubxMsg(ubxClassNAV, ubxIDNAVPVT, make([]byte, 84)) // the old 84-byte NAV-PVT
	bad := ubxPVT(1000, 900_000_001, 0)
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVPVT, bad)...)
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVCLOCK, ubxClock(solutionWeekMS, 0, 0))...)
	stream = append(stream, ubxMsg(ubxClassNAV, ubxIDNAVSTATUS, make([]byte, 15))...)
	frames, errs := scanSolutions(t, stream)
	if len(frames) != 0 || !reflect.DeepEqual(errs, []string{"ubx_navpvt", "ubx_navpvt", "ubx_navclock", "ubx_navstatus"}) {
		t.Fatalf("frames %d, errors %v", len(frames), errs)
	}
}

func TestRecordToFrameSolution(t *testing.T) {
	body := EncodeReceiverSolution(sampleSolution(1000))
	f := recordToFrame(wire.RawRecord{FrameType: TelemReceiverSolution, Raw: body}, "ubx", "obs7")
	if f == nil || f.Solution == nil || f.Words != nil || f.RF != nil || f.RecvStamped {
		t.Fatalf("unstamped solution frame = %+v", f)
	}
	if !reflect.DeepEqual(f.Bytes, body) {
		t.Fatal("exact solution body was not retained for persistence")
	}
	stamp := time.Now().Add(-2 * time.Second)
	f = recordToFrame(wire.RawRecord{FrameType: TelemReceiverSolution, Raw: body, RecvUnixNs: stamp.UnixNano()}, "ubx", "obs7")
	if f == nil || !f.RecvStamped || !f.Recv.Equal(time.Unix(0, stamp.UnixNano())) {
		t.Fatalf("stamped solution frame = %+v", f)
	}
	f = recordToFrame(wire.RawRecord{FrameType: TelemReceiverSolution, Raw: body, RecvUnixNs: time.Now().Add(time.Hour).UnixNano()}, "ubx", "obs7")
	if f == nil || f.RecvStamped {
		t.Fatalf("implausible stamp accepted as the observer's: %+v", f)
	}
	if recordToFrame(wire.RawRecord{FrameType: TelemReceiverSolution, Raw: body, SvID: 3}, "ubx", "obs7") != nil {
		t.Fatal("solution record with satellite header fields accepted")
	}
	if recordToFrame(wire.RawRecord{FrameType: TelemReceiverSolution, Raw: body[:10]}, "ubx", "obs7") != nil {
		t.Fatal("malformed solution body accepted")
	}
}

func FuzzReceiverSolution(f *testing.F) {
	f.Add(EncodeReceiverSolution(sampleSolution(1000)))
	f.Add(EncodeReceiverSolution(&ReceiverSolution{Clock: &SolutionClock{TOWMS: 5}}))
	f.Add([]byte{1, 7})
	f.Fuzz(func(t *testing.T, b []byte) {
		s, err := decodeReceiverSolution(b)
		if err != nil {
			return
		}
		// Anything accepted re-encodes to exactly the accepted bytes.
		if again := EncodeReceiverSolution(s); !bytes.Equal(again, b) {
			t.Fatalf("re-encoding differs:\n%x\n%x", again, b)
		}
	})
}
