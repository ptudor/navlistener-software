package integrity

import (
	"slices"
	"testing"
	"time"
)

// clockAt returns a clock epoch i seconds into a run whose bias follows a constant
// drift of 25 ns/s from 1 µs, like a free-running receiver oscillator.
func clockAt(i int) ClockSample {
	return ClockSample{
		Received: t0.Add(time.Duration(i) * time.Second), TOW: uint32(tow0 + i*1000),
		BiasNs: 1000 + 25*float64(i), DriftNsPerS: 25, TAccNs: 20, FAccPsPerS: 300,
	}
}

func runClock(t *testing.T, c *clockChecks, samples []ClockSample) map[string]Verdict {
	t.Helper()
	var last map[string]Verdict
	for _, s := range samples {
		if v := c.evaluate(s, true, DefaultProfile()); v != nil {
			last = v
		}
	}
	return last
}

func steadyClock(n int) []ClockSample {
	out := make([]ClockSample, n)
	for i := range out {
		out[i] = clockAt(i)
	}
	return out
}

func TestClockSteadyIsAssured(t *testing.T) {
	var c clockChecks
	early := runClock(t, &c, steadyClock(20))
	if early[CheckClockBiasDrift].State != Unavailable || early[CheckClockDriftRate].State != Unavailable {
		t.Fatalf("before the windows fill: %+v", early)
	}
	v := runClock(t, &c, steadyClock(130)[20:])
	for name, verdict := range v {
		if verdict.State != Assured {
			t.Errorf("%s = %+v, want assured", name, verdict)
		}
	}
}

// TestClockMillisecondAdjustmentIgnored: a receiver that keeps its clock within a
// millisecond of GNSS time steps it by whole milliseconds; that is not an anomaly.
func TestClockMillisecondAdjustmentIgnored(t *testing.T) {
	samples := steadyClock(60)
	for i := 40; i < len(samples); i++ {
		samples[i].BiasNs -= 1e6
	}
	var c clockChecks
	v := runClock(t, &c, samples)[CheckClockBiasDrift]
	if v.State != Assured || !slices.Contains(v.Reasons, ReasonClockAdjustment) || v.Metrics["clock_adjustments"] != 1 {
		t.Fatalf("millisecond adjustment: %+v, want assured and recorded", v)
	}
}

func TestClockBiasStepIsUnassured(t *testing.T) {
	samples := steadyClock(60)
	for i := 50; i < len(samples); i++ {
		samples[i].BiasNs += 400 // a 400 ns time step the drift does not explain
	}
	var c clockChecks
	v := runClock(t, &c, samples)[CheckClockBiasDrift]
	if v.State != Unassured || v.Metrics["divergence_ns"] < 399 {
		t.Fatalf("bias step: %+v, want unassured", v)
	}
}

func TestClockDriftRateStep(t *testing.T) {
	samples := steadyClock(130)
	for i := 100; i < len(samples); i++ {
		samples[i].DriftNsPerS += 30
		samples[i].BiasNs += 30 * float64(i-100) // keep bias consistent with the new drift
	}
	var c clockChecks
	v := runClock(t, &c, samples)
	if v[CheckClockDriftRate].State != Unassured {
		t.Fatalf("30 ns/s drift step: %+v, want unassured", v[CheckClockDriftRate])
	}
	if v[CheckClockBiasDrift].State != Assured {
		t.Fatalf("bias consistent with drift: %+v, want assured", v[CheckClockBiasDrift])
	}
}

// TestClockDriftQuantization: one whole-ns/s quantization step must not trip.
func TestClockDriftQuantization(t *testing.T) {
	samples := steadyClock(130)
	for i := 0; i < len(samples); i++ {
		if i%2 == 1 {
			samples[i].DriftNsPerS++
		}
	}
	var c clockChecks
	if v := runClock(t, &c, samples)[CheckClockDriftRate]; v.State != Assured {
		t.Fatalf("1 ns/s dither: %+v, want assured", v)
	}
}

func TestClockNoFixResets(t *testing.T) {
	var c clockChecks
	runClock(t, &c, steadyClock(50))
	v := c.evaluate(clockAt(50), false, DefaultProfile())
	if v[CheckClockBiasDrift].State != Unavailable || v[CheckClockBiasDrift].Reasons[0] != ReasonNoFix {
		t.Fatalf("without a fix: %+v", v)
	}
	if v := c.evaluate(clockAt(51), true, DefaultProfile()); v[CheckClockBiasDrift].State != Unavailable {
		t.Fatalf("history survived a fix loss: %+v", v)
	}
}

func TestClockDuplicateIgnored(t *testing.T) {
	var c clockChecks
	c.evaluate(clockAt(0), true, DefaultProfile())
	if v := c.evaluate(clockAt(0), true, DefaultProfile()); v != nil {
		t.Fatalf("duplicate evaluated: %+v", v)
	}
}

func TestStationReceiverRestartResetsClock(t *testing.T) {
	s := mustStation(t, StationProfile{})
	for i := 0; i < 50; i++ {
		s.ApplyStatus(ReceiverStatus{Received: t0.Add(time.Duration(i) * time.Second), SpoofState: SpoofNone, SinceStartMS: uint32(100_000 + i*1000), HaveStart: true})
		s.ApplyClock(clockAt(i))
	}
	s.ApplyStatus(ReceiverStatus{Received: t0.Add(50 * time.Second), SpoofState: SpoofNone, SinceStartMS: 2000, HaveStart: true})
	if len(s.clock.samples) != 0 {
		t.Fatalf("clock history kept across a receiver restart: %d samples", len(s.clock.samples))
	}
}
