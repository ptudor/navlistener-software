package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// TestExpireStationsEvictsFilteredResidue guards sbas/rf/GLONASS-
// almanac entries were filtered from the feeds when stale but never deleted, so
// a renamed source or decommissioned GEO parked a RAM entry forever while the
// comments claimed "dropped". ExpireStations deletes them past a generous
// multiple of the serving-staleness window; recent entries and the deliberately
// durable capability fingerprint stay.
func TestExpireStationsEvictsFilteredResidue(t *testing.T) {
	s := New(2)
	now := time.Unix(1_700_000_000, 0)
	old := now.Add(-stationEvictAfter - time.Minute)
	fresh := now.Add(-time.Minute)

	// RF: one long-dark station, one live one (both via the real apply path).
	s.Apply(&ingest.RawFrame{Recv: old, Source: "gone", RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 2500}}}})
	s.Apply(&ingest.RawFrame{Recv: fresh, Source: "alive", RF: &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 2500}}}})

	// SBAS + almanac residue seeded directly (white-box, as the maps are internal).
	s.sbas[122] = &sbasState{prn: 122, provider: "EGNOS", lastSeen: old}
	s.sbas[131] = &sbasState{prn: 131, provider: "WAAS", lastSeen: fresh}
	s.gloAlmanac[3] = gloAlmSlot{entry: frame.GLONASSAlmanacEntry{}, lastSeen: now.Add(-gloAlmanacStaleAfter - time.Hour)}
	s.gloAlmanac[7] = gloAlmSlot{entry: frame.GLONASSAlmanacEntry{}, lastSeen: fresh}

	// A durable capability fingerprint far older than every eviction window.
	s.recordCapability("gone", 0, 0, old.Add(-24*time.Hour))

	s.ExpireStations(now)

	if _, ok := s.rf["gone"]; ok {
		t.Error("long-dark rf station survived ExpireStations")
	}
	if _, ok := s.rf["alive"]; !ok {
		t.Error("live rf station was evicted")
	}
	if _, ok := s.sbas[122]; ok {
		t.Error("long-dark SBAS PRN survived ExpireStations")
	}
	if _, ok := s.sbas[131]; !ok {
		t.Error("fresh SBAS PRN was evicted")
	}
	if _, ok := s.gloAlmanac[3]; ok {
		t.Error("stale GLONASS almanac slot survived ExpireStations")
	}
	if _, ok := s.gloAlmanac[7]; !ok {
		t.Error("fresh GLONASS almanac slot was evicted")
	}
	if _, ok := s.caps["gone"]; !ok {
		t.Error("capability fingerprint was evicted — it is durable by design (capability.go)")
	}
}

// Detect's silence operating point, duplicated here deliberately  — the
// same keep-in-sync idiom feed.go uses for liveReceiverWindow, and for the same
// reason: detect imports state, so state cannot import detect's constants back.
// If detect.SBASSilentThreshold / detect.ObserverOfflineThreshold /
// detect.DebounceDuration move, update these and the test below will say whether
// the eviction window still leaves the detectors room to confirm.
const (
	detectMaxSilenceThreshold = 300 * time.Second // max(SBASSilentThreshold, ObserverOfflineThreshold)
	detectDebounceDuration    = 60 * time.Second  // detect.DebounceDuration
)

// TestStationEvictOutlastsDetect guards sbas_lost and station_offline can
// only CONFIRM while the station's sbas/rf RAM entry still exists, so eviction must
// outlast the slowest silence threshold plus the debounce. Nothing in the type
// system enforces that (the thresholds live in detect, which depends on state, and
// stationEvictAfter is unexported), so a future threshold raise would otherwise
// silently disarm the darkness detector that eviction was built around.
func TestStationEvictOutlastsDetect(t *testing.T) {
	need := detectMaxSilenceThreshold + detectDebounceDuration
	if stationEvictAfter <= need {
		t.Fatalf("stationEvictAfter = %v must exceed max(silence threshold)+debounce = %v; "+
			"eviction would delete the rf/sbas entry before sbas_lost/station_offline can confirm",
			stationEvictAfter, need)
	}
}
