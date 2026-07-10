package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
)

// navFrame is a minimal decoded-nav RawFrame for capability tests: a Words payload is what
// marks a frame as a per-signal nav frame (vs RF/observable telemetry).
func navFrame(source string, g gnss.GNSSID, sig int, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{Source: source, GnssID: g, SigID: sig, Recv: recv, Words: []uint32{0}}
}

// TestCapabilityFingerprint records nav frames from two stations across several signals and
// checks the fingerprint tracks the (gnss, sig) set, counts, and first/last-seen per station.
func TestCapabilityFingerprint(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)

	// station A: GPS L1 (twice), Galileo E1-B I/NAV, Galileo E5a F/NAV
	s.recordCapability("obsA", gnss.GPS, 0, t0)
	s.recordCapability("obsA", gnss.GPS, 0, t0.Add(30*time.Second))
	s.recordCapability("obsA", gnss.Galileo, 0, t0.Add(time.Second))
	s.recordCapability("obsA", gnss.Galileo, 3, t0.Add(2*time.Second))
	// station B: GLONASS L1OF only
	s.recordCapability("obsB", gnss.GLONASS, 0, t0)

	caps := s.FeedStationCapabilities(t0.Add(time.Minute))

	a := caps["obsA"]
	if len(a) != 3 {
		t.Fatalf("obsA capabilities = %+v, want 3 signals", a)
	}
	// sorted by (gnss, sig): GPS(0,0), Galileo(2,0), Galileo(2,3)
	if a[0].Gnss != int(gnss.GPS) || a[0].Sig != 0 || a[0].Count != 2 {
		t.Errorf("obsA[0] = %+v, want GPS L1 count 2", a[0])
	}
	if a[0].LastSeen != t0.Add(30*time.Second).Unix() || a[0].FirstSeen != t0.Unix() {
		t.Errorf("obsA[0] seen window = first %d last %d", a[0].FirstSeen, a[0].LastSeen)
	}
	if a[1].Gnss != int(gnss.Galileo) || a[1].Sig != 0 {
		t.Errorf("obsA[1] = %+v, want Galileo I/NAV", a[1])
	}
	if a[2].Gnss != int(gnss.Galileo) || a[2].Sig != 3 {
		t.Errorf("obsA[2] = %+v, want Galileo F/NAV", a[2])
	}

	if b := caps["obsB"]; len(b) != 1 || b[0].Gnss != int(gnss.GLONASS) {
		t.Errorf("obsB capabilities = %+v, want just GLONASS", b)
	}
}

// TestCapabilityReports checks the detector read model: it unions stations that have an
// observed fingerprint with stations that only carry a declared set, and attaches the station's
// last-seen and both signal sets.
func TestCapabilityReports(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	s.SetDeclaredCapabilities(map[string][]CapSignal{
		"obsA": {{Gnss: 0, Sig: 0}, {Gnss: 2, Sig: 3}},
		"obsC": {{Gnss: 6, Sig: 0}}, // declared but never observed
	})
	// obsA produces GPS L1; obsB (no declaration) produces GLONASS.
	s.recordCapability("obsA", gnss.GPS, 0, t0)
	s.recordCapability("obsB", gnss.GLONASS, 0, t0.Add(time.Second))

	reps := s.FeedCapabilityReports(t0.Add(time.Minute))
	if len(reps) != 3 {
		t.Fatalf("reports = %d stations, want 3 (obsA, obsB, obsC)", len(reps))
	}
	a := reps["obsA"]
	if a.StationLastSeen != t0.Unix() || len(a.Observed) != 1 || len(a.Declared) != 2 {
		t.Errorf("obsA report = %+v", a)
	}
	if b := reps["obsB"]; len(b.Observed) != 1 || b.Declared != nil {
		t.Errorf("obsB report = %+v, want observed-only", b)
	}
	c := reps["obsC"]
	if c.StationLastSeen != 0 || len(c.Observed) != 0 || len(c.Declared) != 1 {
		t.Errorf("obsC report = %+v, want declared-only with no observations", c)
	}
}

// TestCapabilityFromApply confirms Apply records capability from a nav frame but NOT from
// station RF telemetry (which has no per-signal identity).
func TestCapabilityFromApply(t *testing.T) {
	s := New(2)
	now := time.Unix(1_700_000_000, 0)

	s.Apply(navFrame("obs1", gnss.BeiDou, 0, now))
	// RF telemetry from the same station must not forge a (gnss=0, sig=0) capability.
	s.Apply(&ingest.RawFrame{Source: "obs1", Recv: now, RF: &ingest.RawRF{
		Bands: []ingest.RFBand{{Block: 0, AGC: 3000}},
	}})

	caps := s.FeedStationCapabilities(now)["obs1"]
	if len(caps) != 1 || caps[0].Gnss != int(gnss.BeiDou) || caps[0].Sig != 0 {
		t.Fatalf("obs1 capabilities = %+v, want only BeiDou B1I from the nav frame", caps)
	}
}

// TestFeedGlobalTotalLiveReceivers guards total_live_receivers was declared
// in the contract but never populated (always 0). It must count distinct stations
// seen recently via either a nav frame (the capability fingerprint) or RF
// telemetry, excluding ones that have gone quiet past liveReceiverWindow.
func TestFeedGlobalTotalLiveReceivers(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	s.Apply(navFrame("obsA", gnss.GPS, 0, now))                            // nav-frame station
	s.Apply(&ingest.RawFrame{Source: "obsB", Recv: now, RF: &ingest.RawRF{ // RF-only station
		Bands: []ingest.RFBand{{Block: 0, AGC: 3000}},
	}})
	s.Apply(navFrame("obsC", gnss.GLONASS, 0, now.Add(-time.Hour))) // long gone quiet

	if got := s.FeedGlobal(now).TotalLiveReceivers; got != 2 {
		t.Errorf("total_live_receivers = %d, want 2 (obsA nav + obsB RF; obsC is stale)", got)
	}

	// obsC comes back inside the window: now 3.
	s.Apply(navFrame("obsC", gnss.GLONASS, 0, now))
	if got := s.FeedGlobal(now).TotalLiveReceivers; got != 3 {
		t.Errorf("total_live_receivers = %d, want 3 after obsC reports again", got)
	}

	// A station reporting both a nav frame and RF telemetry counts once, not twice.
	s.Apply(&ingest.RawFrame{Source: "obsA", Recv: now, RF: &ingest.RawRF{
		Bands: []ingest.RFBand{{Block: 0, AGC: 2900}},
	}})
	if got := s.FeedGlobal(now).TotalLiveReceivers; got != 3 {
		t.Errorf("total_live_receivers = %d, want 3 (obsA counted once across both sources)", got)
	}
}
