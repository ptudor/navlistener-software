package detect

import (
	"fmt"

	"github.com/ptudor/navlistener/internal/state"
)

// detectStationRF runs the four station-scoped PNT-defense classifiers over one observer's
// RF read model (docs/DEFENSE-PNT.md §4). The metrics are computed in internal/state (AGC
// departures from the learned baseline, the C/N₀-vs-elevation residual); this only
// classifies and debounces. The design rule applies: no single gate fires a spoofing alarm
// (a ≥2-gate fusion), and a lone metric departure is reported as a degradation, not an
// attack. Thresholds are conservative/observational in v1 (docs/DEFENSE-PNT.md §4).
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
	// departure must be corroborated by CW or the receiver's own jam flag before it is
	// called an attack (docs/DEFENSE-PNT.md §2). Otherwise it is a degradation, below.
	jamBand, jamSev := "ok", SevWarning
	switch {
	case maxDep >= AGCDepartureSevereThreshold:
		jamBand, jamSev = "crit", SevCritical
	case maxDep >= AGCDepartureThreshold && (cwHigh || rxJam):
		jamBand = "warn"
	}
	emit(id, "jamming", jamBand, func(old string) Event {
		return Event{
			Type: "jamming_detected", OldValue: old, NewValue: jamBand, Severity: jamSev,
			Message: fmt.Sprintf("station %s jamming %s (AGC departure %.0f)", id, jamBand, maxDep),
			Params:  map[string]any{"station": id, "agc_departure": maxDep, "cw": cwHigh, "rx_jam": rxJam},
		}
	})

	// spoofing_suspected: count the independent physics gates that agree at this station
	// and require a quorum (docs/DEFENSE-PNT.md §3 fusion). v1 wires the C/N₀-vs-elevation
	// gate; the others (coherent delta-Hz, clock transient, cross-constellation,
	// measured-iono) plug in here as they are computed, raising the gate count.
	gates := spoofGates(rf)
	spoofBand := "ok"
	if gates >= SpoofGateQuorum {
		spoofBand = "suspected"
	}
	emit(id, "spoofing", spoofBand, func(old string) Event {
		return Event{
			Type: "spoofing_suspected", OldValue: old, NewValue: spoofBand, Severity: SevCritical,
			Message: fmt.Sprintf("station %s spoofing suspected (%d gates)", id, gates),
			Params:  map[string]any{"station": id, "gates": gates},
		}
	})

	// antenna_fault: the receiver's antenna status open/short (a C/N₀ collapse with no
	// jamming signature is a future antenna gate — docs/DEFENSE-PNT.md §4).
	antState := "ok"
	if antFault {
		antState = "fault"
	}
	emit(id, "antenna", antState, func(old string) Event {
		return Event{
			Type: "antenna_fault", OldValue: old, NewValue: antState, Severity: SevWarning,
			Message: fmt.Sprintf("station %s antenna %s", id, antState),
			Params:  map[string]any{"station": id},
		}
	})

	// station_rf_degraded: a single RF metric departs baseline but was not corroborated
	// into a jamming or spoofing claim — a fault-or-early-warning, not an attack assertion.
	// the receiver's own jam flag (jamState >= 2) alone surfaces here too, per
	// DEFENSE-PNT.md §2's single-metric rule — CW-alone and AGC-alone already do; jamInd-alone
	// previously surfaced as nothing. The jamming-ATTACK claim (jamBand) keeps its AGC-departure
	// + CW/jam corroboration requirement unchanged.
	depPresent := (haveDep && maxDep >= AGCDepartureThreshold) || cwHigh || rxJam
	degraded := depPresent && jamBand == "ok" && spoofBand == "ok"
	emit(id, "rf_degraded", boolState(degraded, "degraded", "ok"), func(old string) Event {
		return Event{
			Type: "station_rf_degraded", OldValue: old, NewValue: boolState(degraded, "degraded", "ok"),
			Severity: SevWarning,
			Message:  fmt.Sprintf("station %s RF degraded (AGC departure %.0f, cw=%v, rx_jam=%v)", id, maxDep, cwHigh, rxJam),
			Params:   map[string]any{"station": id, "agc_departure": maxDep, "cw": cwHigh, "rx_jam": rxJam},
		}
	})
}

// spoofGates counts the independent spoofing physics gates currently tripped at a station
// (docs/DEFENSE-PNT.md §3). v1: the C/N₀-vs-elevation gate — a residual variance that has
// collapsed at an unnaturally high, uniform C/N₀ (the single-transmitter signature).
func spoofGates(rf state.StationRF) int {
	gates := 0
	if rf.Cn0Resid != nil && rf.Cn0Mean != nil &&
		*rf.Cn0Resid < Cn0SpoofResidVar && *rf.Cn0Mean > Cn0SpoofMean {
		gates++
	}
	return gates
}
