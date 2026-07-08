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

// TestRealGalileoINAV validates the Galileo I/NAV decoder against the same real
// F9T capture: word types 1–4 (E1-B, sigId 1) are collected per SV, assembled, and
// each ephemeris must propagate to the Galileo shell (~29600 km).
func TestRealGalileoINAV(t *testing.T) {
	data, err := os.ReadFile("testdata/f9t_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) }, func(string) {})

	type words struct{ w1, w2, w3, w4 *frame.GalileoINAV }
	bySV := map[int]*words{}
	for _, f := range frames {
		if f.GnssID != gnss.Galileo || f.SigID != 1 { // E1-B I/NAV
			continue
		}
		w, err := frame.DecodeGalileoINAV(f.Words)
		if err != nil {
			continue
		}
		s := bySV[f.SvID]
		if s == nil {
			s = &words{}
			bySV[f.SvID] = s
		}
		switch w.Type {
		case 1:
			s.w1 = w
		case 2:
			s.w2 = w
		case 3:
			s.w3 = w
		case 4:
			s.w4 = w
		}
	}

	assembled := 0
	for sv, s := range bySV {
		if s.w1 == nil || s.w2 == nil || s.w3 == nil || s.w4 == nil {
			continue
		}
		eph, _, err := frame.AssembleGalileo(sv, s.w1, s.w2, s.w3, s.w4)
		if err != nil {
			continue // words from different IODnav; skip
		}
		pos, err := kepler.Propagate(eph, eph.Toe)
		if err != nil {
			t.Errorf("E%02d propagate: %v", sv, err)
			continue
		}
		if r := pos.Norm(); r < 29.2e6 || r > 30.0e6 {
			t.Errorf("E%02d real ephemeris radius = %.0f m, want the Galileo shell ~29600 km", sv, r)
		}
		assembled++
	}
	if assembled < 4 {
		t.Errorf("assembled only %d Galileo ephemerides, want >= 4", assembled)
	}
	t.Logf("real F9T capture: %d Galileo SVs with full I/NAV ephemerides", assembled)
}

// TestRealBeiDouD1 validates the BeiDou D1 decoder against the real capture:
// subframes 1/2/3 (B1I, sigId 0) assemble per SV; each must propagate to the BeiDou
// MEO shell (~27906 km) and carry the ~55° MEO inclination — the latter guards the
// SF3 orientation fields (i0/Ω/ω), which a radius check alone would not.
func TestRealBeiDouD1(t *testing.T) {
	data, err := os.ReadFile("testdata/f9t_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) }, func(string) {})

	type set struct{ s1, s2, s3 *frame.BeiDouSubframe }
	bySV := map[int]*set{}
	for _, f := range frames {
		if f.GnssID != gnss.BeiDou || f.SigID != 0 { // B1I D1
			continue
		}
		sf, err := frame.DecodeBeiDouD1(f.Words)
		if err != nil {
			continue
		}
		s := bySV[f.SvID]
		if s == nil {
			s = &set{}
			bySV[f.SvID] = s
		}
		switch sf.FraID {
		case 1:
			s.s1 = sf
		case 2:
			s.s2 = sf
		case 3:
			s.s3 = sf
		}
	}

	assembled := 0
	for sv, s := range bySV {
		if s.s1 == nil || s.s2 == nil || s.s3 == nil {
			continue
		}
		eph, _, err := frame.AssembleBeiDou(sv, s.s1, s.s2, s.s3)
		if err != nil {
			continue
		}
		pos, err := kepler.Propagate(eph, eph.Toe)
		if err != nil {
			t.Errorf("C%02d propagate: %v", sv, err)
			continue
		}
		if r := pos.Norm(); r < 27.4e6 || r > 28.4e6 {
			t.Errorf("C%02d real ephemeris radius = %.0f m, want the BeiDou MEO shell ~27906 km", sv, r)
		}
		// MEO inclination ~55° (0.96 rad) — guards the SF3 orientation decode.
		if incDeg := eph.I0 * 180 / 3.14159265; incDeg < 50 || incDeg > 60 {
			t.Errorf("C%02d inclination = %.1f°, want the BeiDou MEO ~55°", sv, incDeg)
		}
		assembled++
	}
	if assembled < 4 {
		t.Errorf("assembled only %d BeiDou ephemerides, want >= 4", assembled)
	}
	t.Logf("real F9T capture: %d BeiDou D1 SVs with full ephemerides", assembled)
}
