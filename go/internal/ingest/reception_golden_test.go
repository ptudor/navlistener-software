package ingest

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"testing"
)

// receptionGoldenPath is the shared case every implementation of ReceptionData body
// version 2 is checked against: this codec, common/reception_data.h in the ESP32 host
// tests, and the C feeder through the collector.
const receptionGoldenPath = "../../../testdata/reception_data_v2.txt"

// navSatBlock is one 12-byte NAV-SAT satellite block, little-endian.
func navSatBlock(gnss, sv, cno byte, elev int8, azim, prRes int16, flags uint32) []byte {
	b := make([]byte, 12)
	b[0], b[1], b[2], b[3] = gnss, sv, cno, byte(elev)
	binary.LittleEndian.PutUint16(b[4:], uint16(azim))
	binary.LittleEndian.PutUint16(b[6:], uint16(prRes))
	binary.LittleEndian.PutUint32(b[8:], flags)
	return b
}

// ubxNavSat builds a NAV-SAT payload (version 1) from satellite blocks.
func ubxNavSat(tow uint32, blocks ...[]byte) []byte {
	p := make([]byte, 8, 8+12*len(blocks))
	binary.LittleEndian.PutUint32(p, tow)
	p[4], p[5] = 1, byte(len(blocks))
	for _, b := range blocks {
		p = append(p, b...)
	}
	return p
}

// goldenNavSat lists an untracked satellite first, so the record must reorder it after
// the tracked ones, and sets bits the record drops (differential correction, smoothing,
// orbit source, ephemeris availability) and the reserved health value.
func goldenNavSat() []byte {
	return ubxNavSat(345_600_000,
		navSatBlock(0, 3, 0, 15, 300, 0, 0x1|1<<4|0x100),                 // untracked, searching
		navSatBlock(0, 5, 47, 61, 123, -12, 7|0x08|1<<4|0x40|0x80|0x800), // used, healthy
		navSatBlock(2, 14, 33, 12, 359, 87, 4|2<<4),                      // tracked, unhealthy
		navSatBlock(6, 3, 25, 91, 0, 32767, 5|0x08|3<<4),                 // elevation unknown, reserved health
		navSatBlock(3, 21, 41, -5, 180, -32768, 6|0x08),                  // below the horizon, health unknown
	)
}

func TestReceptionDataGolden(t *testing.T) {
	payload := goldenNavSat()
	if !*updateGolden {
		g := readGoldenFile(t, receptionGoldenPath)
		if !bytes.Equal(g["nav-sat"], payload) {
			t.Fatalf("golden nav-sat differs from the test's payload:\n%x\n%x", g["nav-sat"], payload)
		}
	}
	frames := scanRF(t, ubxMsg(ubxClassNAV, ubxIDNAVSAT, payload))
	if len(frames) != 1 || frames[0].RF == nil {
		t.Fatalf("frames = %+v", frames)
	}
	body := EncodeReceptionData(frames[0].RF.Sats)
	if *updateGolden {
		var buf bytes.Buffer
		buf.WriteString("# Telemetry 0x01 (reception) golden case, body version 2\n")
		buf.WriteString("# (common/reception_data.h). nav-sat is one UBX-NAV-SAT payload, little-endian as\n")
		buf.WriteString("# the receiver sends it: an untracked satellite listed first, the reserved health\n")
		buf.WriteString("# value, residual extremes, an out-of-range elevation, and flag bits the record\n")
		buf.WriteString("# drops. body is the record body every implementation must produce from it.\n")
		buf.WriteString("# Regenerate with\n")
		buf.WriteString("#   go test ./internal/ingest -run TestReceptionDataGolden -update\n")
		fmt.Fprintf(&buf, "nav-sat %s\n", hex.EncodeToString(payload))
		fmt.Fprintf(&buf, "body %s\n", hex.EncodeToString(body))
		if err := os.WriteFile(receptionGoldenPath, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want := readGoldenFile(t, receptionGoldenPath)["body"]
	if !bytes.Equal(body, want) {
		t.Fatalf("encoded body differs from golden:\n got %x\nwant %x", body, want)
	}
	sats, err := decodeReceptionData(want)
	if err != nil {
		t.Fatal(err)
	}
	wantSats := []SatCN0{
		{GnssID: 0, SvID: 5, Cn0: 47, ElevDeg: 61, Used: true, Extended: true, AziDeg: 123, PrResDM: -12, Quality: 7, Health: 1},
		{GnssID: 2, SvID: 14, Cn0: 33, ElevDeg: 12, Extended: true, AziDeg: 359, PrResDM: 87, Quality: 4, Health: 2},
		{GnssID: 6, SvID: 3, Cn0: 25, ElevDeg: 91, Used: true, Extended: true, AziDeg: 0, PrResDM: 32767, Quality: 5, Health: 0},
		{GnssID: 3, SvID: 21, Cn0: 41, ElevDeg: -5, Used: true, Extended: true, AziDeg: 180, PrResDM: -32768, Quality: 6, Health: 0},
		{GnssID: 0, SvID: 3, Cn0: 0, ElevDeg: 15, Extended: true, AziDeg: 300, PrResDM: 0, Quality: 1, Health: 1},
	}
	if !reflect.DeepEqual(sats, wantSats) {
		t.Fatalf("golden body decodes to\n%+v\nwant\n%+v", sats, wantSats)
	}
	if again := EncodeReceptionData(sats); !bytes.Equal(again, want) {
		t.Fatalf("decode and re-encode changed the body:\n%x\n%x", again, want)
	}
}

// TestReceptionDataVersionOneStaysVersionOne: an observer still sending version 1 is
// accepted, and its satellites, which do not carry the extended fields, are stored as
// version 1 rather than with fields they never had.
func TestReceptionDataVersionOneStaysVersionOne(t *testing.T) {
	v1 := []byte{1, 0, 2, 0, 5, 47, 61, 1, 2, 14, 33, 12, 0}
	sats, err := decodeReceptionData(v1)
	if err != nil || len(sats) != 2 || sats[0].Extended || sats[1].Cn0 != 33 {
		t.Fatalf("version 1 = %+v, %v", sats, err)
	}
	if again := EncodeReceptionData(sats); !bytes.Equal(again, v1) {
		t.Fatalf("version 1 re-encoded as %x", again)
	}
	mixed := append(sats, SatCN0{GnssID: 0, SvID: 9, Cn0: 40, Extended: true, AziDeg: 10})
	if b := EncodeReceptionData(mixed); b[0] != 1 {
		t.Fatalf("a list with any version 1 satellite encoded as version %d", b[0])
	}
}

func TestReceptionDataV2Rejects(t *testing.T) {
	good := EncodeReceptionData([]SatCN0{{GnssID: 0, SvID: 5, Cn0: 40, Extended: true, Health: 1}})
	for name, mutate := range map[string]func([]byte) []byte{
		"short":           func(b []byte) []byte { return b[:len(b)-1] },
		"trailing":        func(b []byte) []byte { return append(b, 0) },
		"count over cap":  func(b []byte) []byte { binary.BigEndian.PutUint16(b[1:], maxTelemSatsV2+1); return b },
		"reserved bit":    func(b []byte) []byte { b[len(b)-1] |= 0x40; return b },
		"reserved health": func(b []byte) []byte { b[len(b)-1] |= 0x30; return b },
		"unknown version": func(b []byte) []byte { b[0] = 3; return b },
	} {
		b := mutate(append([]byte(nil), good...))
		if _, err := decodeReceptionData(b); err == nil {
			t.Errorf("%s accepted: %x", name, b)
		}
	}
}

// TestReceptionDataV2Cap keeps tracked satellites when a receiver lists more than the
// record holds.
func TestReceptionDataV2Cap(t *testing.T) {
	var sats []SatCN0
	for i := 0; i < 150; i++ {
		cn0 := 0
		if i%2 == 1 {
			cn0 = 30 + i%10
		}
		sats = append(sats, SatCN0{GnssID: 0, SvID: i, Cn0: cn0, Extended: true})
	}
	b := EncodeReceptionData(sats)
	if len(b) != 3+maxTelemSatsV2*receptionSatLenV2 || len(b) > 1020 {
		t.Fatalf("body length %d", len(b))
	}
	got, err := decodeReceptionData(b)
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range got[:75] {
		if s.Cn0 == 0 || s.SvID != 2*i+1 {
			t.Fatalf("entry %d = %+v: tracked satellites must come first, in order", i, s)
		}
	}
	for _, s := range got[75:] {
		if s.Cn0 != 0 {
			t.Fatalf("tracked satellite after the untracked ones: %+v", s)
		}
	}
}
