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
		// peek the candidate frame (length + payload + CRC) without
		// consuming it. On any failure below, nothing past this single preamble
		// byte is Discarded, so the next loop iteration resumes scanning from the
		// very next byte instead of skipping the whole claimed (possibly bogus)
		// extent -- which, on a false sync, may contain a real, complete frame.
		lenBytes, err := br.Peek(2)
		if err != nil {
			return err
		}
		lenHi, lenLo := lenBytes[0], lenBytes[1]
		length := int(lenHi&0x03)<<8 | int(lenLo) // low 10 bits

		if length == 0 {
			// length 0 is a legal RTCM3 filler/keepalive message (no
			// message number, no payload) -- verify its CRC and drop it silently
			// (not an rtcm_length error), rather than flagging it as an error and
			// leaving its 3 CRC bytes in the stream to be rescanned as a false
			// preamble (a 0xD3 among them would consume up to 1026 more bytes).
			tail, err := br.Peek(5) // lenHi, lenLo, 3-byte CRC
			if err != nil {
				return err
			}
			full := append([]byte{b}, tail...)
			if !frame.CheckCRC24Q(full) {
				onErr("rtcm_crc")
				continue
			}
			if _, err := br.Discard(5); err != nil {
				return err
			}
			continue
		}
		if length < 3 || length > rtcmMaxLen { // < 3: no room for the 12-bit message number
			onErr("rtcm_length")
			continue
		}
		total := 2 + length + 3 // length bytes + payload + 3-byte CRC-24Q
		peeked, err := br.Peek(total)
		if err != nil {
			return err
		}
		// CRC-24Q covers preamble + length + payload + the trailing CRC (→ 0).
		// full is a fresh copy: peeked aliases br's internal buffer and is only
		// valid until the next read/Discard, but payload (sliced from full below)
		// must outlive this call, since it's handed off via emit.
		full := append([]byte{b}, peeked...)
		if !frame.CheckCRC24Q(full) {
			onErr("rtcm_crc")
			continue
		}
		if _, err := br.Discard(total); err != nil {
			return err
		}
		payload := full[3 : 3+length]
		msgNum := int(payload[0])<<4 | int(payload[1])>>4 // first 12 bits
		emit(&RawFrame{
			Recv:    now(),
			Source:  source,
			MsgType: msgNum,
			Bytes:   payload,
		})
	}
}
