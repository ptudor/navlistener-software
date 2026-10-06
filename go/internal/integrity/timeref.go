package integrity

import (
	"math"
	"time"
)

// Time-reference check names.
const (
	CheckUTCOffset   = "utc_offset"
	CheckPPSRTCPhase = "pps_rtc_phase"
)

var (
	utcOffsetInfo   = Info{Name: CheckUTCOffset, Version: 1, Domain: DomainTimeReference}
	ppsRTCPhaseInfo = Info{Name: CheckPPSRTCPhase, Version: 1, Domain: DomainTimeReference}
)

// Reason codes for the time-reference checks.
const (
	ReasonNoUTC         = "no_utc"
	ReasonNoHostStamp   = "no_host_stamp"
	ReasonUTCOffset     = "utc_offset"
	ReasonNoPhase       = "no_phase"
	ReasonRTCStopped    = "rtc_stopped"
	ReasonRTCTrim       = "rtc_trim"
	ReasonObserverReset = "observer_restart"
	ReasonFitWarmup     = "phase_fit_warming_up"
	ReasonPhaseStep     = "phase_step"
)

// UTCOffsetProfile holds the utc_offset operating points: the receiver's UTC
// against an independent wall-clock stamp of the same record. The stamp follows
// the epoch by the receiver's output and transport latency, so only whole-second
// disagreements are meaningful.
type UTCOffsetProfile struct {
	Inconsistent time.Duration `json:"inconsistent_ns"`
	Unassured    time.Duration `json:"unassured_ns"`
}

// PPSRTCPhaseProfile holds the pps_rtc_phase operating points. The RTC-minus-GNSS
// pulse phase is fitted with a line over samples aged FitMinAge..FitMaxAge
// (observer uptime, at least FitMinSamples of them); the latest phase's residual
// from that line beyond the bands is a step the free-running RTC did not take. The
// default bands allow for an uncompensated crystal RTC during a temperature ramp.
type PPSRTCPhaseProfile struct {
	FitMinAge      time.Duration `json:"fit_min_age_ns"`
	FitMaxAge      time.Duration `json:"fit_max_age_ns"`
	FitMinSamples  int           `json:"fit_min_samples"`
	MaxSampleGap   time.Duration `json:"max_sample_gap_ns"`
	InconsistentNs float64       `json:"inconsistent_ns"`
	UnassuredNs    float64       `json:"unassured_ns"`
}

func utcOffset(s Solution, prof UTCOffsetProfile) Verdict {
	switch {
	case !s.UTCValid || !s.FixOK:
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoUTC}}
	case s.HostStamp.IsZero():
		return Verdict{State: Unavailable, Reasons: []string{ReasonNoHostStamp}}
	}
	offset := s.UTC.Sub(s.HostStamp)
	mag := offset.Abs()
	v := Verdict{
		State:      Assured,
		Metrics:    map[string]float64{"offset_s": offset.Seconds()},
		Thresholds: map[string]float64{"offset_inconsistent_s": prof.Inconsistent.Seconds(), "offset_unassured_s": prof.Unassured.Seconds()},
	}
	switch {
	case mag > prof.Unassured:
		v.State = Unassured
	case mag > prof.Inconsistent:
		v.State = Inconsistent
	}
	if v.State != Assured {
		v.Reasons = []string{ReasonUTCOffset}
	}
	return v
}

// phasePoint is one unwrapped RTC-minus-GNSS phase at an observer uptime.
type phasePoint struct {
	uptimeMS uint64
	phase    float64 // ns, unwrapped
}

// ppsRTCPhase tracks the unwrapped pulse phase between restarts and gaps.
type ppsRTCPhase struct {
	points  []phasePoint
	lastRaw float64
}

func (p *ppsRTCPhase) reset() { p.points = p.points[:0] }

// evaluate folds one timing sample. fixOK reports a currently valid receiver fix:
// without one the GNSS pulse may free-run and then step when the fix returns.
func (p *ppsRTCPhase) evaluate(t TimingSample, fixOK bool, prof PPSRTCPhaseProfile) Verdict {
	if n := len(p.points); n > 0 {
		last := p.points[n-1].uptimeMS
		switch {
		case t.UptimeMS < last:
			p.reset()
			return Verdict{State: Unavailable, Reasons: []string{ReasonObserverReset}}
		case t.UptimeMS == last:
			return Verdict{State: Unavailable, Reasons: []string{ReasonNoWindow}}
		case time.Duration(t.UptimeMS-last)*time.Millisecond > prof.MaxSampleGap:
			p.reset()
		}
	}
	var reason string
	switch {
	case !t.RTCRunning:
		reason = ReasonRTCStopped
	case t.RTCTrim&0x7f != 0:
		reason = ReasonRTCTrim
	case t.PhaseNs == nil:
		reason = ReasonNoPhase
	case !fixOK:
		reason = ReasonNoFix
	}
	if reason != "" {
		p.reset()
		return Verdict{State: Unavailable, Reasons: []string{reason}}
	}

	raw := *t.PhaseNs
	cur := phasePoint{uptimeMS: t.UptimeMS, phase: raw}
	if n := len(p.points); n > 0 {
		step := math.Remainder(raw-p.lastRaw, 1e9) // unwrap across the ±0.5 s boundary
		cur.phase = p.points[n-1].phase + step
	}
	p.lastRaw = raw

	// Fit the samples aged FitMinAge..FitMaxAge before this one.
	var sx, sy, sxx, sxy float64
	k := 0
	for _, q := range p.points {
		age := time.Duration(t.UptimeMS-q.uptimeMS) * time.Millisecond
		if age < prof.FitMinAge || age > prof.FitMaxAge {
			continue
		}
		x := -age.Seconds()
		sx, sy, sxx, sxy = sx+x, sy+q.phase, sxx+x*x, sxy+x*q.phase
		k++
	}
	p.points = append(p.points, cur)
	cutoff := uint64(0)
	if maxAge := uint64(prof.FitMaxAge.Milliseconds()); t.UptimeMS > maxAge {
		cutoff = t.UptimeMS - maxAge
	}
	n := 0
	for n < len(p.points) && p.points[n].uptimeMS < cutoff {
		n++
	}
	p.points = p.points[n:]

	thresholds := map[string]float64{"phase_residual_inconsistent_ns": prof.InconsistentNs, "phase_residual_unassured_ns": prof.UnassuredNs}
	fk := float64(k)
	denom := fk*sxx - sx*sx
	if k < prof.FitMinSamples || denom == 0 {
		return Verdict{State: Unavailable, Reasons: []string{ReasonFitWarmup},
			Metrics: map[string]float64{"fit_samples": fk}, Thresholds: thresholds}
	}
	slope := (fk*sxy - sx*sy) / denom // ns per second of uptime
	intercept := (sy - slope*sx) / fk // predicted phase now (x = 0)
	residual := cur.phase - intercept
	v := Verdict{
		State: Assured,
		Metrics: map[string]float64{
			"phase_residual_ns": residual, "rtc_rate_ppm": slope / 1e3, "fit_samples": fk,
		},
		Thresholds: thresholds,
	}
	switch mag := math.Abs(residual); {
	case mag > prof.UnassuredNs:
		v.State = Unassured
	case mag > prof.InconsistentNs:
		v.State = Inconsistent
	}
	if v.State != Assured {
		v.Reasons = []string{ReasonPhaseStep}
	}
	return v
}
