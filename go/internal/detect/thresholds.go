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

	// PNT-defense Tier-0 operating points (docs/DEFENSE-PNT.md §2/§3/§4). These are
	// deliberately CONSERVATIVE and OBSERVATIONAL in v1: the design mandates learning
	// per-station quiet-time distributions before freezing alert thresholds, so these
	// gate only gross, unambiguous departures and the thresholds are expected to move
	// once real baselines exist (docs/DEFENSE-PNT.md §4 — this file is their single home).

	// AGCDepartureThreshold (AGC counts below the learned baseline) at which a band is
	// considered jamming-suspect; the severe band is a near-total gain collapse.
	AGCDepartureThreshold       = 800.0
	AGCDepartureSevereThreshold = 2000.0

	// CWSuppressThreshold: a CW-suppression / jamming indicator above this points at a
	// narrowband tone (u-blox reports 0..255).
	CWSuppressThreshold = 180

	// Cn0SpoofResidVar / Cn0SpoofMean: the C/N₀-vs-elevation gate fires when the residual
	// variance collapses (an unnaturally flat sky) AT an unnaturally high, uniform C/N₀ —
	// the single-transmitter spoofer signature (docs/DEFENSE-PNT.md §3).
	Cn0SpoofResidVar = 2.0
	Cn0SpoofMean     = 45.0

	// SpoofGateQuorum is the fusion rule: a spoofing_suspected event needs at least this
	// many independent physics gates agreeing at one station (docs/DEFENSE-PNT.md §3). In
	// v1 few gates are wired, so this keeps spoofing alerts corroborated, not trigger-happy.
	SpoofGateQuorum = 2

	// Capability plausibility (docs/INTEGRITY.md §6, CONSTELLATIONS §7). A signal a node has
	// *demonstrated* it can track (produced this many nav frames on) and then stops delivering,
	// while the node is otherwise alive, is a targeted loss (jamming/spoofing/fault) — not the
	// whole receiver going quiet, which the observation/offline detectors already cover.
	CapMinObservations    = 10               // nav frames before a signal counts as demonstrated
	CapSignalLostAfter    = 15 * time.Minute // a demonstrated signal unseen this long is "lost"
	CapStationAliveWindow = 5 * time.Minute  // the station must have produced a frame this recently
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
