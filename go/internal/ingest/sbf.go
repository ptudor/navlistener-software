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
		hdr := make([]byte, 6) // CRC(2), ID(2), Length(2), all LE
		if _, err := io.ReadFull(br, hdr); err != nil {
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
		body := make([]byte, length-8)
		if _, err := io.ReadFull(br, body); err != nil {
			return err
		}
		// CRC-16-CCITT over ID + Length + body.
		if crc16ccitt(hdr[2:6], body) != crc {
			onErr("sbf_crc")
			continue
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
