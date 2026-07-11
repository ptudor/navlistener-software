package frame

import "encoding/binary"

// SBAS L1 C/A decoding (RTCA DO-229 / ICAO Annex 10). u-blox delivers each 250-bit
// SBAS message as one 8-word SFRBX, packed MSB-first across the full 32-bit words:
// an 8-bit preamble (one of 0x53/0x9A/0xC6, cycling), a 6-bit message type, a
// 212-bit body, and a 24-bit CRC. For the sbas health feed (docs/OUTPUT.md §1.5) we
// track the message type per GEO and flag MT 0 (do-not-use); the correction bodies
// (fast/long/iono) are augmentation payloads, not decoded here. Validated against a
// real ZED-F9P capture (WAAS PRNs 131/133/135, valid message types).

// sbasPreambles are the three cycling SBAS message preambles.
var sbasPreambles = [3]uint64{0x53, 0x9A, 0xC6}

// SBASL1 is the decoded header of one SBAS L1 message.
type SBASL1 struct {
	PRN        int
	Type       int    // message type 0–63
	Provider   string // augmentation system, from the PRN
	DoNotUse   bool   // message type 0: "do not use for safety applications"
	PreambleOK bool   // the preamble matched one of the three SBAS values
}

// DecodeSBASL1 decodes one SBAS L1 message. prn is the SBAS PRN (u-blox delivers it
// directly as the svId for gnssId 1, e.g. 131/133/135 for WAAS).
func DecodeSBASL1(prn int, words []uint32) (*SBASL1, error) {
	if len(words) < 8 {
		return nil, ErrShortFrame
	}
	buf := make([]byte, 32)
	for i := 0; i < 8; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	// the 250-bit message ends in a DO-229 CRC-24Q (bits 226-249) that was
	// never checked — a single corrupted bit (e.g. the 6-bit type field flipping to
	// 0) fabricated a "do not use for safety applications" alarm, the exact event
	// this feed exists to report. Not byte-aligned (250 isn't a multiple of 8), so
	// this uses the bit-level CRC-24Q helper (CRC24QBits) rather than CheckCRC24Q.
	if !CheckCRC24QBits(buf, 0, 250) {
		return nil, ErrBadCRC
	}
	r := NewBitReaderN(buf, 256)
	pre, _ := r.Bits(0, 8)
	mt, _ := r.Bits(8, 6)

	m := &SBASL1{PRN: prn, Type: int(mt), Provider: SBASProvider(prn), DoNotUse: mt == 0}
	for _, p := range sbasPreambles {
		if pre == p {
			m.PreambleOK = true
		}
	}
	return m, nil
}

// SBASProvider maps an SBAS PRN to its augmentation-system name
// (docs/CONSTELLATIONS.md §1). Unknown PRNs return "SBAS".
func SBASProvider(prn int) string {
	switch prn {
	case 131, 133, 135, 138:
		return "WAAS" // United States
	case 121, 123, 136, 126:
		return "EGNOS" // Europe
	case 129, 137:
		return "MSAS" // Japan
	case 127, 128, 132:
		return "GAGAN" // India
	case 125, 140, 141:
		return "SDCM" // Russia
	case 130, 143, 144:
		return "BDSBAS" // China
	case 134:
		return "KASS" // Korea
	case 122, 124:
		return "SouthPAN" // Australia / New Zealand
	default:
		return "SBAS"
	}
}
