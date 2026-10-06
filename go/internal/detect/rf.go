package detect

import (
	"fmt"

	"github.com/ptudor/navlistener/internal/state"
)

// detectStationRF runs the station-scoped RF classifiers over one observer's RF read model
// (docs/DEFENSE-PNT.md §4): jamming_detected, antenna_fault and station_rf_degraded. The
// metrics are computed in internal/state (AGC departures from the learned baseline); this
// only classifies and debounces. A lone metric departure is reported as a degradation, not
// an attack. spoofing_suspected fuses evidence domains instead (integrity.go). Thresholds
// are conservative/observational in v1 (docs/DEFENSE-PNT.md §4).
func (d *Detector) detectStationRF(id string, rf state.StationRF, emit emitFunc) {
	// Aggregate the per-band signals: the worst AGC departure, any CW spike, any
	// antenna fault, and whether the receiver itself flags jamming.
	var maxDep float64
	haveDep := false
	cwHigh, antFault, rxJam := false, false, false
	for _, b := range rf.Bands {
		if b.AGCDeparture != nil {
			haveDep = true
			if *b.AGCDeparture > maxDep {
				maxDep = *b.AGCDeparture
			}
		}
		if b.CWSuppress >= CWSuppressThreshold {
			cwHigh = true
		}
		if b.AntStatus == 3 || b.AntStatus == 4 { // short / open
			antFault = true
		}
		if b.JamState >= 2 { // receiver's own warning/critical jam flag
			rxJam = true
		}
	}

	// jamming_detected: a broadband AGC collapse is unambiguous on its own; a lesser
	// departure must be corroborated by CW, the receiver's own jam flag, or a
	// simultaneous C/N₀ drop across the station's signals before it is called an
	// attack (docs/DEFENSE-PNT.md §2). Otherwise it is a degradation, below.
	cn0Drop := rf.Cn0Drop
	jamBand, jamSev := "ok", SevInfo
	switch {
	case maxDep >= AGCDepartureSevereThreshold:
		jamBand, jamSev = "crit", SevCritical
	case maxDep >= AGCDepartureThreshold && (cwHigh || rxJam || cn0Drop):
		jamBand, jamSev = "warn", SevWarning
	}
	// no current MON-RF band measurement means unknown, not measured-ok.
	// Hold the band-derived machines in their last state until evidence resumes.
	if len(rf.Bands) > 0 {
		emit(id, "jamming", jamBand, func(old string) Event {
			return Event{
				Type: "jamming_detected", OldValue: old, NewValue: jamBand, Severity: jamSev,
				Message: fmt.Sprintf("station %s jamming %s (AGC departure %.0f)", id, jamBand, maxDep),
				Params:  map[string]any{"station": id, "agc_departure": maxDep, "cw": cwHigh, "rx_jam": rxJam, "cn0_drop": cn0Drop},
			}
		})
	}

	// antenna_fault: the receiver's antenna status open/short (a C/N₀ collapse with no
	// jamming signature is a future antenna gate — docs/DEFENSE-PNT.md §4).
	antState, antSev := "ok", SevInfo
	if antFault {
		antState, antSev = "fault", SevWarning
	}
	if len(rf.Bands) > 0 {
		emit(id, "antenna", antState, func(old string) Event {
			return Event{
				Type: "antenna_fault", OldValue: old, NewValue: antState, Severity: antSev,
				Message: fmt.Sprintf("station %s antenna %s", id, antState),
				Params:  map[string]any{"station": id},
			}
		})
	}

	// station_rf_degraded: a single RF metric departs baseline but was not corroborated
	// into a jamming claim — a fault-or-early-warning, not an attack assertion. A
	// simultaneous C/N₀ drop alone (an obstructed or failing antenna can cause one)
	// surfaces here too.
	// the receiver's own jam flag (jamState >= 2) alone surfaces here too, per
	// DEFENSE-PNT.md §2's single-metric rule — CW-alone and AGC-alone already do; jamInd-alone
	// previously surfaced as nothing. The jamming-ATTACK claim (jamBand) keeps its AGC-departure
	// + CW/jam corroboration requirement unchanged.
	depPresent := (haveDep && maxDep >= AGCDepartureThreshold) || cwHigh || rxJam || cn0Drop
	degraded := depPresent && jamBand == "ok"
	degradedSev := SevInfo
	if degraded {
		degradedSev = SevWarning
	}
	if len(rf.Bands) > 0 {
		emit(id, "rf_degraded", boolState(degraded, "degraded", "ok"), func(old string) Event {
			return Event{
				Type: "station_rf_degraded", OldValue: old, NewValue: boolState(degraded, "degraded", "ok"),
				Severity: degradedSev,
				Message:  fmt.Sprintf("station %s RF degraded (AGC departure %.0f, cw=%v, rx_jam=%v, cn0_drop=%v)", id, maxDep, cwHigh, rxJam, cn0Drop),
				Params:   map[string]any{"station": id, "agc_departure": maxDep, "cw": cwHigh, "rx_jam": rxJam, "cn0_drop": cn0Drop},
			}
		})
	}
}

// detectStationOffline classifies one observer's liveness (regression fix, wiring the
// station_offline event regression fix defined but never emitted): a station unseen —
// no decoded nav frame AND no RF telemetry — past ObserverOfflineThreshold is
// offline. lastSeenS comes from the unfiltered caps ∪ rf union
// (state.StationLastSeen); the capability half of that union is never evicted,
// so the machine can observe (and hold) the offline state indefinitely rather
// than freezing when the rf entry is evicted. Severity is warning in
// both directions (the regression fix direction-blind-severity disposition shared by
// every silence-family classifier; INTEGRITY §5's crit escalation tier is
// reserved until a second threshold is defined). First sight of an
// already-offline station seeds silently — no phantom event at daemon start.
func (d *Detector) detectStationOffline(id string, lastSeenS int, emit emitFunc) {
	offline := float64(lastSeenS) > ObserverOfflineThreshold
	emit(id, "offline", boolState(offline, "offline", "online"), func(old string) Event {
		return Event{
			Type: "station_offline", OldValue: old, NewValue: boolState(offline, "offline", "online"),
			Severity: SevWarning,
			Message:  fmt.Sprintf("station %s %s (unseen %ds)", id, boolState(offline, "offline", "online"), lastSeenS),
			Params:   map[string]any{"station": id, "last_seen_s": lastSeenS},
		}
	})
}
