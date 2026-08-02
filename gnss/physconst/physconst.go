// Package physconst holds the per-constellation physical constants that the
// propagators and geometry use. Source: docs/MATH.md §0, transcribed from each
// constellation's ICD. The central discipline: these are per-constellation and
// must NOT be averaged or shared. GPS uses a gravitational parameter of
// 3.986005e14; the Galileo and BeiDou ICDs specify 3.986004418e14,
// and the difference moves the relativity term F in the 8th digit — exactly the
// scale the 2.5 ns clock-disco integrity threshold cares about.
package physconst

import "github.com/ptudor/gnss"

// SpeedOfLight is c in m/s (exact, shared by all constellations).
const SpeedOfLight = 299792458.0

// Pi is the value of π the ICDs mandate for converting broadcast angles from
// semicircles to radians (radians = semicircles × Pi). Using this fixed value
// rather than math.Pi makes decoded angles match the ICD bit-for-bit
// (docs/MATH.md conventions). It is applied by the frame decoders at decode time.
const Pi = 3.1415926535898

// Ellipsoid is a reference ellipsoid: equatorial radius A (m) and inverse
// flattening InvF (docs/MATH.md §5.1).
type Ellipsoid struct {
	A    float64 // semi-major axis, metres
	InvF float64 // 1/f
}

// F returns the flattening f.
func (e Ellipsoid) F() float64 { return 1.0 / e.InvF }

// E2 returns the first eccentricity squared, e² = 2f − f².
func (e Ellipsoid) E2() float64 {
	f := e.F()
	return 2*f - f*f
}

// The reference ellipsoids (docs/MATH.md §5.1).
var (
	WGS84    = Ellipsoid{A: 6378137, InvF: 298.257223563}
	PZ90     = Ellipsoid{A: 6378136, InvF: 298.25784} // PZ-90.11
	CGCS2000 = Ellipsoid{A: 6378137, InvF: 298.257222101}
)

// Params is the constant set for one constellation's Keplerian propagation and
// clock math (docs/MATH.md §0).
type Params struct {
	Mu     float64   // gravitational parameter GM, m³/s²
	OmegaE float64   // Earth rotation rate ωe, rad/s
	RelF   float64   // relativity constant F = −2√μ/c², s/√m (0 for GLONASS)
	Datum  Ellipsoid // the reference ellipsoid for ECEF↔geodetic
}

// paramsByID indexes Params by constellation. QZSS and NavIC use the GPS
// gravitational/rotation constants on the WGS-84 ellipsoid (QZSS-PNT-006 §5.3.4
// and NAVIC-SPS-L5S App. A/B print the GPS F, μ, and ωe verbatim as "WGS 84
// value[s]"; QZSST is GPST, while NavIC runs its own IRNWT but
// keeps the GPS constants — docs/MATH.md §0); Galileo and BeiDou use the smaller
// 3.986004418e14 μ from their own ICDs; GLONASS is a Cartesian model with no
// relativity F term (docs/MATH.md §0).
//
// Galileo's Datum is WGS84 deliberately, not an oversight: the broadcast frame
// is GTRF (GAL-OS-SIS-ICD-2.2 Table 68 — "GTRF coordinates of the SV antenna
// phase centre"), but that ICD defines no ellipsoid for GTRF (Table 68 supplies
// only μ, ωe, c, and π), so ECEF↔geodetic conversion uses the WGS-84 ellipsoid.
//
// BeiDou's F (regression fix, verified against BDS-SIS-B1I-3.0 §5.2.4.9): the BDS ICD
// publishes no rounded digit string — it defines F = −2μ^½/C² with
// μ = 3.986004418e14 and C = 2.99792458e8, so the computed value below (equal to
// Galileo's, since the μ is identical) is exactly the ICD-prescribed constant.
var paramsByID = map[gnss.GNSSID]Params{
	gnss.GPS:     {Mu: 3.986005e14, OmegaE: 7.2921151467e-5, RelF: -4.442807633e-10, Datum: WGS84},
	gnss.QZSS:    {Mu: 3.986005e14, OmegaE: 7.2921151467e-5, RelF: -4.442807633e-10, Datum: WGS84},
	gnss.NavIC:   {Mu: 3.986005e14, OmegaE: 7.2921151467e-5, RelF: -4.442807633e-10, Datum: WGS84},
	gnss.Galileo: {Mu: 3.986004418e14, OmegaE: 7.2921151467e-5, RelF: -4.442807309e-10, Datum: WGS84},
	gnss.BeiDou:  {Mu: 3.986004418e14, OmegaE: 7.2921150e-5, RelF: -4.442807309e-10, Datum: CGCS2000},
	gnss.GLONASS: {Mu: 3.986004418e14, OmegaE: 7.2921150e-5, RelF: 0, Datum: PZ90},
}

// For returns the physical constants for constellation id, and ok=false for a
// constellation with no Keplerian parameter set (SBAS, IMES, or an unknown id).
func For(id gnss.GNSSID) (Params, bool) {
	p, ok := paramsByID[id]
	return p, ok
}

// MustFor returns the constants for id, panicking if none exist. Use only with a
// compile-time-known constellation; prefer For at runtime.
func MustFor(id gnss.GNSSID) Params {
	p, ok := paramsByID[id]
	if !ok {
		panic("physconst: no parameters for gnssId " + id.String())
	}
	return p
}

// GLONASS-specific constants for the PZ-90 numerical propagator (docs/MATH.md §0,
// §3). The equations of motion work in kilometres, so these are the km forms.
const (
	// GloMuKm is GM in km³/s² (3.986004418e14 m³/s² = 398600.4418 km³/s²).
	GloMuKm = 398600.4418
	// GloAeKm is the PZ-90 equatorial radius aₑ in km.
	GloAeKm = 6378.136
	// GloJ2 is the second zonal harmonic J₂ (the ICD writes C₂₀ = −J₂).
	GloJ2 = 1.0826257e-3
	// GloOmegaE is the PZ-90 Earth rotation rate ωe in rad/s.
	GloOmegaE = 7.2921150e-5
)
