package detect

import (
	"fmt"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/integrity"
)

// TickIntegrity advances the station integrity classifiers
// (docs/proposals/STATION-ASSURANCE.md) over each station's assessment:
//
//   - spoofing_suspected confirms when the evidence domains indicate spoofing:
//     SpoofGateQuorum physics domains unassured together, or one with the
//     receiver's own spoofing indication (integrity.Fuse).
//   - station_assurance confirms changes of the fused station state.
//
// Every event carries the full evidence: the fused state and score, the unassured
// domains, each available check's state, values, thresholds and version, and the
// configuration hash, so a client can say why without the raw inputs. A station
// whose physics checks are all unavailable holds its spoofing machine, and an
// unavailable fused state holds the assurance machine: a lack of input is never a
// recovery.
func (d *Detector) TickIntegrity(now time.Time, stations map[string]integrity.Assessment) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run(now, familyStationIntegrity, func(emit emitFunc) {
		for id, a := range stations {
			d.detectIntegrity(id, a, emit)
		}
	})
}

func (d *Detector) detectIntegrity(id string, a integrity.Assessment, emit emitFunc) {
	if physicsAvailable(a) {
		band, sev := "ok", SevInfo
		if a.SpoofingIndicated {
			band, sev = "suspected", SevCritical
		}
		emit(id, "spoofing", band, func(old string) Event {
			return Event{
				Type: "spoofing_suspected", OldValue: old, NewValue: band, Severity: sev,
				Message: fmt.Sprintf("station %s spoofing %s (unassured: %s)", id, band, domainList(a.UnassuredDomains)),
				Params:  assessmentParams(id, a),
			}
		})
	}
	if a.State != integrity.Unavailable {
		state := string(a.State)
		sev := SevInfo
		switch a.State {
		case integrity.Unassured:
			sev = SevCritical
		case integrity.Inconsistent:
			sev = SevWarning
		}
		emit(id, "assurance", state, func(old string) Event {
			return Event{
				Type: "station_assurance", OldValue: old, NewValue: state, Severity: sev,
				Message: fmt.Sprintf("station %s assurance %s (unassured: %s)", id, state, domainList(a.UnassuredDomains)),
				Params:  assessmentParams(id, a),
			}
		})
	}
}

// physicsAvailable reports whether any physics-domain check has an available state,
// the minimum for a spoofing verdict in either direction.
func physicsAvailable(a integrity.Assessment) bool {
	for _, r := range a.Checks {
		if r.Domain.Physics() && r.State != integrity.Unavailable {
			return true
		}
	}
	return false
}

func domainList(domains []integrity.Domain) string {
	if len(domains) == 0 {
		return "none"
	}
	names := make([]string, len(domains))
	for i, d := range domains {
		names[i] = string(d)
	}
	return strings.Join(names, ", ")
}

// assessmentParams flattens an assessment into event parameters: plain maps and
// slices, so the event pipeline's sanitizer can walk every number. Unavailable checks
// are named, not detailed.
func assessmentParams(id string, a integrity.Assessment) map[string]any {
	checks := make([]any, 0, len(a.Checks))
	var unavailable []any
	for _, r := range a.Checks {
		if r.State == integrity.Unavailable {
			unavailable = append(unavailable, r.Check)
			continue
		}
		check := map[string]any{
			"check": r.Check, "version": r.Version, "domain": string(r.Domain),
			"state": string(r.State), "candidate": string(r.Candidate), "since": r.Since,
			"metrics": floatMap(r.Metrics), "thresholds": floatMap(r.Thresholds),
		}
		if len(r.Reasons) > 0 {
			reasons := make([]any, len(r.Reasons))
			for i, reason := range r.Reasons {
				reasons[i] = reason
			}
			check["reasons"] = reasons
		}
		checks = append(checks, check)
	}
	domains := make([]any, len(a.UnassuredDomains))
	for i, d := range a.UnassuredDomains {
		domains[i] = string(d)
	}
	params := map[string]any{
		"station": id, "state": string(a.State), "unassured_domains": domains,
		"spoofing_indicated": a.SpoofingIndicated, "engine_version": a.Engine, "config_hash": a.ConfigHash,
		"mode": string(a.Mode), "surveyed_position": a.Surveyed, "checks": checks,
	}
	if a.Score != nil {
		params["score"] = *a.Score
	}
	if len(unavailable) > 0 {
		params["unavailable_checks"] = unavailable
	}
	return params
}

func floatMap(m map[string]float64) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
