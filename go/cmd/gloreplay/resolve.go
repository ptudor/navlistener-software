// Deferred time-disco resolution and per-SV string delivery accounting.
//
// The first regression fix capture run reported 111 of 123 changeovers with
// time_disco_ns "absent (gated)" — language that reads as the detector
// declining to define the metric. It wasn't: state's applyGlonass DEFERS the
// time-disco when the incoming set assembles clockless at the changeover
// (string 4 is broadcast ~2 s after string 3, GLO-ICD-5.1 §4.4 broadcast
// order, so at the string-3-triggered adoption the new frame's clock has not
// arrived yet), and completes it when the same-tb set reassembles with the
// clock. The sampler read the feed exactly once, at the changeover instant —
// when a deferral is pending BY CONSTRUCTION — and never looked again, so
// every deferred-then-completed measurement was misfiled as absent. This file
// is the follow-up: track each deferral to its actual outcome, and count
// per-SV string delivery so a genuinely missing string 4 (a receiver/reception
// problem) is distinguishable from a state-side completion failure.
package main

import (
	"fmt"
	"sort"
	"time"

	"github.com/ptudor/gnss/frame"
)

// Outcomes for changeover.TimeOutcome. Every recorded changeover ends in
// exactly one of them; the report's resolution section is their tally.
const (
	// outcomeImmediate: the adopting assembly already carried the new clock, so
	// computeGloDisco measured the time-disco at the changeover itself.
	outcomeImmediate = "immediate"
	// outcomeCompleted: the changeover deferred (clockless adoption) and a later
	// same-tb string 4 completed it — the pendClk path working as designed.
	outcomeCompleted = "completed"
	// outcomeSuperseded: the next changeover arrived while the deferral was
	// still pending. computeGloDisco's publish clears (or re-arms) the pend at
	// every changeover, so the old sample's time-disco is now unmeasurable.
	outcomeSuperseded = "superseded"
	// outcomeEOF: the replay ended while the deferral was pending. Censored —
	// the detector may well have completed it moments after the capture stopped.
	outcomeEOF = "unresolved_eof"
)

// gloStringNumber decodes just the string number of one GLONASS string, with
// the same Hamming/range validation state applies (frame.DecodeGLONASSString) —
// a string the tool counts must be one the detector would have accepted.
func gloStringNumber(words []uint32) (int, error) {
	s, err := frame.DecodeGLONASSString(words)
	if err != nil {
		return 0, err
	}
	return s.Number, nil
}

// svStrings counts one SV's decoded string numbers across the replay. Every
// broadcast frame carries strings 1..4 once (GLO-ICD-5.1 §4.4: immediate data
// occupy strings 1-4 of every frame), so s1≈s2≈s3≈s4 is the healthy shape and
// a chronically low s4 means the clock string is not being delivered — every
// changeover's time-disco would then defer and eventually be superseded. Both
// L1OF and L2OF copies are counted (state keys both into one Sig-0 entry,
// regression fix), so absolute counts are ~2× the frame count when both bands track.
type svStrings struct {
	s1, s2, s3, s4 int
	other          int // strings 5..15: time string and almanac pages
	err            int // Hamming/format rejects — frames state would also drop
}

func (c *svStrings) count(number int) {
	switch number {
	case 1:
		c.s1++
	case 2:
		c.s2++
	case 3:
		c.s3++
	case 4:
		c.s4++
	default:
		c.other++
	}
}

// clockPend is one SV's in-flight deferred time-disco: the changeover sample it
// belongs to and what has been seen since. s4LagS is set at the FIRST string-4
// sighting after the changeover, independent of whether the completion fires —
// the difference between "string 4 seen" and "completion observed" is exactly
// the evidence that separates a reception gap from a state-side failure.
type clockPend struct {
	sampleIdx int
	at        time.Time
	s4LagS    *float64
}

// pendSet tracks at most one pending deferral per SV — mirroring state, which
// keeps a single discoPendClk per svState and voids it at every changeover.
type pendSet struct{ m map[string]*clockPend }

func newPendSet() *pendSet { return &pendSet{m: map[string]*clockPend{}} }

// arm starts watching sv's just-recorded changeover (samples[sampleIdx]) for a
// late completion. Any prior pend for sv must have been finalized already (the
// sampler supersedes before recording a new changeover).
func (p *pendSet) arm(sv string, sampleIdx int, at time.Time) {
	p.m[sv] = &clockPend{sampleIdx: sampleIdx, at: at}
}

// sawS4 records the first string-4 arrival for an armed SV. No-op when nothing
// is pending or a string 4 was already seen.
func (p *pendSet) sawS4(sv string, now time.Time) {
	pc := p.m[sv]
	if pc == nil || pc.s4LagS != nil {
		return
	}
	lag := now.Sub(pc.at).Seconds()
	pc.s4LagS = &lag
}

// complete resolves sv's pending deferral with the value the feed now serves.
// Between changeovers the only writer of a nil→value transition on the served
// time_disco_ns is state's pendClk completion (applyGlonass), so attribution to
// the armed sample is unambiguous. No-op when nothing is pending.
func (p *pendSet) complete(samples []changeover, sv string, v float64, now time.Time) {
	pc := p.m[sv]
	if pc == nil {
		return
	}
	lag := now.Sub(pc.at).Seconds()
	smp := &samples[pc.sampleIdx]
	smp.TimeNsLate, smp.TimeLagS = &v, &lag
	smp.TimeOutcome = outcomeCompleted
	smp.S4LagS = pc.s4LagS
	delete(p.m, sv)
}

// supersede finalizes sv's pending deferral as voided by a newer changeover.
// No-op when nothing is pending.
func (p *pendSet) supersede(samples []changeover, sv string) {
	pc := p.m[sv]
	if pc == nil {
		return
	}
	smp := &samples[pc.sampleIdx]
	smp.TimeOutcome = outcomeSuperseded
	smp.S4LagS = pc.s4LagS
	delete(p.m, sv)
}

// finish finalizes every still-pending deferral as censored by the end of the
// replay. Call once, after the last frame.
func (p *pendSet) finish(samples []changeover) {
	for sv, pc := range p.m {
		smp := &samples[pc.sampleIdx]
		smp.TimeOutcome = outcomeEOF
		smp.S4LagS = pc.s4LagS
		delete(p.m, sv)
	}
}

// reportResolution prints the per-outcome tally and the completion-lag
// distribution. Lags are measured on the replay's clock: real reception time
// for -from-store, the synthetic clock for -capture (accurate when -duration
// matches the capture's true span).
func reportResolution(samples []changeover) {
	var immediate, completed, superseded, eof int
	var lags []float64
	s4Seen := map[string]int{} // outcome → count with a string 4 sighted while pending
	for _, s := range samples {
		switch s.TimeOutcome {
		case outcomeImmediate:
			immediate++
		case outcomeCompleted:
			completed++
			if s.TimeLagS != nil {
				lags = append(lags, *s.TimeLagS)
			}
		case outcomeSuperseded:
			superseded++
			if s.S4LagS != nil {
				s4Seen[outcomeSuperseded]++
			}
		case outcomeEOF:
			eof++
			if s.S4LagS != nil {
				s4Seen[outcomeEOF]++
			}
		}
	}

	fmt.Printf("time-disco resolution (n = %d changeovers):\n", len(samples))
	fmt.Printf("  immediate at the changeover:   %d\n", immediate)
	if len(lags) > 0 {
		sort.Float64s(lags)
		fmt.Printf("  completed by later string 4:   %d   lag s: p50 %.3g  p95 %.3g  max %.3g\n",
			completed, percentile(lags, 50), percentile(lags, 95), lags[len(lags)-1])
	} else {
		fmt.Printf("  completed by later string 4:   %d\n", completed)
	}
	fmt.Printf("  superseded by next changeover: %d", superseded)
	if superseded > 0 {
		// A string 4 seen while pending with no completion is the state-side
		// failure signature; none seen is a plain reception gap.
		fmt.Printf("   (string 4 seen while pending: %d)", s4Seen[outcomeSuperseded])
	}
	fmt.Println()
	fmt.Printf("  unresolved at end of replay:   %d", eof)
	if eof > 0 {
		fmt.Printf("   (string 4 seen while pending: %d; censored, not failures)", s4Seen[outcomeEOF])
	}
	fmt.Println()
}

// reportStrings prints the per-SV string-delivery table, sorted by SV key.
func reportStrings(strs map[string]*svStrings) {
	if len(strs) == 0 {
		return
	}
	keys := make([]string, 0, len(strs))
	for k := range strs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	fmt.Println("\nper-SV string delivery (both bands; s4 ≪ s1 means the clock string is not arriving):")
	fmt.Printf("  %-8s %6s %6s %6s %6s %6s %5s\n", "sv", "s1", "s2", "s3", "s4", "other", "err")
	for _, k := range keys {
		c := strs[k]
		fmt.Printf("  %-8s %6d %6d %6d %6d %6d %5d\n", k, c.s1, c.s2, c.s3, c.s4, c.other, c.err)
	}
}
