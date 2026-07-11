package frame

import (
	"encoding/binary"
	"errors"
	"math"

	"github.com/ptudor/gnss/glonass"
)

// errGLONASSStringOrder is returned when AssembleGLONASS's arguments aren't
// strings 1/2/3 in that order (regression fix defense-in-depth).
var errGLONASSStringOrder = errors.New("frame: GLONASS strings not in 1/2/3 order")

// glonassBlock packs the four 32-bit words of a GLONASS string big-endian into a
// 128-bit block and returns a bit reader over it. The 85-bit ICD string maps into the
// block as block bit = 85 − (ICD bit number), so an ICD field spanning bits [lo..hi]
// (hi = MSB / sign) is read at block offset 85−hi, width hi−lo+1 (the offsets below
// are pre-computed from GLONASS ICD Ed. 5.1 Tables 4.11 that way).
func glonassBlock(words []uint32) *BitReader {
	buf := make([]byte, 16)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	return NewBitReaderN(buf, 128)
}

// GLONASS L1OF/L2OF string decoding (GLONASS ICD Ed. 5.1 §4). u-blox delivers each
// 85-bit string as one UBX-RXM-SFRBX of four 32-bit words = a 128-bit block; the
// string number m sits at bits 1–4 and the string content follows (bit N of the
// 85-bit string maps to block bit 85−N). Strings 1–4 carry the immediate ephemeris
// as a PZ-90 Cartesian state; strings 1/2/3 hold the x/y/z components (with the
// same field offsets per axis), string 2 also the reference time tb and health.
//
// GLONASS fields are sign-magnitude, not two's complement. Offsets and scale
// factors validated against real ZED-F9T frames: √(x²+y²+z²) ≈ 25510 km for every
// SV, and the state stays on-shell when RK4-propagated.

// GLONASS scale factors (km, km/s, km/s²; clock terms dimensionless/seconds).
const (
	gloPos   = 1.0 / (1 << 11)      // 2^-11 km
	gloVel   = 1.0 / float64(1<<20) // 2^-20 km/s
	gloAccel = 1.0 / float64(1<<30) // 2^-30 km/s²
	gloTbSec = 900.0                // tb LSB = 15 min = 900 s

	// SV clock scale factors (regression fix; ICD Ed. 5.1 Table 4.5): γn(tb) LSB = 2^-40
	// (dimensionless), τn(tb) and Δτn LSB = 2^-30 s.
	gloGamma2m40 = 1.0 / float64(uint64(1)<<40)
	gloClk2m30   = 1.0 / float64(1<<30)
)

// GLONASSString holds the decoded fields of one nav string. Coord/Vel/Accel are
// this string's axis component (x for string 1, y for 2, z for 3).
type GLONASSString struct {
	Number int
	Coord  float64 // km
	Vel    float64 // km/s
	Accel  float64 // km/s²
	Health int     // string 2: Bn health flags
	Tb     float64 // string 2: reference time, seconds of day

	// SV clock terms (regression fix; ICD Ed. 5.1 Tables 4.5/4.6, sign-magnitude per
	// Table 4.5 Note 2): γn(tb) from string 3, τn(tb) and Δτn from string 4.
	GammaN    float64 // string 3: relative frequency deviation, dimensionless
	TauN      float64 // string 4: SV-time-to-GLONASS-time correction at tb, s
	DeltaTauN float64 // string 4: L2−L1 group-delay difference, s
}

// DecodeGLONASSString decodes one string from its four words.
func DecodeGLONASSString(words []uint32) (*GLONASSString, error) {
	if len(words) < 4 {
		return nil, ErrShortFrame
	}
	r := glonassBlock(words)
	m, _ := r.Bits(1, 4)
	s := &GLONASSString{Number: int(m)}

	// Strings 1–3 share the coordinate/velocity/acceleration field positions.
	coord, _ := r.SignMag(50, 27)
	vel, _ := r.SignMag(21, 24)
	acc, _ := r.SignMag(45, 5)
	s.Coord = float64(coord) * gloPos
	s.Vel = float64(vel) * gloVel
	s.Accel = float64(acc) * gloAccel

	if m == 2 {
		bn, _ := r.Bits(5, 3)
		tb, _ := r.Bits(9, 7)
		s.Health = int(bn)
		s.Tb = float64(tb) * gloTbSec
	}
	if m == 3 {
		gamma, _ := r.SignMag(6, 11) // γn(tb) — ICD Table 4.6: string 3 bits 69–79 (85−79 = 6)
		s.GammaN = float64(gamma) * gloGamma2m40
	}
	if m == 4 {
		tau, _ := r.SignMag(5, 22)  // τn(tb) — ICD Table 4.6: string 4 bits 59–80 (85−80 = 5)
		dtau, _ := r.SignMag(27, 5) // Δτn — ICD Table 4.6: string 4 bits 54–58 (85−58 = 27)
		s.TauN = float64(tau) * gloClk2m30
		s.DeltaTauN = float64(dtau) * gloClk2m30
	}
	return s, nil
}

// GLONASS almanac scale factors (GLONASS ICD Ed. 5.1 Table 4.9). Angular words are in
// semicircles; ×π converts to radians.
const (
	gloAlm2m20 = 1.0 / (1 << 20) // λ, Δi (semicircle), ε (dimensionless)
	gloAlm2m15 = 1.0 / (1 << 15) // ω (semicircle)
	gloAlm2m9  = 1.0 / (1 << 9)  // ΔT (s)
	gloAlm2m14 = 1.0 / (1 << 14) // ΔT' (s)
	gloAlm2m5  = 1.0 / (1 << 5)  // tλ (s)
	gloAlm2m18 = 1.0 / (1 << 18) // τ (s)
)

// GLONASSAlmanacEntry is one satellite's decoded almanac (GLONASS ICD Ed. 5.1 §4.5),
// assembled from its two-string pair and the frame's NA day number (string 5). Alm is
// the SI-scaled element set the analytic propagator consumes; Cn is the generalized
// health flag broadcast at almanac upload (1 = operable).
type GLONASSAlmanacEntry struct {
	Alm     glonass.Almanac
	Cn      int     // generalized "unhealthy flag": 1 operable, 0 not (ICD §4.5)
	SatType int     // MnA: 0 = GLONASS, 1 = GLONASS-M
	TauNA   float64 // coarse clock correction to GLONASS time, s
}

// DecodeGLONASSAlmanac assembles one satellite's almanac from its two-string pair:
// first ∈ {6,8,10,12,14} carries slot/type/health/τ/λ/Δi/ε, second ∈ {7,9,11,13,15}
// carries ω/tλ/ΔT/ΔT'/H. na is the day number NA from string 5 (the day the almanac
// refers to). Fields are placed per ICD Ed. 5.1 Tables 4.9/4.11; sign-magnitude words
// (τ, λ, Δi, ΔT, ΔT', ω) use the ICD MSB-sign convention (Table 4.9 Note 2). Angles are
// converted from semicircles to radians. The block offsets are 85−(ICD hi-bit).
func DecodeGLONASSAlmanac(first, second []uint32, na int) (GLONASSAlmanacEntry, error) {
	if len(first) < 4 || len(second) < 4 {
		return GLONASSAlmanacEntry{}, ErrShortFrame
	}
	r1 := glonassBlock(first)
	r2 := glonassBlock(second)

	// First string of the pair (ICD Table 4.11): nA 73-77, MnA 78-79, τnA 63-72,
	// λnA 42-62, ΔinA 24-41, εnA 9-23, CnA 80.
	slot, _ := r1.Bits(8, 5)        // nA   (85−77 = 8)
	mType, _ := r1.Bits(6, 2)       // MnA  (85−79 = 6)
	cn, _ := r1.Bits(5, 1)          // CnA  (85−80 = 5)
	tau, _ := r1.SignMag(13, 10)    // τnA  (85−72 = 13), 2^-18 s
	lambda, _ := r1.SignMag(23, 21) // λnA  (85−62 = 23), 2^-20 sc
	deltaI, _ := r1.SignMag(44, 18) // ΔinA (85−41 = 44), 2^-20 sc
	ecc, _ := r1.Bits(62, 15)       // εnA  (85−23 = 62), 2^-20

	// Second string of the pair (ICD Table 4.11): HnA 10-14, tλnA 44-64, ΔTnA 22-43,
	// ΔT'nA 15-21, ωnA 65-80.
	omega, _ := r2.SignMag(5, 16)     // ωnA   (85−80 = 5),  2^-15 sc
	tLambda, _ := r2.Bits(21, 21)     // tλnA  (85−64 = 21), 2^-5 s
	deltaT, _ := r2.SignMag(42, 22)   // ΔTnA  (85−43 = 42), 2^-9 s
	deltaTdot, _ := r2.SignMag(64, 7) // ΔT'nA (85−21 = 64), 2^-14 s
	hn, _ := r2.Bits(71, 5)           // HnA   (85−14 = 71)

	return GLONASSAlmanacEntry{
		Cn:      int(cn),
		SatType: int(mType),
		TauNA:   float64(tau) * gloAlm2m18,
		Alm: glonass.Almanac{
			NA:        na,
			Slot:      int(slot),
			FreqCh:    gloHnToChannel(int(hn)),
			Lambda:    float64(lambda) * gloAlm2m20 * math.Pi,
			DeltaI:    float64(deltaI) * gloAlm2m20 * math.Pi,
			Omega:     float64(omega) * gloAlm2m15 * math.Pi,
			Ecc:       float64(ecc) * gloAlm2m20,
			Tlambda:   float64(tLambda) * gloAlm2m5,
			DeltaT:    float64(deltaT) * gloAlm2m9,
			DeltaTdot: float64(deltaTdot) * gloAlm2m14,
		},
	}, nil
}

// gloHnToChannel maps the broadcast frequency-number word HnA to the FDMA channel k
// (GLONASS ICD Ed. 5.1 Table 4.10: values 25..31 encode channels −7..−1).
func gloHnToChannel(h int) int {
	if h >= 25 {
		return h - 32
	}
	return h
}

// DecodeGLONASSFrameNA reads the calendar day number NA from string 5 (ICD Ed. 5.1
// Table 4.11: NA at string bits 70-80). NA is common to every almanac in the frame —
// the day the almanac elements refer to.
func DecodeGLONASSFrameNA(words []uint32) (int, error) {
	if len(words) < 4 {
		return 0, ErrShortFrame
	}
	na, _ := glonassBlock(words).Bits(5, 11) // NA (85−80 = 5), 11 bits
	return int(na), nil
}

// AssembleGLONASS combines strings 1/2/3 into the PZ-90 Cartesian ephemeris the
// RK4 propagator consumes. slot is the GLONASS slot number; freqID is the UBX
// freqId (the FDMA channel is freqID−7). tb and the health come from string 2;
// γn rides string 3. s4 is nil-tolerant : when present it must be a
// same-frame string 4 and contributes the SV clock terms τn/Δτn (ClockKnown);
// when nil the ephemeris assembles clockless — the caller enforces the
// same-frame temporal window for all strings, exactly as for strings 1–3.
func AssembleGLONASS(slot, freqID int, s1, s2, s3, s4 *GLONASSString) (glonass.Ephemeris, error) {
	if s1 == nil || s2 == nil || s3 == nil {
		return glonass.Ephemeris{}, ErrShortFrame
	}
	// regression fix (defense-in-depth): the caller is expected to pass strings in their
	// broadcast slots, but assert it rather than silently combine x/y/z components
	// from the wrong string numbers if a caller ever mis-wires the arguments.
	if s1.Number != 1 || s2.Number != 2 || s3.Number != 3 {
		return glonass.Ephemeris{}, errGLONASSStringOrder
	}
	if s4 != nil && s4.Number != 4 {
		return glonass.Ephemeris{}, errGLONASSStringOrder
	}
	eph := glonass.Ephemeris{
		X: s1.Coord, Vx: s1.Vel, Ax: s1.Accel,
		Y: s2.Coord, Vy: s2.Vel, Ay: s2.Accel,
		Z: s3.Coord, Vz: s3.Vel, Az: s3.Accel,
		Tb:       s2.Tb,
		TodKnown: true, // string 2 carries tb; the day is anchored
		GammaN:   s3.GammaN,
		FreqCh:   freqID - 7,
		Slot:     slot,
	}
	if s4 != nil {
		eph.TauN, eph.DeltaTauN, eph.ClockKnown = s4.TauN, s4.DeltaTauN, true
	}
	return eph, nil
}
