package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

// TestPosIODStampedAtPropagation guards the regression fix verification follow-up: a
// feed built between an ephemeris-changeover apply and the next Propagate tick
// serves the NEW data set's iod but the OLD set's position — PosIOD must label
// the position with the set that actually produced it, or the cross-signal
// check pairs a changeover delta under one IOD label and reads it as
// divergence.
func TestPosIODStampedAtPropagation(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	st.Propagate(now)

	// A new data set (IODE 86) applies; no Propagate has run yet.
	st.Apply(gpsFrame(sf1Words(86), now.Add(time.Second)))
	st.Apply(gpsFrame(sf2Words(86, 205075516), now.Add(time.Second)))
	st.Apply(gpsFrame(sf3Words(86), now.Add(time.Second)))

	sv := st.FeedSVs(now.Add(2 * time.Second))["G05@0"]
	if sv.IOD == nil || *sv.IOD != 86 {
		t.Fatalf("served iod = %v, want the current set 86", sv.IOD)
	}
	if sv.PosIOD == nil || *sv.PosIOD != 85 {
		t.Fatalf("PosIOD = %v, want 85 (the set that produced the position)", sv.PosIOD)
	}

	st.Propagate(now.Add(3 * time.Second))
	sv = st.FeedSVs(now.Add(4 * time.Second))["G05@0"]
	if sv.PosIOD == nil || *sv.PosIOD != 86 {
		t.Fatalf("PosIOD after re-propagation = %v, want 86", sv.PosIOD)
	}
}

// TestWeekForBeiDouConsistentAcrossWeekBoundary guards weekFor and
// towFor must derive from the same BDT-shifted (GPST − 14 s) seconds value, or
// a consumer reconstructing an absolute BDT instant from the served (wn, tow)
// pair is a full week off during the 14 s each GPS week where GPS tow ∈ [0, 14)
// — the unshifted week has already rolled over while the shifted tow still
// reports the tail of the previous BDT week. dt=5 sits squarely in that window;
// the other offsets bracket it to confirm nothing else regressed.
// TestTowWeekForMatchLegacyReduction guards refactor: gpsTOW/towFor/
// weekFor now reduce through gnsstime's single epoch table, and must reproduce
// the previous inline formulas exactly — re-derived here from first principles
// (the pre-refactor literals), swept across a GPS week boundary and the BDT
// 14 s window.
func TestTowWeekForMatchLegacyReduction(t *testing.T) {
	base := int64(2000)*weekSeconds - gpsUTCOffset + gpsEpochUnix
	for _, dt := range []int64{-100, -14, -1, 0, 1, 5, 13, 14, 20, 3600, 400000} {
		now := time.Unix(base+dt, 0)
		gps := now.Unix() - gpsEpochUnix + gpsUTCOffset

		wantGPSTow := float64(((gps % weekSeconds) + weekSeconds) % weekSeconds)
		if got := gpsTOW(now); got != wantGPSTow {
			t.Errorf("dt=%d: gpsTOW = %.3f, want %.3f", dt, got, wantGPSTow)
		}
		for _, g := range []gnss.GNSSID{gnss.GPS, gnss.QZSS, gnss.Galileo} {
			if got := towFor(g, now); got != wantGPSTow {
				t.Errorf("dt=%d: towFor(%v) = %.3f, want %.3f", dt, g, got, wantGPSTow)
			}
			if wn, ok := weekFor(g, now); !ok || int64(wn) != gps/weekSeconds {
				t.Errorf("dt=%d: weekFor(%v) = %d/%v, want %d", dt, g, wn, ok, gps/weekSeconds)
			}
		}
		wantBDTTow := wantGPSTow - 14
		if wantBDTTow < 0 {
			wantBDTTow += weekSeconds
		}
		if got := towFor(gnss.BeiDou, now); got != wantBDTTow {
			t.Errorf("dt=%d: towFor(BeiDou) = %.3f, want %.3f", dt, got, wantBDTTow)
		}
		if wn, ok := weekFor(gnss.BeiDou, now); !ok || int64(wn) != (gps-14)/weekSeconds-1356 {
			t.Errorf("dt=%d: weekFor(BeiDou) = %d/%v, want %d", dt, wn, ok, (gps-14)/weekSeconds-1356)
		}
		if _, ok := weekFor(gnss.GLONASS, now); ok {
			t.Errorf("dt=%d: weekFor(GLONASS) ok=true, want false", dt)
		}
	}
}

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
