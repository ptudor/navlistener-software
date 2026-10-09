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
