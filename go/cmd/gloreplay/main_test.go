package main

import (
	"math"
	"strings"
	"testing"

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
