package ingest

import (
	"bufio"
	"encoding/binary"
	"io"
	"time"

	"github.com/ptudor/gnss"
)

// UBX protocol constants (u-blox interface description). SFRBX carries raw nav
// subframe words per gnssId/sigId (docs/CONSTELLATIONS.md §2.1).
const (
	ubxSync1      = 0xB5
	ubxSync2      = 0x62
	ubxClassRXM   = 0x02
	ubxIDSFRBX    = 0x13
	ubxMaxPayload = 1 << 14 // guard against a corrupt length prefix
)

// scanUBX reads a UBX byte stream and calls emit for each UBX-RXM-SFRBX message,
// converted to a RawFrame. It resynchronises on the 0xB5 0x62 sync pattern and
// drops any message whose Fletcher checksum fails, so a mid-stream connect or a
// corrupt frame never derails the reader. It returns when the reader errors (EOF /
// connection drop), which the connector treats as a reconnect trigger.
func scanUBX(r io.Reader, source string, now func() time.Time, emit func(*RawFrame), onErr func(kind string)) error {
	br := bufio.NewReaderSize(r, 1<<16)
	for {
		// Resynchronise to the sync pattern.
		if err := syncTo(br, ubxSync1, ubxSync2); err != nil {
			return err
		}
		hdr := make([]byte, 4) // class, id, length(2, LE)
		if _, err := io.ReadFull(br, hdr); err != nil {
			return err
		}
		length := int(binary.LittleEndian.Uint16(hdr[2:]))
		if length > ubxMaxPayload {
			onErr("ubx_length") // implausible length: skip and resync
			continue
		}
		payload := make([]byte, length+2) // payload + 2 checksum bytes
		if _, err := io.ReadFull(br, payload); err != nil {
			return err
		}
		body := payload[:length]
		ckA, ckB := fletcher8(hdr[0], hdr[1], hdr[2], hdr[3], body)
		if ckA != payload[length] || ckB != payload[length+1] {
			onErr("ubx_checksum")
			continue
		}
		if hdr[0] == ubxClassRXM && hdr[1] == ubxIDSFRBX {
			if f := parseSFRBX(body, source, now()); f != nil {
				emit(f)
			} else {
				onErr("ubx_sfrbx")
			}
		}
	}
}

// parseSFRBX converts a UBX-RXM-SFRBX payload to a RawFrame. Layout (F9/M9
// generation): gnssId, svId, sigId, freqId, numWords, reserved, version,
// reserved, then numWords little-endian 32-bit dwrds each holding one native nav
// word right-aligned. Returns nil on a malformed/short payload.
func parseSFRBX(p []byte, source string, recv time.Time) *RawFrame {
	if len(p) < 8 {
		return nil
	}
	gnssID := gnss.GNSSID(p[0])
	svID := int(p[1])
	sigID := int(p[2])
	freqID := int(p[3])
	numWords := int(p[4])
	if numWords == 0 || 8+numWords*4 > len(p) {
		return nil
	}
	words := make([]uint32, numWords)
	for i := 0; i < numWords; i++ {
		words[i] = binary.LittleEndian.Uint32(p[8+i*4:])
	}
	return &RawFrame{
		Recv:   recv,
		Source: source,
		GnssID: gnssID,
		SvID:   svID,
		SigID:  sigID,
		FreqID: freqID,
		Words:  words,
	}
}

// syncTo advances the reader until the two-byte sync pattern s1,s2 is consumed.
func syncTo(br *bufio.Reader, s1, s2 byte) error {
	for {
		b, err := br.ReadByte()
		if err != nil {
			return err
		}
		if b != s1 {
			continue
		}
		b2, err := br.ReadByte()
		if err != nil {
			return err
		}
		if b2 == s2 {
			return nil
		}
		// Not the pattern; b2 might itself be s1 — push it back to re-examine.
		if b2 == s1 {
			_ = br.UnreadByte()
		}
	}
}

// fletcher8 computes the UBX 8-bit Fletcher checksum over class, id, the two
// length bytes, and the payload.
func fletcher8(cls, id, lenLo, lenHi byte, body []byte) (ckA, ckB byte) {
	add := func(x byte) {
		ckA += x
		ckB += ckA
	}
	add(cls)
	add(id)
	add(lenLo)
	add(lenHi)
	for _, x := range body {
		add(x)
	}
	return ckA, ckB
}
