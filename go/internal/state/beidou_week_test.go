package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

// TestWeekForBeiDouConsistentAcrossWeekBoundary guards weekFor and
// towFor must derive from the same BDT-shifted (GPST − 14 s) seconds value, or
// a consumer reconstructing an absolute BDT instant from the served (wn, tow)
// pair is a full week off during the 14 s each GPS week where GPS tow ∈ [0, 14)
// — the unshifted week has already rolled over while the shifted tow still
// reports the tail of the previous BDT week. dt=5 sits squarely in that window;
// the other offsets bracket it to confirm nothing else regressed.
func TestWeekForBeiDouConsistentAcrossWeekBoundary(t *testing.T) {
	// base is an instant with gpsTOW == 0 (a GPS week boundary), 2000 GPS weeks
	// past the GPS epoch -- comfortably past the BDT epoch (1356 weeks in).
	base := int64(2000)*weekSeconds - gpsUTCOffset + gpsEpochUnix

	for _, dt := range []int64{-20, -5, 0, 5, 13, 14, 20, 100} {
		now := time.Unix(base+dt, 0)
		wn, ok := weekFor(gnss.BeiDou, now)
		if !ok {
			t.Fatalf("dt=%d: weekFor(BeiDou) ok=false", dt)
		}
		tow := towFor(gnss.BeiDou, now)

		gps := now.Unix() - gpsEpochUnix + gpsUTCOffset
		wantShifted := gps - 14
		gotShifted := int64(wn+1356)*weekSeconds + int64(tow)
		if gotShifted != wantShifted {
			t.Errorf("dt=%d: (wn=%d, tow=%.0f) reconstructs to shifted-seconds %d, want %d (off by %d = %.0fs)",
				dt, wn, tow, gotShifted, wantShifted, gotShifted-wantShifted, float64(gotShifted-wantShifted))
		}
	}
}
