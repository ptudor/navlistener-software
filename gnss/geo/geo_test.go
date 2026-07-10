package geo

import (
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/physconst"
)

func TestGeodeticToECEFEquator(t *testing.T) {
	// lat=0, lon=0, h=0 sits on the +X axis at the equatorial radius.
	p := GeodeticToECEF(Geodetic{Lat: 0, Lon: 0, Height: 0}, physconst.WGS84)
	if math.Abs(p.X-physconst.WGS84.A) > 1e-6 || math.Abs(p.Y) > 1e-6 || math.Abs(p.Z) > 1e-6 {
		t.Errorf("equator point = %+v, want (%v,0,0)", p, physconst.WGS84.A)
	}
}

func TestGeodeticRoundTrip(t *testing.T) {
	cases := []Geodetic{
		{Lat: 0, Lon: 0, Height: 0},
		{Lat: Rad(37.7749), Lon: Rad(-122.4194), Height: 52}, // San Francisco
		{Lat: Rad(-33.8688), Lon: Rad(151.2093), Height: 58}, // Sydney
		{Lat: Rad(51.4779), Lon: Rad(-0.0015), Height: 45},   // Greenwich
		{Lat: Rad(89.9), Lon: Rad(30), Height: 1200},         // near north pole
		{Lat: Rad(-45), Lon: Rad(179.9), Height: 20000000},   // high altitude (SV-like)
	}
	for _, g := range cases {
		p := GeodeticToECEF(g, physconst.WGS84)
		back := ECEFToGeodetic(p, physconst.WGS84)
		if math.Abs(back.Lat-g.Lat) > 1e-9 || math.Abs(back.Lon-g.Lon) > 1e-9 {
			t.Errorf("round-trip angle mismatch for %+v: got lat=%v lon=%v", g, back.Lat, back.Lon)
		}
		if math.Abs(back.Height-g.Height) > 1e-4 {
			t.Errorf("round-trip height mismatch for %+v: got %v", g, back.Height)
		}
	}
}

func TestECEFToGeodeticPole(t *testing.T) {
	// A point on the +Z axis is the north pole: lat = +90°, height = Z − b.
	b := physconst.WGS84.A * math.Sqrt(1-physconst.WGS84.E2())
	g := ECEFToGeodetic(gnss.ECEF{X: 0, Y: 0, Z: b + 100}, physconst.WGS84)
	if math.Abs(g.Lat-math.Pi/2) > 1e-9 {
		t.Errorf("north pole lat = %v deg, want 90", Deg(g.Lat))
	}
	if math.Abs(g.Height-100) > 1e-6 {
		t.Errorf("north pole height = %v, want 100", g.Height)
	}
}

// receiver at the equator/prime meridian, where the ENU axes are simple:
// up=+X, east=+Y, north=+Z. Used to check AzEl against exact geometry.
var eqRecv = Geodetic{Lat: 0, Lon: 0, Height: 0}

func TestAzElOverhead(t *testing.T) {
	r := GeodeticToECEF(eqRecv, physconst.WGS84)
	sv := r.Add(gnss.ECEF{X: 2e7}) // straight up (+X = up at this receiver)
	_, el := AzEl(sv, eqRecv, physconst.WGS84)
	if math.Abs(Deg(el)-90) > 1e-6 {
		t.Errorf("overhead elevation = %v deg, want 90", Deg(el))
	}
}

func TestAzElNorthHorizon(t *testing.T) {
	r := GeodeticToECEF(eqRecv, physconst.WGS84)
	sv := r.Add(gnss.ECEF{Z: 2e7}) // due north (+Z = north here), on the horizon
	az, el := AzEl(sv, eqRecv, physconst.WGS84)
	if math.Abs(Deg(az)) > 1e-6 {
		t.Errorf("north azimuth = %v deg, want 0", Deg(az))
	}
	if math.Abs(Deg(el)) > 1e-6 {
		t.Errorf("horizon elevation = %v deg, want 0", Deg(el))
	}
}

func TestAzElEastHorizon(t *testing.T) {
	r := GeodeticToECEF(eqRecv, physconst.WGS84)
	sv := r.Add(gnss.ECEF{Y: 2e7}) // due east (+Y = east here)
	az, el := AzEl(sv, eqRecv, physconst.WGS84)
	if math.Abs(Deg(az)-90) > 1e-6 {
		t.Errorf("east azimuth = %v deg, want 90", Deg(az))
	}
	if math.Abs(Deg(el)) > 1e-6 {
		t.Errorf("east horizon elevation = %v deg, want 0", Deg(el))
	}
}

// TestAzElCoincidentPoint guards sv == recv previously divided by zero
// (d.Norm() == 0) and silently returned a NaN elevation. Degenerate input, but
// AzEl has no error return, so it must not propagate NaN — "directly overhead"
// is the defined convention for zero separation.
func TestAzElCoincidentPoint(t *testing.T) {
	r := GeodeticToECEF(eqRecv, physconst.WGS84)
	_, el := AzEl(r, eqRecv, physconst.WGS84) // sv == recv exactly
	if math.IsNaN(el) {
		t.Fatal("elevation is NaN for a coincident SV/receiver point")
	}
	if math.Abs(Deg(el)-90) > 1e-6 {
		t.Errorf("coincident-point elevation = %v deg, want 90 (directly overhead convention)", Deg(el))
	}
}
