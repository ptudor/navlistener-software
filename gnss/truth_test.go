// External truth-vector tests; see docs/MATH.md section 12.
// vendored verbatim from a BKG merged BRDC RINEX 3.05 file, is propagated with this
// library and compared against the ESA/ESOC MGEX precise orbit (SP3) at the same
// instant. Unlike the analytic/property tests, these vectors are produced entirely
// outside this codebase, so a swapped harmonic coefficient (Cus/Cuc, Crc/Crs), a
// Φ-vs-2Φ argument error, a δi sign flip, or a GLONASS Coriolis-sign error — each
// invisible to radius-window and reversibility tests — fails here by hundreds of
// metres to kilometres.
//
// Reference-point caveat (docs/MATH.md §12 item 2): broadcast ephemerides describe
// the antenna phase centre, SP3 the centre of mass — up to ~1–3 m apart, mostly
// radial. Added to the broadcast ephemeris' own SIS error (sub-metre for Galileo,
// ~1 m GPS, a few m BDS/QZSS/GLONASS), the expected honest disagreement is single-
// digit metres; the per-case tolerances absorb that while staying ~2 orders of
// magnitude below any real propagator bug.
package gnss_test

import (
	"bufio"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/gnss/kepler"
)

// rinexFields slices the fixed-width RINEX 3 nav-record float fields out of one
// line (format 4X,4D19.12 — fields can abut when negative, so strings.Fields is
// not safe). base is the column of the first field: 23 on the SV/epoch line, 4 on
// continuation lines. A blank field (spare) parses as 0.
func rinexFields(t *testing.T, line string, base int) []float64 {
	t.Helper()
	var out []float64
	for col := base; col < len(line); col += 19 {
		end := col + 19
		if end > len(line) {
			end = len(line)
		}
		s := strings.TrimSpace(line[col:end])
		if s == "" {
			out = append(out, 0)
			continue
		}
		s = strings.NewReplacer("D", "e", "d", "e").Replace(s)
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			t.Fatalf("bad RINEX field %q in line %q: %v", s, line, err)
		}
		out = append(out, v)
	}
	return out
}

// loadNavRecords reads the trimmed BRDC fixture and returns each record's numeric
// fields keyed by "SVN yyyy mm dd hh mm ss" (the record's first 23 columns).
// Field order follows the RINEX 3.05 nav-record layout: f[0..2] are the clock
// terms from the epoch line, then 4 fields per continuation line — 7 continuation
// lines for the Kepler constellations, 3 for GLONASS.
func loadNavRecords(t *testing.T, path string) map[string][]float64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open nav fixture: %v", err)
	}
	defer f.Close()

	recs := make(map[string][]float64)
	sc := bufio.NewScanner(f)
	inHeader := true
	for sc.Scan() {
		line := sc.Text()
		if inHeader {
			if strings.Contains(line, "END OF HEADER") {
				inHeader = false
			}
			continue
		}
		if len(line) < 23 || line[0] == ' ' {
			t.Fatalf("unexpected nav fixture line %q", line)
		}
		key := line[:23]
		fields := rinexFields(t, line, 23)
		cont := 7
		if line[0] == 'R' || line[0] == 'S' {
			cont = 3
		}
		for i := 0; i < cont && sc.Scan(); i++ {
			fields = append(fields, rinexFields(t, sc.Text(), 4)...)
		}
		recs[key] = fields
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan nav fixture: %v", err)
	}
	return recs
}

// sp3Position returns the ECEF position (metres) of sv at the SP3 epoch line
// starting with epochPrefix in the trimmed SP3 fixture. SP3 positions are km.
func sp3Position(t *testing.T, path, epochPrefix, sv string) gnss.ECEF {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open SP3 fixture: %v", err)
	}
	defer f.Close()

	inEpoch := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "* ") {
			inEpoch = strings.HasPrefix(line, epochPrefix)
			continue
		}
		if !inEpoch || !strings.HasPrefix(line, "P"+sv) {
			continue
		}
		xyz := strings.Fields(line[4:])
		if len(xyz) < 3 {
			t.Fatalf("short SP3 position line %q", line)
		}
		var p [3]float64
		for i := 0; i < 3; i++ {
			v, err := strconv.ParseFloat(xyz[i], 64)
			if err != nil {
				t.Fatalf("bad SP3 coordinate %q: %v", xyz[i], err)
			}
			p[i] = v * 1000 // km → m
		}
		return gnss.ECEF{X: p[0], Y: p[1], Z: p[2]}
	}
	t.Fatalf("SP3 fixture has no %s at epoch %q", sv, epochPrefix)
	return gnss.ECEF{}
}

const (
	navFixture = "testdata/BRDC00WRD_R_20240100000_truth.rnx"
	sp3Fixture = "testdata/ESA0MGNFIN_20240100000_truth.sp3"

	// 2024-01-10 is a Wednesday: GPS day-of-week 3 (weeks start Sunday).
	dowSeconds = 3 * 86400.0
	// BDT = GPST − 14 s, constant (gnsstime: BDT epoch 2006-01-01, GPS−UTC then 14).
	bdtMinusGPST = -14.0
	// GPS−UTC broadcast leap count ΔtLS since 2017-01-01, still 18 in Jan 2024.
	gpsMinusUTC = 18.0
)

// TestKeplerTruthVectors propagates one vendored broadcast ephemeris for each
// Kepler-family constellation to a precise-orbit epoch and requires agreement
// with the ESA MGEX SP3 truth position. tow is in the constellation's own
// timescale: GPS/QZSS/Galileo count the SP3 epoch's GPS seconds-of-week
// directly; BeiDou subtracts the constant 14 s BDT offset. NavIC is absent
// because no public precise-orbit product carries it (its Kepler propagation is
// this same code path with GPS-compatible constants — see physconst).
func TestKeplerTruthVectors(t *testing.T) {
	recs := loadNavRecords(t, navFixture)

	cases := []struct {
		name     string
		key      string      // record key in the nav fixture
		id       gnss.GNSSID // constellation
		svid     int
		tow      float64 // propagation target, constellation timescale
		sp3Epoch string  // SP3 epoch line prefix (GPST)
		sp3SV    string
		tolM     float64
	}{
		// 12:30:00 GPST = tow 304200; broadcast toe 12:00 (tk = +1800 s).
		{"GPS", "G05 2024 01 10 12 00 00", gnss.GPS, 5,
			dowSeconds + 45000, "*  2024  1 10 12 30", "G05", 8},
		// Same instant; I/NAV ephemeris with toe 12:10 (tk = +1200 s).
		{"Galileo", "E21 2024 01 10 12 10 00", gnss.Galileo, 21,
			dowSeconds + 45000, "*  2024  1 10 12 30", "E21", 8},
		// Same instant in BDT (−14 s); C11 is a MEO, exercising the standard
		// (non-GEO) BeiDou path with the BDS-specific constants.
		{"BeiDou", "C11 2024 01 10 12 00 00", gnss.BeiDou, 11,
			dowSeconds + 45000 + bdtMinusGPST, "*  2024  1 10 12 30", "C11", 10},
		// 09:30:00 GPST = tow 293400; QZO orbit with e≈0.075, the highest
		// eccentricity any live constellation broadcasts (tk = +1800 s).
		{"QZSS", "J02 2024 01 10 09 00 00", gnss.QZSS, 2,
			dowSeconds + 34200, "*  2024  1 10  9 30", "J02", 12},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, ok := recs[c.key]
			if !ok {
				t.Fatalf("nav fixture missing record %q", c.key)
			}
			e := kepler.Ephemeris{
				ID: c.id, SVID: c.svid,
				SqrtA: f[10], Ecc: f[8], M0: f[6], DeltaN: f[5],
				I0: f[15], IDot: f[19], Omega0: f[13], OmegaDot: f[18],
				Omega: f[17],
				Cuc:   f[7], Cus: f[9], Crc: f[16], Crs: f[4],
				Cic: f[12], Cis: f[14],
				Toe: f[11],
			}
			got, err := kepler.Propagate(e, c.tow)
			if err != nil {
				t.Fatalf("Propagate: %v", err)
			}
			want := sp3Position(t, sp3Fixture, c.sp3Epoch, c.sp3SV)
			diff := got.Sub(want).Norm()
			t.Logf("%s |broadcast−precise| = %.2f m (tolerance %.0f m)", c.name, diff, c.tolM)
			if diff > c.tolM {
				t.Errorf("%s broadcast-vs-SP3 miss = %.2f m, want < %.0f m (got %+v, want %+v)",
					c.name, diff, c.tolM, got, want)
			}
		})
	}
}

// TestGLONASSTruthVector integrates the vendored R09 PZ-90 state vector (tb
// 12:15:00 UTC) forward to the 12:30:00 GPST SP3 epoch and requires agreement
// with the precise orbit. This externally pins the RK4 equations of motion —
// J₂ term, centrifugal and Coriolis signs — that the reversibility and
// radius-window tests cannot distinguish from their sign-flipped variants.
func TestGLONASSTruthVector(t *testing.T) {
	recs := loadNavRecords(t, navFixture)
	f, ok := recs["R09 2024 01 10 12 15 00"]
	if !ok {
		t.Fatal("nav fixture missing the R09 record")
	}
	e := glonass.Ephemeris{
		X: f[3], Vx: f[4], Ax: f[5],
		Y: f[7], Vy: f[8], Ay: f[9],
		Z: f[11], Vz: f[12], Az: f[13],
		Tb:       12*3600 + 15*60, // record epoch, seconds of UTC day
		TodKnown: true,
		FreqCh:   int(f[10]),
		Slot:     9,
	}
	if e.FreqCh != -2 {
		t.Fatalf("R09 frequency channel = %d, fixture expects -2", e.FreqCh)
	}
	// SP3 epoch 12:30:00 GPST → 12:29:42 UTC; tk is UTC-day-relative.
	tk := (45000 - gpsMinusUTC) - e.Tb
	got, err := glonass.Propagate(e, tk)
	if err != nil {
		t.Fatalf("Propagate: %v", err)
	}
	want := sp3Position(t, sp3Fixture, "*  2024  1 10 12 30", "R09")
	diff := got.Sub(want).Norm()
	t.Logf("GLONASS |broadcast−precise| = %.2f m (tolerance 12 m)", diff)
	if diff > 12 {
		t.Errorf("GLONASS broadcast-vs-SP3 miss = %.2f m, want < 12 m (got %+v, want %+v)",
			diff, got, want)
	}
	if math.Abs(tk-882) > 1e-9 {
		t.Errorf("tk = %v, want 882 (12:29:42 UTC − 12:15:00 UTC)", tk)
	}
}
