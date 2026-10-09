package state

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
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
