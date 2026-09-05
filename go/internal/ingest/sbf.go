package ingest

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"time"
)

// Septentrio SBF framing (docs/CONSTELLATIONS.md §2.2). A block is: sync "$@",
// CRC-16 (LE), ID (LE; low 13 bits = block number, high 3 = revision), Length (LE;
// total block length incl the 8-byte header, a multiple of 4), then the body. The
// CRC-16-CCITT covers ID+Length+body. SBF delivers already-de-interleaved ICD nav
// bits, so the collector emits the raw block body tagged with its block number.
// SBF sources are deliberately capture-only today: the block number and body are
// retained with the original header for historian/future replay, but no block-specific live-state
// decoder is dispatched.
const (
	sbfSync1 = '$'
	sbfSync2 = '@'
	// sbfScanBuf is the bufio buffer scanSBF peeks whole blocks out of. The real
	// overrun invariant  is: the largest Peek extent is the largest
	// multiple-of-4 uint16 Length (65532) minus the 2 already-consumed sync bytes
	// = 65530 <= sbfScanBuf, so Peek(total) always fits. The guard in scanSBF is
	// therefore unreachable while Length is a uint16 — it exists to pin the
	// invariant against a future edit (shrinking this buffer, widening the length
	// field): a Peek past the buffer returns bufio.ErrBufferFull, which the
	// connector treats as a stream error — a permanent reconnect loop replaying
	// the same block. (The previous guard compared the uint16 length against
	// 1<<16, which was always false and enforced nothing.)
	sbfScanBuf = 1 << 16
)

func scanSBF(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error {
	br := bufio.NewReaderSize(r, sbfScanBuf)
	for {
		if err := syncTo(br, sbfSync1, sbfSync2); err != nil {
			return err
		}
		// peek the header+body without consuming it. On a length or CRC
		// failure, nothing here is Discarded, so the next syncTo call resumes
		// scanning byte-by-byte from right after the sync pattern instead of
		// skipping the whole claimed (possibly bogus) extent (up to ~64 KiB),
		// which may contain a real, complete block.
		hdr, err := br.Peek(6) // CRC(2), ID(2), Length(2), all LE
		if err != nil {
			return err
		}
		crc := binary.LittleEndian.Uint16(hdr[0:])
		id := binary.LittleEndian.Uint16(hdr[2:])
		length := int(binary.LittleEndian.Uint16(hdr[4:]))
		// Length counts the whole block (2 sync + 6 header + body) and is a
		// multiple of 4. Body length is length − 8. total is the Peek extent:
		// header(6) + body(length-8); the 2 sync bytes are already consumed.
		total := length - 2
		if length < 8 || length%4 != 0 || total > sbfScanBuf {
			onErr("sbf_length")
			continue
		}
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
		header := append([]byte{sbfSync1, sbfSync2}, peeked[:6]...)
		if _, err := br.Discard(total); err != nil {
			return err
		}
		emit(&RawFrame{
			Recv:      now(),
			Source:    source,
			MsgType:   int(id & 0x1FFF), // block number; revision remains in SBFHeader
			SBFHeader: header,
			Bytes:     body,
		})
	}
}

// crc16ccitt is the CRC-16-CCITT (poly 0x1021, init 0, unreflected) Septentrio
// uses (SBF-REF, cite-only per reference/REFERENCES.md §3c), computed over the
// given byte groups in order. validated against an independently-authored
// implementation of the same named algorithm (CPython's binascii.crc_hqx — see
// TestCRC16CCITTGoldenVectors), so a wrong polynomial/init/shift here cannot pass
// the suite. A vector from a REAL captured SBF block is still wanted to confirm
// Septentrio's on-wire CRC is this exact algorithm end-to-end; no Septentrio
// hardware exists in the fleet yet, and inventing one is prohibited.
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

// SBFRevision reports the recorded revision. Legacy body-only captures remain
// explicitly unknown; zero is a real revision and must not stand in for missing.
func (f *RawFrame) SBFRevision() (uint8, bool) {
	if len(f.SBFHeader) != 8 {
		return 0, false
	}
	return uint8(binary.LittleEndian.Uint16(f.SBFHeader[4:6]) >> 13), true
}

// RestoreSBF reconstructs a captured block without dispatching a live decoder.
// Header nil is the legacy body-only representation: readable, revision unknown,
// and no original wire header/CRC can be promised. New headers are validated
// against both the stored block number and body before reconstruction.
func RestoreSBF(block int, body, header []byte) (*RawFrame, error) {
	f := &RawFrame{MsgType: block, Bytes: append([]byte(nil), body...), SBFHeader: append([]byte(nil), header...)}
	if len(header) == 0 {
		return f, nil
	}
	if len(header) != 8 || header[0] != sbfSync1 || header[1] != sbfSync2 {
		return nil, fmt.Errorf("invalid SBF capture header")
	}
	id := binary.LittleEndian.Uint16(header[4:6])
	size := int(binary.LittleEndian.Uint16(header[6:8]))
	if int(id&0x1fff) != block || size != len(body)+8 || size%4 != 0 ||
		binary.LittleEndian.Uint16(header[2:4]) != crc16ccitt(header[4:8], body) {
		return nil, fmt.Errorf("SBF capture metadata/CRC mismatch")
	}
	return f, nil
}

// SBFWire returns the exact original block when its header was retained. Legacy
// captures return an error instead of inventing revision zero or an original CRC.
func (f *RawFrame) SBFWire() ([]byte, error) {
	if len(f.SBFHeader) == 0 {
		return nil, fmt.Errorf("legacy SBF capture: revision/header unknown")
	}
	if _, err := RestoreSBF(f.MsgType, f.Bytes, f.SBFHeader); err != nil {
		return nil, err
	}
	return append(append([]byte(nil), f.SBFHeader...), f.Bytes...), nil
}
