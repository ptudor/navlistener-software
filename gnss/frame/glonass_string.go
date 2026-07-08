package frame

import (
	"encoding/binary"

	"github.com/ptudor/gnss/glonass"
)

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

// GLONASS scale factors (km, km/s, km/s²).
const (
	gloPos   = 1.0 / (1 << 11)      // 2^-11 km
	gloVel   = 1.0 / float64(1<<20) // 2^-20 km/s
	gloAccel = 1.0 / float64(1<<30) // 2^-30 km/s²
	gloTbSec = 900.0                // tb LSB = 15 min = 900 s
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
}

// DecodeGLONASSString decodes one string from its four words.
func DecodeGLONASSString(words []uint32) (*GLONASSString, error) {
	if len(words) < 4 {
		return nil, ErrShortFrame
	}
	buf := make([]byte, 16)
	for i := 0; i < 4; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	r := NewBitReaderN(buf, 128)
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
	return s, nil
}

// AssembleGLONASS combines strings 1/2/3 into the PZ-90 Cartesian ephemeris the
// RK4 propagator consumes. slot is the GLONASS slot number; freqID is the UBX
// freqId (the FDMA channel is freqID−7). tb and the health come from string 2.
func AssembleGLONASS(slot, freqID int, s1, s2, s3 *GLONASSString) (glonass.Ephemeris, error) {
	if s1 == nil || s2 == nil || s3 == nil {
		return glonass.Ephemeris{}, ErrShortFrame
	}
	return glonass.Ephemeris{
		X: s1.Coord, Vx: s1.Vel, Ax: s1.Accel,
		Y: s2.Coord, Vy: s2.Vel, Ay: s2.Accel,
		Z: s3.Coord, Vz: s3.Vel, Az: s3.Accel,
		Tb:       s2.Tb,
		TodKnown: true, // string 2 carries tb; the day is anchored
		FreqCh:   freqID - 7,
		Slot:     slot,
	}, nil
}
