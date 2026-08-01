package state

import (
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

// TestGLONASSPropagateEphAgeCap guards regression fix (the GLONASS twin of regression fix): a
// GLONASS SV whose ephemeris apply time (gloEphAt) is beyond gloPropagateMaxEphAge
// must stop having positions propagated. Before the fix, the frozen PZ-90 state was
// re-integrated and re-stamped fresh (posAt = now) on every tick with no validity
// gate at all — through the km-wrong 0.5–2 h regime and, past 12 h, into the
// EphAgeDay ±half-day wrap, where the served eph_age_m flips negative (un-firing
// the eph_aged detector on a worsening SV) and the RK4 integrates ~12 h backward.
// The fix: positions stop at the cap, the served position expires via
// posStaleBound, and eph_age_m switches to the un-wrappable wall-clock age so
// eph_aged latches and stays latched.
func TestGLONASSPropagateEphAgeCap(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	// One coherent frame: strings 1/2/3. tb index 5 (= 4500 s of Moscow day) sits
	// ~100 s from t0's actual MT time-of-day (tod(t0) = 4400 s), i.e. a REALISTIC
	// current set — the serve path now also gates on the broadcast-time interval
	// tk (gloServeMaxTk, the frozen-tb guard), so a test tb hours away from the
	// wall-clock TOD would be rejected as broadcast-stale, exactly as designed.
	// Coords large enough for the propagator's degenerate-state guard.
	s.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t0))
	s.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 5, t0.Add(2*time.Second)))
	s.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	// Fresh: position served, day-wrapped age served (bounded by the ±720 min wrap).
	s.Propagate(t0.Add(5 * time.Second))
	sv, ok := s.FeedSVs(t0.Add(5 * time.Second))["R07@0"]
	if !ok {
		t.Fatal("R07@0 missing from svs feed after a coherent triple")
	}
	if sv.XM == nil {
		t.Fatal("fresh GLONASS ephemeris: no position served")
	}
	if sv.EphAgeM == nil {
		t.Fatal("fresh GLONASS ephemeris: eph_age_m absent")
	}

	// Inside the cap (30 min): still served — routine extrapolation is by design.
	t1 := t0.Add(30 * time.Minute)
	s.Propagate(t1)
	if sv = s.FeedSVs(t1)["R07@0"]; sv.XM == nil {
		t.Fatal("30 min old GLONASS ephemeris: position must still be served")
	}

	// Past the cap (2 h): Propagate must skip the SV, so the position expires via
	// posStaleBound instead of a stale state vector serving fresh forever.
	t2 := t0.Add(2 * time.Hour)
	s.Propagate(t2)
	sv = s.FeedSVs(t2)["R07@0"]
	if sv.XM != nil {
		t.Error("2 h stale GLONASS ephemeris still serving a position (the regression fix failure)")
	}
	// eph_age_m must be the monotone wall-clock age, ≥ the true elapsed 120 min.
	if sv.EphAgeM == nil {
		t.Fatal("eph_age_m absent for a 2 h stale GLONASS ephemeris")
	}
	if *sv.EphAgeM < 119 {
		t.Errorf("eph_age_m = %.1f min at 2 h, want ≥ 119 (wall-clock age)", *sv.EphAgeM)
	}

	// Past the ±12 h EphAgeDay wrap horizon (13 h): the day-wrapped age would now
	// read NEGATIVE (~−660 min), flipping eph_aged back to "fresh" while the SV
	// worsens. The wall-clock age must stay monotone instead.
	t3 := t0.Add(13 * time.Hour)
	s.Propagate(t3)
	sv = s.FeedSVs(t3)["R07@0"]
	if sv.XM != nil {
		t.Error("13 h stale GLONASS ephemeris still serving a position")
	}
	if sv.EphAgeM == nil {
		t.Fatal("eph_age_m absent for a 13 h stale GLONASS ephemeris")
	}
	if *sv.EphAgeM < 12*60 {
		t.Errorf("eph_age_m = %.1f min at 13 h, want ≥ %d (monotone wall-clock, not the wrapped/negative day age)",
			*sv.EphAgeM, 12*60)
	}
}

// TestGLONASSFrozenTbGuard checks that the
// wall-clock gate (gloPropagateMaxEphAge) measures RECEPTION staleness, so an SV
// that keeps transmitting valid strings with a FROZEN tb (a gross SV/CS failure —
// integrity-monitor territory) kept gloEphAt seconds-fresh forever and reached
// the same ±12 h wrap pathology through broadcast-time staleness: positions
// integrated from an ever-older epoch and, past +12 h, backward, while eph_age_m
// re-wrapped negative and un-fired eph_aged. The tk gate (gloServeMaxTk/MinTk)
// must retire the position once the broadcast-time interval leaves the
// legitimate [−45, +90] min window, and the served age must clamp at the +720
// wrap ceiling instead of going negative.
func TestGLONASSFrozenTbGuard(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	// tb index 5 ≈ the current MT time-of-day at t0 (see TestGLONASSPropagateEphAgeCap).
	apply := func(at time.Time) {
		s.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, at))
		s.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 5, at.Add(2*time.Second)))
		s.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, at.Add(4*time.Second)))
	}
	apply(t0)
	s.Propagate(t0.Add(5 * time.Second))
	if sv := s.FeedSVs(t0.Add(5 * time.Second))["R07@0"]; sv.XM == nil {
		t.Fatal("fresh set: no position served")
	}

	// 3 h later the SAME tb is still being broadcast and reassembles (reception
	// live, gloEphAt fresh) — tk ≈ +178 min is outside the legitimate window, so
	// the position must NOT be served, while the wrapped age (still truthful
	// here) keeps eph_aged fireable.
	t1 := t0.Add(3 * time.Hour)
	apply(t1)
	s.Propagate(t1.Add(5 * time.Second))
	sv := s.FeedSVs(t1.Add(5 * time.Second))["R07@0"]
	if sv.XM != nil {
		t.Error("frozen-tb set at +3 h still serving a position (wall gate blind to broadcast-time staleness)")
	}
	if sv.EphAgeM == nil || *sv.EphAgeM < 140 {
		t.Errorf("eph_age_m = %v at +3 h frozen tb, want > 140 (wrapped broadcast age)", sv.EphAgeM)
	}

	// 13 h in: the wrapped age would now read ≈ −660 min (the alias that
	// un-fired eph_aged). the served age is now the monotone
	// wall-clock time since the tb value last changed (~780 min), not the old
	// 720 clamp — it must keep growing, never wrap or step down.
	t2 := t0.Add(13 * time.Hour)
	apply(t2)
	s.Propagate(t2.Add(5 * time.Second))
	sv = s.FeedSVs(t2.Add(5 * time.Second))["R07@0"]
	if sv.XM != nil {
		t.Error("frozen-tb set at +13 h still serving a position (backward integration)")
	}
	if sv.EphAgeM == nil || *sv.EphAgeM < 770 {
		t.Errorf("eph_age_m = %v at +13 h frozen tb, want ≥ 770 (monotone tb age; a wrapped/clamped value un-fires or plateaus eph_aged)", sv.EphAgeM)
	}

	// regression fix seam one: at +24 h the day-wrapped tk re-enters the legitimate
	// [−45, +90] min window, so the tk gate alone re-served day-old positions
	// as fresh for ~2¼ h daily while the wrapped age collapsed to ≈ 0 and
	// eph_aged debounce-confirmed a false recovery. The gloTbAt gate must hold:
	// no position, and a still-monotone ≈ 1440 min age.
	t3 := t0.Add(24 * time.Hour)
	apply(t3)
	s.Propagate(t3.Add(5 * time.Second))
	sv = s.FeedSVs(t3.Add(5 * time.Second))["R07@0"]
	if sv.XM != nil {
		t.Error("frozen-tb set at +24 h re-served a position (the day-periodic seam)")
	}
	if sv.EphAgeM == nil || *sv.EphAgeM < 1430 {
		t.Errorf("eph_age_m = %v at +24 h frozen tb, want ≥ 1430 (a collapsed age falsely recovers eph_aged)", sv.EphAgeM)
	}

	// regression fix seam two: reception dies DURING the episode (last reassembly at
	// +24 h). The old wall-switch replaced the served age with now−gloEphAt
	// starting at 60 — a DOWNWARD crossing of the 140 min threshold that
	// un-fired eph_aged. The tb age must keep growing monotonically instead.
	t4 := t3.Add(90 * time.Minute) // 90 min of silence, wall-switch regime under the old code
	s.Propagate(t4)
	sv = s.FeedSVs(t4)["R07@0"]
	if sv.XM != nil {
		t.Error("frozen-tb-then-dark SV serving a position")
	}
	if sv.EphAgeM == nil || *sv.EphAgeM < 1430+89 {
		t.Errorf("eph_age_m = %v after reception loss mid-episode, want ≥ %d (a step down to ~90 un-fires eph_aged)",
			sv.EphAgeM, 1430+89)
	}
}

// TestGLONASSReplayStaleEph guards GLONASS half: a spool drain applies
// a days-old replayed set with gloTbAt/gloEphAt ≈ now, and if the stale tb's
// time-of-day happens to land inside the legitimate tk window (~9 % of a day's
// sets do), the RK4 would serve a days-old state vector as fresh. The forensic
// reception stamp must refuse it.
func TestGLONASSReplayStaleEph(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	recvAt := now.Add(-3 * 24 * time.Hour) // received 3 days ago, drained now
	// tb index 5 lands ~100 s from now's Moscow time-of-day (see
	// TestGLONASSPropagateEphAgeCap) — inside the tk window by construction,
	// the exact aliasing case.
	for _, f := range []*ingest.RawFrame{
		glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, recvAt),
		glonassStringFrame(7, 2, 20000000, 20, 2, 0, 5, recvAt.Add(2*time.Second)),
		glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, recvAt.Add(4*time.Second)),
	} {
		f.RecvLocal = now
		s.Apply(f)
	}
	s.Propagate(now)
	sv := s.FeedSVs(now)["R07@0"]
	if sv.XM != nil {
		t.Error("3 d old replayed GLONASS set (applied just now, tk in-window) served a position")
	}
	if sv.EphAgeM == nil || *sv.EphAgeM < 3*24*60-10 {
		t.Errorf("eph_age_m = %v for a replayed 3 d old set, want ≈ %d (forensic age)", sv.EphAgeM, 3*24*60)
	}
}
