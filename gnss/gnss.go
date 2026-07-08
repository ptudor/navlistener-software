// Package gnss is the root of the project, clean-room GNSS math and decode
// library. It holds the value types shared across the sub-packages (kepler,
// glonass, clock, geo, accuracy, iono, gnsstime, frame): the ECEF vector and the
// constellation identity.
//
// Every line of GNSS math in this module is authored from the public Interface
// Control Documents (IS-GPS-200, Galileo OS-SIS-ICD, BDS-SIS-ICD, GLONASS ICD,
// IS-QZSS, IRNSS SPS ICD) — no galmon code is copied or transliterated. See the
// per-function doc comments for the exact ICD citation.
//
// The library is Apache-2.0 and I/O-free: it turns raw broadcast bits and time
// into positions, clocks, and geometry, and returns errors (never NaN) on
// degenerate input, so it can be linked by the daemon, tudorgps, and the map
// clients alike.
package gnss

import "math"

// ECEF is an Earth-Centered, Earth-Fixed position or vector, in metres. The datum
// (WGS-84 / PZ-90.11 / CGCS2000) is carried alongside by the caller — the
// differences are cm-level (docs/MATH.md §5.1) and only matter where a common
// frame is required.
type ECEF struct {
	X, Y, Z float64
}

// Sub returns a − b.
func (a ECEF) Sub(b ECEF) ECEF { return ECEF{a.X - b.X, a.Y - b.Y, a.Z - b.Z} }

// Add returns a + b.
func (a ECEF) Add(b ECEF) ECEF { return ECEF{a.X + b.X, a.Y + b.Y, a.Z + b.Z} }

// Scale returns a scaled by s.
func (a ECEF) Scale(s float64) ECEF { return ECEF{a.X * s, a.Y * s, a.Z * s} }

// Dot returns the dot product a·b.
func (a ECEF) Dot(b ECEF) float64 { return a.X*b.X + a.Y*b.Y + a.Z*b.Z }

// Norm returns the Euclidean length |a|.
func (a ECEF) Norm() float64 { return math.Sqrt(a.X*a.X + a.Y*a.Y + a.Z*a.Z) }

// GNSSID is the constellation identifier in the single internal numbering used
// everywhere — the u-blox gnssId order, native to UBX-RXM-SFRBX and emitted in
// the feeds (docs/CONSTELLATIONS.md §0). IMES (4) is defined for completeness but
// is never emitted.
type GNSSID uint8

const (
	GPS     GNSSID = 0
	SBAS    GNSSID = 1
	Galileo GNSSID = 2
	BeiDou  GNSSID = 3
	IMES    GNSSID = 4
	QZSS    GNSSID = 5
	GLONASS GNSSID = 6
	NavIC   GNSSID = 7
)

// Letter returns the RINEX 3.x / IGS constellation letter (G R E C J I S), or
// '?' for IMES / an unknown id. SV names in the feeds are letter + zero-padded
// PRN, e.g. "G05", "J03" — but no consumer parses the letter; they key on the
// numeric id (docs/OUTPUT.md §0).
func (g GNSSID) Letter() byte {
	switch g {
	case GPS:
		return 'G'
	case SBAS:
		return 'S'
	case Galileo:
		return 'E'
	case BeiDou:
		return 'C'
	case QZSS:
		return 'J'
	case GLONASS:
		return 'R'
	case NavIC:
		return 'I'
	default:
		return '?'
	}
}

// String returns the lowercase constellation name used for metric labels and the
// global feed's per-constellation counts (docs/OUTPUT.md §1.2).
func (g GNSSID) String() string {
	switch g {
	case GPS:
		return "gps"
	case SBAS:
		return "sbas"
	case Galileo:
		return "galileo"
	case BeiDou:
		return "beidou"
	case IMES:
		return "imes"
	case QZSS:
		return "qzss"
	case GLONASS:
		return "glonass"
	case NavIC:
		return "navic"
	default:
		return "unknown"
	}
}

// Valid reports whether g is a constellation navlistener decodes (every id except
// IMES, which is never emitted, and out-of-range values).
func (g GNSSID) Valid() bool {
	return g <= NavIC && g != IMES
}
