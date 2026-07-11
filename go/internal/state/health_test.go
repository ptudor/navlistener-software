package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
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
		// Re-broadcast subframe 1 only, same IODC (85) so no data-set changeover, health flipped.
		st.Apply(gpsFrame(sf1WordsHealth(85, 0x20), now))
		if code := st.FeedSVs(now)["G05@0"].HealthCode; code != 2 {
			t.Errorf("health_code after mid-interval flip = %d, want 2 (not-ok)", code)
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
		st.Apply(bdsD1FrameHealth(6, 1, 100, 0, 1, now))
		if code := st.FeedSVs(now)["C06@0"].HealthCode; code != 2 {
			t.Errorf("health_code after mid-interval SatH1 flip = %d, want 2 (not-ok)", code)
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
		m11bad := bcnav2Frame(prn, 11, 100005, func(buf []byte) { setAbsBits(buf, 30, 2, 1) }) // HS = 1
		st.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m11bad})
		if code := st.FeedSVs(now)["C20@8"].HealthCode; code != 2 {
			t.Errorf("health_code after mid-interval HS flip = %d, want 2 (not-ok)", code)
		}
	})
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
		{0, 1}, // all clear: healthy
		{1, 1}, // low bit only (non-health flag): still healthy
		{2, 1}, // second bit only (non-health flag): still healthy
		{3, 1}, // both low bits, MSB clear: still healthy
		{4, 2}, // MSB set: malfunctioning
		{7, 2}, // all bits set: malfunctioning
	}
	for _, c := range cases {
		code, level := healthFor(gnss.GLONASS, c.raw)
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
