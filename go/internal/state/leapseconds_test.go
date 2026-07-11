package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

// TestSetLeapSecondsUpdatesFeedAndPropagationEpochs guards the
// compiled-in ΔtLS (gpsUTCOffset) drives both the served leap_seconds counter
// and every wall-clock→GPS/BDT time-of-week conversion. An interim config
// override (state.SetLeapSeconds) must move both consistently -- a leap event
// applied only to one but not the other would desync the served tow from the
// leap_seconds it's supposed to be computed under.
func TestSetLeapSecondsUpdatesFeedAndPropagationEpochs(t *testing.T) {
	orig := gpsUTCOffset
	defer func() { gpsUTCOffset = orig }()

	SetLeapSeconds(18)
	s := New(1)
	now := time.Unix(1_700_000_000, 0)

	before := s.FeedGlobal(now)
	if before.LeapSeconds != 18 {
		t.Fatalf("leap_seconds = %d, want 18", before.LeapSeconds)
	}
	towBefore := towFor(gnss.GPS, now)

	SetLeapSeconds(19) // a hypothetical future leap second
	after := s.FeedGlobal(now)
	if after.LeapSeconds != 19 {
		t.Errorf("leap_seconds after override = %d, want 19", after.LeapSeconds)
	}
	towAfter := towFor(gnss.GPS, now)
	if towAfter-towBefore != 1 {
		t.Errorf("tow shift = %v, want exactly +1s for a +1s ΔtLS override", towAfter-towBefore)
	}

	// n <= 0 must be a no-op (keeps whatever is currently set), not a reset to 0.
	SetLeapSeconds(0)
	if gpsUTCOffset != 19 {
		t.Errorf("SetLeapSeconds(0) must be a no-op, gpsUTCOffset = %d, want 19", gpsUTCOffset)
	}
}
