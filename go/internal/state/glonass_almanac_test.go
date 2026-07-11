package state

import (
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/glonass"
)

// icdAlmanac is the validated GLONASS ICD Ed. 5.1 §A.3.2.3 element set (SI-scaled),
// reused as a known-good almanac to drive the feed wiring.
func icdAlmanac(slot int) frame.GLONASSAlmanacEntry {
	return frame.GLONASSAlmanacEntry{
		Cn: 1,
		Alm: glonass.Almanac{
			NA:        615,
			Slot:      slot,
			FreqCh:    3,
			Lambda:    -0.189986229 * math.Pi,
			Tlambda:   27122.09375,
			DeltaI:    0.011929512 * math.Pi,
			DeltaT:    -2655.76171875,
			DeltaTdot: 0.000549316,
			Ecc:       0.001482010,
			Omega:     0.440277100 * math.Pi,
		},
	}
}

// TestFeedGlonassAlmanacOutOfView confirms a decoded GLONASS almanac surfaces an
// almanac-only SV in the almanac feed (docs/OUTPUT.md §1.4): Observed=false, on the
// orbital shell, with the GLONASS-only lambda_na / t_lambda_na fields present.
func TestFeedGlonassAlmanacOutOfView(t *testing.T) {
	s := New(4)
	s.gloNA = 615
	s.gloAlmanac[7] = gloAlmSlot{entry: icdAlmanac(7), lastSeen: time.Now()}

	out := s.FeedAlmanac(time.Now())
	ent, ok := out["R07"]
	if !ok {
		t.Fatal("R07 not present in the almanac feed")
	}
	if ent.Observed {
		t.Error("almanac-only SV should be Observed=false")
	}
	if ent.GnssID != int(gnss.GLONASS) {
		t.Errorf("gnssid = %d, want GLONASS", ent.GnssID)
	}
	r := math.Sqrt(ent.EcefXM*ent.EcefXM+ent.EcefYM*ent.EcefYM+ent.EcefZM*ent.EcefZM) / 1000
	if r < 25000 || r > 26000 {
		t.Errorf("radius %.0f km off the GLONASS shell", r)
	}
	if ent.LambdaNA == nil || ent.TLambdaNA == nil {
		t.Error("GLONASS lambda_na / t_lambda_na must be present")
	}
	if math.Abs(*ent.TLambdaNA-27122.09375) > 1e-3 {
		t.Errorf("t_lambda_na = %v, want the broadcast tλ", *ent.TLambdaNA)
	}
}

// TestFeedGlonassAlmanacObservedWins confirms an in-view SV keeps its precise
// broadcast-ephemeris entry (Observed=true) rather than being overwritten by the coarse
// almanac for the same slot.
func TestFeedGlonassAlmanacObservedWins(t *testing.T) {
	s := New(4)
	s.gloNA = 615
	s.gloAlmanac[7] = gloAlmSlot{entry: icdAlmanac(7), lastSeen: time.Now()}

	// Seed an observed (precise) fix for slot 7, as the ephemeris/propagate stage would.
	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := s.shardFor(key)
	sh.m[key] = &svState{
		key:        key,
		havePos:    true,
		haveGloEph: true,
		pos:        gnss.ECEF{X: 1.1e7, Y: 2.2e7, Z: 5.0e6},
		lastSeen:   time.Now(),
	}

	out := s.FeedAlmanac(time.Now())
	ent := out["R07"]
	if !ent.Observed {
		t.Fatal("in-view slot 7 must keep its Observed=true precise entry")
	}
	if ent.LambdaNA != nil {
		t.Error("precise entry should not carry the almanac lambda_na")
	}
	if ent.EcefXM != 1.1e7 {
		t.Errorf("precise position was overwritten by the almanac: x=%v", ent.EcefXM)
	}
}

// TestFeedGlonassAlmanacAgesOutStaleSlot guards a GLONASS almanac slot
// unseen past gloAlmanacStaleAfter (a decommissioned/ghost slot) must not be
// served forever, propagated to an ever-more-speculative position at the
// current day number.
func TestFeedGlonassAlmanacAgesOutStaleSlot(t *testing.T) {
	s := New(4)
	s.gloNA = 615
	now := time.Now()
	s.gloAlmanac[7] = gloAlmSlot{entry: icdAlmanac(7), lastSeen: now.Add(-(gloAlmanacStaleAfter + time.Hour))}

	if out := s.FeedAlmanac(now); len(out) != 0 {
		t.Errorf("stale GLONASS almanac slot still served: %+v", out)
	}

	s.gloAlmanac[7] = gloAlmSlot{entry: icdAlmanac(7), lastSeen: now}
	out := s.FeedAlmanac(now)
	if _, ok := out["R07"]; !ok {
		t.Error("fresh GLONASS almanac slot missing")
	}
}
