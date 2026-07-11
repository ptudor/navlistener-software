package ingest

import (
	"bytes"
	"math"
	"os"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/glonass"
)

// TestGLONASSAlmanacCrossOracle validates the GLONASS almanac-string decoder (ICD Ed. 5.1
// §4.5) against the broadcast ephemeris of the same satellites in a real F9P capture — the
// same differential discipline that validated the other decoders. For each slot the capture
// carries both an almanac (strings 6-15) and a Cartesian ephemeris (strings 1-3), it
// propagates the almanac to the ephemeris epoch tb and requires agreement within the ICD's
// almanac accuracy (Table 4.8: sub-km to a few km depending on age). It also checks every
// decoded almanac lands on the GLONASS orbital shell with a valid FDMA channel — the sanity
// gates a bit-offset or scale error would fail.
func TestGLONASSAlmanacCrossOracle(t *testing.T) {
	data, err := os.ReadFile("testdata/f9p_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}

	type ephParts struct {
		s1, s2, s3 *frame.GLONASSString
		freqID     int
	}
	ephs := map[int]*ephParts{} // by slot (SFRBX svId)
	alms := map[int]glonass.Almanac{}
	na := -1
	var pendFirst []uint32 // the first string of an almanac pair, awaiting its second
	var pendNum int

	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime, func(f *RawFrame) {
		if f.Obs != nil || f.GnssID != gnss.GLONASS || len(f.Words) < 4 {
			return
		}
		s, err := frame.DecodeGLONASSString(f.Words)
		if err != nil {
			return
		}
		switch {
		case s.Number >= 1 && s.Number <= 3:
			e := ephs[f.SvID]
			if e == nil {
				e = &ephParts{}
				ephs[f.SvID] = e
			}
			e.freqID = f.FreqID
			switch s.Number {
			case 1:
				e.s1 = s
			case 2:
				e.s2 = s
			case 3:
				e.s3 = s
			}
		case s.Number == 5:
			if n, err := frame.DecodeGLONASSFrameNA(f.Words); err == nil {
				na = n
			}
		case s.Number%2 == 0 && s.Number >= 6 && s.Number <= 14:
			pendFirst = append([]uint32(nil), f.Words...) // first string of a pair (6/8/10/12/14)
			pendNum = s.Number
		case s.Number%2 == 1 && s.Number >= 7 && s.Number <= 15:
			if pendFirst != nil && s.Number == pendNum+1 { // its matching second string
				if a, err := frame.DecodeGLONASSAlmanac(pendFirst, f.Words, na); err == nil {
					alms[a.Alm.Slot] = a.Alm
				}
			}
			pendFirst = nil
		}
	}, func(string) {})

	if na < 1 || na > 1461 {
		t.Fatalf("NA (day number) = %d, want 1..1461", na)
	}
	if len(alms) < 15 {
		t.Fatalf("decoded only %d GLONASS almanacs; expected the full constellation", len(alms))
	}

	crossChecked := 0
	for slot, a := range alms {
		if a.Slot != slot || slot < 1 || slot > 24 {
			t.Errorf("slot %d: bad slot number %d", slot, a.Slot)
		}
		if a.FreqCh < -7 || a.FreqCh > 6 {
			t.Errorf("slot %d: FDMA channel %d out of range [-7,6]", slot, a.FreqCh)
		}
		if a.Ecc < 0 || a.Ecc > 0.01 {
			t.Errorf("slot %d: eccentricity %.4f implausible for GLONASS", slot, a.Ecc)
		}
		// Every almanac must land on the GLONASS shell (~25,510 km).
		p, err := glonass.PropagateAlmanacECEF(a, a.NA, a.Tlambda+1800)
		if err != nil {
			t.Errorf("slot %d: almanac propagation failed: %v", slot, err)
			continue
		}
		r := math.Sqrt(p.X*p.X+p.Y*p.Y+p.Z*p.Z) / 1000
		if r < 25000 || r > 26000 {
			t.Errorf("slot %d: almanac radius %.0f km off the GLONASS shell", slot, r)
		}

		// Cross-oracle: where the same SV's broadcast ephemeris is in the capture, the
		// almanac propagated to tb must agree to almanac accuracy.
		e := ephs[slot]
		if e == nil || e.s1 == nil || e.s2 == nil || e.s3 == nil {
			continue
		}
		eph, err := frame.AssembleGLONASS(slot, e.freqID, e.s1, e.s2, e.s3, nil)
		if err != nil {
			continue
		}
		pa, err := glonass.PropagateAlmanacECEF(a, a.NA, eph.Tb)
		if err != nil {
			t.Errorf("slot %d: propagate-to-tb failed: %v", slot, err)
			continue
		}
		dx, dy, dz := pa.X/1000-eph.X, pa.Y/1000-eph.Y, pa.Z/1000-eph.Z
		dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
		if dist > 10 { // Table 4.8: a few km at realistic almanac age; 10 km is a wide guard
			t.Errorf("slot %d: almanac vs ephemeris @tb = %.1f km, want < 10 km", slot, dist)
		}
		crossChecked++
	}
	if crossChecked < 3 {
		t.Fatalf("only %d slots cross-checked against ephemeris; expected several in view", crossChecked)
	}
	t.Logf("NA=%d: %d GLONASS almanacs on-shell, %d cross-checked vs ephemeris < 10 km", na, len(alms), crossChecked)
}
