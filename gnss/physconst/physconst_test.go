package physconst

import (
	"math"
	"testing"

	"github.com/ptudor/gnss"
)

// TestRelativityConstant checks that each constellation's broadcast relativity
// constant F is consistent with its own μ via F = −2√μ/c². This catches a
// transcription error (e.g. accidentally pairing the GPS F with the Galileo μ). The ICD F values are rounded, so compare
// with a loose relative tolerance rather than demanding the ICD's last digit.
func TestRelativityConstant(t *testing.T) {
	c2 := SpeedOfLight * SpeedOfLight
	for _, id := range []gnss.GNSSID{gnss.GPS, gnss.QZSS, gnss.NavIC, gnss.Galileo, gnss.BeiDou} {
		p := MustFor(id)
		want := -2 * math.Sqrt(p.Mu) / c2
		if rel := math.Abs((p.RelF - want) / want); rel > 1e-5 {
			t.Errorf("%s: F=%.6e but −2√μ/c²=%.6e (rel err %.2e)", id, p.RelF, want, rel)
		}
	}
}

// TestGalileoBeidouMuDiffersFromGPS checks per-constellation constants: Galileo and BeiDou
// must NOT carry the GPS gravitational parameter (docs/MATH.md §0).
func TestGalileoBeidouMuDiffersFromGPS(t *testing.T) {
	gps := MustFor(gnss.GPS)
	for _, id := range []gnss.GNSSID{gnss.Galileo, gnss.BeiDou} {
		if MustFor(id).Mu == gps.Mu {
			t.Errorf("%s μ must be 3.986004418e14, not the GPS value %.6e", id, gps.Mu)
		}
	}
	// QZSS and NavIC, by contrast, DO share the GPS value (GPS-compatible).
	for _, id := range []gnss.GNSSID{gnss.QZSS, gnss.NavIC} {
		if MustFor(id).Mu != gps.Mu {
			t.Errorf("%s μ should equal the GPS value (GPS-compatible)", id)
		}
	}
}

func TestEllipsoidE2(t *testing.T) {
	// WGS-84 first eccentricity squared, the standard reference value.
	if got := WGS84.E2(); math.Abs(got-0.0066943799901) > 1e-12 {
		t.Errorf("WGS84 e² = %.13f, want 0.0066943799901", got)
	}
	// The three ellipsoids are close but not interchangeable: PZ-90.11's
	// semi-major axis is a full metre shorter than WGS-84's.
	if PZ90.A == WGS84.A {
		t.Error("PZ-90.11 semi-major axis should differ from WGS-84 (6378136 vs 6378137)")
	}
}

func TestForUnknown(t *testing.T) {
	// SBAS and IMES have no Keplerian parameter set.
	if _, ok := For(gnss.SBAS); ok {
		t.Error("SBAS should have no Keplerian params")
	}
	if _, ok := For(gnss.GPS); !ok {
		t.Error("GPS must have params")
	}
}

func TestGlonassHasNoRelativityF(t *testing.T) {
	p := MustFor(gnss.GLONASS)
	if p.RelF != 0 {
		t.Errorf("GLONASS uses a Cartesian model with no F term, got %v", p.RelF)
	}
	if p.Datum != PZ90 {
		t.Error("GLONASS datum must be PZ-90.11")
	}
}
