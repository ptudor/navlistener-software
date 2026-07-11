package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// sbasRawWords builds a synthetic 250-bit SBAS L1 message (8-word buffer) with
// the given preamble and message type, and a valid trailing CRC-24Q over the
// leading 226 bits — mirrors gnss/frame's own sbasWords test helper, needed
// here too since DecodeSBASL1 now enforces the CRC.
func sbasRawWords(preamble uint64, mt int) []uint32 {
	buf := make([]byte, 32)
	setAbsBits(buf, 0, 8, preamble)
	setAbsBits(buf, 8, 6, uint64(mt))
	crc := frame.CRC24QBits(buf, 0, 226)
	setAbsBits(buf, 226, 24, uint64(crc))

	words := make([]uint32, 8)
	for i := 0; i < 8; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestFeedSBASExcludesStaleEntries guards an SBAS PRN unseen past
// sbasStaleAfter (a decommissioned/dark GEO) must be omitted from the feed,
// not served forever with an ever-growing last_seen_s.
func TestFeedSBASExcludesStaleEntries(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.sbas[133] = &sbasState{prn: 133, provider: "WAAS", lastType: 1, lastSeen: now}

	if got := s.FeedSBAS(now.Add(sbasStaleAfter + time.Minute)); len(got) != 0 {
		t.Errorf("stale SBAS PRN still present: %+v", got)
	}
	if got := s.FeedSBAS(now.Add(time.Minute)); len(got) != 1 {
		t.Errorf("fresh SBAS PRN missing: %+v", got)
	}
}

// TestApplySBASSkipsUpdateWhenPreambleNotOK guards a structurally
// self-consistent (valid CRC-24Q) message whose preamble doesn't match one of
// the three ICD-mandated SBAS values (0x53/0x9A/0xC6) must not update state at
// all — PreambleOK previously gated nothing, so even a non-standard-preamble
// message could set doNotUse.
func TestApplySBASSkipsUpdateWhenPreambleNotOK(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	// A message with a preamble not in {0x53, 0x9A, 0xC6}, type 0 (would set
	// doNotUse if applied) -- must be entirely ignored.
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 133, SigID: 0, Recv: now,
		Words: sbasRawWords(0x00, 0)})
	if got := s.FeedSBAS(now); len(got) != 0 {
		t.Fatalf("bad-preamble message must not create SBAS state: %+v", got)
	}

	// A message with a valid preamble and the same type 0 -- must apply normally.
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 133, SigID: 0, Recv: now,
		Words: sbasRawWords(0x53, 0)})
	got := s.FeedSBAS(now)
	ent, ok := got["133"]
	if !ok || ent.HealthCode != 3 {
		t.Fatalf("valid-preamble message must apply: %+v", got)
	}
}
