package frame

import (
	"errors"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
)

// GPS/QZSS LNAV decoding (IS-GPS-200 §20.3.3; IS-QZSS-PNT defers to it, so QZSS L1
// C/A shares this path with gnssId=5 and PRN = svId+192). A subframe is 10 words ×
// 30 bits; the 24 data bits of each word are extracted after a parity check and
// laid contiguously, then fields are read at their ICD offsets. Scale factors and
// semicircle→radian conversion (using the ICD π) match docs/MATH.md.

// scale factors (powers of two) as multiplicative constants.
const (
	p2m5  = 1.0 / (1 << 5)  // 2^-5
	p2m19 = 1.0 / (1 << 19) // 2^-19
	p2m29 = 1.0 / (1 << 29) // 2^-29
	p2m31 = 1.0 / (1 << 31) // 2^-31
	p2m33 = 1.0 / (1 << 33) // 2^-33
	p2m43 = 1.0 / float64(uint64(1)<<43)
	p2m55 = 1.0 / float64(uint64(1)<<55)
	p2p4  = 16.0 // 2^4
)

// ErrParity is returned when a subframe word fails its LNAV parity check.
var ErrParity = errors.New("frame: LNAV parity check failed")

// ErrShortFrame is returned when a decoder is handed too few words/bytes.
var ErrShortFrame = errors.New("frame: short frame")

// errIODMismatch is returned when the parts of an ephemeris carry inconsistent
// issue-of-data tags (they belong to different data sets).
var errIODMismatch = errors.New("frame: ephemeris IOD mismatch")

// GPSSubframe holds the decoded fields of a single LNAV subframe. Only the fields
// belonging to this subframe's ID are populated; a full ephemeris is assembled
// from subframes 1, 2, and 3 (AssembleGPS).
type GPSSubframe struct {
	SubframeID int
	TOW        float64 // seconds of week (HOW truncated TOW × 6)

	// Subframe 1 (clock & health).
	WN            int
	URAIndex      int
	Health        int
	IODC          int
	Toc           float64
	Af0, Af1, Af2 float64
	TGD           float64

	// Subframe 2/3 (ephemeris), scaled to SI (radians, seconds, metres).
	IODE int
	eph  kepler.Ephemeris // partial: sf2 or sf3 elements
	Toe  float64
}

// DecodeGPSLNAV decodes one LNAV subframe from ten 30-bit words as delivered by
// UBX-RXM-SFRBX (and, verified against real ZED-F9T frames, SBF GPSRawCA): the
// receiver has already validated parity and resolved the D30* data inversion, so
// each word carries the true 24 data bits in bits 29..6. We extract those directly
// — re-running the broadcast parity on receiver-supplied words fails, because the
// receiver normalises them (this is why RTKLIB also trusts u-blox SFRBX). The
// GPSParity primitive remains for a future raw-signal path. Bounds are still fully
// checked by BitReader; a short input is an error.
func DecodeGPSLNAV(words []uint32) (*GPSSubframe, error) {
	if len(words) < 10 {
		return nil, ErrShortFrame
	}
	// Pack the 24 data bits (bits 29..6) of each word contiguously (240 bits).
	buf := make([]byte, 30)
	for i := 0; i < 10; i++ {
		data := (words[i] >> 6) & 0xFFFFFF
		for b := 0; b < 24; b++ {
			if data&(1<<uint(23-b)) != 0 {
				pos := i*24 + b
				buf[pos>>3] |= 1 << uint(7-(pos&7))
			}
		}
	}
	r := NewBitReaderN(buf, 240)

	// HOW (word 2): TOW count in data bits 1..17 (×6 s), subframe ID in bits 20..22.
	towCount, _ := r.Bits(field(2, 1), 17)
	sfID, _ := r.Bits(field(2, 20), 3)

	sf := &GPSSubframe{SubframeID: int(sfID), TOW: float64(towCount) * 6}
	switch sfID {
	case 1:
		decodeGPSSf1(r, sf)
	case 2:
		decodeGPSSf2(r, sf)
	case 3:
		decodeGPSSf3(r, sf)
	}
	return sf, nil
}

// field maps an ICD "word w (1-indexed), data bit a (1-indexed, 1..24)" to the
// absolute bit offset in the packed 240-bit data buffer.
func field(w, a int) int { return (w-1)*24 + (a - 1) }

func decodeGPSSf1(r *BitReader, sf *GPSSubframe) {
	wn, _ := r.Bits(field(3, 1), 10)
	ura, _ := r.Bits(field(3, 13), 4)
	hlth, _ := r.Bits(field(3, 17), 6)
	iodcHi, _ := r.Bits(field(3, 23), 2)
	iodcLo, _ := r.Bits(field(8, 1), 8)
	toc, _ := r.Bits(field(8, 9), 16)
	tgd, _ := r.Signed(field(7, 17), 8)
	af2, _ := r.Signed(field(9, 1), 8)
	af1, _ := r.Signed(field(9, 9), 16)
	af0, _ := r.Signed(field(10, 1), 22)

	sf.WN = int(wn)
	sf.URAIndex = int(ura)
	sf.Health = int(hlth)
	sf.IODC = int(iodcHi<<8 | iodcLo)
	sf.Toc = float64(toc) * p2p4
	sf.TGD = float64(tgd) * p2m31
	sf.Af2 = float64(af2) * p2m55
	sf.Af1 = float64(af1) * p2m43
	sf.Af0 = float64(af0) * p2m31
}

func decodeGPSSf2(r *BitReader, sf *GPSSubframe) {
	semi := physconst.Pi
	iode, _ := r.Bits(field(3, 1), 8)
	crs, _ := r.Signed(field(3, 9), 16)
	dn, _ := r.Signed(field(4, 1), 16)
	m0, _ := r.ConcatSigned(field(4, 17), 8, field(5, 1), 24)
	cuc, _ := r.Signed(field(6, 1), 16)
	ecc, _ := r.Concat(field(6, 17), 8, field(7, 1), 24)
	cus, _ := r.Signed(field(8, 1), 16)
	sqrtA, _ := r.Concat(field(8, 17), 8, field(9, 1), 24)
	toe, _ := r.Bits(field(10, 1), 16)

	sf.IODE = int(iode)
	sf.Toe = float64(toe) * p2p4
	sf.eph.Crs = float64(crs) * p2m5
	sf.eph.DeltaN = float64(dn) * p2m43 * semi
	sf.eph.M0 = float64(m0) * p2m31 * semi
	sf.eph.Cuc = float64(cuc) * p2m29
	sf.eph.Ecc = float64(ecc) * p2m33
	sf.eph.Cus = float64(cus) * p2m29
	sf.eph.SqrtA = float64(sqrtA) * p2m19
	sf.eph.Toe = sf.Toe
}

func decodeGPSSf3(r *BitReader, sf *GPSSubframe) {
	semi := physconst.Pi
	cic, _ := r.Signed(field(3, 1), 16)
	omg0, _ := r.ConcatSigned(field(3, 17), 8, field(4, 1), 24)
	cis, _ := r.Signed(field(5, 1), 16)
	i0, _ := r.ConcatSigned(field(5, 17), 8, field(6, 1), 24)
	crc, _ := r.Signed(field(7, 1), 16)
	omega, _ := r.ConcatSigned(field(7, 17), 8, field(8, 1), 24)
	omgDot, _ := r.Signed(field(9, 1), 24)
	iode, _ := r.Bits(field(10, 1), 8)
	idot, _ := r.Signed(field(10, 9), 14)

	sf.IODE = int(iode)
	sf.eph.Cic = float64(cic) * p2m29
	sf.eph.Omega0 = float64(omg0) * p2m31 * semi
	sf.eph.Cis = float64(cis) * p2m29
	sf.eph.I0 = float64(i0) * p2m31 * semi
	sf.eph.Crc = float64(crc) * p2m5
	sf.eph.Omega = float64(omega) * p2m31 * semi
	sf.eph.OmegaDot = float64(omgDot) * p2m43 * semi
	sf.eph.IDot = float64(idot) * p2m43 * semi
}

// AssembleGPS combines a matching subframe 1/2/3 triple for one SV into the
// kepler ephemeris and clock model the propagators consume. It requires the IODE
// of subframes 2 and 3 to agree and their low 8 bits to match the IODC (the LNAV
// data-set consistency rule, IS-GPS-200 §20.3.4.4). id/svid tag the constellation
// (GPS or QZSS with PRN = svid+192).
func AssembleGPS(id gnss.GNSSID, svid int, sf1, sf2, sf3 *GPSSubframe) (kepler.Ephemeris, clock.Model, error) {
	if sf1 == nil || sf2 == nil || sf3 == nil {
		return kepler.Ephemeris{}, clock.Model{}, ErrShortFrame
	}
	if sf2.IODE != sf3.IODE || sf2.IODE != (sf1.IODC&0xFF) {
		return kepler.Ephemeris{}, clock.Model{}, errors.New("frame: LNAV IODE/IODC mismatch")
	}
	eph := sf2.eph // sf2 elements
	// Merge sf3 orbital elements.
	eph.Cic = sf3.eph.Cic
	eph.Omega0 = sf3.eph.Omega0
	eph.Cis = sf3.eph.Cis
	eph.I0 = sf3.eph.I0
	eph.Crc = sf3.eph.Crc
	eph.Omega = sf3.eph.Omega
	eph.OmegaDot = sf3.eph.OmegaDot
	eph.IDot = sf3.eph.IDot
	eph.ID = id
	eph.SVID = svid

	clk := clock.Model{
		ID:  id,
		Af0: sf1.Af0, Af1: sf1.Af1, Af2: sf1.Af2,
		Toc: sf1.Toc, TGD: sf1.TGD,
	}
	return eph, clk, nil
}
