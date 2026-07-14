package state

import (
	"encoding/binary"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// gloWords is THE canonical builder for synthetic GLONASS strings in this
// package (independent validation): it builds the 4-word (128-bit) string block
// with the given string number (bits 1-4), lets fill set any additional fields
// via the shared setAbsBits/setSignMag helpers (the same MSB-first convention
// glonassBlock's BitReader uses internally), and ALWAYS stamps the ICD §4.7
// check bits — a hand-packed string without them is rejected by every decoder
// since regression fix with a confusing ErrGLONASSHamming. Build new test strings on top
// of this (see glonassStringWords / glonassString4Frame); do not pack words by
// hand.
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
	frame.StampGLONASSHamming(words) // valid §4.7 check bits
	return words
}

// TestGloAlmanacFrame5Strings1415Skipped guards frame 5 carries almanac only for
// slots 21–24 (strings 6–13); its strings 14/15 are B1/B2/KP UT1/leap data, not almanac.
// When the frame's base slot (string 6) is ≥ 21 (frame 5), strings 14/15 must NOT be paired
// as an almanac; in frames 1–4 (base 1..16) they must pair normally.
func TestGloAlmanacFrame5Strings1415Skipped(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const svid = 12
	mk := func(s *Store, num, slot int, at time.Time) {
		w := gloWords(num, func(buf []byte) {
			if slot > 0 {
				setAbsBits(buf, 8, 5, uint64(slot))
			}
		})
		s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: at, Words: w})
	}
	send5 := func(s *Store, at time.Time) {
		na5 := gloWords(5, func(buf []byte) { setAbsBits(buf, 5, 11, 615) })
		s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: at, Words: na5})
	}
	stored := func(s *Store, slot int) bool {
		s.gloAlmMu.Lock()
		defer s.gloAlmMu.Unlock()
		_, ok := s.gloAlmanac[slot]
		return ok
	}

	// Frame 5: string 6/7 base slot 21 → strings 14/15 (a B1-bits misread "slot 23") skipped.
	s := New(4)
	send5(s, now)
	mk(s, 6, 21, now)
	mk(s, 7, 0, now.Add(2*time.Second)) // pairs → base slot 21
	mk(s, 14, 23, now.Add(4*time.Second))
	mk(s, 15, 0, now.Add(6*time.Second)) // frame-5 string 14/15: skipped
	if stored(s, 23) {
		t.Error("frame-5 string 14/15 wrongly stored an almanac from B1/B2/KP bits ")
	}

	// Frame 1: base slot 1 → string 14/15 (slot 5) stored normally.
	s2 := New(4)
	send5(s2, now)
	mk(s2, 6, 1, now)
	mk(s2, 7, 0, now.Add(2*time.Second)) // base slot 1
	mk(s2, 14, 5, now.Add(4*time.Second))
	mk(s2, 15, 0, now.Add(6*time.Second))
	if !stored(s2, 5) {
		t.Error("frame-1 string 14/15 almanac not stored — regression fix guard too broad")
	}
}

// TestGloNTDay guards day derivation: gloNTDay must return the current MT
// (UTC+3h) calendar day number NT within the four-year interval (1..1461), where NT=1 is
// 1 Jan of the interval's leap-year start (1996, 2000, …, 2024, 2028).
func TestGloNTDay(t *testing.T) {
	cases := []struct {
		name string
		when time.Time
		want int
	}{
		// 2024-01-01 00:00 MT == 2023-12-31 21:00 UTC → first day of the 2024–2027 interval.
		{"cycle start", time.Date(2023, 12, 31, 21, 0, 0, 0, time.UTC), 1},
		{"second day", time.Date(2024, 1, 1, 21, 0, 0, 0, time.UTC), 2},
		// 2027-12-31 12:00 MT → last day of the 1461-day interval.
		{"cycle end", time.Date(2027, 12, 31, 9, 0, 0, 0, time.UTC), 1461},
		// 2028-01-01 00:00 MT → rolls to the next interval, back to day 1.
		{"next cycle", time.Date(2027, 12, 31, 21, 0, 0, 0, time.UTC), 1},
	}
	for _, c := range cases {
		if got := gloNTDay(c.when); got != c.want {
			t.Errorf("%s: gloNTDay = %d, want %d", c.name, got, c.want)
		}
	}
}

// TestGloAlmanacPairCoherenceWindow guards the even/odd almanac string pair must
// arrive within one frame window. A stale even string pairing with a later frame's odd
// string merges two DIFFERENT subject satellites into a chimera almanac for the wrong slot.
func TestGloAlmanacPairCoherenceWindow(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	const svid = 12
	const slot = 7

	send5 := func(s *Store, at time.Time) {
		na5 := gloWords(5, func(buf []byte) { setAbsBits(buf, 5, 11, 615) })
		s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: at, Words: na5})
	}
	first := func() []uint32 { return gloWords(6, func(buf []byte) { setAbsBits(buf, 8, 5, slot) }) }
	second := func() []uint32 { return gloWords(7, nil) }

	// Odd string arrives 30 s after the even one → cross-frame → must NOT be stored.
	s := New(4)
	send5(s, now)
	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now, Words: first()})
	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now.Add(30 * time.Second), Words: second()})
	s.gloAlmMu.Lock()
	_, stale := s.gloAlmanac[slot]
	s.gloAlmMu.Unlock()
	if stale {
		t.Error("almanac pair 30 s apart (cross-frame) must not be stored")
	}

	// Odd string within the frame window → same frame → stored.
	s2 := New(4)
	send5(s2, now)
	s2.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now, Words: first()})
	s2.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: svid, SigID: 0, Recv: now.Add(2 * time.Second), Words: second()})
	s2.gloAlmMu.Lock()
	_, fresh := s2.gloAlmanac[slot]
	s2.gloAlmMu.Unlock()
	if !fresh {
		t.Error("almanac pair within the frame window must be stored")
	}
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
