package integrity

import (
	"math"
	"time"
)

// Receiver-clock check names.
const (
	CheckClockBiasDrift = "clock_bias_drift"
	CheckClockDriftRate = "clock_drift_rate"
)

var (
	clockBiasDriftInfo = Info{Name: CheckClockBiasDrift, Version: 1, Domain: DomainReceiverClock}
	clockDriftRateInfo = Info{Name: CheckClockDriftRate, Version: 1, Domain: DomainReceiverClock}
)

// Reason codes for the clock checks.
const (
	ReasonNoWindow            = "no_window"
	ReasonNoFix               = "no_fix"
	ReasonBiasDriftDivergence = "bias_drift_divergence"
	ReasonDriftRate           = "drift_rate"
	ReasonClockAdjustment     = "clock_ms_adjustment"
	ReasonReceiverRestart     = "receiver_restart"
)

// ClockBiasDriftProfile holds the clock_bias_drift operating points. The test
// statistic is |Δbias − ∫drift dt| over a window of MinWindow..MaxWindow (receiver
// time), after removing whole-millisecond clock adjustments recognised within
// AdjustmentToleranceNs.
type ClockBiasDriftProfile struct {
	MinWindow             time.Duration `json:"min_window_ns"`
	MaxWindow             time.Duration `json:"max_window_ns"`
	InconsistentNs        float64       `json:"inconsistent_ns"`
	UnassuredNs           float64       `json:"unassured_ns"`
	AdjustmentToleranceNs float64       `json:"adjustment_tolerance_ns"`
}

// ClockDriftRateProfile holds the clock_drift_rate operating points. The test
// statistic is |Δdrift| / Δt over a window of MinWindow..MaxWindow.
type ClockDriftRateProfile struct {
	MinWindow           time.Duration `json:"min_window_ns"`
	MaxWindow           time.Duration `json:"max_window_ns"`
	InconsistentNsPerS2 float64       `json:"inconsistent_ns_per_s2"`
	UnassuredNsPerS2    float64       `json:"unassured_ns_per_s2"`
}

// msNs is one millisecond in nanoseconds: the step of a receiver clock adjustment.
const msNs = 1e6

// clockSample is one accepted clock epoch with the per-step residual leading to it.
type clockSample struct {
	t        int64 // continuous receiver time, ms
	bias     float64
	drift    float64
	residual float64 // (bias − previous bias) − trapezoid ∫drift over the step, adjustments removed
	adjusted bool    // this step removed a whole-millisecond clock adjustment
}

// clockChecks evaluates the receiver-clock domain.
type clockChecks struct {
	lastTOW uint32
	clock   int64
	started bool
	samples []clockSample
}

func (c *clockChecks) reset() { c.samples = c.samples[:0] }

// evaluate folds one clock epoch and returns both verdicts, or nil for a duplicate or
// out-of-order epoch. fixOK reports whether the receiver currently has a valid fix
// (true when no solution is reported at all).
func (c *clockChecks) evaluate(s ClockSample, fixOK bool, prof Profile) map[string]Verdict {
	if c.started {
		d := towDelta(c.lastTOW, s.TOW)
		if d <= 0 {
			return nil
		}
		c.clock += d
		if d > maxEpochGapMS {
			c.reset()
		}
	}
	c.started, c.lastTOW = true, s.TOW
	if !fixOK {
		c.reset()
		v := Verdict{State: Unavailable, Reasons: []string{ReasonNoFix}}
		return map[string]Verdict{CheckClockBiasDrift: v, CheckClockDriftRate: v}
	}

	cur := clockSample{t: c.clock, bias: s.BiasNs, drift: s.DriftNsPerS}
	if n := len(c.samples); n > 0 {
		prev := c.samples[n-1]
		step := float64(cur.t-prev.t) / 1000
		r := (cur.bias - prev.bias) - (cur.drift+prev.drift)/2*step
		if k := math.Round(r / msNs); k != 0 && math.Abs(r-k*msNs) <= prof.ClockBiasDrift.AdjustmentToleranceNs {
			r -= k * msNs
			cur.adjusted = true
		}
		cur.residual = r
	}
	c.samples = append(c.samples, cur)
	maxWindow := prof.ClockBiasDrift.MaxWindow
	if prof.ClockDriftRate.MaxWindow > maxWindow {
		maxWindow = prof.ClockDriftRate.MaxWindow
	}
	cutoff := c.clock - maxWindow.Milliseconds()
	n := 0
	for n < len(c.samples)-1 && c.samples[n].t < cutoff {
		n++
	}
	c.samples = c.samples[n:]

	return map[string]Verdict{
		CheckClockBiasDrift: c.biasDrift(prof.ClockBiasDrift),
		CheckClockDriftRate: c.driftRate(prof.ClockDriftRate),
	}
}

// base returns the index of the newest sample at least minW and at most maxW older
// than the latest, or −1.
func (c *clockChecks) base(minW, maxW time.Duration) int {
	now := c.samples[len(c.samples)-1].t
	for i := len(c.samples) - 2; i >= 0; i-- {
		age := now - c.samples[i].t
		if age >= minW.Milliseconds() {
			if age <= maxW.Milliseconds() {
				return i
			}
			return -1
		}
	}
	return -1
}

func (c *clockChecks) biasDrift(prof ClockBiasDriftProfile) Verdict {
	b := c.base(prof.MinWindow, prof.MaxWindow)
	if b < 0 {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoWindow}}
	}
	var sum float64
	adjustments := 0
	for _, s := range c.samples[b+1:] {
		sum += s.residual
		if s.adjusted {
			adjustments++
		}
	}
	div := math.Abs(sum)
	v := Verdict{
		State: Assured,
		Metrics: map[string]float64{
			"divergence_ns": div, "window_s": float64(c.samples[len(c.samples)-1].t-c.samples[b].t) / 1000,
			"clock_adjustments": float64(adjustments),
		},
		Thresholds: map[string]float64{"divergence_inconsistent_ns": prof.InconsistentNs, "divergence_unassured_ns": prof.UnassuredNs},
	}
	switch {
	case div > prof.UnassuredNs:
		v.State = Unassured
	case div > prof.InconsistentNs:
		v.State = Inconsistent
	}
	if v.State != Assured {
		v.Reasons = append(v.Reasons, ReasonBiasDriftDivergence)
	}
	if adjustments > 0 {
		v.Reasons = append(v.Reasons, ReasonClockAdjustment)
	}
	return v
}

func (c *clockChecks) driftRate(prof ClockDriftRateProfile) Verdict {
	b := c.base(prof.MinWindow, prof.MaxWindow)
	if b < 0 {
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoWindow}}
	}
	last, first := c.samples[len(c.samples)-1], c.samples[b]
	dt := float64(last.t-first.t) / 1000
	rate := math.Abs(last.drift-first.drift) / dt
	v := Verdict{
		State:      Assured,
		Metrics:    map[string]float64{"drift_rate_ns_per_s2": rate, "window_s": dt, "drift_ns_per_s": last.drift},
		Thresholds: map[string]float64{"drift_rate_inconsistent_ns_per_s2": prof.InconsistentNsPerS2, "drift_rate_unassured_ns_per_s2": prof.UnassuredNsPerS2},
	}
	switch {
	case rate > prof.UnassuredNsPerS2:
		v.State = Unassured
	case rate > prof.InconsistentNsPerS2:
		v.State = Inconsistent
	}
	if v.State != Assured {
		v.Reasons = []string{ReasonDriftRate}
	}
	return v
}
