package integrity

import (
	"math"
	"testing"
	"time"
)

func TestUTCOffset(t *testing.T) {
	prof := DefaultProfile().UTCOffset
	for _, tc := range []struct {
		offset time.Duration
		want   State
	}{{-300 * time.Millisecond, Assured}, {3 * time.Second, Inconsistent}, {-10 * time.Second, Unassured}} {
		s := solutionAt(0, 0, 0, 0, 0, 0, 0)
		s.UTC = s.HostStamp.Add(tc.offset)
		if v := utcOffset(s, prof); v.State != tc.want || math.Abs(v.Metrics["offset_s"]-tc.offset.Seconds()) > 1e-9 {
			t.Errorf("offset %s: %+v, want %s", tc.offset, v, tc.want)
		}
	}
	s := solutionAt(0, 0, 0, 0, 0, 0, 0)
	s.HostStamp = time.Time{}
	if v := utcOffset(s, prof); v.State != Unavailable || v.Reasons[0] != ReasonNoHostStamp {
		t.Errorf("no stamp: %+v", v)
	}
	s = solutionAt(0, 0, 0, 0, 0, 0, 0)
	s.UTCValid = false
	if v := utcOffset(s, prof); v.State != Unavailable || v.Reasons[0] != ReasonNoUTC {
		t.Errorf("no UTC: %+v", v)
	}
}

// timingAt returns sample i of a crystal RTC running 20 ppm fast against GNSS, whose
// phase starts near the +0.5 s wrap so the test crosses it.
func timingAt(i int, stepNs float64) TimingSample {
	phase := math.Remainder(0.4999e9+20_000*float64(i)+stepNs, 1e9)
	return TimingSample{Received: t0.Add(time.Duration(i) * time.Second), UptimeMS: uint64(600_000 + i*1000), PhaseNs: &phase, RTCRunning: true}
}

func runPPS(p *ppsRTCPhase, from, to int, stepFrom int, stepNs float64) Verdict {
	var v Verdict
	for i := from; i < to; i++ {
		step := 0.0
		if i >= stepFrom {
			step = stepNs
		}
		v = p.evaluate(timingAt(i, step), true, DefaultProfile().PPSRTCPhase)
	}
	return v
}

func TestPPSRTCPhaseSteadyAcrossWrap(t *testing.T) {
	var p ppsRTCPhase
	if v := runPPS(&p, 0, 40, 1<<30, 0); v.State != Unavailable || v.Reasons[0] != ReasonFitWarmup {
		t.Fatalf("warm-up: %+v", v)
	}
	v := runPPS(&p, 40, 200, 1<<30, 0)
	if v.State != Assured || math.Abs(v.Metrics["phase_residual_ns"]) > 1 || math.Abs(v.Metrics["rtc_rate_ppm"]-20) > 1e-6 {
		t.Fatalf("steady 20 ppm RTC: %+v, want assured", v)
	}
}

func TestPPSRTCPhaseStep(t *testing.T) {
	var p ppsRTCPhase
	runPPS(&p, 0, 150, 1<<30, 0)
	if v := runPPS(&p, 150, 152, 150, 30_000); v.State != Inconsistent {
		t.Fatalf("30 µs step: %+v, want inconsistent", v)
	}
	var q ppsRTCPhase
	runPPS(&q, 0, 150, 1<<30, 0)
	if v := runPPS(&q, 150, 152, 150, -250_000); v.State != Unassured || v.Reasons[0] != ReasonPhaseStep {
		t.Fatalf("250 µs step: %+v, want unassured", v)
	}
}

func TestPPSRTCPhaseUnavailable(t *testing.T) {
	prof := DefaultProfile().PPSRTCPhase
	var p ppsRTCPhase
	runPPS(&p, 0, 100, 1<<30, 0)
	trimmed := timingAt(100, 0)
	trimmed.RTCTrim = 0x05
	if v := p.evaluate(trimmed, true, prof); v.State != Unavailable || v.Reasons[0] != ReasonRTCTrim {
		t.Fatalf("digital trim: %+v", v)
	}
	if len(p.points) != 0 {
		t.Fatal("history kept across an unusable sample")
	}
	if v := p.evaluate(timingAt(101, 0), false, prof); v.State != Unavailable || v.Reasons[0] != ReasonNoFix {
		t.Fatalf("no fix: %+v", v)
	}
	runPPS(&p, 102, 110, 1<<30, 0)
	restarted := timingAt(110, 0)
	restarted.UptimeMS = 5000
	if v := p.evaluate(restarted, true, prof); v.State != Unavailable || v.Reasons[0] != ReasonObserverReset {
		t.Fatalf("observer restart: %+v", v)
	}
	stopped := timingAt(111, 0)
	stopped.RTCRunning = false
	if v := p.evaluate(stopped, true, prof); v.State != Unavailable || v.Reasons[0] != ReasonRTCStopped {
		t.Fatalf("RTC stopped: %+v", v)
	}
}

// TestStationPPSNeedsSolution: without any solution the GNSS pulse may be free-running,
// so the station never evaluates the phase.
func TestStationPPSNeedsSolution(t *testing.T) {
	s := mustStation(t, StationProfile{})
	for i := 0; i < 100; i++ {
		s.ApplyTiming(timingAt(i, 0))
	}
	if r := checkResult(t, s.Assess(t0.Add(100*time.Second)), CheckPPSRTCPhase); r.State != Unavailable {
		t.Fatalf("phase without a solution: %+v", r)
	}
}
