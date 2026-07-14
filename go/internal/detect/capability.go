package detect

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/state"
)

// TickCapabilities advances the capability-plausibility classifiers (docs/INTEGRITY.md §6,
// CONSTELLATIONS §7) over the same debounced state machines as the SV/RF metrics, so a
// capability event shares the confirmation discipline and event contract. The daemon calls it
// alongside Tick/TickStations on the detector cadence. reports is the live read model
// (state.FeedCapabilityReports) — observed signal recency plus each node's declared tudorgps
// fingerprint.
func (d *Detector) TickCapabilities(now time.Time, reports map[string]state.StationCapReport) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run(now, func(emit emitFunc) {
		for id, rep := range reports {
			d.detectCapability(id, rep, now, emit)
		}
	})
}

// detectCapability classifies one station's capability state:
//
//   - capability_signal_lost — a signal the node has *demonstrated* (produced ≥
//     CapMinObservations nav frames on) has gone silent past CapSignalLostAfter while the node
//     is otherwise alive (it produced some frame within CapStationAliveWindow). That rules out
//     a whole-receiver outage — this is a targeted loss on one signal, the jamming/spoofing/
//     fault signature. Per-signal, so each signal debounces independently.
//   - capability_impossible — the node produced a signal that is NOT in its declared tudorgps
//     capability set: it reported something its silicon cannot natively receive, a strong
//     spoofing or misconfiguration signal. Only checked when a declared set exists.
func (d *Detector) detectCapability(id string, rep state.StationCapReport, now time.Time, emit emitFunc) {
	stationAlive := rep.StationLastSeen != 0 &&
		now.Unix()-rep.StationLastSeen <= int64(CapStationAliveWindow.Seconds())

	// Per-signal loss of a demonstrated capability.
	for _, sig := range rep.Observed {
		// when the whole station is dark, a signal is neither demonstrably "lost"
		// (that classification is a TARGETED per-signal loss while the node is otherwise
		// alive) nor "present" (nothing is present). Hold the machine instead of emitting a
		// false lost→present "recovery" SevWarning mid-outage — with station_offline
		// detection skipped, that lie would be the only event an operator sees.
		if !stationAlive {
			continue
		}
		lost := sig.Count >= CapMinObservations &&
			now.Unix()-sig.LastSeen > int64(CapSignalLostAfter.Seconds())
		metric := fmt.Sprintf("cap_lost:%d:%d", sig.Gnss, sig.Sig)
		gnss, s := sig.Gnss, sig.Sig
		staleS := now.Unix() - sig.LastSeen
		emit(id, metric, boolState(lost, "lost", "present"), func(old string) Event {
			message := fmt.Sprintf("station %s resumed delivering signal %d:%d", id, gnss, s)
			if lost {
				message = fmt.Sprintf("station %s stopped delivering signal %d:%d (unseen %ds, station live)", id, gnss, s, staleS)
			}
			return Event{
				Type: "capability_signal_lost", OldValue: old, NewValue: boolState(lost, "lost", "present"),
				Severity: SevWarning,
				Message:  message,
				Params:   map[string]any{"station": id, "gnss": gnss, "sig": s, "unseen_s": staleS},
			}
		})
	}

	// Impossible signal: observed but not declared (only when a declared set is configured).
	if len(rep.Declared) == 0 {
		return
	}
	declared := make(map[state.CapSignal]bool, len(rep.Declared))
	for _, c := range rep.Declared {
		declared[c] = true
	}
	var offending []string
	for _, sig := range rep.Observed {
		if !declared[state.CapSignal{Gnss: sig.Gnss, Sig: sig.Sig}] {
			offending = append(offending, fmt.Sprintf("%d:%d", sig.Gnss, sig.Sig))
		}
	}
	sort.Strings(offending)
	impossible := len(offending) > 0
	emit(id, "cap_impossible", boolState(impossible, "impossible", "ok"), func(old string) Event {
		message := fmt.Sprintf("station %s no longer reporting undeclared signals", id)
		if impossible {
			message = fmt.Sprintf("station %s reported signals its silicon cannot produce: %s", id, strings.Join(offending, ", "))
		}
		return Event{
			Type: "capability_impossible", OldValue: old, NewValue: boolState(impossible, "impossible", "ok"),
			Severity: SevCritical,
			Message:  message,
			Params:   map[string]any{"station": id, "signals": offending},
		}
	})
}
