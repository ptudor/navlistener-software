package gnss

import (
	"math"
	"testing"
)

func TestECEFMath(t *testing.T) {
	a := ECEF{3, 4, 12}
	if got := a.Norm(); got != 13 {
		t.Errorf("Norm = %v, want 13", got)
	}
	b := ECEF{1, 1, 2}
	if got := a.Sub(b); got != (ECEF{2, 3, 10}) {
		t.Errorf("Sub = %+v", got)
	}
	if got := a.Add(b); got != (ECEF{4, 5, 14}) {
		t.Errorf("Add = %+v", got)
	}
	if got := a.Scale(2); got != (ECEF{6, 8, 24}) {
		t.Errorf("Scale = %+v", got)
	}
	if got := a.Dot(b); got != 3+4+24 {
		t.Errorf("Dot = %v, want 31", got)
	}
}

func TestGNSSIDLetter(t *testing.T) {
	cases := map[GNSSID]byte{
		GPS: 'G', SBAS: 'S', Galileo: 'E', BeiDou: 'C',
		QZSS: 'J', GLONASS: 'R', NavIC: 'I', IMES: '?',
	}
	for id, want := range cases {
		if got := id.Letter(); got != want {
			t.Errorf("GNSSID(%d).Letter() = %q, want %q", id, got, want)
		}
	}
}

func TestGNSSIDValid(t *testing.T) {
	// Every constellation we decode is valid; IMES and out-of-range are not.
	for _, id := range []GNSSID{GPS, SBAS, Galileo, BeiDou, QZSS, GLONASS, NavIC} {
		if !id.Valid() {
			t.Errorf("GNSSID(%d) should be valid", id)
		}
	}
	if IMES.Valid() {
		t.Error("IMES must not be valid (never emitted)")
	}
	if GNSSID(8).Valid() {
		t.Error("GNSSID(8) out of range must not be valid")
	}
}

func TestGNSSIDString(t *testing.T) {
	if GPS.String() != "gps" || QZSS.String() != "qzss" || NavIC.String() != "navic" {
		t.Errorf("String() names wrong: %s %s %s", GPS, QZSS, NavIC)
	}
}

// ensure math import is used even if the above change.
var _ = math.Sqrt
