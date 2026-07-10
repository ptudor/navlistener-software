package ingest

import (
	"bufio"
	"io"
	"time"

	"github.com/ptudor/gnss/frame"
)

// RTCM3 framing (docs/CONSTELLATIONS.md §2.3). A message is: 0xD3 preamble, 6 bits
// reserved + 10-bit big-endian length, that many payload bytes, then a 3-byte
// CRC-24Q over the preamble, length, and payload. The message number is the first
// 12 bits of the payload. The collector emits the raw payload tagged with its
// message number for the RTCM decoders (ephemeris 1019/1020/1041/1042/1044/1045/
// 1046, SSR 1057–1068) to consume.
const (
	rtcmPreamble = 0xD3
	rtcmMaxLen   = 1023 // 10-bit length field
)

func scanRTCM(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error {
	br := bufio.NewReaderSize(r, 1<<15)
	for {
		// Find the preamble.
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		if b != rtcmPreamble {
			continue
		}
		lenHi, err := br.ReadByte()
		if err != nil {
			return err
		}
		lenLo, err := br.ReadByte()
		if err != nil {
			return err
		}
		length := int(lenHi&0x03)<<8 | int(lenLo) // low 10 bits
		if length < 3 || length > rtcmMaxLen {    // < 3: no room for the 12-bit message number
			onErr("rtcm_length")
			continue
		}
		rest := make([]byte, length+3) // payload + 3-byte CRC-24Q
		if _, err := io.ReadFull(br, rest); err != nil {
			return err
		}
		// CRC-24Q covers preamble + length + payload + the trailing CRC (→ 0).
		full := make([]byte, 0, 3+length+3)
		full = append(full, b, lenHi, lenLo)
		full = append(full, rest...)
		if !frame.CheckCRC24Q(full) {
			onErr("rtcm_crc")
			continue
		}
		payload := rest[:length]
		msgNum := int(payload[0])<<4 | int(payload[1])>>4 // first 12 bits
		emit(&RawFrame{
			Recv:    now(),
			Source:  source,
			MsgType: msgNum,
			Bytes:   payload,
		})
	}
}
