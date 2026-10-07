package ingest

import (
	"testing"

	"github.com/ptudor/gnss"
)

// sfrbxVersionCases is the SFRBX message-version rule the three parsers share (this
// scanner, feeder/navfeeder.c emit_sfrbx and the ESP32 ubx framer): a version 1 (M8)
// payload carries a reserved byte where version 2 (F9/M9) carries sigId, so byte 2 must
// be ignored for version 1 — an M8 is L1-only and its sigId is 0 by definition — and
// honoured for version 2. Without the check a non-zero reserved byte labels the frame
// with a bogus signal and a wrong NavType, persisted as such.
var sfrbxVersionCases = []struct {
	name     string
	version  byte
	byte2    byte
	wantSig  int
	wantType int
}{
	{"version 1 with a non-zero reserved byte", 1, 0x5a, 0, 0x10},
	{"version 1 with a zero reserved byte", 1, 0, 0, 0x10},
	{"version 2 sigId honoured", 2, 3, 3, 0x11},
}

func sfrbxVersionMessage(version, byte2 byte) []byte {
	words := []uint32{0x22c00000, 1, 2, 3, 4, 5, 6, 7, 8, 9}
	payload := buildSFRBXPayload(gnss.GPS, 7, byte2, 0, words)
	payload[6] = version
	return buildUBX(ubxClassRXM, ubxIDSFRBX, payload)
}

func TestScanUBXSFRBXVersionOneIgnoresReservedByte(t *testing.T) {
	for _, tc := range sfrbxVersionCases {
		t.Run(tc.name, func(t *testing.T) {
			frames, errs := collect(t, scanUBX, sfrbxVersionMessage(tc.version, tc.byte2))
			if len(errs) != 0 || len(frames) != 1 {
				t.Fatalf("frames=%d errs=%v, want one clean frame", len(frames), errs)
			}
			if f := frames[0]; f.SigID != tc.wantSig || f.NavType() != tc.wantType {
				t.Fatalf("sigId=%d NavType=%#x, want sigId=%d NavType=%#x",
					f.SigID, f.NavType(), tc.wantSig, tc.wantType)
			}
		})
	}
}

// TestNavfeederSFRBXVersionCrossOracle drives the same three messages through the real C
// feeder and asserts the collector receives exactly what the Go scanner decodes: the
// version byte rule must hold on both sides of the wire or the two ingest modes diverge.
func TestNavfeederSFRBXVersionCrossOracle(t *testing.T) {
	bin := feederBinary(t)
	var capBytes []byte
	for _, tc := range sfrbxVersionCases {
		capBytes = append(capBytes, sfrbxVersionMessage(tc.version, tc.byte2)...)
	}
	expected, errs := collect(t, scanUBX, capBytes)
	if len(errs) != 0 || len(expected) != len(sfrbxVersionCases) {
		t.Fatalf("scanner: frames=%d errs=%v", len(expected), errs)
	}
	got := feederFrames(t, bin, "sfrbx-version-e2e", capBytes, len(sfrbxVersionCases))
	for i, tc := range sfrbxVersionCases {
		e, g := expected[i], got[i]
		if g.SigID != tc.wantSig || g.NavType() != tc.wantType {
			t.Errorf("%s: feeder delivered sigId=%d NavType=%#x, want sigId=%d NavType=%#x",
				tc.name, g.SigID, g.NavType(), tc.wantSig, tc.wantType)
		}
		if e.SigID != g.SigID || e.NavType() != g.NavType() || e.SvID != g.SvID {
			t.Errorf("%s: scanner (sv %d sig %d type %#x) and feeder (sv %d sig %d type %#x) disagree",
				tc.name, e.SvID, e.SigID, e.NavType(), g.SvID, g.SigID, g.NavType())
		}
	}
}
