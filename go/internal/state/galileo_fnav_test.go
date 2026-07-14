package state

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// fnavPageWords builds one raw 8-word (256-bit) Galileo E5a F/NAV page with the given page type
// and IODnav, at the bit offsets DecodeGalileoFNAV reads (page type @0, 6 bits; IODnav @12 for
// page 1 — after its SVID field — else @6). Every other field is left zero: this exercises the
// state-level dispatch → accumulate → assemble wiring, not orbit numerics (the field offsets and
// IODnav-match rules are covered by gnss/frame's F/NAV unit tests).
func fnavPageWords(pageType, iod int) []uint32 {
	buf := make([]byte, 32)
	setBits := func(off int, v uint64, n int) {
		for i := 0; i < n; i++ {
			if v&(1<<uint(n-1-i)) != 0 {
				p := off + i
				buf[p>>3] |= 1 << uint(7-(p&7))
			}
		}
	}
	setBits(0, uint64(pageType), 6)
	if pageType == 1 {
		setBits(12, uint64(iod), 10)
	} else {
		setBits(6, uint64(iod), 10)
	}
	words := make([]uint32, 8)
	for i := 0; i < 8; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	frame.StampGalileoFNAVCRC(words)
	return words
}

func hasCap(caps []StationCapability, g, sig int) bool {
	for _, c := range caps {
		if c.Gnss == g && c.Sig == sig {
			return true
		}
	}
	return false
}

// TestApplyGalileoFNAVAssemblesE5aEntry verifies F/NAV wiring: E5a F/NAV pages (u-blox
// sigId 3, E5a-I) are dispatched to applyGalileoFNAV, accumulated across the 4-page cadence,
// assembled into a SEPARATE E##@3 SV-state entry (not overwriting the E1-B I/NAV Sig:0 set),
// and recorded as an observed (Galileo, 3) capability.
func TestApplyGalileoFNAVAssemblesE5aEntry(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const svid, iod, source = 14, 7, "obs-fnav"

	for _, pt := range []int{1, 2, 3, 4} {
		s.Apply(&ingest.RawFrame{
			GnssID: gnss.Galileo, SvID: svid, SigID: 3, Source: source, Recv: now,
			Words: fnavPageWords(pt, iod),
		})
	}

	key := Key{G: gnss.Galileo, Sv: svid, Sig: 3}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph {
		t.Fatalf("E5a F/NAV ephemeris not assembled: %+v", st)
	}
	if st.iod != iod {
		t.Errorf("assembled IODnav = %d, want %d", st.iod, iod)
	}

	// The E1-B I/NAV entry (Sig:0) must be untouched — F/NAV is its own signal, keyed on sig 3.
	inavKey := Key{G: gnss.Galileo, Sv: svid, Sig: 0}
	if inav := s.shardFor(inavKey).m[inavKey]; inav != nil {
		t.Errorf("F/NAV must not create/modify the I/NAV Sig:0 entry, got %+v", inav)
	}

	if caps := s.FeedStationCapabilities(now)[source]; !hasCap(caps, int(gnss.Galileo), 3) {
		t.Errorf("observed capability (Galileo,3) not recorded: %+v", caps)
	}
}

// TestApplyGalileoFNAVR119RejectsBadPageType guards regression fix completeness half: an E5a
// frame whose page type is outside the F/NAV nominal set (here 7) is a decode error — it must
// NOT create SV state and must NOT install the durable capability fingerprint (a corrupt or
// mis-tagged frame otherwise permanently arms the capability-loss/impossible detectors).
func TestApplyGalileoFNAVR119RejectsBadPageType(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const svid, source = 21, "obs-junk"

	s.Apply(&ingest.RawFrame{
		GnssID: gnss.Galileo, SvID: svid, SigID: 3, Source: source, Recv: now,
		Words: fnavPageWords(7, 0), // page type 7: outside the nominal F/NAV set (1..6)
	})

	key := Key{G: gnss.Galileo, Sv: svid, Sig: 3}
	if st := s.shardFor(key).m[key]; st != nil {
		t.Errorf("a bad-page-type F/NAV frame must not create SV state, got %+v", st)
	}
	if caps := s.FeedStationCapabilities(now)[source]; hasCap(caps, int(gnss.Galileo), 3) {
		t.Errorf("a bad-page-type frame must not install durable (Galileo,3) capability: %+v", caps)
	}
}

func TestApplyGalileoFNAVDummyPageIsNotError(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const svid, source = 22, "obs-dummy"
	errCounter := metrics.DecodeErrorsTotal.WithLabelValues("2", "fnav")
	before := testutil.ToFloat64(errCounter)
	s.Apply(&ingest.RawFrame{
		GnssID: gnss.Galileo, SvID: svid, SigID: 3, Source: source, Recv: now,
		Words: fnavPageWords(63, 0),
	})
	if got := testutil.ToFloat64(errCounter) - before; got != 0 {
		t.Errorf("dummy page decode-error delta = %v, want 0", got)
	}
	key := Key{G: gnss.Galileo, Sv: svid, Sig: 3}
	if st := s.shardFor(key).m[key]; st != nil {
		t.Errorf("dummy page created SV state: %+v", st)
	}
}
