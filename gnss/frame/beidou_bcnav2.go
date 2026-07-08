package frame

import (
	"encoding/binary"
	"math"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// BeiDou B2a B-CNAV2 decoding (BDS-SIS-ICD-B2a v1.0 §6.2, §7.7). u-blox delivers
// each 288-bit message as one 9-word SFRBX, packed MSB-first across the full 32-bit
// words. The ephemeris is split across message types 10 (Ephemeris I: toe/ΔA/M0/e/ω,
// after a 61-bit header ending in IODE) and 11 (Ephemeris II: Ω0/i0/Ω̇/IDOT/C-terms,
// after a 42-bit header). Like GPS CNAV it uses the ΔA parameterization (A = A_ref +
// ΔA); A_ref is 27906100 m (MEO) or 42162200 m (IGSO/GEO) by SatType. Field bit
// positions and scale factors are ICD Table 7-8; confirmed against real ZED-F9T
// frames (a≈27906 km, i₀≈55°, and M0/e/i0 match the B1I D1 decode).

// B-CNAV2 semi-major-axis reference values (ICD Table 7-8), metres.
const (
	bcnavArefMEO  = 27906100.0 // MEO
	bcnavArefIGSO = 42162200.0 // IGSO/GEO
)

// BeiDouBCNAV2 holds the decoded fields of one B-CNAV2 message.
type BeiDouBCNAV2 struct {
	MesType int
	PRN     int
	SOW     int
	SatType int // 1 GEO, 2 IGSO, 3 MEO
	eph     kepler.Ephemeris
	hasEph2 bool
}

// DecodeBeiDouBCNAV2 decodes one B-CNAV2 message (nine words). Message types 10 and
// 11 carry the ephemeris; other types (clock/SISAI/iono/reduced almanac) are
// recognised by their header but their bodies are not decoded here.
func DecodeBeiDouBCNAV2(words []uint32) (*BeiDouBCNAV2, error) {
	if len(words) < 9 {
		return nil, ErrShortFrame
	}
	buf := make([]byte, 36) // 9 words × 32 bits = 288 bits
	for i := 0; i < 9; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	r := NewBitReaderN(buf, 288)
	u := func(p, n int) uint64 { v, _ := r.Bits(p, n); return v }
	s := func(p, n int) int64 { v, _ := r.Signed(p, n); return v }

	m := &BeiDouBCNAV2{
		PRN:     int(u(0, 6)),
		MesType: int(u(6, 6)),
		SOW:     int(u(12, 18)),
	}
	semi := physconst.Pi
	switch m.MesType {
	case 10: // Ephemeris I (header 61 bits, ending in IODE@53)
		m.SatType = int(u(72, 2))
		aref := bcnavArefMEO
		if m.SatType != 3 {
			aref = bcnavArefIGSO
		}
		m.eph.Toe = float64(u(61, 11)) * 300
		m.eph.SqrtA = math.Sqrt(aref + float64(s(74, 26))*p2m9)
		m.eph.DeltaN = float64(s(125, 17)) * p2m44 * semi
		m.eph.M0 = float64(s(165, 33)) * p2m32 * semi
		m.eph.Ecc = float64(u(198, 33)) * p2m34
		m.eph.Omega = float64(s(231, 33)) * p2m32 * semi
	case 11: // Ephemeris II (header 42 bits)
		m.eph.Omega0 = float64(s(42, 33)) * p2m32 * semi
		m.eph.I0 = float64(s(75, 33)) * p2m32 * semi
		m.eph.OmegaDot = float64(s(108, 19)) * p2m44 * semi
		m.eph.IDot = float64(s(127, 15)) * p2m44 * semi
		m.eph.Cis = float64(s(142, 16)) * p2m30
		m.eph.Cic = float64(s(158, 16)) * p2m30
		m.eph.Crs = float64(s(174, 24)) * p2m8
		m.eph.Crc = float64(s(198, 24)) * p2m8
		m.eph.Cus = float64(s(222, 21)) * p2m30
		m.eph.Cuc = float64(s(243, 21)) * p2m30
		m.hasEph2 = true
	}
	return m, nil
}

// AssembleBeiDouBCNAV2 combines message types 10 and 11 (Ephemeris I + II) into the
// kepler ephemeris. svid tags the constellation; kepler applies the GEO rotation
// for C01–C05/C59–C63.
func AssembleBeiDouBCNAV2(svid int, m10, m11 *BeiDouBCNAV2) (kepler.Ephemeris, clock.Model, error) {
	if m10 == nil || m11 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	eph := m10.eph
	eph.Omega0, eph.I0, eph.OmegaDot, eph.IDot = m11.eph.Omega0, m11.eph.I0, m11.eph.OmegaDot, m11.eph.IDot
	eph.Cis, eph.Cic, eph.Crs, eph.Crc, eph.Cus, eph.Cuc = m11.eph.Cis, m11.eph.Cic, m11.eph.Crs, m11.eph.Crc, m11.eph.Cus, m11.eph.Cuc
	eph.ID = gnss.BeiDou
	eph.SVID = svid
	return eph, clock.Model{ID: gnss.BeiDou}, nil
}
