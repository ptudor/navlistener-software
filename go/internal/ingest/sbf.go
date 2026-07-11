package ingest

import (
	"bufio"
	"encoding/binary"
	"io"
	"time"
)

// Septentrio SBF framing (docs/CONSTELLATIONS.md §2.2). A block is: sync "$@",
// CRC-16 (LE), ID (LE; low 13 bits = block number, high 3 = revision), Length (LE;
// total block length incl the 8-byte header, a multiple of 4), then the body. The
// CRC-16-CCITT covers ID+Length+body. SBF delivers already-de-interleaved ICD nav
// bits, so the collector emits the raw block body tagged with its block number for
// the (block-specific) decoders to consume.
const (
	sbfSync1     = '$'
	sbfSync2     = '@'
	sbfMaxLength = 1 << 16
)

func scanSBF(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		if err := syncTo(br, sbfSync1, sbfSync2); err != nil {
			return err
		}
		// peek the header+body without consuming it. On a length or CRC
		// failure, nothing here is Discarded, so the next syncTo call resumes
		// scanning byte-by-byte from right after the sync pattern instead of
		// skipping the whole claimed (possibly bogus) extent (up to sbfMaxLength =
		// 64 KiB), which may contain a real, complete block.
		hdr, err := br.Peek(6) // CRC(2), ID(2), Length(2), all LE
		if err != nil {
			return err
		}
		crc := binary.LittleEndian.Uint16(hdr[0:])
		id := binary.LittleEndian.Uint16(hdr[2:])
		length := int(binary.LittleEndian.Uint16(hdr[4:]))
		// Length counts the whole block (2 sync + 6 header + body) and is a
		// multiple of 4. Body length is length − 8.
		if length < 8 || length > sbfMaxLength || length%4 != 0 {
			onErr("sbf_length")
			continue
		}
		total := length - 2 // header(6) + body(length-8); the 2 sync bytes are already consumed
		peeked, err := br.Peek(total)
		if err != nil {
			return err
		}
		body := peeked[6:total]
		// CRC-16-CCITT over ID + Length + body.
		if crc16ccitt(peeked[2:6], body) != crc {
			onErr("sbf_crc")
			continue
		}
		// Copy out before Discard invalidates br's buffer view (peeked aliases it).
		body = append([]byte(nil), body...)
		if _, err := br.Discard(total); err != nil {
			return err
		}
		emit(&RawFrame{
			Recv:    now(),
			Source:  source,
			MsgType: int(id & 0x1FFF), // block number (drop the 3-bit revision)
			Bytes:   body,
		})
	}
}

// crc16ccitt is the CRC-16-CCITT (poly 0x1021, init 0) Septentrio uses, computed
// over the given byte groups in order.
func crc16ccitt(groups ...[]byte) uint16 {
	var crc uint16
	for _, g := range groups {
		for _, b := range g {
			crc ^= uint16(b) << 8
			for i := 0; i < 8; i++ {
				if crc&0x8000 != 0 {
					crc = (crc << 1) ^ 0x1021
				} else {
					crc <<= 1
				}
			}
		}
	}
	return crc
}
