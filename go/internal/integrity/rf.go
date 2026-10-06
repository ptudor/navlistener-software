package integrity

import (
	"maps"
	"slices"
)

// RF and receiver-verdict check names.
const (
	CheckCn0Uniformity    = "cn0_uniformity"
	CheckAGC              = "agc"
	CheckReceiverSpoofing = "receiver_spoofing"
)

var (
	cn0UniformityInfo    = Info{Name: CheckCn0Uniformity, Version: 1, Domain: DomainSignalPower, LowerOnly: true}
	agcInfo              = Info{Name: CheckAGC, Version: 1, Domain: DomainRFEnvironment, LowerOnly: true}
	receiverSpoofingInfo = Info{Name: CheckReceiverSpoofing, Version: 1, Domain: DomainReceiverVerdict, LowerOnly: true}
)

// Reason codes for the RF and receiver-verdict checks.
const (
	ReasonTooFewSatellites = "too_few_satellites"
	ReasonUniformCn0       = "uniform_cn0"
	ReasonNoBands          = "no_rf_bands"
	ReasonNoBaseline       = "no_agc_baseline"
	ReasonAGCDeparture     = "agc_departure"
	ReasonAGCCollapse      = "agc_collapse"
	ReasonCWTone           = "cw_tone"
	ReasonReceiverJamFlag  = "receiver_jam_flag"
	ReasonAntennaFault     = "antenna_fault"
	ReasonSpoofingUnknown  = "receiver_spoofing_unknown"
	ReasonSpoofingFlag     = "receiver_spoofing_flag"
)

// Station RF defaults. The station event classifiers in internal/detect use the same
// values (docs/DEFENSE-PNT.md §4).
const (
	// DefaultAGCDeparture is the AGC departure below the learned baseline, in AGC
	// counts, at which a band is jamming-suspect; DefaultAGCDepartureSevere is a
	// near-total gain collapse.
	DefaultAGCDeparture       = 800.0
	DefaultAGCDepartureSevere = 2000.0
	// DefaultCWSuppress is the CW-suppression indicator (0..255) above which a
	// band is taken to hold a narrowband tone.
	DefaultCWSuppress = 180
	// DefaultCn0ResidVar and DefaultCn0Mean: the C/N₀-vs-elevation gate trips when
	// the residual variance (dB²) collapses at an unnaturally high mean (dB-Hz).
	DefaultCn0ResidVar = 2.0
	DefaultCn0Mean     = 45.0
	// DefaultCn0MinSats is the fewest satellites for a meaningful fit.
	DefaultCn0MinSats = 5
)

// Cn0UniformityProfile holds the cn0_uniformity operating points.
type Cn0UniformityProfile struct {
	ResidVar float64 `json:"resid_var_db2"`
	Mean     float64 `json:"mean_db_hz"`
	MinSats  int     `json:"min_sats"`
}

// AGCProfile holds the agc operating points.
type AGCProfile struct {
	Departure       float64 `json:"departure"`
	DepartureSevere float64 `json:"departure_severe"`
	CWSuppress      int     `json:"cw_suppress"`
}

// cn0Uniformity applies the C/N₀-vs-elevation gate to the aggregate fit and to each
// constellation's own fit; either tripping is the same single gate.
func cn0Uniformity(f Cn0Fit, prof Cn0UniformityProfile) Verdict {
	tripped := func(mean, resid float64) bool { return resid < prof.ResidVar && mean > prof.Mean }
	v := Verdict{
		State:      Assured,
		Metrics:    map[string]float64{"mean_db_hz": f.Mean, "resid_var_db2": f.ResidVar, "num_sats": float64(f.NumSats)},
		Thresholds: map[string]float64{"resid_var_below_db2": prof.ResidVar, "mean_above_db_hz": prof.Mean, "min_sats": float64(prof.MinSats)},
	}
	evaluated := false
	if f.NumSats >= prof.MinSats {
		evaluated = true
		if tripped(f.Mean, f.ResidVar) {
			v.State = Unassured
		}
	}
	// Sorted, so the reported constellation is the same on every run and replay.
	for _, gnssID := range slices.Sorted(maps.Keys(f.PerConstellation)) {
		g := f.PerConstellation[gnssID]
		if g.NumSats < prof.MinSats {
			continue
		}
		evaluated = true
		if !tripped(g.Mean, g.ResidVar) {
			continue
		}
		v.State = Unassured
		if _, recorded := v.Metrics["tripped_gnss_id"]; !recorded {
			v.Metrics["tripped_gnss_id"] = float64(gnssID)
			v.Metrics["tripped_mean_db_hz"] = g.Mean
			v.Metrics["tripped_resid_var_db2"] = g.ResidVar
		}
	}
	switch {
	case !evaluated:
		return Verdict{State: Unavailable, Reasons: []string{ReasonTooFewSatellites}, Metrics: v.Metrics, Thresholds: v.Thresholds}
	case v.State == Unassured:
		v.Reasons = []string{ReasonUniformCn0}
	}
	return v
}

// agc classifies the front end like the jamming classifiers do: a severe gain
// collapse, or a departure corroborated by a CW tone or the receiver's own jam flag,
// is unassured; any single sign of interference, or an antenna fault, is
// inconsistent.
func agc(rf RFSample, prof AGCProfile) Verdict {
	if len(rf.Bands) == 0 {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoBands}}
	}
	var maxDep float64
	haveDep, cw, rxJam, ant := false, false, false, false
	for _, b := range rf.Bands {
		if b.Departure != nil {
			if !haveDep || *b.Departure > maxDep {
				maxDep = *b.Departure
			}
			haveDep = true
		}
		cw = cw || b.CWSuppress >= prof.CWSuppress
		rxJam = rxJam || b.JamState >= 2
		ant = ant || b.AntStatus == 3 || b.AntStatus == 4
	}
	v := Verdict{
		State: Assured,
		Metrics: map[string]float64{
			"cw": boolMetric(cw), "receiver_jam": boolMetric(rxJam), "antenna_fault": boolMetric(ant), "bands": float64(len(rf.Bands)),
		},
		Thresholds: map[string]float64{"departure": prof.Departure, "departure_severe": prof.DepartureSevere, "cw_suppress": float64(prof.CWSuppress)},
	}
	if haveDep {
		v.Metrics["max_departure"] = maxDep
	}
	dep := haveDep && maxDep >= prof.Departure
	switch {
	case haveDep && maxDep >= prof.DepartureSevere:
		v.State, v.Reasons = Unassured, []string{ReasonAGCCollapse}
	case dep && (cw || rxJam):
		v.State, v.Reasons = Unassured, []string{ReasonAGCDeparture}
	case dep || cw || rxJam || ant:
		v.State = Inconsistent
		if dep {
			v.Reasons = append(v.Reasons, ReasonAGCDeparture)
		}
	case !haveDep:
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoBaseline}, Metrics: v.Metrics, Thresholds: v.Thresholds}
	}
	if cw {
		v.Reasons = append(v.Reasons, ReasonCWTone)
	}
	if rxJam {
		v.Reasons = append(v.Reasons, ReasonReceiverJamFlag)
	}
	if ant {
		v.Reasons = append(v.Reasons, ReasonAntennaFault)
	}
	return v
}

func boolMetric(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// receiverSpoofing reports the receiver's own spoofing indication. It is never a
// verdict by itself: as a lower-only check in its own domain it can only corroborate
// a physics domain.
func receiverSpoofing(state int) Verdict {
	v := Verdict{Metrics: map[string]float64{"spoofing_state": float64(state)}}
	switch state {
	case SpoofNone:
		v.State = Assured
	case SpoofIndicated, SpoofMultiple:
		v.State, v.Reasons = Unassured, []string{ReasonSpoofingFlag}
	default:
		v.State, v.Reasons = Unavailable, []string{ReasonSpoofingUnknown}
	}
	return v
}
