package state

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
)

// gloWords builds a 4-word (128-bit) GLONASS string block with the given string
// number (bits 1-4) and lets fill set any additional fields via the shared
// setAbsBits helper — the same MSB-first convention glonassBlock's BitReader
// uses internally.
func gloWords(number int, fill func(buf []byte)) []uint32 {
	buf := make([]byte, 16)
	setAbsBits(buf, 1, 4, uint64(number))
	if fill != nil {
		fill(buf)
	}
	words := make([]uint32, 4)
	for i := 0; i < 4; i++ {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	return words
}

// TestApplyGloAlmanacRejectsBeforeNAKnown guards an almanac string pair
// decoded before string 5 has ever set the frame day-number NA must not be
// stored (it would mis-epoch PropagateAlmanacECEF with Alm.NA=0, outside the
// ICD's valid 1..1461 range). Once a valid NA has been seen, the identical pair
// must store normally.
func TestApplyGloAlmanacRejectsBeforeNAKnown(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const slot = 7
	const svid = 12 // transmitting SV, distinct from the almanac's subject slot

	first := gloWords(6, func(buf []byte) { setAbsBits(buf, 8, 5, slot) })
	second := gloWords(7, nil)

	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now, Words: first})
	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now, Words: second})

	s.gloAlmMu.Lock()
	_, stored := s.gloAlmanac[slot]
	s.gloAlmMu.Unlock()
	if stored {
		t.Fatal("almanac pair decoded before any string 5 must not be stored (NA unknown)")
	}

	// Now send string 5 (sets NA), then repeat the identical pair.
	na5 := gloWords(5, func(buf []byte) { setAbsBits(buf, 5, 11, 615) })
	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now, Words: na5})

	first2 := gloWords(6, func(buf []byte) { setAbsBits(buf, 8, 5, slot) })
	second2 := gloWords(7, nil)
	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now, Words: first2})
	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now, Words: second2})

	s.gloAlmMu.Lock()
	entry, stored := s.gloAlmanac[slot]
	s.gloAlmMu.Unlock()
	if !stored {
		t.Fatal("almanac pair decoded after a valid string 5 must be stored")
	}
	if entry.entry.Alm.NA != 615 {
		t.Errorf("stored NA = %d, want 615", entry.entry.Alm.NA)
	}
}
