package gnsstime

import "testing"

// TestEphAgeDomain documents EphAge's contract: it is defined only for both inputs
// in [0, WeekSeconds). It subtracts once and wraps once, so an out-of-domain
// reference does not fail — it aliases into a plausible interval, which is why
// every decoder bounds toe/toc/t0e/t0c and the TOW counts at the week before any
// value reaches a propagation.
func TestEphAgeDomain(t *testing.T) {
	// In-domain references wrap across the week boundary as intended.
	if got := EphAge(100, WeekSeconds-100); got != 200 {
		t.Errorf("EphAge(100, week−100) = %v, want 200 (wrapped forward)", got)
	}
	if got := EphAge(WeekSeconds-100, 100); got != -200 {
		t.Errorf("EphAge(week−100, 100) = %v, want −200 (wrapped backward)", got)
	}
	// An out-of-domain reference (a 16-bit toe count × 16 well past the week)
	// aliases: 432 000 − 956 288 = −524 288, wrapped once to +80 512 — a plausible
	// 22 h age for a reference that cannot exist.
	if got := EphAge(432000, 956288); got != 80512 {
		t.Errorf("EphAge(432000, 956288) = %v, want the aliased 80512 this test documents", got)
	}
}
