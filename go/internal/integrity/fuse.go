package integrity

import "slices"

// Fusion is a station's combined state.
type Fusion struct {
	State State `json:"state"`
	// Score is the weighted mean level in [−1, +1] of the checks that took part,
	// absent when none did.
	Score *float64 `json:"score,omitempty"`
	// UnassuredDomains lists, sorted, every domain with at least one unassured check.
	UnassuredDomains []Domain `json:"unassured_domains,omitempty"`
	// SpoofingIndicated is the spoofing evidence rule: at least two physics
	// domains unassured, or one together with the receiver's own spoofing
	// indication.
	SpoofingIndicated bool `json:"spoofing_indicated"`
}

// Fuse combines served check results. Every check with an available state takes part
// in a weighted mean of levels (unassured −1, inconsistent 0, assured +1) with the
// given weights (absent means 1; zero or negative excludes the check), except that
// a lower-only check takes part only when it is not assured. The mean maps to a
// state at ±0.5, as in the PNT Integrity library. Two domain rules then apply:
//
//   - The station is unassured only when at least one physics domain is unassured and
//     a second domain agrees: another physics domain, the receiver's own verdict, or
//     the RF environment. Otherwise any unassured check holds the station at
//     inconsistent, so a single measurement family cannot condemn a station.
//   - An assured mean never hides an unassured check.
func Fuse(results []Result, weights map[string]float64) Fusion {
	var sum, sumW float64
	domains := map[Domain]bool{}
	for _, r := range results {
		if r.State == Unassured {
			domains[r.Domain] = true
		}
		lvl, ok := r.State.level()
		if !ok || (r.lowerOnly && r.State == Assured) {
			continue
		}
		w := 1.0
		if v, set := weights[r.Check]; set {
			w = v
		}
		if w <= 0 {
			continue
		}
		sum += w * lvl
		sumW += w
	}

	var f Fusion
	for d := range domains {
		f.UnassuredDomains = append(f.UnassuredDomains, d)
	}
	slices.Sort(f.UnassuredDomains)

	physics := 0
	for d := range domains {
		if d.Physics() {
			physics++
		}
	}
	corroborated := physics
	if domains[DomainReceiverVerdict] {
		corroborated++
	}
	if domains[DomainRFEnvironment] {
		corroborated++
	}
	f.SpoofingIndicated = physics >= 2 || (physics >= 1 && domains[DomainReceiverVerdict])

	f.State = Unavailable
	if sumW > 0 {
		score := sum / sumW
		f.Score = &score
		switch {
		case score < -0.5:
			f.State = Unassured
		case score <= 0.5:
			f.State = Inconsistent
		default:
			f.State = Assured
		}
	}
	switch {
	case physics >= 1 && corroborated >= 2:
		f.State = Unassured
	case len(domains) > 0:
		f.State = Inconsistent
	}
	return f
}
