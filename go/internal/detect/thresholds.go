// Package detect is the integrity DETECT stage (docs/INTEGRITY.md §1): a debounced
// state machine that turns the per-SV metrics computed in internal/state
// (orbit-disco, time-disco, health, ephemeris age, SISA, silence) into confirmed,
// typed integrity events. Compute is in internal/state; this package only detects
// transitions and emits the event contract (docs/OUTPUT.md §3). It is pure — Tick
// takes a snapshot and returns events — so the daemon owns persistence and the SSE
// broadcast.
package detect

import "time"

// Thresholds are the detection operating points (docs/INTEGRITY.md §2). The values
// are the standard defined there (cross-checked against intsat's shipped detector);
// they are collected here so the whole detector is tuned in one place.
const (
	// Ephemeris age (minutes): Galileo refreshes fastest, so it is stricter; the
	// GPS value is the default for every non-Galileo constellation (QZSS/NavIC
	// inherit it).
	EphAgeThresholdGalileo = 105.0
	EphAgeThresholdGPS     = 140.0

	// Orbit-disco (metres): warn band, then a severe band.
	OrbitDiscoThreshold       = 1.45
	OrbitDiscoSevereThreshold = 10.0

	// Time-disco / clock jump (nanoseconds): warn band, then a severe band.
	TimeDiscoThreshold       = 2.5
	TimeDiscoSevereThreshold = 10.0

	// SISA/URA accuracy degradation crossing (metres). A boundary-dithering value
	// cannot flap: the debounce window is the flap filter (docs/INTEGRITY.md §4).
	SISAAlertThreshold = 3.0

	// Silence: an SV unseen this long is lost; an observer unseen this long is
	// offline (shorter, operator-actionable).
	SilentThreshold          = 3600.0
	ObserverOfflineThreshold = 300.0

	// FreshReceiver bounds how recently a receiver must have seen an SV for its
	// vote to count (docs/INTEGRITY.md §2/§6).
	FreshReceiverThreshold = 60.0
)

// DebounceDuration is how long a provisional state change must persist before it is
// confirmed and emitted — the flap filter (docs/INTEGRITY.md §4).
const DebounceDuration = 60 * time.Second

// Severity codes (the SSE / gnss_events contract, docs/OUTPUT.md §2.2).
const (
	SevInfo     = 0
	SevWarning  = 1
	SevCritical = 2
)

// ephAgeThreshold returns the constellation's ephemeris-age alert threshold in
// minutes (docs/INTEGRITY.md §2): Galileo is stricter, everything else uses the GPS
// default.
func ephAgeThreshold(gnssID int) float64 {
	if gnssID == 2 { // Galileo
		return EphAgeThresholdGalileo
	}
	return EphAgeThresholdGPS
}
