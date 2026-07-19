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

	// SISA/URA accuracy degradation crossing (metres): enters degraded at
	// SISAAlertThreshold, clears back to ok only below SISAExitThreshold. 	// URA/SISA are quantized (e.g. GPS URA steps 2.83 m / 4.0 m straddle 3.0 m),
	// so real SVs dwell on both sides of a plain threshold for minutes at a time
	// -- longer than the debounce window, so the debounce alone cannot suppress
	// the repeated ok/degraded pairs. The asymmetric exit band is the real flap
	// filter for this metric; SISAAlertThreshold itself must not move.
	SISAAlertThreshold = 3.0
	SISAExitThreshold  = 2.5

	// Silence: an SV unseen this long is lost; an observer unseen this long is
	// offline (shorter, operator-actionable).
	SilentThreshold          = 3600.0
	ObserverOfflineThreshold = 300.0

	// SilenceMinReceivers  is the fleet-footprint floor below which the
	// per-SV silence classifier does not run at all. observation_lost's premise is
	// "the constellation is always in view" (docs/INTEGRITY.md §2) — true of the
	// constellation as seen by a globally distributed fleet, NOT of any single
	// station's sky: from one station every MEO/IGSO SV legitimately sets below
	// the horizon once per orbital pass, and with sv_ttl (2 h) > SilentThreshold
	// (1 h) each pass confirmed a false observation_lost/recovery warning pair —
	// dozens per day of orbital-mechanics noise that trains operators to ignore
	// the events channel. Until per-station geometry lands (the observer-geometry
	// pass: propagated elevation > mask from ≥1 live station — the real gate),
	// the live-receiver count is the only footprint signal available, so silence
	// classification is suppressed entirely below this floor. 4 is a conservative
	// NECESSARY-not-sufficient proxy: fewer than 4 stations cannot plausibly hold
	// a MEO constellation in continuous view however they are placed, while 4+
	// only *may* (they could all share one footprint) — operators with a
	// concentrated 4+ fleet should expect residual rise/set noise until the
	// geometry gate replaces this. Below the floor, constellation-outage coverage
	// comes from the visibility-independent detectors instead: eph_aged,
	// position_unknown, and capability_signal_lost (a demonstrated signal gone
	// dark station-wide). Documented as the standard in docs/INTEGRITY.md §2.
	SilenceMinReceivers = 4

	// FreshReceiver bounds how recently a receiver must have seen an SV for its
	// vote to count (docs/INTEGRITY.md §2/§6).
	FreshReceiverThreshold = 60.0

	// SBASSilentThreshold : an SBAS PRN unseen this long is lost. SBAS
	// GEOs need no visibility gate — geostationary, broadcasting ~1 message/s
	// continuously — so silence is unambiguous where the MEO/IGSO threshold
	// above needs the regression fix fleet floor. 300 s mirrors state.sbasStaleAfter
	// (the boundary where the served sbas feed already drops the PRN as stale,
	// regression fix), so the event confirms one debounce after the entry leaves the
	// feed — the event channel and the feed tell the same story. Duplicated
	// rather than imported because detect depends on state, not the reverse
	// (the liveReceiverWindow precedent); keep the two in sync if either moves.
	// This is an operational bound derived from the in-repo staleness standard,
	// NOT a DO-229 message-timeout value — RTCA DO-229 is paywalled/unvendored
	// (reference/REFERENCES.md "RTCA-DO-229"); if it is ever acquired, re-derive
	// this from its timeout table (the regression fix flag).
	SBASSilentThreshold = 300.0

	// SBASHealthCurrentWindow  bounds how stale an SBAS entry may be
	// before its health_code stops being classified. The served health is the
	// regression fix MT0-recency latch, whose horizon is state.sbasType0Hold (60 s,
	// the DO-229-family exclusion interval — mirrored here, keep in sync): once
	// no message of ANY type has arrived for longer than that, the latch has
	// decayed for lack of input, not because the provider cleared it, and
	// classifying the resulting code-1 would fire a fabricated
	// do_not_use → ok "recovery" on a test-mode GEO that simply went dark
	// (~2 min into the outage, well before SBASSilentThreshold fires the real
	// sbas_lost). Beyond this window the health machine holds (regression fix rule).
	SBASHealthCurrentWindow = 60.0

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
