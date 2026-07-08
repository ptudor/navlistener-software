package ingest

import (
	"bytes"
	"os"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/kepler"
)

// TestRealF9TCapture is the real-frame regression: a 90 s UBX capture from a
// live u-blox ZED-F9T (testdata/f9t_capture.ubx) is run through the actual UBX
// scanner and GPS LNAV decoder, and every assembled ephemeris must propagate to a
// GPS-shell radius. This is the test that caught the u-blox parity/inversion
// convention (the receiver pre-validates and un-inverts its words); it guards
// against a regression in the SFRBX parse or the LNAV field offsets.
func TestRealF9TCapture(t *testing.T) {
	data, err := os.ReadFile("testdata/f9t_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}

	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) },
		func(string) {})
	if len(frames) < 100 {
		t.Fatalf("only %d SFRBX parsed from the capture", len(frames))
	}

	// Collect GPS L1 C/A subframes 1/2/3 per SV.
	type triple struct{ sf1, sf2, sf3 *frame.GPSSubframe }
	bySV := map[int]*triple{}
	crcFail := 0
	for _, f := range frames {
		if f.GnssID != gnss.GPS || f.SigID != 0 {
			continue
		}
		sf, err := frame.DecodeGPSLNAV(f.Words)
		if err != nil {
			crcFail++
			continue
		}
		tr := bySV[f.SvID]
		if tr == nil {
			tr = &triple{}
			bySV[f.SvID] = tr
		}
		switch sf.SubframeID {
		case 1:
			tr.sf1 = sf
		case 2:
			tr.sf2 = sf
		case 3:
			tr.sf3 = sf
		}
	}
	if crcFail != 0 {
		t.Errorf("%d GPS subframes failed to decode (u-blox words should all decode)", crcFail)
	}

	assembled := 0
	for sv, tr := range bySV {
		if tr.sf1 == nil || tr.sf2 == nil || tr.sf3 == nil {
			continue
		}
		eph, clk, err := frame.AssembleGPS(gnss.GPS, sv, tr.sf1, tr.sf2, tr.sf3)
		if err != nil {
			continue // subframes from different IOD cycles; skip
		}
		// Propagate at the ephemeris reference epoch (deterministic, no wall-clock).
		pos, err := kepler.Propagate(eph, eph.Toe)
		if err != nil {
			t.Errorf("G%02d propagate: %v", sv, err)
			continue
		}
		if r := pos.Norm(); r < 25.8e6 || r > 27.2e6 {
			t.Errorf("G%02d real ephemeris radius = %.0f m, want the GPS shell ~26560 km", sv, r)
		}
		if clk.Toc == 0 && clk.Af0 == 0 {
			t.Errorf("G%02d clock model not populated", sv)
		}
		assembled++
	}
	if assembled < 6 {
		t.Errorf("assembled only %d GPS ephemerides from the real capture, want >= 6", assembled)
	}
	t.Logf("real F9T capture: %d SFRBX, %d GPS SVs with full ephemerides", len(frames), assembled)
}
