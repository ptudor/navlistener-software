package state

import (
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/kepler"
)

// TestFeedSVTowMatchesPropagationEpoch guards tow/wn must be served from the
// propagation epoch that produced x_m/y_m/z_m (svState.posAt), not recomputed
// against "now" at feed-build time -- otherwise the (tow, position) pair can
// disagree by up to km (SV ECEF speeds are ~2.6-3.9 km/s, so every second of skew
// between the tick and the feed build costs km).
func TestFeedSVTowMatchesPropagationEpoch(t *testing.T) {
	st := New(4)
	tick := time.Unix(1_700_000_000, 0)
	st.Apply(gpsFrame(sf1Words(85), tick))
	st.Apply(gpsFrame(sf2Words(85, 205075516), tick))
	st.Apply(gpsFrame(sf3Words(85), tick))
	st.Propagate(tick)

	// Build the feed 5s after the propagate tick -- exactly the fix spec's
	// "FeedSVs at tick+5s reports the tick's tow" scenario.
	buildTime := tick.Add(5 * time.Second)
	svs := st.FeedSVs(buildTime)
	sv, ok := svs["G05@0"]
	if !ok {
		t.Fatal("G05@0 missing from the feed")
	}
	if sv.Tow == nil {
		t.Fatal("tow missing")
	}
	if sv.XM == nil || sv.YM == nil || sv.ZM == nil {
		t.Fatal("position missing")
	}

	wantTow := towFor(gnss.GPS, tick)
	if math.Abs(float64(*sv.Tow)-wantTow) > 0.5 {
		t.Errorf("tow = %d, want %.0f (the tick's tow, not %.0f at feed-build time+5s)",
			*sv.Tow, wantTow, towFor(gnss.GPS, buildTime))
	}

	// The served position must be exactly what kepler.Propagate(eph, reportedTow)
	// produces -- the definitive check that tow and position name the same solution.
	key := Key{G: gnss.GPS, Sv: 5, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	eph := sh.m[key].eph
	sh.mu.Unlock()

	wantPos, err := kepler.Propagate(eph, float64(*sv.Tow))
	if err != nil {
		t.Fatalf("kepler.Propagate(eph, reportedTow): %v", err)
	}
	if wantPos.X != *sv.XM || wantPos.Y != *sv.YM || wantPos.Z != *sv.ZM {
		t.Errorf("kepler.Propagate(eph, reportedTow) = %+v, want the served position (%v, %v, %v)",
			wantPos, *sv.XM, *sv.YM, *sv.ZM)
	}
}

// TestFeedSVPositionOmittedWhenStale guards staleness bound: a position
// frozen by a repeatedly-failing propagate tick (havePos stays true; posAt stops
// advancing) must eventually be omitted, along with its tow/wn, rather than served
// forever alongside an ever-fresher-looking tow.
func TestFeedSVPositionOmittedWhenStale(t *testing.T) {
	st := New(4)
	tick := time.Unix(1_700_000_000, 0)
	st.Apply(gpsFrame(sf1Words(85), tick))
	st.Apply(gpsFrame(sf2Words(85, 205075516), tick))
	st.Apply(gpsFrame(sf3Words(85), tick))
	st.Propagate(tick)

	fresh := st.FeedSVs(tick.Add(5 * time.Second))["G05@0"]
	if fresh.XM == nil || fresh.Tow == nil {
		t.Fatal("position/tow missing immediately after propagation")
	}

	stale := st.FeedSVs(tick.Add(posStaleBound + time.Second))["G05@0"]
	if stale.XM != nil || stale.YM != nil || stale.ZM != nil {
		t.Error("position still served long past posStaleBound with no further propagation")
	}
	if stale.Tow != nil || stale.Wn != nil {
		t.Error("tow/wn still served long past posStaleBound with no position to pair them with")
	}
}
