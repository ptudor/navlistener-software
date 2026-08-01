package state

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
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

// fnavPage1WithBGD builds an F/NAV page 1 carrying a raw BGD(E1,E5a) value at
// bit 143 (GAL-OS-SIS-ICD-2.2 Table 30, 10-bit two's complement × 2⁻³²)
// alongside the page type and IODnav — everything else zero, as in
// fnavPageWords.
func fnavPage1WithBGD(iod int, bgdRaw uint64) []uint32 {
	buf := make([]byte, 32)
	setBits := func(off int, v uint64, n int) {
		for i := 0; i < n; i++ {
			if v&(1<<uint(n-1-i)) != 0 {
				p := off + i
				buf[p>>3] |= 1 << uint(7-(p&7))
			}
		}
	}
	setBits(0, 1, 6) // page type 1
	setBits(12, uint64(iod), 10)
	setBits(143, bgdRaw&0x3FF, 10)
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

// TestApplyGalileoFNAVTGDRefresh guards regression fix (F/NAV half, the regression fix fold):
// BGD(E1,E5a) rides page 1 but sits OUTSIDE the IODnav-covered data set
// (GAL-OS-SIS-ICD-2.2 §5.1.9.2/Table 78), so a mid-data-set revision arrives on
// a page-1 repeat with an unchanged IODnav — which the assembly path below the
// fold early-returns on. Only the freshest-wins fold in the page-1 arm carries
// it into the served @3 clock, and deleting that fold failed nothing before
// this test. The expected value is the ALREADY-SCALED Eq. 19 group delay
// ((f_E1/f_E5a)²·BGD), because the F/NAV entry's tracked signal E5a is the f2
// of the (E1,E5a) clock pair — a fold that forwarded the raw BGD instead would
// also fail here.
func TestApplyGalileoFNAVTGDRefresh(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const svid, iod, source = 19, 23, "obs-fnav-bgd"
	const bgdScale = 1.0 / float64(uint64(1)<<32)

	apply := func(words []uint32) {
		s.Apply(&ingest.RawFrame{
			GnssID: gnss.Galileo, SvID: svid, SigID: 3, Source: source, Recv: now, Words: words,
		})
	}

	// Pages 2–4 first, then page 1: the INITIAL TGD therefore arrives through
	// the assembly (the page-1 fold is a no-op while haveClk is still false),
	// so whatever the second page 1 changes is attributable to the fold alone.
	for _, pt := range []int{2, 3, 4} {
		apply(fnavPageWords(pt, iod))
	}
	apply(fnavPage1WithBGD(iod, 41))

	key := Key{G: gnss.Galileo, Sv: svid, Sig: 3}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph {
		t.Fatalf("E5a F/NAV ephemeris not assembled: %+v", st)
	}
	if want := 41 * bgdScale * clock.E5aGroupDelayFactor; st.clk.TGD != want {
		t.Fatalf("initial TGD = %v, want %v", st.clk.TGD, want)
	}

	// Same data set (IODnav unchanged), revised BGD — negative, so a sign
	// regression in the refresh path cannot pass either.
	apply(fnavPage1WithBGD(iod, 1013)) // 10-bit two's complement −11
	if want := -11 * bgdScale * clock.E5aGroupDelayFactor; st.clk.TGD != want {
		t.Errorf("TGD after BGD revision = %v, want %v — the page-1 freshest-wins fold did not refresh the served @3 clock", st.clk.TGD, want)
	}
	if st.iod != iod {
		t.Errorf("IODnav = %d, want %d (a BGD revision must not be mistaken for a new data set)", st.iod, iod)
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
