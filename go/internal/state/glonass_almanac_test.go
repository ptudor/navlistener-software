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
	now := time.Now()
	s.gloNA = 615
	s.gloAlmanac[7] = gloAlmSlot{entry: icdAlmanac(7), lastSeen: now}

	// Seed an observed (precise) fix for slot 7, as the ephemeris/propagate stage would.
	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := s.shardFor(key)
	sh.m[key] = &svState{
		key:        key,
		havePos:    true,
		haveGloEph: true,
		pos:        gnss.ECEF{X: 1.1e7, Y: 2.2e7, Z: 5.0e6},
		posAt:      now,
		lastSeen:   now,
	}

	out := s.FeedAlmanac(now)
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
	if ent.InclinationRad < 62*math.Pi/180 || ent.InclinationRad > 66*math.Pi/180 {
		t.Errorf("observed inclination = %v rad, want fresh almanac metadata", ent.InclinationRad)
	}
	if ent.T0e != int(icdAlmanac(7).Alm.Tlambda) {
		t.Errorf("observed t0e = %d, want almanac Tlambda %d", ent.T0e, int(icdAlmanac(7).Alm.Tlambda))
	}
}

// TestFeedGlonassAlmanacOperable guards the almanac CnA ground-segment
// health flag — the ONLY broadcast health surface for an out-of-view slot — was
// decoded, stored, and then dropped at feed-build time, so a slot ground control
// had flagged inoperable served indistinguishably from a healthy one. The feed
// must carry it re-normalized (broadcast polarity is inverted: Cn = 0 means
// malfunction, GLO-ICD-5.1 §5.3), an inoperable slot's entry must STILL be
// served (the flag was missing, not the position), and observed entries get the
// flag too via the regression fix metadata merge.
func TestFeedGlonassAlmanacOperable(t *testing.T) {
	s := New(4)
	now := time.Now()
	s.gloNA = 615
	healthy := icdAlmanac(7) // Cn = 1
	sick := icdAlmanac(9)
	sick.Cn = 0
	s.gloAlmanac[7] = gloAlmSlot{entry: healthy, lastSeen: now}
	s.gloAlmanac[9] = gloAlmSlot{entry: sick, lastSeen: now}

	out := s.FeedAlmanac(now)
	h, ok := out["R07"]
	if !ok || h.Operable == nil {
		t.Fatalf("R07: entry/operable missing (%+v)", h)
	}
	if !*h.Operable {
		t.Error("R07 Cn=1 must serve operable=true")
	}
	sv9, ok := out["R09"]
	if !ok {
		t.Fatal("R09 (Cn=0) missing — an inoperable slot's entry must still be served")
	}
	if sv9.Operable == nil || *sv9.Operable {
		t.Errorf("R09 Cn=0 must serve operable=false, got %+v", sv9.Operable)
	}

	// Observed entry: precise position wins, but the ground-segment flag rides along.
	key := Key{G: gnss.GLONASS, Sv: 9, Sig: 0}
	sh := s.shardFor(key)
	sh.m[key] = &svState{
		key: key, havePos: true, haveGloEph: true,
		pos: gnss.ECEF{X: 1.1e7, Y: 2.2e7, Z: 5.0e6}, posAt: now, lastSeen: now,
	}
	obs := s.FeedAlmanac(now)["R09"]
	if !obs.Observed {
		t.Fatal("slot 9 must be Observed once in view")
	}
	if obs.Operable == nil || *obs.Operable {
		t.Errorf("observed R09 must still carry operable=false, got %+v", obs.Operable)
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
