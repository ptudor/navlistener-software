package state

import (
	"testing"
	"time"
)

// TestFeedSBASExcludesStaleEntries guards an SBAS PRN unseen past
// sbasStaleAfter (a decommissioned/dark GEO) must be omitted from the feed,
// not served forever with an ever-growing last_seen_s.
func TestFeedSBASExcludesStaleEntries(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.sbas[133] = &sbasState{prn: 133, provider: "WAAS", lastType: 1, lastSeen: now}

	if got := s.FeedSBAS(now.Add(sbasStaleAfter + time.Minute)); len(got) != 0 {
		t.Errorf("stale SBAS PRN still present: %+v", got)
	}
	if got := s.FeedSBAS(now.Add(time.Minute)); len(got) != 1 {
		t.Errorf("fresh SBAS PRN missing: %+v", got)
	}
}
