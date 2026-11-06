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
	d.detectEnvironment(id, a, emit)
	d.detectPressure(id, a, emit)
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

func (d *Detector) detectPressure(id string, a integrity.Assessment, emit emitFunc) {
	if regional, ok := assessmentCheck(a, integrity.CheckRegionalPressure); ok && regional.EvaluatedAt != 0 {
		band, severity := "coherent", SevInfo
		switch regional.State {
		case integrity.Inconsistent:
			band, severity = "local_change", SevWarning
		case integrity.Unassured:
			band, severity = "severe", SevWarning
		case integrity.Unavailable:
			band = "unknown"
			if containsReason(regional.Reasons, integrity.ReasonRegionalPressureIncoherent) ||
				containsReason(regional.Reasons, integrity.ReasonPressureSensorUnhealthy) ||
				containsReason(regional.Reasons, integrity.ReasonStale) {
				severity = SevWarning
			}
		}
		emit(id, "regional_pressure", band, func(old string) Event {
			return Event{
				Type: "regional_pressure_anomaly", OldValue: old, NewValue: band, Severity: severity,
				Message: fmt.Sprintf("station %s regional pressure comparison %s", id, band),
				Params:  assessmentParams(id, a),
			}
		})
	}
	if barometric, ok := assessmentCheck(a, integrity.CheckBarometricHeight); ok && barometric.EvaluatedAt != 0 {
		band, severity := "consistent", SevInfo
		switch barometric.State {
		case integrity.Inconsistent:
			band, severity = "inconsistent", SevWarning
		case integrity.Unassured:
			band, severity = "diverged", SevWarning
		case integrity.Unavailable:
			band = "unknown"
			if containsReason(barometric.Reasons, integrity.ReasonLocalPressureAmbiguous) ||
				containsReason(barometric.Reasons, integrity.ReasonPressureSensorUnhealthy) ||
				containsReason(barometric.Reasons, integrity.ReasonRegionalPressureIncoherent) ||
				containsReason(barometric.Reasons, integrity.ReasonStale) {
				severity = SevWarning
			}
		}
		emit(id, "barometric_height", band, func(old string) Event {
			return Event{
				Type: "barometric_height_consistency", OldValue: old, NewValue: band, Severity: severity,
				Message: fmt.Sprintf("station %s barometric height comparison %s", id, band),
				Params:  assessmentParams(id, a),
			}
		})
	}
}

func (d *Detector) detectEnvironment(id string, a integrity.Assessment, emit emitFunc) {
	if health, ok := assessmentCheck(a, integrity.CheckEnvironmentHealth); ok && health.EvaluatedAt != 0 {
		band, severity := "ok", SevInfo
		switch health.State {
		case integrity.Inconsistent:
			band, severity = "degraded", SevWarning
		case integrity.Unassured:
			band, severity = "fault", SevWarning
		case integrity.Unavailable:
			band = "unknown"
			severity = SevWarning
			if containsReason(health.Reasons, integrity.ReasonStale) {
				band = "stale"
			}
		}
		emit(id, "environment_health", band, func(old string) Event {
			return Event{
				Type: "environment_sensor_health", OldValue: old, NewValue: band, Severity: severity,
				Message: fmt.Sprintf("station %s environmental sensor health %s", id, band),
				Params:  assessmentParams(id, a),
			}
		})
	}
	if change, ok := assessmentCheck(a, integrity.CheckEnvironmentChange); ok && change.EvaluatedAt != 0 {
		band, severity := "normal", SevInfo
		switch change.State {
		case integrity.Inconsistent:
			band, severity = "changed", SevWarning
		case integrity.Unassured:
			band, severity = "severe", SevWarning
		case integrity.Unavailable:
			band = "unknown"
			if containsReason(change.Reasons, integrity.ReasonStale) {
				severity = SevWarning
			}
		}
		emit(id, "environment_change", band, func(old string) Event {
			return Event{
				Type: "environmental_change", OldValue: old, NewValue: band, Severity: severity,
				Message: fmt.Sprintf("station %s environmental conditions %s", id, band),
				Params:  assessmentParams(id, a),
			}
		})
	}
}

func assessmentCheck(a integrity.Assessment, name string) (integrity.Result, bool) {
	for _, result := range a.Checks {
		if result.Check == name {
			return result, true
		}
	}
	return integrity.Result{}, false
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
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
// retain their reasons and any last metrics as well as the compatibility name list;
// unknown must remain explainable rather than looking like an empty normal result.
func assessmentParams(id string, a integrity.Assessment) map[string]any {
	checks := make([]any, 0, len(a.Checks))
	var unavailable []any
	for _, r := range a.Checks {
		if r.State == integrity.Unavailable {
			unavailable = append(unavailable, r.Check)
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
