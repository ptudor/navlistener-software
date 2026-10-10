package ingest

import (
	"bytes"
	"os"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/kepler"
)

// TestRealGPSAlmanacMatchesBroadcastEphemeris pins the LNAV almanac page
// layout to real receiver bytes. Every almanac page in the three captures
// converts to an orbit in the GPS shell, and wherever the same capture also
// carries a satellite's full broadcast ephemeris, the almanac places it within
// 5 km of that ephemeris at toe. In the GLONASS superframe capture that holds
// for seven satellites with the almanac evaluated 1.7 days before its toa
// (observed misses 0.4 to 2.9 km); a mis-read field would miss by thousands.
func TestRealGPSAlmanacMatchesBroadcastEphemeris(t *testing.T) {
	compared := 0
	for _, name := range []string{"f9t_capture.ubx", "f9p_capture.ubx", "glo_superframe_capture.ubx"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var frames []*RawFrame
		_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
			func(f *RawFrame) { frames = append(frames, f) }, func(string) {})
		type triple struct{ sf1, sf2, sf3 *frame.GPSSubframe }
		bySV := map[int]*triple{}
		almanacs := map[int]*frame.LNAVAlmanac{}
		for _, f := range frames {
			if f.GnssID != gnss.GPS || f.SigID != 0 {
				continue
			}
			sf, err := frame.DecodeGPSLNAV(f.Words)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if sf.Almanac != nil {
				almanacs[sf.Almanac.SVID] = sf.Almanac
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
		if len(almanacs) < 6 {
			t.Fatalf("%s: only %d almanac pages decoded", name, len(almanacs))
		}
		for sv, a := range almanacs {
			alm, err := a.Ephemeris(gnss.GPS)
			if err != nil {
				t.Fatalf("%s: G%02d almanac rejected: %v", name, sv, err)
			}
			p, err := kepler.Propagate(alm, a.Toa)
			if err != nil || p.Norm() < 26.0e6 || p.Norm() > 27.2e6 {
				t.Fatalf("%s: G%02d almanac radius %.0f m, err %v", name, sv, p.Norm(), err)
			}
			tr := bySV[sv]
			if tr == nil || tr.sf1 == nil || tr.sf2 == nil || tr.sf3 == nil {
				continue
			}
			eph, _, err := frame.AssembleGPS(gnss.GPS, sv, tr.sf1, tr.sf2, tr.sf3)
			if err != nil {
				continue
			}
			got, _ := kepler.Propagate(alm, eph.Toe)
			want, _ := kepler.Propagate(eph, eph.Toe)
			if miss := got.Sub(want).Norm(); miss > 5000 {
				t.Fatalf("%s: G%02d almanac %.0f m from its broadcast ephemeris", name, sv, miss)
			}
			compared++
		}
	}
	if compared < 7 {
		t.Fatalf("only %d almanac/ephemeris pairs compared", compared)
	}
}

// TestRealGalileoAlmanacMatchesBroadcastEphemeris pins the I/NAV word type
// 7–10 layout to real E1-B pages. Consecutive almanac words from one
// transmitter join into a satellite's almanac; every completed almanac lies on
// the Galileo shell, and where a capture also holds the satellite's full
// ephemeris the almanac is within 5 km of it at toe (seven satellites, 1.0 to
// 3.1 km observed). Unused entries (SVID 0) and a pair split by an almanac
// batch change are refused rather than joined.
func TestRealGalileoAlmanacMatchesBroadcastEphemeris(t *testing.T) {
	compared, unused := 0, 0
	for _, name := range []string{"f9t_capture.ubx", "f9p_capture.ubx", "glo_superframe_capture.ubx"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var frames []*RawFrame
		_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
			func(f *RawFrame) { frames = append(frames, f) }, func(string) {})
		last := map[int]*frame.GalileoINAV{}
		ephWords := map[int]*[5]*frame.GalileoINAV{}
		almanacs := map[int]frame.GalileoAlmanac{}
		for _, f := range frames {
			if f.GnssID != gnss.Galileo || f.SigID != 1 {
				continue
			}
			w, err := frame.DecodeGalileoINAV(f.Words)
			if err != nil {
				continue
			}
			if w.Type >= 1 && w.Type <= 4 {
				ws := ephWords[f.SvID]
				if ws == nil {
					ws = &[5]*frame.GalileoINAV{}
					ephWords[f.SvID] = ws
				}
				ws[w.Type] = w
			}
			if w.Almanac == nil {
				continue
			}
			if prev := last[f.SvID]; prev != nil && prev.Type+1 == w.Type {
				a, err := frame.CompleteGalileoAlmanac(prev, w)
				switch {
				case err == nil:
					almanacs[a.SVID] = a
				case prev.Almanac.HeadSVID == 0:
					unused++
				case prev.Almanac.IODa == w.Almanac.IODa:
					t.Fatalf("%s: E%02d almanac words %d+%d refused: %v", name, f.SvID, prev.Type, w.Type, err)
				}
			}
			last[f.SvID] = w
		}
		if len(almanacs) < 4 {
			t.Fatalf("%s: only %d almanacs completed", name, len(almanacs))
		}
		for sv, a := range almanacs {
			alm := a.Ephemeris()
			p, err := kepler.Propagate(alm, a.T0a)
			if err != nil || p.Norm() < 29.4e6 || p.Norm() > 29.8e6 {
				t.Fatalf("%s: E%02d almanac radius %.0f m, err %v", name, sv, p.Norm(), err)
			}
			ws := ephWords[sv]
			if ws == nil || ws[1] == nil || ws[2] == nil || ws[3] == nil || ws[4] == nil {
				continue
			}
			eph, _, err := frame.AssembleGalileo(sv, ws[1], ws[2], ws[3], ws[4], nil)
			if err != nil {
				continue
			}
			got, _ := kepler.Propagate(alm, eph.Toe)
			want, _ := kepler.Propagate(eph, eph.Toe)
			if miss := got.Sub(want).Norm(); miss > 5000 {
				t.Fatalf("%s: E%02d almanac %.0f m from its broadcast ephemeris", name, sv, miss)
			}
			compared++
		}
	}
	if compared < 7 || unused == 0 {
		t.Fatalf("compared %d almanac/ephemeris pairs, %d unused entries", compared, unused)
	}
}

// TestRealBeiDouMidiAlmanacMatchesD1Ephemeris pins the B-CNAV2 message type 40
// midi almanac layout to real B2a frames. Every midi almanac converts to an
// orbit on the IGSO or MEO shell its SatType names, and where the capture
// holds the same satellite's D1 ephemeris the almanac, three days from its
// reference time, is within 25 km of it at toe (7.5 to 16.6 km observed; the
// 2⁻⁴ √A step alone allows about 17 km of along-track drift in three days).
func TestRealBeiDouMidiAlmanacMatchesD1Ephemeris(t *testing.T) {
	compared, placed := 0, 0
	for _, name := range []string{"f9t_capture.ubx", "glo_superframe_capture.ubx"} {
		data, err := os.ReadFile("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		var frames []*RawFrame
		_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
			func(f *RawFrame) { frames = append(frames, f) }, func(string) {})
		d1 := map[int]*[4]*frame.BeiDouSubframe{}
		midi := map[int]*frame.BeiDouMidiAlmanac{}
		for _, f := range frames {
			if f.GnssID != gnss.BeiDou {
				continue
			}
			switch f.SigID {
			case 0:
				if sf, err := frame.DecodeBeiDouD1(f.Words); err == nil && sf.FraID <= 3 {
					x := d1[f.SvID]
					if x == nil {
						x = &[4]*frame.BeiDouSubframe{}
						d1[f.SvID] = x
					}
					x[sf.FraID] = sf
				}
			case 8:
				m, err := frame.DecodeBeiDouBCNAV2(f.Words)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if m.Almanac != nil {
					midi[m.Almanac.PRN] = m.Almanac
				}
			}
		}
		if len(midi) < 20 {
			t.Fatalf("%s: only %d midi almanacs", name, len(midi))
		}
		for prn, a := range midi {
			alm, err := a.Ephemeris()
			if err != nil {
				t.Fatalf("%s: C%02d: %v", name, prn, err)
			}
			p, err := kepler.Propagate(alm, a.Toa)
			shell := map[int][2]float64{2: {41.5e6, 42.8e6}, 3: {27.6e6, 28.2e6}}[a.SatType]
			if err != nil || p.Norm() < shell[0] || p.Norm() > shell[1] {
				t.Fatalf("%s: C%02d (SatType %d) radius %.0f m, err %v", name, prn, a.SatType, p.Norm(), err)
			}
			placed++
			x := d1[prn]
			if x == nil || x[1] == nil || x[2] == nil || x[3] == nil {
				continue
			}
			eph, _, err := frame.AssembleBeiDou(prn, x[1], x[2], x[3])
			if err != nil {
				continue
			}
			got, _ := kepler.Propagate(alm, eph.Toe)
			want, _ := kepler.Propagate(eph, eph.Toe)
			if miss := got.Sub(want).Norm(); miss > 25000 {
				t.Fatalf("%s: C%02d midi almanac %.0f m from its D1 ephemeris", name, prn, miss)
			}
			compared++
		}
	}
	if compared < 8 || placed < 50 {
		t.Fatalf("compared %d almanac/ephemeris pairs, placed %d almanacs", compared, placed)
	}
}
