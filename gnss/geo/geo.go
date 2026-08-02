// Package geo converts between ECEF and geodetic coordinates and computes
// topocentric azimuth/elevation. Source: docs/MATH.md §5. Each function takes the
// reference ellipsoid explicitly so the caller carries the right datum per
// constellation (WGS-84 / PZ-90.11 / CGCS2000); the differences are a metre of
// semi-major axis at worst, but we keep them correct.
package geo

import (
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/physconst"
)

// Geodetic is a geodetic coordinate: latitude and longitude in radians, height
// above the ellipsoid in metres.
type Geodetic struct {
	Lat, Lon, Height float64
}

// GeodeticToECEF converts a geodetic coordinate to ECEF metres on ellipsoid ell.
func GeodeticToECEF(g Geodetic, ell physconst.Ellipsoid) gnss.ECEF {
	e2 := ell.E2()
	sinLat, cosLat := math.Sincos(g.Lat)
	sinLon, cosLon := math.Sincos(g.Lon)
	n := ell.A / math.Sqrt(1-e2*sinLat*sinLat)
	return gnss.ECEF{
		X: (n + g.Height) * cosLat * cosLon,
		Y: (n + g.Height) * cosLat * sinLon,
		Z: (n*(1-e2) + g.Height) * sinLat,
	}
}

// ECEFToGeodetic converts ECEF metres to geodetic on ellipsoid ell by the standard
// fixed-point latitude/height iteration (docs/MATH.md §5.1; Bowring's closed form
// is the non-iterative alternative given there).
func ECEFToGeodetic(p gnss.ECEF, ell physconst.Ellipsoid) Geodetic {
	e2 := ell.E2()
	lon := math.Atan2(p.Y, p.X)
	hyp := math.Hypot(p.X, p.Y)

	// Near the poles hyp → 0; latitude is ±90° and height is |Z| − polar radius.
	if hyp < 1e-9 {
		lat := math.Copysign(math.Pi/2, p.Z)
		b := ell.A * math.Sqrt(1-e2) // semi-minor axis
		return Geodetic{Lat: lat, Lon: lon, Height: math.Abs(p.Z) - b}
	}

	lat := math.Atan2(p.Z, hyp*(1-e2)) // seed
	for i := 0; i < 10; i++ {
		sinLat := math.Sin(lat)
		n := ell.A / math.Sqrt(1-e2*sinLat*sinLat)
		h := hyp/math.Cos(lat) - n
		next := math.Atan2(p.Z, hyp*(1-e2*n/(n+h)))
		if math.Abs(next-lat) < 1e-12 {
			lat = next
			break
		}
		lat = next
	}

	// Final height from the better-conditioned form: hyp/cos(lat) blows up near
	// the poles (cos→0), while Z/sin(lat) blows up near the equator, so pick the
	// larger denominator.
	sinLat, cosLat := math.Sincos(lat)
	n := ell.A / math.Sqrt(1-e2*sinLat*sinLat)
	var h float64
	if hyp > math.Abs(p.Z) {
		h = hyp/cosLat - n
	} else {
		h = p.Z/sinLat - n*(1-e2)
	}
	return Geodetic{Lat: lat, Lon: lon, Height: h}
}

// AzEl returns the topocentric azimuth and elevation (radians) of an SV at ECEF
// position sv as seen from a receiver at geodetic position recv on ellipsoid ell.
// Azimuth is measured clockwise from north in [0, 2π); elevation in [−π/2, π/2]
// (docs/MATH.md §5.2). The receiver ECEF is derived from recv on the same datum.
func AzEl(sv gnss.ECEF, recv Geodetic, ell physconst.Ellipsoid) (az, el float64) {
	r := GeodeticToECEF(recv, ell)
	d := sv.Sub(r) // receiver → SV vector, ECEF

	sinLat, cosLat := math.Sincos(recv.Lat)
	sinLon, cosLon := math.Sincos(recv.Lon)

	// ENU basis at the receiver.
	east := gnss.ECEF{X: -sinLon, Y: cosLon, Z: 0}
	north := gnss.ECEF{X: -sinLat * cosLon, Y: -sinLat * sinLon, Z: cosLat}
	up := gnss.ECEF{X: cosLat * cosLon, Y: cosLat * sinLon, Z: sinLat}

	e := d.Dot(east)
	n := d.Dot(north)
	u := d.Dot(up)

	az = math.Atan2(e, n)
	if az < 0 {
		az += 2 * math.Pi
	}
	// sv == recv is degenerate (cannot occur for a real SV/receiver pair —
	// no broadcast satellite coincides with a ground receiver) but would otherwise
	// divide by zero and silently propagate NaN. Directly overhead is the natural
	// convention for "zero separation."
	if norm := d.Norm(); norm != 0 {
		// math.Asin(u/norm) can return NaN for an SV exactly overhead —
		// u = d.Dot(up) and norm = d.Norm() come from different float paths, so
		// when d is nearly parallel to up, rounding can push the ratio to
		// 1+ε, which Asin rejects. math.Atan2(u, Hypot(e, n)) is
		// unconditionally safe (no ratio, no domain restriction) and is
		// mathematically identical to Asin(u/norm) since Hypot(e,n) is the
		// horizontal component of d.
		el = math.Atan2(u, math.Hypot(e, n))
	} else {
		el = math.Pi / 2
	}
	return az, el
}

// Deg converts radians to degrees (a convenience for the feed's *_deg fields).
func Deg(rad float64) float64 { return rad * 180 / math.Pi }

// Rad converts degrees to radians.
func Rad(deg float64) float64 { return deg * math.Pi / 180 }
