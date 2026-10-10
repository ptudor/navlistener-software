package state

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
	"github.com/ptudor/navlistener/internal/ingest"
)

// almanacPageWords builds an LNAV almanac page (IS-GPS-200N Figure 20-1 sheet
// 4) for svID with the given data ID, toa count (× 2^12 s) and mean anomaly;
// the other elements are a GPS-like orbit, or a GEO one for data ID 3.
func almanacPageWords(dataID, svID, toaRaw, m0 int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 1, 17, 1000)
	setField(buf, 2, 20, 3, 5)
	setField(buf, 3, 1, 2, dataID)
	setField(buf, 3, 3, 6, svID)
	setField(buf, 4, 1, 8, toaRaw)
	setField(buf, 5, 1, 16, -690)
	setField(buf, 7, 1, 24, -2021000)
	setField(buf, 8, 1, 24, 3298500)
	setField(buf, 9, 1, 24, m0)
	if dataID == 3 {
		setField(buf, 6, 1, 24, 13297664) // √A ≈ 6493, a GEO radius
		return packWords(buf)
	}
	setField(buf, 3, 9, 16, 12109) // e ≈ 0.0058
	setField(buf, 4, 9, 16, 4498)  // i ≈ 0.3086 semicircles
	setField(buf, 6, 1, 24, 10554971)
	return packWords(buf)
}

// toaRawNear returns the toa count closest to at's GPS time of week.
func toaRawNear(at time.Time) int64 {
	return min(int64(math.Round(gpsTOW(at)/4096)), 147)
}

func almanacFrame(g gnss.GNSSID, transmitter int, words []uint32, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{Recv: recv, Source: "test", GnssID: g, SvID: transmitter, SigID: 0, Words: words}
}

func monitoringByName(s *Store, now time.Time) map[string]MonitoringSatellite {
	out := map[string]MonitoringSatellite{}
	for _, sv := range s.MonitoringSatellites(now) {
		out[sv.Name] = sv
	}
	return out
}

// TestGPSAlmanacPlacesUnheardSatellite: an almanac page broadcast by G05
// describes G12, which no station hears. The coverage feed then places G12 on
// the GPS shell with no witnesses, and the almanac feed carries it as an
// unobserved entry referenced to the page's toa.
func TestGPSAlmanacPlacesUnheardSatellite(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	toaRaw := toaRawNear(now)
	s.Apply(almanacFrame(gnss.GPS, 5, almanacPageWords(1, 12, toaRaw, 1000000), now))

	sats := monitoringByName(s, now)
	g12, ok := sats["G12"]
	if !ok || g12.Position == nil || g12.PositionSource != "almanac" || len(g12.WitnessTimes) != 0 {
		t.Fatalf("G12 = %+v", g12)
	}
	if r := math.Sqrt(g12.Position[0]*g12.Position[0] + g12.Position[1]*g12.Position[1] + g12.Position[2]*g12.Position[2]); r < 26.0e6 || r > 27.2e6 {
		t.Fatalf("G12 almanac radius %.0f m", r)
	}
	if g05 := sats["G05"]; g05.Position != nil || len(g05.WitnessTimes) != 1 {
		t.Fatalf("the transmitter G05 = %+v; its own position needs its own almanac or ephemeris", g05)
	}
	ent, ok := s.FeedAlmanac(now)["G12"]
	if !ok || ent.Observed || ent.GnssID != int(gnss.GPS) || ent.T0e != int(toaRaw*4096) {
		t.Fatalf("almanac feed G12 = %+v", ent)
	}
}

// TestEphemerisPositionWinsOverAlmanac: a satellite with a fresh broadcast
// ephemeris keeps that precise position in both feeds.
func TestEphemerisPositionWinsOverAlmanac(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(gpsFrame(sf1Words(85), now))
	s.Apply(gpsFrame(sf2Words(85, 205075516), now))
	s.Apply(gpsFrame(sf3Words(85), now))
	s.Apply(almanacFrame(gnss.GPS, 5, almanacPageWords(1, 5, toaRawNear(now), 1000000), now))
	s.Propagate(now)
	if g05 := monitoringByName(s, now)["G05"]; g05.PositionSource != "ephemeris" {
		t.Fatalf("G05 position source %q", g05.PositionSource)
	}
	if ent := s.FeedAlmanac(now)["G05"]; !ent.Observed {
		t.Fatalf("almanac replaced the observed G05 entry: %+v", ent)
	}
}

// TestAlmanacValidityWindow: a GPS almanac is served while t is within 3.5
// days of toa (IS-GPS-200N §20.3.3.5.2.2) and then evicted; a QZS almanac
// received 80 hours from its toa is outside the 72 h QZSS bound and ignored.
func TestAlmanacValidityWindow(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(almanacFrame(gnss.GPS, 5, almanacPageWords(1, 12, toaRawNear(now), 1000000), now))
	later := now.Add(83 * time.Hour)
	if g12 := monitoringByName(s, later)["G12"]; g12.Position == nil {
		t.Fatal("G12 dropped inside its validity window")
	}
	expired := now.Add(85 * time.Hour)
	if g12 := monitoringByName(s, expired)["G12"]; g12.Position != nil {
		t.Fatal("G12 served past 3.5 days from toa")
	}
	s.ExpireStations(expired)
	if len(s.almanacs) != 0 {
		t.Fatal("expired almanac kept in RAM")
	}

	q := New(4)
	q.Apply(almanacFrame(gnss.QZSS, 2, almanacPageWords(3, 7, toaRawNear(now.Add(-80*time.Hour)), 0), now))
	if len(q.almanacs) != 0 {
		t.Fatal("QZS almanac outside 72 h of toa accepted")
	}
}

// TestOlderAlmanacNeverReplacesNewer: a late, spooled page from an earlier
// almanac set cannot roll an entry back; a re-upload under the same toa that
// arrives later replaces it.
func TestOlderAlmanacNeverReplacesNewer(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	toaRaw := toaRawNear(now)
	s.Apply(almanacFrame(gnss.GPS, 5, almanacPageWords(1, 12, toaRaw, 1000000), now))
	s.Apply(almanacFrame(gnss.GPS, 6, almanacPageWords(1, 12, toaRaw-1, 2000000), now.Add(time.Minute)))
	key := almanacKey{G: gnss.GPS, Sv: 12}
	newest := s.almanacs[key]
	if newest.eph.Toe != float64(toaRaw*4096) {
		t.Fatalf("older set replaced newer: toe %v", newest.eph.Toe)
	}
	s.Apply(almanacFrame(gnss.GPS, 7, almanacPageWords(1, 12, toaRaw, 3000000), now.Add(2*time.Minute)))
	if s.almanacs[key].eph.M0 == newest.eph.M0 {
		t.Fatal("a later page under the same toa did not replace the entry")
	}
	s.Apply(almanacFrame(gnss.GPS, 8, almanacPageWords(1, 12, toaRaw, 1000000), now.Add(-time.Minute)))
	if s.almanacs[key].eph.M0 == newest.eph.M0 {
		t.Fatal("an earlier-received page under the same toa replaced a later one")
	}
}

// TestQZSAlmanacKeyedBySubject: a QZS almanac page (data ID 3) for SV ID 7, a
// GEO satellite, is stored as J07 and placed near geostationary radius; a GPS
// data ID on a QZSS frame is not a QZS almanac.
func TestQZSAlmanacKeyedBySubject(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(almanacFrame(gnss.QZSS, 2, almanacPageWords(3, 7, toaRawNear(now), 0), now))
	j07 := monitoringByName(s, now)["J07"]
	if j07.Position == nil || j07.PositionSource != "almanac" || j07.GNSS != int(gnss.QZSS) {
		t.Fatalf("J07 = %+v", j07)
	}
	if r := math.Sqrt(j07.Position[0]*j07.Position[0] + j07.Position[1]*j07.Position[1] + j07.Position[2]*j07.Position[2]); r < 41.0e6 || r > 43.0e6 {
		t.Fatalf("J07 radius %.0f m", r)
	}
	s.Apply(almanacFrame(gnss.QZSS, 2, almanacPageWords(1, 12, toaRawNear(now), 0), now))
	if _, ok := s.almanacs[almanacKey{G: gnss.QZSS, Sv: 12}]; ok {
		t.Fatal("a GPS almanac page on a QZSS frame was stored")
	}
}

// TestGlonassAlmanacPlacesSatelliteOnMap: the coverage feed now uses the
// GLONASS almanac for slots without an ephemeris position, as it does for GPS.
func TestGlonassAlmanacPlacesSatelliteOnMap(t *testing.T) {
	s := New(4)
	now := time.Now()
	s.gloNA = 615
	s.gloAlmanac[7] = gloAlmSlot{entry: icdAlmanac(7), lastSeen: now}
	r07 := monitoringByName(s, now)["R07"]
	if r07.Position == nil || r07.PositionSource != "almanac" || r07.GNSS != int(gnss.GLONASS) {
		t.Fatalf("R07 = %+v", r07)
	}
	s.gloNA = 0
	if r07 := monitoringByName(s, now)["R07"]; r07.Position != nil {
		t.Fatal("GLONASS almanac placed without its day-number anchor")
	}
}

// TestResetClearsAlmanacs: an audience reset drops every learned almanac with
// the rest of the audience's state.
func TestResetClearsAlmanacs(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(almanacFrame(gnss.GPS, 5, almanacPageWords(1, 12, toaRawNear(now), 1000000), now))
	s.Reset()
	if len(s.almanacs) != 0 || monitoringByName(s, now)["G12"].Position != nil {
		t.Fatal("reset kept an almanac")
	}
}

// TestRealCaptureAlmanacPlacesGPSSatellites replays the GPS frames of a real
// receiver capture: the almanac pages it carries place eleven satellites,
// including G23 and G24, which the receiver never tracked.
func TestRealCaptureAlmanacPlacesGPSSatellites(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	s := New(4)
	tracked := map[int]bool{}
	for _, f := range captureFrames(t, "../ingest/testdata/glo_superframe_capture.ubx", at, gnss.GPS) {
		tracked[f.SvID] = true
		s.Apply(f)
	}
	sats := monitoringByName(s, at)
	for _, sv := range []int{1, 2, 3, 4, 5, 23, 24, 25, 26, 27, 28} {
		name := fmt.Sprintf("G%02d", sv)
		if p := sats[name]; p.Position == nil || p.PositionSource != "almanac" {
			t.Fatalf("%s = %+v", name, p)
		}
	}
	if tracked[23] || tracked[24] {
		t.Fatal("the capture tracks G23/G24; pick satellites it does not hear")
	}
}

// galAlmanacWordAt builds I/NAV almanac word type wt (7–10) of batch iod whose
// reference matches at: WNa is the two low bits of at's GST week and t0a the
// 600 s step nearest at's time of week. The satellite it begins (word types
// 7–9) is head; every satellite gets a nominal Galileo orbit (Δ(√A) = 0, e = 0,
// δi = 0) with distinct M0.
func galAlmanacWordAt(t *testing.T, wt, iod, head int, at time.Time) []uint32 {
	t.Helper()
	week, ok := gnsstime.WeekAt(gnsstime.SysGalileo, float64(at.Unix()), float64(gpsUTCOffset))
	if !ok {
		t.Fatal("no GST week")
	}
	t0a := min(uint64(math.Round(gpsTOW(at)/600)), 1007)
	content := make([]byte, 16)
	inavSetBits(content, 0, uint64(wt), 6)
	inavSetBits(content, 6, uint64(iod), 4)
	switch wt {
	case 7:
		inavSetBits(content, 10, uint64(week&3), 2)
		inavSetBits(content, 12, t0a, 10)
		inavSetBits(content, 22, uint64(head), 6)
		inavSetBits(content, 106, 1000, 16) // M0
	case 8:
		inavSetBits(content, 43, uint64(head), 6)
	case 9:
		inavSetBits(content, 10, uint64(week&3), 2)
		inavSetBits(content, 12, t0a, 10)
		inavSetBits(content, 22, 2000, 16) // the previous satellite's M0
		inavSetBits(content, 71, uint64(head), 6)
	case 10:
		inavSetBits(content, 37, 3000, 16) // the previous satellite's M0
	}
	return inavContentWords(content)
}

func galAlmanacFrame(source string, transmitter int, words []uint32, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{GnssID: gnss.Galileo, SvID: transmitter, SigID: 1, Source: source, Recv: recv, Words: words}
}

// TestGalileoAlmanacJoinsPerRelay: a satellite's almanac is completed only by
// the next almanac word from the same relay, within a minute on the feeder
// clock. Another station's word in between does not break the pair, a lost
// word does not let a stale one join, and the placed satellite lands on the
// Galileo shell.
func TestGalileoAlmanacJoinsPerRelay(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(galAlmanacFrame("a", 2, galAlmanacWordAt(t, 7, 5, 11, now), now))
	s.Apply(galAlmanacFrame("b", 2, galAlmanacWordAt(t, 8, 5, 12, now), now.Add(time.Second)))
	if _, ok := s.almanacs[almanacKey{G: gnss.Galileo, Sv: 11}]; ok {
		t.Fatal("another relay's word 8 completed relay a's SVID 11")
	}
	s.Apply(galAlmanacFrame("a", 2, galAlmanacWordAt(t, 8, 5, 12, now), now.Add(2*time.Second)))
	e11 := monitoringByName(s, now)["E11"]
	if e11.Position == nil || e11.PositionSource != "almanac" || e11.GNSS != int(gnss.Galileo) {
		t.Fatalf("E11 = %+v", e11)
	}
	if r := math.Sqrt(e11.Position[0]*e11.Position[0] + e11.Position[1]*e11.Position[1] + e11.Position[2]*e11.Position[2]); math.Abs(r-29600000) > 1000 {
		t.Fatalf("E11 radius %.0f m, want the nominal 29 600 km", r)
	}
	// Relay a's word 9 arrives two minutes later: a word was lost, so SVID 12
	// is not completed from a stale word 8.
	s.Apply(galAlmanacFrame("a", 2, galAlmanacWordAt(t, 9, 5, 13, now), now.Add(2*time.Minute)))
	if _, ok := s.almanacs[almanacKey{G: gnss.Galileo, Sv: 12}]; ok {
		t.Fatal("SVID 12 joined across a gap")
	}
	// Word 10 right after word 9 completes SVID 13.
	s.Apply(galAlmanacFrame("a", 2, galAlmanacWordAt(t, 10, 5, 0, now), now.Add(2*time.Minute+2*time.Second)))
	if _, ok := s.almanacs[almanacKey{G: gnss.Galileo, Sv: 13}]; !ok {
		t.Fatal("SVID 13 not completed by word 10")
	}
}

// TestGalileoAlmanacChecksReferenceWeek: a batch whose two-bit WNa disagrees
// with the GST week its t0a resolves to is not stored.
func TestGalileoAlmanacChecksReferenceWeek(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	lastWeek := now.Add(-7 * 24 * time.Hour)
	w7 := galAlmanacWordAt(t, 7, 5, 11, lastWeek) // last week's WNa, this week's t0a
	s.Apply(galAlmanacFrame("a", 2, w7, now))
	s.Apply(galAlmanacFrame("a", 2, galAlmanacWordAt(t, 8, 5, 12, now), now.Add(time.Second)))
	if len(s.almanacs) != 0 {
		t.Fatal("almanac with a mismatched WNa stored")
	}
}

// TestRealCaptureAlmanacPlacesGalileoSatellites replays a real capture's E1-B
// pages, stamped in a week whose low bits match its WNa: the joined almanacs
// place Galileo satellites the receiver never tracked.
func TestRealCaptureAlmanacPlacesGalileoSatellites(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	for {
		week, _ := gnsstime.WeekAt(gnsstime.SysGalileo, float64(at.Unix()), float64(gpsUTCOffset))
		if week&3 == 3 { // the capture's WNa
			break
		}
		at = at.Add(7 * 24 * time.Hour)
	}
	s := New(4)
	tracked := map[int]bool{}
	for _, f := range captureFrames(t, "../ingest/testdata/glo_superframe_capture.ubx", at, gnss.Galileo) {
		if f.SigID == 1 {
			tracked[f.SvID] = true
			s.Apply(f)
		}
	}
	placed, untracked := 0, 0
	for _, sv := range s.MonitoringSatellites(at) {
		if sv.GNSS == int(gnss.Galileo) && sv.PositionSource == "almanac" {
			placed++
			var n int
			fmt.Sscanf(sv.Name, "E%d", &n)
			if !tracked[n] {
				untracked++
			}
		}
	}
	if placed < 8 || untracked < 2 {
		t.Fatalf("almanacs placed %d Galileo satellites, %d untracked", placed, untracked)
	}
}

// midiAlmanacFrameAt builds a B-CNAV2 message type 40 from transmitter tx whose
// midi almanac describes prn (a MEO satellite) with toa = the 2^12 s step of
// toa's BDT time of week in its BDT week.
func midiAlmanacFrameAt(t *testing.T, tx, prn int, toa, recv time.Time) *ingest.RawFrame {
	t.Helper()
	week, ok := gnsstime.WeekAt(gnsstime.SysBeiDou, float64(toa.Unix()), float64(gpsUTCOffset))
	if !ok {
		t.Fatal("no BDT week")
	}
	toaRaw := min(uint64(towFor(gnss.BeiDou, toa)/4096), 147)
	words := bcnav2Frame(tx, 40, 3000, func(buf []byte) {
		setAbsBits(buf, 69, 6, uint64(prn))
		setAbsBits(buf, 75, 2, 3) // MEO
		setAbsBits(buf, 77, 13, uint64(week))
		setAbsBits(buf, 90, 8, toaRaw)
		setAbsBits(buf, 120, 17, 84522) // √A ≈ 5282.6
		setAbsBits(buf, 180, 16, 1000)  // M0
	})
	return &ingest.RawFrame{GnssID: gnss.BeiDou, SvID: tx, SigID: 8, Recv: recv, Words: words}
}

// TestBeiDouMidiAlmanacServedForSevenDays: a midi almanac's own week fixes its
// toa, so it is served across the half-week time-of-week wrap until it is 7
// days old (BDS-SIS-B1I-3.0 Table 5-1), then evicted.
func TestBeiDouMidiAlmanacServedForSevenDays(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(midiAlmanacFrameAt(t, 20, 33, now, now))
	for _, age := range []time.Duration{0, 4 * 24 * time.Hour, 6 * 24 * time.Hour} {
		c33 := monitoringByName(s, now.Add(age))["C33"]
		if c33.Position == nil || c33.PositionSource != "almanac" {
			t.Fatalf("C33 at %v = %+v", age, c33)
		}
		if r := math.Sqrt(c33.Position[0]*c33.Position[0] + c33.Position[1]*c33.Position[1] + c33.Position[2]*c33.Position[2]); r < 27.6e6 || r > 28.2e6 {
			t.Fatalf("C33 radius %.0f m at %v", r, age)
		}
	}
	expired := now.Add(7*24*time.Hour + 2*time.Hour)
	if c33 := monitoringByName(s, expired)["C33"]; c33.Position != nil {
		t.Fatal("C33 served past 7 days")
	}
	s.ExpireStations(expired)
	if len(s.almanacs) != 0 {
		t.Fatal("expired BeiDou almanac kept")
	}
	// A page received more than 7 days from its toa is not stored at all.
	s.Apply(midiAlmanacFrameAt(t, 20, 33, now, now.Add(8*24*time.Hour)))
	if len(s.almanacs) != 0 {
		t.Fatal("stale midi almanac stored")
	}
}

// TestAlmanacReepochIsExact: re-referencing an almanac by whole weeks gives the
// same position as evaluating the Table 7-15 algorithm with the true elapsed
// time, which the propagator's own half-week wrap cannot represent.
func TestAlmanacReepochIsExact(t *testing.T) {
	toa := time.Unix(1_700_000_000, 0)
	eph := kepler.Ephemeris{
		ID: gnss.BeiDou, SVID: 33, Almanac: true,
		SqrtA: 5282.6, Ecc: 0.0005, M0: 1.1, I0: 0.95, Omega0: -2.0, OmegaDot: -6.9e-9, Omega: 0.4,
		Toe: towFor(gnss.BeiDou, toa),
	}
	p, _ := physconst.For(gnss.BeiDou)
	// oracle evaluates BDS-SIS-B2a-1.0 Table 7-15 with tk as given.
	oracle := func(tk float64) gnss.ECEF {
		a := eph.SqrtA * eph.SqrtA
		m := eph.M0 + math.Sqrt(p.Mu/(a*a*a))*tk
		e := m
		for i := 0; i < 30; i++ {
			e = m + eph.Ecc*math.Sin(e)
		}
		nu := math.Atan2(math.Sqrt(1-eph.Ecc*eph.Ecc)*math.Sin(e), math.Cos(e)-eph.Ecc)
		u := nu + eph.Omega
		r := a * (1 - eph.Ecc*math.Cos(e))
		om := eph.Omega0 + (eph.OmegaDot-p.OmegaE)*tk - p.OmegaE*eph.Toe
		x, y := r*math.Cos(u), r*math.Sin(u)
		return gnss.ECEF{
			X: x*math.Cos(om) - y*math.Cos(eph.I0)*math.Sin(om),
			Y: x*math.Sin(om) + y*math.Cos(eph.I0)*math.Cos(om),
			Z: y * math.Sin(eph.I0),
		}
	}
	for _, days := range []float64{1, 4, 6.5, -5} {
		now := toa.Add(time.Duration(days * 24 * float64(time.Hour)))
		shifted, ok := almanacAt(eph, toa, now)
		if !ok {
			t.Fatal("re-epoch refused")
		}
		got, err := kepler.Propagate(shifted, towFor(gnss.BeiDou, now))
		if err != nil {
			t.Fatal(err)
		}
		if miss := got.Sub(oracle(now.Sub(toa).Seconds())).Norm(); miss > 0.01 {
			t.Fatalf("%.1f days from toa: %.3f m from the true-elapsed-time position", days, miss)
		}
	}
}

// TestRealCaptureMidiAlmanacPlacesBeiDouSatellites replays a real capture's B2a
// frames: midi almanacs place the IGSO and MEO satellites, including ones the
// receiver never tracked.
func TestRealCaptureMidiAlmanacPlacesBeiDouSatellites(t *testing.T) {
	// The capture's almanacs reference BDT week 1070, toa 421 888 s, and its
	// ephemerides were broadcast three days later; stamp it then.
	unix, _ := gnsstime.GNSSTime{Sys: gnsstime.SysBeiDou, Week: 1071, TOW: 79200}.ToUnix(float64(gpsUTCOffset))
	at := time.Unix(int64(unix), 0)
	s := New(4)
	tracked := map[int]bool{}
	for _, f := range captureFrames(t, "../ingest/testdata/glo_superframe_capture.ubx", at, gnss.BeiDou) {
		if f.SigID == 8 {
			tracked[f.SvID] = true
			s.Apply(f)
		}
	}
	placed, untracked := 0, 0
	for _, sv := range s.MonitoringSatellites(at) {
		if sv.GNSS == int(gnss.BeiDou) && sv.PositionSource == "almanac" {
			placed++
			var n int
			fmt.Sscanf(sv.Name, "C%d", &n)
			if !tracked[n] {
				untracked++
			}
		}
	}
	if placed < 25 || untracked < 10 {
		t.Fatalf("midi almanacs placed %d BeiDou satellites, %d untracked", placed, untracked)
	}
}
