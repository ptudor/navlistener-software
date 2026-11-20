package detect

import (
	"fmt"

	"github.com/ptudor/navlistener/internal/state"
)

// detectStationRF runs the station-scoped RF classifiers over one observer's RF read model
// (docs/DEFENSE-PNT.md §4): jamming_detected, antenna_fault and station_rf_degraded. The
// metrics are computed in internal/state (AGC and front-end gain departures from the learned
// baselines); this only classifies and debounces. A lone metric departure is reported as a
// degradation, not an attack. spoofing_suspected fuses evidence domains instead (integrity.go).
// Thresholds are conservative/observational in v1 (docs/DEFENSE-PNT.md §4).
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
	// A Septentrio receiver's front ends: the largest gain cut and the largest gain
	// rise against the learned baselines, in dB, and the front end showing each. A
	// cut is the same evidence as a band's AGC departure, on its own thresholds; a
	// rise is less power arriving, which only ever degrades.
	gain := frontEndGain(rf.FrontEnds)

	// jamming_detected: a broadband AGC collapse is unambiguous on its own; a lesser
	// departure must be corroborated by CW, the receiver's own jam flag, a
	// simultaneous C/N₀ drop across the station's signals, or interference at a
	// neighbouring station at the same time before it is called an attack
	// (docs/DEFENSE-PNT.md §2). Otherwise it is a degradation, below. The read
	// model names only the nearest few corroborating neighbours and carries the
	// total separately, so the event payload stays bounded whatever the fleet size.
	cn0Drop := rf.Cn0Drop
	neighbours := make([]any, len(rf.Neighbours))
	for i, n := range rf.Neighbours {
		neighbours[i] = n
	}
	neighbourCount := max(rf.NeighbourCount, len(rf.Neighbours))
	departed := maxDep >= AGCDepartureThreshold || gain.cut >= GainDepartureThreshold
	jamBand, jamSev := "ok", SevInfo
	switch {
	case maxDep >= AGCDepartureSevereThreshold || gain.cut >= GainDepartureSevereThreshold:
		jamBand, jamSev = "crit", SevCritical
	case departed && (cwHigh || rxJam || cn0Drop || neighbourCount > 0):
		jamBand, jamSev = "warn", SevWarning
	}
	// no current MON-RF band or front-end measurement means unknown, not measured-ok.
	// Hold the band-derived machines in their last state until evidence resumes.
	measured := len(rf.Bands) > 0 || len(rf.FrontEnds) > 0
	if measured {
		emit(id, "jamming", jamBand, func(old string) Event {
			params := map[string]any{"station": id, "agc_departure": maxDep, "cw": cwHigh, "rx_jam": rxJam, "cn0_drop": cn0Drop,
				"neighbours": neighbours, "neighbour_count": neighbourCount}
			gain.addParams(params)
			return Event{
				Type: "jamming_detected", OldValue: old, NewValue: jamBand, Severity: jamSev,
				Message: fmt.Sprintf("station %s jamming %s (%s)", id, jamBand, departureText(maxDep, gain)),
				Params:  params,
			}
		})
	}

	// antenna_fault: the receiver's antenna status open/short (a C/N₀ collapse with no
	// jamming signature is a future antenna gate — docs/DEFENSE-PNT.md §4). Only a
	// MON-RF band carries that status; a front end's risen gain is a degradation, below.
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
	// + CW/jam corroboration requirement unchanged. A front end whose gain has risen by the
	// gain departure surfaces here and nowhere else: the antenna, its amplifier or its cable
	// is giving less, which is a fault to look at and never an interference claim.
	depPresent := (haveDep && maxDep >= AGCDepartureThreshold) || gain.cut >= GainDepartureThreshold ||
		gain.rise >= GainDepartureThreshold || cwHigh || rxJam || cn0Drop
	degraded := depPresent && jamBand == "ok"
	degradedSev := SevInfo
	if degraded {
		degradedSev = SevWarning
	}
	if measured {
		emit(id, "rf_degraded", boolState(degraded, "degraded", "ok"), func(old string) Event {
			params := map[string]any{"station": id, "agc_departure": maxDep, "cw": cwHigh, "rx_jam": rxJam, "cn0_drop": cn0Drop}
			gain.addParams(params)
			return Event{
				Type: "station_rf_degraded", OldValue: old, NewValue: boolState(degraded, "degraded", "ok"),
				Severity: degradedSev,
				Message:  fmt.Sprintf("station %s RF degraded (%s, cw=%v, rx_jam=%v, cn0_drop=%v)", id, departureText(maxDep, gain), cwHigh, rxJam, cn0Drop),
				Params:   params,
			}
		})
	}
}

// frontEndGains is a Septentrio station's gain evidence: the largest cut and the largest
// rise against the learned baselines among its front ends, in dB, with the front end that
// shows each. Both are zero, and the names empty, when no front end has moved that way.
type frontEndGains struct {
	reported          bool // the station has front ends at all
	cut, rise         float64
	cutName, riseName string
}

func frontEndGain(ends []state.StationFrontEnd) frontEndGains {
	g := frontEndGains{reported: len(ends) > 0}
	for _, e := range ends {
		if e.GainDeparture == nil {
			continue
		}
		if dep := *e.GainDeparture; dep > g.cut {
			g.cut, g.cutName = dep, e.Name
		} else if -dep > g.rise {
			g.rise, g.riseName = -dep, e.Name
		}
	}
	return g
}

// addParams adds the gain evidence to a station RF event's parameters. A u-blox station
// has none, and its events keep the parameters they always had.
func (g frontEndGains) addParams(params map[string]any) {
	if !g.reported {
		return
	}
	params["gain_cut_db"], params["gain_rise_db"] = g.cut, g.rise
	if g.cutName != "" {
		params["gain_cut_front_end"] = g.cutName
	}
	if g.riseName != "" {
		params["gain_rise_front_end"] = g.riseName
	}
}

// departureText words the departure a station RF event reports: the AGC departure in
// counts for a u-blox station, and the front ends' gain cut and rise in dB for a
// Septentrio one.
func departureText(agcDeparture float64, g frontEndGains) string {
	if !g.reported {
		return fmt.Sprintf("AGC departure %.0f", agcDeparture)
	}
	text := fmt.Sprintf("gain cut %.1f dB", g.cut)
	if g.cutName != "" {
		text += " on " + g.cutName
	}
	text += fmt.Sprintf(", gain rise %.1f dB", g.rise)
	if g.riseName != "" {
		text += " on " + g.riseName
	}
	return text
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
