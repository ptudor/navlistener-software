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
