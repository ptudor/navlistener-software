package ingest

import (
	"bytes"
	"math"
	"os"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/glonass"
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

// TestRealGalileoFNAVAgreesWithINAV validates the E5a F/NAV decoder against the
// same capture by the strongest possible check: for every SV present on both
// signals, the F/NAV-decoded position must agree with the I/NAV-decoded position
// (same broadcast ephemeris, two signals) to within a few metres.
func TestRealGalileoFNAVAgreesWithINAV(t *testing.T) {
	data, err := os.ReadFile("testdata/f9t_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) }, func(string) {})

	type inav struct{ w1, w2, w3, w4 *frame.GalileoINAV }
	type fnav struct{ p1, p2, p3, p4 *frame.GalileoFNAV }
	iByte := map[int]*inav{}
	fByte := map[int]*fnav{}
	for _, f := range frames {
		if f.GnssID != gnss.Galileo {
			continue
		}
		switch f.SigID {
		case 1: // E1-B I/NAV
			w, err := frame.DecodeGalileoINAV(f.Words)
			if err != nil {
				continue
			}
			s := iByte[f.SvID]
			if s == nil {
				s = &inav{}
				iByte[f.SvID] = s
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
		case 3: // E5a F/NAV
			w, err := frame.DecodeGalileoFNAV(f.Words)
			if err != nil {
				continue
			}
			s := fByte[f.SvID]
			if s == nil {
				s = &fnav{}
				fByte[f.SvID] = s
			}
			switch w.PageType {
			case 1:
				s.p1 = w
			case 2:
				s.p2 = w
			case 3:
				s.p3 = w
			case 4:
				s.p4 = w
			}
		}
	}

	agreed := 0
	for sv, fs := range fByte {
		is := iByte[sv]
		if is == nil || is.w1 == nil || is.w2 == nil || is.w3 == nil || is.w4 == nil {
			continue
		}
		if fs.p1 == nil || fs.p2 == nil || fs.p3 == nil || fs.p4 == nil {
			continue
		}
		iEph, _, err1 := frame.AssembleGalileo(sv, is.w1, is.w2, is.w3, is.w4)
		fEph, _, err2 := frame.AssembleGalileoFNAV(sv, fs.p1, fs.p2, fs.p3, fs.p4)
		if err1 != nil || err2 != nil {
			continue
		}
		if iEph.Toe != fEph.Toe {
			continue // different data sets in this window; skip
		}
		ip, e1 := kepler.Propagate(iEph, iEph.Toe)
		fp, e2 := kepler.Propagate(fEph, fEph.Toe)
		if e1 != nil || e2 != nil {
			continue
		}
		if dist := ip.Sub(fp).Norm(); dist > 5 {
			t.Errorf("E%02d F/NAV vs I/NAV position disagree by %.1f m", sv, dist)
		}
		agreed++
	}
	if agreed < 3 {
		t.Errorf("only %d SVs cross-checked F/NAV against I/NAV, want >= 3", agreed)
	}
	t.Logf("real F9T capture: %d Galileo SVs agree F/NAV↔I/NAV to <5 m", agreed)
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

// TestRealGPSCNAVAgreesWithLNAV validates the GPS L2C CNAV decoder against a real
// ZED-F9P capture (testdata/f9p_capture.ubx has both L1 C/A LNAV and L2C CNAV): for
// every GPS SV present on both, the CNAV-decoded position must agree with the
// LNAV-decoded position (same broadcast ephemeris, ΔA vs √A parameterizations) to
// within a few metres.
func TestRealGPSCNAVAgreesWithLNAV(t *testing.T) {
	data, err := os.ReadFile("testdata/f9p_capture.ubx")
	if err != nil {
		t.Skipf("no F9P capture fixture: %v", err)
	}
	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) }, func(string) {})

	type ln struct{ sf1, sf2, sf3 *frame.GPSSubframe }
	type cn struct{ m10, m11 *frame.GPSCNAV }
	lnav := map[int]*ln{}
	cnav := map[int]*cn{}
	for _, f := range frames {
		if f.GnssID != gnss.GPS {
			continue
		}
		switch f.SigID {
		case 0: // L1 C/A LNAV
			sf, err := frame.DecodeGPSLNAV(f.Words)
			if err != nil {
				continue
			}
			s := lnav[f.SvID]
			if s == nil {
				s = &ln{}
				lnav[f.SvID] = s
			}
			switch sf.SubframeID {
			case 1:
				s.sf1 = sf
			case 2:
				s.sf2 = sf
			case 3:
				s.sf3 = sf
			}
		case 4: // L2C CNAV
			m, err := frame.DecodeGPSCNAV(gnss.GPS, f.Words)
			if err != nil {
				continue
			}
			s := cnav[f.SvID]
			if s == nil {
				s = &cn{}
				cnav[f.SvID] = s
			}
			switch m.MsgType {
			case 10:
				s.m10 = m
			case 11:
				s.m11 = m
			}
		}
	}

	agreed := 0
	for sv, c := range cnav {
		l := lnav[sv]
		if l == nil || l.sf1 == nil || l.sf2 == nil || l.sf3 == nil || c.m10 == nil || c.m11 == nil {
			continue
		}
		lEph, _, err1 := frame.AssembleGPS(gnss.GPS, sv, l.sf1, l.sf2, l.sf3)
		cEph, _, err2 := frame.AssembleGPSCNAV(gnss.GPS, sv, c.m10, c.m11, nil)
		if err1 != nil || err2 != nil {
			continue
		}
		// LNAV and CNAV update independently, so they rarely share a toe; propagate
		// both to a common instant and require same-orbit agreement. The residual is
		// the genuine difference between two ephemeris uploads propagated a few
		// hundred–thousand seconds apart (tens of metres), NOT a decode error — a
		// wrong CNAV offset would put the SV kilometres off.
		tow := lEph.Toe
		lp, e1 := kepler.Propagate(lEph, tow)
		cp, e2 := kepler.Propagate(cEph, tow)
		if e1 != nil || e2 != nil {
			continue
		}
		tol := 5.0 + math.Abs(lEph.Toe-cEph.Toe)*0.02 // ~5 m + growth with the toe gap
		if dist := lp.Sub(cp).Norm(); dist > tol {
			t.Errorf("G%02d CNAV vs LNAV disagree by %.1f m (tol %.0f, toe L=%.0f C=%.0f) — likely a decode error",
				sv, dist, tol, lEph.Toe, cEph.Toe)
		}
		agreed++
	}
	if agreed < 4 {
		t.Errorf("only %d GPS SVs cross-checked CNAV against LNAV, want >= 4", agreed)
	}
	t.Logf("real F9P capture: %d GPS SVs agree CNAV↔LNAV (same orbit, within the toe-gap tolerance)", agreed)
}

// TestRealSBAS validates the SBAS L1 decoder against the real F9P capture: every
// SBAS message decodes with a valid preamble and message type, and the WAAS PRNs
// (131/133/135) map to the WAAS provider.
func TestRealSBAS(t *testing.T) {
	data, err := os.ReadFile("testdata/f9p_capture.ubx")
	if err != nil {
		t.Skipf("no F9P capture fixture: %v", err)
	}
	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) }, func(string) {})

	types := map[int]bool{}
	n, waas := 0, 0
	for _, f := range frames {
		if f.GnssID != gnss.SBAS {
			continue
		}
		m, err := frame.DecodeSBASL1(f.SvID, f.Words)
		if err != nil {
			t.Fatalf("SBAS decode: %v", err)
		}
		if !m.PreambleOK {
			t.Errorf("PRN %d: bad SBAS preamble", m.PRN)
		}
		if m.Type < 0 || m.Type > 63 {
			t.Errorf("PRN %d: message type %d out of range", m.PRN, m.Type)
		}
		types[m.Type] = true
		if m.PRN == 131 || m.PRN == 133 || m.PRN == 135 {
			if m.Provider != "WAAS" {
				t.Errorf("PRN %d provider = %q, want WAAS", m.PRN, m.Provider)
			}
			waas++
		}
		n++
	}
	if n < 100 {
		t.Fatalf("only %d SBAS messages decoded", n)
	}
	if waas == 0 {
		t.Error("no WAAS messages recognised")
	}
	t.Logf("real F9P capture: %d SBAS messages, %d WAAS, message types seen: %v", n, waas, types)
}

// TestRealGLONASS validates the GLONASS string decoder + RK4 propagator against
// the real capture: strings 1/2/3 assemble the PZ-90 Cartesian state per SV, which
// must sit on the ~25510 km shell at tb (position decode) and stay there after a
// 300 s RK4 propagation (which exercises the velocity decode too).
func TestRealGLONASS(t *testing.T) {
	data, err := os.ReadFile("testdata/f9t_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	var frames []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { frames = append(frames, f) }, func(string) {})

	type strset struct {
		s1, s2, s3 *frame.GLONASSString
		freqID     int
	}
	bySV := map[int]*strset{}
	for _, f := range frames {
		if f.GnssID != gnss.GLONASS || f.SigID != 0 {
			continue
		}
		s, err := frame.DecodeGLONASSString(f.Words)
		if err != nil {
			continue
		}
		set := bySV[f.SvID]
		if set == nil {
			set = &strset{freqID: f.FreqID}
			bySV[f.SvID] = set
		}
		switch s.Number {
		case 1:
			set.s1 = s
		case 2:
			set.s2 = s
		case 3:
			set.s3 = s
		}
	}

	assembled := 0
	for sv, set := range bySV {
		if set.s1 == nil || set.s2 == nil || set.s3 == nil {
			continue
		}
		eph, err := frame.AssembleGLONASS(sv, set.freqID, set.s1, set.s2, set.s3)
		if err != nil {
			continue
		}
		for _, tk := range []float64{0, 300} {
			pos, err := glonass.Propagate(eph, tk)
			if err != nil {
				t.Errorf("R%02d propagate tk=%.0f: %v", sv, tk, err)
				continue
			}
			if r := pos.Norm(); r < 25.0e6 || r > 26.0e6 {
				t.Errorf("R%02d radius at tk=%.0f = %.0f m, want the GLONASS shell ~25510 km", sv, tk, r)
			}
		}
		assembled++
	}
	if assembled < 4 {
		t.Errorf("assembled only %d GLONASS ephemerides, want >= 4", assembled)
	}
	t.Logf("real F9T capture: %d GLONASS SVs with full ephemerides", assembled)
}
