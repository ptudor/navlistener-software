package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// galWord builds a minimal decodable Galileo I/NAV page for the given word
// type: the Even/Odd and Page Type flag bits satisfy DecodeGalileoINAV's regression fix
// validation (odd-part bit0 = 1, everything else 0), and the word type occupies
// the low 6 bits of byte 0 (page bits 2-7). Every other field decodes to its
// zero value, which is enough to assemble a (degenerate but valid) ephemeris.
func galWord(wordType int) []uint32 {
	w := make([]uint32, 8)
	w[0] = uint32(wordType) << 24
	w[4] = 0x80000000
	frame.StampGalileoINAVCRC(w)
	return w
}

func galFrame(svid, wordType int, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{
		Recv: recv, Source: "test", GnssID: gnss.Galileo, SvID: svid, SigID: 0,
		Words: galWord(wordType),
	}
}

// TestGalileoHealthUnknownUntilWord5 guards health_code must read 0
// ("unknown") once an ephemeris is assembled from word types 1-4 alone --
// Galileo health arrives only on word 5, which can lag the ephemeris-bearing
// words -- and only becomes the real decoded code once word 5 actually
// arrives, not the false "OK" st.health's zero value used to produce.
func TestGalileoHealthUnknownUntilWord5(t *testing.T) {
	s := New(2)
	now := time.Unix(1_700_000_000, 0)

	for _, wt := range []int{1, 2, 3, 4} {
		s.Apply(galFrame(11, wt, now))
	}

	svs := s.FeedSVs(now)
	sv, ok := svs["E11@0"]
	if !ok {
		t.Fatal("E11@0 missing from svs feed after words 1-4 assembled an ephemeris")
	}
	if sv.HealthCode != 0 || sv.HealthIssueLevel != 0 {
		t.Errorf("health_code = %d/%d before word 5, want 0/0 (unknown)", sv.HealthCode, sv.HealthIssueLevel)
	}

	s.Apply(galFrame(11, 5, now))
	svs = s.FeedSVs(now)
	sv = svs["E11@0"]
	if sv.HealthCode != 1 || sv.HealthIssueLevel != 0 {
		t.Errorf("health_code = %d/%d after word 5, want 1/0 (decoded OK)", sv.HealthCode, sv.HealthIssueLevel)
	}
}

// TestHealthChangeAppliedWithoutIODChangeover guards a health-bit flip that
// arrives without an ephemeris data-set changeover (a re-broadcast subframe 1 / FraID 1 /
// type 11 under an unchanged IODC/toe/IODE) must reach live state promptly, not be dropped
// until the next full ephemeris cutover. Per constellation: assemble a healthy set, then
// re-send only the health-bearing message with the health flipped and the changeover key
// unchanged; FeedSVs must now report health_code 2.
func TestHealthChangeAppliedWithoutIODChangeover(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)

	t.Run("GPS_LNAV", func(t *testing.T) {
		st := New(4)
		st.Apply(gpsFrame(sf1WordsHealth(85, 0), now)) // IODC low = 85, healthy
		st.Apply(gpsFrame(sf2Words(85, 205075516), now))
		st.Apply(gpsFrame(sf3Words(85), now))
		if code := st.FeedSVs(now)["G05@0"].HealthCode; code != 1 {
			t.Fatalf("initial health_code = %d, want 1 (OK)", code)
		}
		// Re-broadcast subframe 1 only, same IODC (85) so no data-set changeover, health
		// flipped to MSB-set (0x20 = "some or all LNAV data are bad"). the MSB
		// says the CEI data this daemon serves is broadcast-flagged bad — do-not-use
		// (code 3), no longer the old opaque not-ok (2).
		st.Apply(gpsFrame(sf1WordsHealth(85, 0x20), now))
		if code := st.FeedSVs(now)["G05@0"].HealthCode; code != 3 {
			t.Errorf("health_code after mid-interval flip = %d, want 3 (nav data bad → do-not-use)", code)
		}
	})

	t.Run("BeiDou_D1", func(t *testing.T) {
		st := New(4)
		st.Apply(bdsD1Frame(6, 1, 100, 0, now))
		st.Apply(bdsD1Frame(6, 2, 106, 800, now))
		st.Apply(bdsD1Frame(6, 3, 112, 800, now))
		if code := st.FeedSVs(now)["C06@0"].HealthCode; code != 1 {
			t.Fatalf("initial health_code = %d, want 1 (OK)", code)
		}
		// Re-broadcast subframe 1 only (same SOW/toe changeover key), SatH1 flipped.
		// SatH1=1 ("not good", B1I §5.2.4.6) now maps to do-not-use.
		st.Apply(bdsD1FrameHealth(6, 1, 100, 0, 1, now))
		if code := st.FeedSVs(now)["C06@0"].HealthCode; code != 3 {
			t.Errorf("health_code after mid-interval SatH1 flip = %d, want 3 (do-not-use)", code)
		}
	})

	t.Run("BeiDou_BCNAV2", func(t *testing.T) {
		st := New(4)
		const prn = 20
		m10 := bcnav2Frame(prn, 10, 100002, func(buf []byte) {
			setAbsBits(buf, 30, 13, 1)  // WN
			setAbsBits(buf, 53, 8, 7)   // IODE = 7
			setAbsBits(buf, 61, 11, 10) // Toe
			setAbsBits(buf, 72, 2, 3)   // SatType = MEO
		})
		m11 := bcnav2Frame(prn, 11, 100002, nil) // HS = 0 (healthy)
		st.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m10})
		st.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m11})
		if code := st.FeedSVs(now)["C20@8"].HealthCode; code != 1 {
			t.Fatalf("initial health_code = %d, want 1 (OK)", code)
		}
		// Re-broadcast type 11 only (same IODE, no fresh type-30), HS flipped.
		// HS=1 ("does not provide services", Table 7-22) → do-not-use.
		m11bad := bcnav2Frame(prn, 11, 100005, func(buf []byte) { setAbsBits(buf, 30, 2, 1) }) // HS = 1
		st.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m11bad})
		if code := st.FeedSVs(now)["C20@8"].HealthCode; code != 3 {
			t.Errorf("health_code after mid-interval HS flip = %d, want 3 (do-not-use)", code)
		}
	})
}

// TestGPSHealthWordSplit guards the GPS subframe-1 6-bit word is MSB
// (LNAV-data summary) + 5-bit signal-component code (IS-GPS-200N §20.3.3.3.1.4,
// Table 20-VIII), classified per §6.4.6.3's marginal/unusable split — not an
// opaque "any nonzero is not-ok/error".
func TestGPSHealthWordSplit(t *testing.T) {
	cases := []struct {
		raw                 int
		wantCode, wantLevel int
		why                 string
	}{
		{0x00, 1, 0, "all OK"},
		{0x01, 2, 1, "all signals weak: §6.4.6.3 marginal, warning not critical"},
		{0x0E, 2, 1, "L2C signal dead: L1 C/A unaffected — marginal"},
		{0x1D, 2, 1, "SV will be temporarily out (use with caution): marginal"},
		{0x1E, 2, 1, "signals deformed, URA valid: marginal"},
		{0x02, 3, 2, "all signals dead: §6.4.6.3 carve-out, do-not-use"},
		{0x1C, 3, 2, "SV temporarily out (do not use this pass): do-not-use"},
		{0x20, 3, 2, "MSB: some or all LNAV data bad — served CEI data untrustworthy"},
		{0x3F, 3, 2, "all-ones: nav data bad + multiple anomalies"},
	}
	for _, c := range cases {
		code, level := healthFor(gnss.GPS, 0, c.raw)
		if code != c.wantCode || level != c.wantLevel {
			t.Errorf("healthFor(GPS, 0, %#04x) = (%d,%d), want (%d,%d): %s",
				c.raw, code, level, c.wantCode, c.wantLevel, c.why)
		}
	}
}

// TestQZSSHealthWord guards QZSS half: QZSS-PNT-006 §4.1.2.3(4) redefines
// the 6-bit word (MSB = transmitted-L1-signal health; 5 LSBs = per-signal bits
// L1C/A·L2C·L5·L1C·L1C/B), and the exclusive L1C/A / L1C/B pair means exactly one
// of those bits is 1 in NORMAL operation — the old GPS-style nonzero test
// classified every healthy QZSS SV as not-ok/critical.
func TestQZSSHealthWord(t *testing.T) {
	cases := []struct {
		raw                 int
		wantCode, wantLevel int
		why                 string
	}{
		{0x00, 1, 0, "all clear"},
		{0x01, 1, 0, "L1C/B flagged while L1C/A transmits: designed normal ops"},
		{0x10, 1, 0, "L1C/A flagged while L1C/B transmits: designed normal ops"},
		{0x11, 2, 1, "both exclusive-pair bits with L1 summary healthy: inconsistent — marginal"},
		{0x08, 2, 1, "L2C unhealthy: tracked L1 fine — marginal"},
		{0x05, 2, 1, "L5 unhealthy (+normal L1C/B bit): marginal"},
		{0x20, 3, 2, "transmitted L1 signal unhealthy: do-not-use"},
		{0x3F, 3, 2, "everything flagged: do-not-use"},
	}
	for _, c := range cases {
		code, level := healthFor(gnss.QZSS, 0, c.raw)
		if code != c.wantCode || level != c.wantLevel {
			t.Errorf("healthFor(QZSS, 0, %#04x) = (%d,%d), want (%d,%d): %s",
				c.raw, code, level, c.wantCode, c.wantLevel, c.why)
		}
	}
}

// TestGalileoSHSMapping guards the SHS enum per GAL-OS-SIS-ICD-2.2
// Table 84 — 0 Signal OK, 1 Signal out of service, 2 Signal in Extended
// Operations Mode (EOM, REDEFINED in Issue 2.2 from the old "will be out of
// service"), 3 Signal Component currently in Test. EOM is a usable published
// operational mode: it must map to not-ok/warning like in-test, never to
// do-not-use — the regression this test pins is someone "fixing" SHS=2 to
// (3,2) from the superseded pre-2.2 semantics.
func TestGalileoSHSMapping(t *testing.T) {
	cases := []struct {
		raw                 int
		wantCode, wantLevel int
		why                 string
	}{
		{0, 1, 0, "Signal OK"},
		{1, 3, 2, "Signal out of service: do-not-use"},
		{2, 2, 1, "EOM (Issue 2.2 redefinition): usable mode — warning, NOT do-not-use"},
		{3, 2, 1, "Signal Component currently in Test: warning"},
	}
	for _, c := range cases {
		code, level := healthFor(gnss.Galileo, 0, c.raw)
		if code != c.wantCode || level != c.wantLevel {
			t.Errorf("healthFor(Galileo, 0, %d) = (%d,%d), want (%d,%d): %s",
				c.raw, code, level, c.wantCode, c.wantLevel, c.why)
		}
	}
}

// TestBeiDouHealthMapping guards B-CNAV2 HS=1 is BDS-SIS-ICD-B2a v1.0
// Table 7-22's "The satellite does not provide services" — do-not-use (3), the
// same enum GPS's nav-data-bad and Galileo's SHS=1 get — and D1's 1-bit SatH1=1
// (B1I v3.0 §5.2.4.6 "not good") shares that arm. The reserved HS values 2/3
// stay at not-ok: undefined is not known-dead.
func TestBeiDouHealthMapping(t *testing.T) {
	cases := []struct {
		raw                 int
		wantCode, wantLevel int
		why                 string
	}{
		{0, 1, 0, "healthy / provides services"},
		{1, 3, 2, "HS=1 does not provide services (SatH1=1 not good): do-not-use"},
		{2, 2, 2, "reserved: not-ok, not promoted to do-not-use"},
		{3, 2, 2, "reserved: not-ok, not promoted to do-not-use"},
	}
	for _, c := range cases {
		for _, sig := range []int{0, 8} { // D1 (B1I) and B-CNAV2 (B2a) entries share the arm
			code, level := healthFor(gnss.BeiDou, sig, c.raw)
			if code != c.wantCode || level != c.wantLevel {
				t.Errorf("healthFor(BeiDou, %d, %d) = (%d,%d), want (%d,%d): %s",
					sig, c.raw, code, level, c.wantCode, c.wantLevel, c.why)
			}
		}
	}
}

// TestCNAVCarrierHealthMapping guards the per-signal arm: a GPS/QZSS CNAV entry
// (sig != 0) stores the tracked carrier's own 1-bit health (IS-GPS-200N
// §30.3.3.1.1.2), where 1 = "all codes and data on this carrier are bad or
// unavailable" — do-not-use for that entry, never the 6-bit LNAV semantics.
func TestCNAVCarrierHealthMapping(t *testing.T) {
	for _, g := range []gnss.GNSSID{gnss.GPS, gnss.QZSS} {
		if code, level := healthFor(g, 3, 0); code != 1 || level != 0 {
			t.Errorf("healthFor(%v, sig 3, 0) = (%d,%d), want (1,0)", g, code, level)
		}
		if code, level := healthFor(g, 3, 1); code != 3 || level != 2 {
			t.Errorf("healthFor(%v, sig 3, 1) = (%d,%d), want (3,2)", g, code, level)
		}
	}
}

// TestGLONASSHealthMasksToMSB guards frame.DecodeGLONASSString stores
// the raw 3-bit Bn field, and only bit 2 (value 4, the MSB) is the GLONASS ICD
// Ed. 5.1 malfunction flag -- the two low-order bits are other status, not
// overall SV health, and must not alone flip a healthy SV to not-ok.
func TestGLONASSHealthMasksToMSB(t *testing.T) {
	cases := []struct {
		raw      int
		wantCode int
	}{
		{0, 1},  // all clear: healthy
		{1, 1},  // low bit only (non-health flag): still healthy
		{2, 1},  // second bit only (non-health flag): still healthy
		{3, 1},  // both low bits, MSB clear: still healthy
		{4, 2},  // Bn MSB set: malfunctioning
		{7, 2},  // all Bn bits set: malfunctioning
		{8, 2},  // packed ℓn fast flag alone: malfunctioning (Table 5.1 joint gate)
		{12, 2}, // Bn MSB and ℓn both set: malfunctioning
		{11, 2}, // ℓn set over benign Bn low bits: malfunctioning
	}
	for _, c := range cases {
		code, level := healthFor(gnss.GLONASS, 0, c.raw)
		if code != c.wantCode {
			t.Errorf("healthFor(GLONASS, raw=%d) code = %d, want %d", c.raw, code, c.wantCode)
		}
		if c.wantCode == 1 && level != 0 {
			t.Errorf("healthFor(GLONASS, raw=%d) level = %d, want 0", c.raw, level)
		}
		if c.wantCode == 2 && level != 2 {
			t.Errorf("healthFor(GLONASS, raw=%d) level = %d, want 2", c.raw, level)
		}
	}
}
