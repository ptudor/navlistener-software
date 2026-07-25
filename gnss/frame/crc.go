package frame

// CRC24Q computes the CRC-24Q checksum (polynomial 0x1864CFB, init 0, no reflect,
// MSB-first over whole bytes) used by Galileo I/NAV·F/NAV·C/NAV, BeiDou B-CNAV,
// NavIC SPS, and RTCM3 (docs/CONSTELLATIONS.md §2). The caller passes byte-aligned
// frame data; the 24-bit result is right-aligned.
func CRC24Q(data []byte) uint32 {
	const poly = 0x1864CFB
	var crc uint32
	for _, b := range data {
		crc ^= uint32(b) << 16
		for i := 0; i < 8; i++ {
			crc <<= 1
			if crc&0x1000000 != 0 {
				crc ^= poly
			}
		}
	}
	return crc & 0xFFFFFF
}

// CheckCRC24Q reports whether data ends with a valid CRC-24Q: the last three bytes
// are the transmitted CRC over the preceding bytes. It runs the CRC over the whole
// buffer, which yields zero exactly when the trailing CRC is correct. A buffer
// shorter than the 3-byte CRC is never valid.
func CheckCRC24Q(data []byte) bool {
	if len(data) < 3 {
		return false
	}
	return CRC24Q(data) == 0
}

// CRC24QBits computes CRC-24Q (see CRC24Q) over an arbitrary bit range
// [bitOffset, bitOffset+bitLen) of data, MSB-first within each byte (matching
// BitReader's convention) — not necessarily byte-aligned. Used where the ICD's
// CRC coverage doesn't land on a byte boundary (GPS/QZSS CNAV's 300-bit
// message, CRC over bits 0-299 including its own trailing 24-bit CRC, has no
// byte-aligned split at all). Equivalent to CRC24Q for a byte-aligned,
// whole-byte range (TestCRC24QBitsMatchesByteWiseCRC24Q).
//
// The caller must supply a non-negative range wholly contained in data. This
// low-level primitive deliberately does not repeat BitReader's bounds checks;
// the frame decoders call it only with format constants after checking the
// delivered frame length.
func CRC24QBits(data []byte, bitOffset, bitLen int) uint32 {
	const poly = 0x1864CFB
	var crc uint32
	for i := 0; i < bitLen; i++ {
		p := bitOffset + i
		bit := uint32((data[p>>3] >> uint(7-(p&7))) & 1)
		crc ^= bit << 23
		crc <<= 1
		if crc&0x1000000 != 0 {
			crc ^= poly
		}
	}
	return crc & 0xFFFFFF
}

// CheckCRC24QBits reports whether the bit range [bitOffset, bitOffset+bitLen)
// of data is self-consistent under CRC-24Q — the same "run the CRC over data
// plus its own trailing CRC and check for zero remainder" idiom as
// CheckCRC24Q, generalized to a non-byte-aligned range. As with CRC24QBits, the
// range must already be known to fit in data; this helper only adds the minimum
// 24-bit checksum-width check.
func CheckCRC24QBits(data []byte, bitOffset, bitLen int) bool {
	if bitLen < 24 {
		return false
	}
	return CRC24QBits(data, bitOffset, bitLen) == 0
}
