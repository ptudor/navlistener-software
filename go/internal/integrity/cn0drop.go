package integrity

import (
	"slices"
	"time"
)

// CheckCn0Drop is the simultaneous C/N₀ drop check.
const CheckCn0Drop = "cn0_drop"

// cn0_drop shares NAV-SAT with cn0_uniformity, but a drop across every satellite at
// once is interference evidence, not spoofing evidence: a jammer raises the noise
// floor under every signal together. It therefore sits in the RF environment domain,
// beside the AGC check it corroborates, and cannot join the spoofing quorum.
var cn0DropInfo = Info{Name: CheckCn0Drop, Version: 1, Domain: DomainRFEnvironment, LowerOnly: true}

// ReasonSimultaneousDrop: every compared signal lost C/N₀ within the window.
const ReasonSimultaneousDrop = "simultaneous_cn0_drop"

// maxCn0Snapshots bounds the comparison history whatever the NAV-SAT rate.
const maxCn0Snapshots = 64

// Cn0DropProfile holds the cn0_drop operating points.
type Cn0DropProfile struct {
	// Window is how far back a reference snapshot may be; MinSpan is how far back it
	// must be. The oldest snapshot inside the window that is at least MinSpan old is
	// the reference.
	Window  time.Duration `json:"window_ns"`
	MinSpan time.Duration `json:"min_span_ns"`
	// MinSignals is the fewest signals, used in the reference and tracked now, that
	// make "every signal" meaningful.
	MinSignals int `json:"min_signals"`
	// EveryDB is the drop every compared signal must show. InconsistentDB and
	// UnassuredDB grade the median drop of a simultaneous drop.
	EveryDB        float64 `json:"every_drop_db"`
	InconsistentDB float64 `json:"inconsistent_median_db"`
	UnassuredDB    float64 `json:"unassured_median_db"`
}

// cn0DropCheck keeps the recent NAV-SAT snapshots the drop test compares against.
type cn0DropCheck struct {
	recent []Cn0Snapshot
}

// evaluate compares one snapshot with the reference inside the window and records it.
// ok is false for a duplicate or out-of-order snapshot, which is ignored.
//
// Following the CISA Epsilon C/N₀ drop monitor, the test is that every signal lost
// C/N₀ over a few seconds: one signal fading is geometry or multipath, and every
// signal falling together is the noise floor rising. Only signals the receiver used
// in the reference and still tracks now are compared, standing in for Epsilon's
// quality gate; a signal lost outright is not a measured drop. A slow ramp below the
// per-window step does not show here; the AGC baseline's bounded learning covers it.
func (c *cn0DropCheck) evaluate(snap Cn0Snapshot, prof Cn0DropProfile) (Verdict, bool) {
	if n := len(c.recent); n > 0 && !snap.Received.After(c.recent[n-1].Received) {
		return Verdict{}, false
	}
	keep := c.recent[:0]
	for _, old := range c.recent {
		if snap.Received.Sub(old.Received) <= prof.Window {
			keep = append(keep, old)
		}
	}
	c.recent = keep
	var ref *Cn0Snapshot
	for i := range c.recent {
		if snap.Received.Sub(c.recent[i].Received) >= prof.MinSpan {
			ref = &c.recent[i]
			break
		}
	}
	thresholds := map[string]float64{
		"every_drop_db": prof.EveryDB, "inconsistent_median_db": prof.InconsistentDB,
		"unassured_median_db": prof.UnassuredDB, "min_signals": float64(prof.MinSignals),
	}
	var v Verdict
	if ref == nil {
		v = Verdict{State: Unavailable, Reasons: []string{ReasonNoWindow}, Thresholds: thresholds}
	} else {
		v = cn0Drops(*ref, snap, prof, thresholds)
	}
	c.recent = append(c.recent, snap)
	if len(c.recent) > maxCn0Snapshots {
		c.recent = slices.Delete(c.recent, 0, len(c.recent)-maxCn0Snapshots)
	}
	return v, true
}

// cn0Drops grades the drops from ref to now of the signals tracked in both.
func cn0Drops(ref, now Cn0Snapshot, prof Cn0DropProfile, thresholds map[string]float64) Verdict {
	type key struct{ gnss, sv int }
	before := make(map[key]int, len(ref.Signals))
	for _, s := range ref.Signals {
		k := key{s.GnssID, s.SvID}
		if _, dup := before[k]; !dup && s.Used && s.Cn0 > 0 {
			before[k] = s.Cn0
		}
	}
	var drops []float64
	seen := make(map[key]bool, len(now.Signals))
	for _, s := range now.Signals {
		k := key{s.GnssID, s.SvID}
		was, ok := before[k]
		if !ok || seen[k] || s.Cn0 <= 0 {
			continue
		}
		seen[k] = true
		drops = append(drops, float64(was-s.Cn0))
	}
	metrics := map[string]float64{
		"signals": float64(len(drops)), "span_s": now.Received.Sub(ref.Received).Seconds(),
	}
	if len(drops) < prof.MinSignals {
		return Verdict{State: Unavailable, Reasons: []string{ReasonTooFewSatellites}, Metrics: metrics, Thresholds: thresholds}
	}
	slices.Sort(drops)
	minDrop := drops[0]
	median := drops[len(drops)/2]
	if len(drops)%2 == 0 {
		median = (drops[len(drops)/2-1] + median) / 2
	}
	metrics["min_drop_db"], metrics["median_drop_db"] = minDrop, median
	v := Verdict{State: Assured, Metrics: metrics, Thresholds: thresholds}
	if minDrop >= prof.EveryDB {
		switch {
		case median >= prof.UnassuredDB:
			v.State, v.Reasons = Unassured, []string{ReasonSimultaneousDrop}
		case median >= prof.InconsistentDB:
			v.State, v.Reasons = Inconsistent, []string{ReasonSimultaneousDrop}
		}
	}
	return v
}
