package state

import (
	"encoding/binary"
	"fmt"
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

func TestGloAlmanacOrderedSourceCoherence(t *testing.T) {
	for _, replay := range []bool{false, true} {
		for _, tc := range []struct {
			name            string
			delta           time.Duration
			source, session string
			sig             int
			want            bool
		}{
			{"boundary", 8 * time.Second, "a", "boot", 0, true},
			{"outside", 8*time.Second + time.Nanosecond, "a", "boot", 0, false},
			{"backwards", -time.Millisecond, "a", "boot", 0, false},
			{"old foreign frame", -time.Hour, "b", "boot", 0, false},
			{"foreign source", 2 * time.Second, "b", "boot", 0, false},
			{"foreign boot", 2 * time.Second, "a", "new", 0, false},
			{"other signal", 2 * time.Second, "a", "boot", 2, false},
			{"day rollover", 2 * time.Second, "a", "boot", 0, true},
		} {
			t.Run(fmt.Sprintf("%s/replay=%v", tc.name, replay), func(t *testing.T) {
				s := New(1)
				s.setGloNA(615)
				at := time.Date(2026, 9, 4, 23, 59, 59, 0, time.UTC)
				even := &ingest.RawFrame{GnssID: gnss.GLONASS, SvID: 12, Source: "a", Session: "boot", Recv: at, Words: gloWords(6, func(b []byte) { setAbsBits(b, 8, 5, 7) })}
				odd := &ingest.RawFrame{GnssID: gnss.GLONASS, SvID: 12, Source: tc.source, Session: tc.session, SigID: tc.sig, Recv: at.Add(tc.delta), Words: gloWords(7, nil)}
				if replay {
					even.RecvLocal = at.Add(72 * time.Hour)
					odd.RecvLocal = even.RecvLocal.Add(time.Millisecond)
				}
				s.Apply(even)
				s.Apply(odd)
				if _, got := s.gloAlmanac[7]; got != tc.want {
					t.Fatalf("stored=%v want %v", got, tc.want)
				}
			})
		}
	}
}

// TestGloAlmanacInterleavedRelaysPairIndependently is the astra-6 verification
// regression for source-coherence gate: N stations (and L1OF+L2OF of
// one dual-band receiver, regression fix) relay the same broadcast into ONE shared SV
// state, and their strings interleave on the wire. Each relay must pair its
// own even/odd halves without disturbing another relay's buffered even string.
func TestGloAlmanacInterleavedRelaysPairIndependently(t *testing.T) {
	type relay struct {
		src, session string
		sig          int
	}
	a := relay{"a", "boot", 0}
	b := relay{"b", "boot", 0}
	l2 := relay{"a", "boot", 2}
	for _, tc := range []struct {
		name  string
		order []struct {
			num int
			r   relay
			d   time.Duration
		}
		want bool
	}{
		{"two stations A6 B6 A7 B7", []struct {
			num int
			r   relay
			d   time.Duration
		}{{6, a, 0}, {6, b, 5 * time.Millisecond}, {7, a, 2 * time.Second}, {7, b, 2*time.Second + 5*time.Millisecond}}, true},
		{"one station L1OF+L2OF interleaved", []struct {
			num int
			r   relay
			d   time.Duration
		}{{6, a, 0}, {6, l2, time.Millisecond}, {7, a, 2 * time.Second}, {7, l2, 2*time.Second + time.Millisecond}}, true},
		{"relay A odd lost, relay B still pairs", []struct {
			num int
			r   relay
			d   time.Duration
		}{{6, a, 0}, {6, b, time.Millisecond}, {7, b, 2 * time.Second}}, true},
		{"relay B odd lost, relay A still pairs", []struct {
			num int
			r   relay
			d   time.Duration
		}{{6, a, 0}, {6, b, time.Millisecond}, {7, a, 2 * time.Second}}, true},
		{"only a foreign odd string never pairs", []struct {
			num int
			r   relay
			d   time.Duration
		}{{6, a, 0}, {7, b, 2 * time.Second}}, false},
		{"same relay stale even is consumed by its own late odd", []struct {
			num int
			r   relay
			d   time.Duration
		}{{6, a, 0}, {7, a, 9 * time.Second}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(1)
			s.setGloNA(615)
			at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
			for _, step := range tc.order {
				var fill func([]byte)
				if step.num == 6 {
					fill = func(b []byte) { setAbsBits(b, 8, 5, 7) }
				}
				s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: 12, Source: step.r.src, Session: step.r.session, SigID: step.r.sig, Recv: at.Add(step.d), Words: gloWords(step.num, fill)})
			}
			if _, got := s.gloAlmanac[7]; got != tc.want {
				t.Fatalf("slot 7 stored=%v want %v", got, tc.want)
			}
		})
	}
}

// TestGloAlmanacPendingIsBounded: a feeder minting a fresh session per reconnect
// (or an adversary) cannot grow the per-relay even-string buffer without bound,
// and a relay whose mate never arrives is aged out on the collector clock.
func TestGloAlmanacPendingIsBounded(t *testing.T) {
	s := New(1)
	s.setGloNA(615)
	at := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	even := func(session string, local time.Time) *ingest.RawFrame {
		return &ingest.RawFrame{GnssID: gnss.GLONASS, SvID: 12, Source: "a", Session: session, Recv: at, RecvLocal: local, Words: gloWords(6, func(b []byte) { setAbsBits(b, 8, 5, 7) })}
	}
	for i := 0; i < 5*gloAlmPendingMax; i++ {
		s.Apply(even(fmt.Sprint("boot-", i), at.Add(time.Duration(i)*time.Millisecond)))
	}
	st := s.shardFor(Key{G: gnss.GLONASS, Sv: 12}).m[Key{G: gnss.GLONASS, Sv: 12}]
	if n := len(st.gloAlmPending); n != gloAlmPendingMax {
		t.Fatalf("pending relays = %d, want cap %d", n, gloAlmPendingMax)
	}
	// The newest relay survived eviction and can still pair.
	last := fmt.Sprint("boot-", 5*gloAlmPendingMax-1)
	s.Apply(&ingest.RawFrame{GnssID: gnss.GLONASS, SvID: 12, Source: "a", Session: last, Recv: at.Add(2 * time.Second), RecvLocal: at.Add(time.Second), Words: gloWords(7, nil)})
	if _, ok := s.gloAlmanac[7]; !ok {
		t.Fatal("newest relay could not pair after eviction of older relays")
	}
	// Idle relays age out on the collector clock once a fresh even string arrives.
	s.Apply(even("fresh", at.Add(gloAlmPendingStale+time.Minute)))
	if n := len(st.gloAlmPending); n != 1 {
		t.Fatalf("stale relays not pruned: %d pending, want 1", n)
	}
}
