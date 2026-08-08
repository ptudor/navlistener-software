package ingest

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

// TestReplayUBXMatchesScanUBX pins that the exported replay entry point is the
// live path's scanner and not a second parser: the same capture through both
// must yield byte-identical frame sequences. If ReplayUBX ever grows its own
// decode, the calibration numbers it produces stop describing what the collector
// actually computes, which is the whole point of replaying.
func TestReplayUBXMatchesScanUBX(t *testing.T) {
	data, err := os.ReadFile("testdata/glo_superframe_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}

	var direct, replayed []*RawFrame
	_ = scanUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { direct = append(direct, f) }, func(string) {})
	if err := ReplayUBX(bytes.NewReader(data), "cap", fixedTime,
		func(f *RawFrame) { replayed = append(replayed, f) }, func(string) {}); err != nil {
		t.Fatalf("ReplayUBX: %v", err)
	}

	if len(direct) == 0 {
		t.Fatal("capture yielded no frames through scanUBX")
	}
	if len(replayed) != len(direct) {
		t.Fatalf("ReplayUBX emitted %d frames, scanUBX %d", len(replayed), len(direct))
	}
	for i := range direct {
		a, b := direct[i], replayed[i]
		if a.GnssID != b.GnssID || a.SvID != b.SvID || a.SigID != b.SigID || a.FreqID != b.FreqID {
			t.Fatalf("frame %d identity differs: %+v vs %+v", i, a, b)
		}
		if len(a.Words) != len(b.Words) {
			t.Fatalf("frame %d word count differs: %d vs %d", i, len(a.Words), len(b.Words))
		}
		for w := range a.Words {
			if a.Words[w] != b.Words[w] {
				t.Fatalf("frame %d word %d differs: %#x vs %#x", i, w, a.Words[w], b.Words[w])
			}
		}
	}
}

// TestReplayUBXCleanEOF pins that exhausting a capture is not reported as an
// error — including a capture truncated mid-message, which is what a recording
// stopped by a signal looks like. A replay tool must be able to distinguish "the
// file ended" from "the read failed".
func TestReplayUBXCleanEOF(t *testing.T) {
	data, err := os.ReadFile("testdata/glo_superframe_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	if err := ReplayUBX(bytes.NewReader(data), "cap", fixedTime, func(*RawFrame) {}, func(string) {}); err != nil {
		t.Errorf("whole capture: got %v, want nil", err)
	}
	// Cut mid-message: the tail is a partial UBX frame.
	if n := len(data) - 7; n > 0 {
		if err := ReplayUBX(bytes.NewReader(data[:n]), "cap", fixedTime, func(*RawFrame) {}, func(string) {}); err != nil {
			t.Errorf("truncated capture: got %v, want nil", err)
		}
	}
}

// TestReplayUBXPropagatesReadError pins the other side of the EOF rule: a
// genuine read failure must surface, not be swallowed as end-of-capture.
func TestReplayUBXPropagatesReadError(t *testing.T) {
	want := errors.New("disk went away")
	err := ReplayUBX(failingReader{err: want}, "cap", fixedTime, func(*RawFrame) {}, func(string) {})
	if !errors.Is(err, want) {
		t.Errorf("got %v, want %v", err, want)
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }

// TestReplayUBXAdvancesSyntheticClock pins that now is consulted per frame in
// stream order, which is how a replay tool imposes a synthetic receive clock on
// a capture that carries no timestamps.
func TestReplayUBXAdvancesSyntheticClock(t *testing.T) {
	data, err := os.ReadFile("testdata/glo_superframe_capture.ubx")
	if err != nil {
		t.Skipf("no capture fixture: %v", err)
	}
	base := time.Unix(1_700_000_000, 0).UTC()
	tick := 0
	clock := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * time.Second)
	}
	var stamps []time.Time
	var glo int
	if err := ReplayUBX(bytes.NewReader(data), "cap", clock, func(f *RawFrame) {
		stamps = append(stamps, f.Recv)
		if f.GnssID == gnss.GLONASS {
			glo++
		}
	}, func(string) {}); err != nil {
		t.Fatalf("ReplayUBX: %v", err)
	}
	if len(stamps) < 2 {
		t.Fatalf("only %d frames stamped", len(stamps))
	}
	for i := 1; i < len(stamps); i++ {
		if !stamps[i].After(stamps[i-1]) {
			t.Fatalf("stamp %d (%v) not after %d (%v) — clock not advancing per frame",
				i, stamps[i], i-1, stamps[i-1])
		}
	}
	if glo == 0 {
		t.Error("GLONASS superframe capture yielded no GLONASS frames")
	}
}
