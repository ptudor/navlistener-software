package detect

import (
	"fmt"
	"time"
)

// OrbitReferenceComparison is one live broadcast position compared at the
// identical epoch with an independently downloaded broadcast orbit. The
// daemon computes the geometry; detect owns only classification and dwell.
type OrbitReferenceComparison struct {
	Name            string
	GNSSID          int
	SigID           int
	DeltaM          float64
	Conf            int
	ConfWeighted    float64
	PositionAt      time.Time
	ReferenceEpoch  time.Time
	ReferenceSource string
	PositionSource  string
}

// TickOrbitReference advances the independent-reference classifier. A missing
// comparison means unavailable evidence and therefore holds the machine: a
// delayed download, an absent reference SV, or an unpropagatable orbit can
// neither raise nor clear an alarm.
func (d *Detector) TickOrbitReference(now time.Time, comparisons map[string]OrbitReferenceComparison) []Event {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.run(now, familyOrbitReference, func(emit emitFunc) {
		for subject, comparison := range comparisons {
			metric, eventType := "reference_orbit", "reference_orbit_divergence"
			threshold, severeThreshold := ReferenceOrbitThreshold, ReferenceOrbitSevereThreshold
			positionSource := "ephemeris"
			switch comparison.PositionSource {
			case "", "ephemeris":
			case "almanac":
				positionSource = "almanac"
				metric, eventType = "reference_almanac", "reference_almanac_divergence"
				threshold, severeThreshold = ReferenceAlmanacThreshold, ReferenceAlmanacSevereThreshold
			default:
				continue
			}
			band, sev := discoBand(comparison.DeltaM, threshold, severeThreshold)
			emit(subject, metric, band, func(old string) Event {
				params := map[string]any{
					"sv": comparison.Name, "gnssid": comparison.GNSSID,
					"reference_orbit_delta_m": comparison.DeltaM,
					"position_epoch":          comparison.PositionAt,
					"position_source":         positionSource,
					"reference_orbit_epoch":   comparison.ReferenceEpoch,
					"reference_source":        comparison.ReferenceSource,
				}
				if positionSource == "ephemeris" {
					params["sigid"] = comparison.SigID
					params["conf"] = comparison.Conf
					params["conf_weighted"] = comparison.ConfWeighted
				}
				return Event{
					Type: eventType, OldValue: old, NewValue: band, Severity: sev,
					Message: fmt.Sprintf("%s %s independent reference-orbit delta %.0f m", comparison.Name, positionSource, comparison.DeltaM),
					Params:  params,
				}
			})
		}
	})
}
