package main

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

// TestTbWatchDetectsChangeover walks one SV through the age sequence a real
// replay produces: a first ephemeris (no previous set, so not a changeover), the
// age growing while that set stays current, then the drop that marks a new tb.
func TestTbWatchDetectsChangeover(t *testing.T) {
	w := newTbWatch()
	steps := []struct {
		age  float64
		want bool
		why  string
	}{
		{age: 12.0, want: false, why: "first observation has no previous set to difference against"},
		{age: 14.0, want: false, why: "age growing within the same set"},
		{age: 29.5, want: false, why: "still the same set, near the update interval"},
		{age: 0.5, want: true, why: "age fell ~29 min: a new tb was adopted"},
		{age: 2.0, want: false, why: "growing again within the new set"},
		{age: 1.5, want: false, why: "0.5 min dip is below the jitter floor, not a changeover"},
		{age: 1.2, want: false, why: "another sub-floor dip; the floor applies per step, not cumulatively"},
		{age: 0.2, want: true, why: "1.0 min fall meets the floor exactly and counts"},
	}
	for i, s := range steps {
		if got := w.observe("R07@0", s.age); got != s.want {
			t.Errorf("step %d (age %.1f): got %v, want %v — %s", i, s.age, got, s.want, s.why)
		}
	}
}

// TestTbWatchPerSV pins that SVs are tracked independently: one satellite's
// changeover must not consume or mask another's, which a shared cursor would do.
func TestTbWatchPerSV(t *testing.T) {
	w := newTbWatch()
	w.observe("R01@0", 30.0)
	w.observe("R02@0", 5.0)
	// R02's low age must not read as a drop for R01.
	if w.observe("R01@0", 29.6) {
		t.Error("R01 fell only 0.4 min (below the floor) but registered a changeover")
	}
	if !w.observe("R01@0", 1.0) {
		t.Error("R01 genuine changeover missed")
	}
	if w.observe("R02@0", 6.0) {
		t.Error("R02 age grew; must not register a changeover")
	}
	if !w.observe("R02@0", 0.1) {
		t.Error("R02 genuine changeover missed")
	}
}

// TestPercentileNearestRank pins the nearest-rank definition: every reported
// figure must be a value that was actually measured, so a threshold argument
// never rests on an interpolated number no satellite produced.
func TestPercentileNearestRank(t *testing.T) {
	v := []float64{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	cases := []struct{ p, want float64 }{
		{50, 5},   // rank round(10*0.5)=5 -> v[4]
		{95, 10},  // rank round(9.5)=10 -> v[9]
		{100, 10}, // clamped to the last element
		{0, 1},    // clamped to the first element
	}
	for _, c := range cases {
		if got := percentile(v, c.p); got != c.want {
			t.Errorf("percentile(p%v) = %v, want %v", c.p, got, c.want)
		}
	}
	if got := percentile(nil, 50); got != 0 {
		t.Errorf("percentile of empty = %v, want 0", got)
	}
	if got := percentile([]float64{42}, 50); got != 42 {
		t.Errorf("percentile of single sample = %v, want 42", got)
	}
}

// TestPercentileIsAMeasuredValue guards the property the nearest-rank choice
// exists for, across a spread that would tempt an interpolating implementation.
func TestPercentileIsAMeasuredValue(t *testing.T) {
	v := []float64{0.02, 0.31, 1.44, 1.46, 9.9}
	for _, p := range []float64{10, 25, 50, 75, 90, 95, 99} {
		got := percentile(v, p)
		found := false
		for _, x := range v {
			if math.Abs(got-x) < 1e-12 {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("percentile(p%v) = %v is not one of the measured values %v", p, got, v)
		}
	}
}

// TestWordsFromRawRoundTrip pins the reconstruction against the writer it must
// reverse: ingest.RawFrame.RawBytes serialises nav words big-endian back to back,
// and a store replay has only those bytes to rebuild the frame from. A disagreement
// here would not fail loudly — it would decode into plausible-looking garbage.
func TestWordsFromRawRoundTrip(t *testing.T) {
	words := []uint32{0x01020304, 0xDEADBEEF, 0x00000000, 0xFFFFFFFF}
	f := &ingest.RawFrame{Words: words}
	got := wordsFromRaw(f.RawBytes())
	if len(got) != len(words) {
		t.Fatalf("got %d words, want %d", len(got), len(words))
	}
	for i := range words {
		if got[i] != words[i] {
			t.Errorf("word %d = %#08x, want %#08x", i, got[i], words[i])
		}
	}
}

// TestWordsFromRawDropsPartialWord pins that a trailing partial word is dropped
// rather than zero-extended. Zero-extending would invent bits the satellite never
// broadcast and hand them to a CRC check that might even pass.
func TestWordsFromRawDropsPartialWord(t *testing.T) {
	if got := wordsFromRaw([]byte{1, 2, 3, 4, 5, 6}); len(got) != 1 || got[0] != 0x01020304 {
		t.Errorf("wordsFromRaw(6 bytes) = %#v, want exactly [0x01020304]", got)
	}
	if got := wordsFromRaw(nil); len(got) != 0 {
		t.Errorf("wordsFromRaw(nil) = %#v, want empty", got)
	}
}

// TestPendSetCompletesDeferral walks the designed pendClk shape: a changeover
// defers (clockless adoption), string 4 is sighted ~2 s later, and the feed
// serves the completed value in the same apply — the sample must carry the late
// value, both lags, and the completed outcome, and the pend must be consumed so
// later served values (every subsequent frame re-serves the completed disco)
// cannot re-attribute to it.
func TestPendSetCompletesDeferral(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	samples := []changeover{{SV: "R07@0"}}
	p := newPendSet()
	p.arm("R07@0", 0, t0)
	p.sawS4("R07@0", t0.Add(2*time.Second))
	p.complete(samples, "R07@0", 42.5, t0.Add(2*time.Second))

	s := samples[0]
	if s.TimeOutcome != outcomeCompleted {
		t.Fatalf("outcome = %q, want %q", s.TimeOutcome, outcomeCompleted)
	}
	if s.TimeNsLate == nil || *s.TimeNsLate != 42.5 {
		t.Errorf("TimeNsLate = %v, want 42.5", s.TimeNsLate)
	}
	if s.TimeLagS == nil || math.Abs(*s.TimeLagS-2.0) > 1e-9 {
		t.Errorf("TimeLagS = %v, want 2.0", s.TimeLagS)
	}
	if s.S4LagS == nil || math.Abs(*s.S4LagS-2.0) > 1e-9 {
		t.Errorf("S4LagS = %v, want 2.0", s.S4LagS)
	}
	// The pend was consumed: a later served value must not rewrite the sample.
	p.complete(samples, "R07@0", 99.9, t0.Add(30*time.Second))
	if *samples[0].TimeNsLate != 42.5 {
		t.Errorf("consumed pend re-completed: TimeNsLate = %v, want 42.5", *samples[0].TimeNsLate)
	}
}

// TestPendSetFirstS4LagWins pins that s4LagS records the FIRST sighting: string 4
// repeats every ~30 s frame (and arrives twice per frame with both bands), and
// the delay question is "how soon after the changeover did one arrive".
func TestPendSetFirstS4LagWins(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	samples := []changeover{{SV: "R07@0"}}
	p := newPendSet()
	p.arm("R07@0", 0, t0)
	p.sawS4("R07@0", t0.Add(2*time.Second))
	p.sawS4("R07@0", t0.Add(32*time.Second))
	p.supersede(samples, "R07@0")
	if samples[0].S4LagS == nil || math.Abs(*samples[0].S4LagS-2.0) > 1e-9 {
		t.Errorf("S4LagS = %v, want 2.0 (first sighting)", samples[0].S4LagS)
	}
}

// TestPendSetSupersedeSeparatesReceptionFromStateFailure pins the diagnostic
// distinction the whole extension exists for: a superseded pend WITHOUT a
// string-4 sighting is a reception gap, one WITH a sighting is a completion
// that should have fired — S4LagS present/absent is that evidence.
func TestPendSetSupersedeSeparatesReceptionFromStateFailure(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	samples := []changeover{{SV: "R03@0"}, {SV: "R04@0"}}
	p := newPendSet()
	p.arm("R03@0", 0, t0)
	p.arm("R04@0", 1, t0)
	p.sawS4("R04@0", t0.Add(4*time.Second))
	p.supersede(samples, "R03@0")
	p.supersede(samples, "R04@0")

	if samples[0].TimeOutcome != outcomeSuperseded || samples[1].TimeOutcome != outcomeSuperseded {
		t.Fatalf("outcomes = %q/%q, want both %q", samples[0].TimeOutcome, samples[1].TimeOutcome, outcomeSuperseded)
	}
	if samples[0].S4LagS != nil {
		t.Errorf("R03 S4LagS = %v, want nil (no string 4 seen: reception gap)", *samples[0].S4LagS)
	}
	if samples[1].S4LagS == nil {
		t.Error("R04 S4LagS = nil, want set (string 4 seen but never completed: state-side signal)")
	}
	if samples[0].TimeNsLate != nil || samples[1].TimeNsLate != nil {
		t.Error("superseded samples must not carry a late value")
	}
}

// TestPendSetFinishCensors pins that end-of-replay finalization marks pending
// deferrals as censored rather than leaving an empty outcome (which would read
// as unaccounted in the resolution tally) or counting them as failures.
func TestPendSetFinishCensors(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	samples := []changeover{{SV: "R11@0"}, {SV: "R12@0"}}
	p := newPendSet()
	p.arm("R11@0", 0, t0)
	p.arm("R12@0", 1, t0)
	p.sawS4("R12@0", t0.Add(6*time.Second))
	p.finish(samples)
	if samples[0].TimeOutcome != outcomeEOF || samples[1].TimeOutcome != outcomeEOF {
		t.Fatalf("outcomes = %q/%q, want both %q", samples[0].TimeOutcome, samples[1].TimeOutcome, outcomeEOF)
	}
	if samples[1].S4LagS == nil {
		t.Error("censored pend lost its string-4 sighting evidence")
	}
	// finish drained the set: a second call must not touch the samples again.
	samples[0].TimeOutcome = "sentinel"
	p.finish(samples)
	if samples[0].TimeOutcome != "sentinel" {
		t.Error("finish ran twice over the same pend")
	}
}

// TestPendSetUnarmedNoOps pins that sightings and completions for an SV with no
// pending deferral are ignored: after a completion (or an immediate changeover)
// the feed keeps serving a non-nil time_disco_ns on every subsequent frame, and
// each of those reads reaches the pend set.
func TestPendSetUnarmedNoOps(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0)
	samples := []changeover{{SV: "R07@0", TimeOutcome: outcomeImmediate}}
	p := newPendSet()
	p.sawS4("R07@0", t0)
	p.complete(samples, "R07@0", 13.5, t0)
	p.supersede(samples, "R07@0")
	s := samples[0]
	if s.TimeOutcome != outcomeImmediate || s.TimeNsLate != nil || s.TimeLagS != nil || s.S4LagS != nil {
		t.Errorf("unarmed no-ops mutated the sample: %+v", s)
	}
}

// TestSvStringsCount pins the bucket mapping, including the 5..15 lump: the
// delivery diagnosis rests on s1..s4 being individually distinguishable while
// almanac traffic stays out of their columns.
func TestSvStringsCount(t *testing.T) {
	var c svStrings
	for num, times := range map[int]int{1: 3, 2: 4, 3: 5, 4: 2, 5: 7, 15: 1} {
		for i := 0; i < times; i++ {
			c.count(num)
		}
	}
	if c.s1 != 3 || c.s2 != 4 || c.s3 != 5 || c.s4 != 2 {
		t.Errorf("s1..s4 = %d/%d/%d/%d, want 3/4/5/2", c.s1, c.s2, c.s3, c.s4)
	}
	if c.other != 8 {
		t.Errorf("other = %d, want 8 (strings 5 and 15)", c.other)
	}
}

// TestRedactDSN pins that a password never reaches stdout: QA output gets pasted
// into review docs and tickets.
func TestRedactDSN(t *testing.T) {
	got := redactDSN("postgres://app:s3cret@db.invalid:5432/nav")
	if strings.Contains(got, "s3cret") {
		t.Errorf("redactDSN leaked the password: %q", got)
	}
	if !strings.Contains(got, "db.invalid") {
		t.Errorf("redactDSN dropped the host, leaving nothing identifiable: %q", got)
	}
}
