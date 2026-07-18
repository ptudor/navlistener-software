package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/navlistener/internal/ingest"
)

// inavContentWords packs a 128-bit I/NAV nav-word content blob into the raw
// 8-word (256-bit) even+odd page DecodeGalileoINAV reads: content[0:112) at
// page bit 2 (even data), content[112:128) at page bit 130 (odd data), the odd
// part's Even/Odd flag (page bit 128) set to 1, CRC stamped. Mirrors the
// gnss/frame test builder — the state tests exercise dispatch/fold-in wiring,
// not field offsets (those are covered by the frame unit tests).
func inavContentWords(content []byte) []uint32 {
	page := make([]byte, 32)
	page[16] = 0x80 // odd-part Even/Odd flag, page bit 128 (regression fix gate)
	cp := func(dstOff, srcOff, n int) {
		for i := 0; i < n; i++ {
			sp := srcOff + i
			if content[sp>>3]&(1<<uint(7-(sp&7))) != 0 {
				dp := dstOff + i
				page[dp>>3] |= 1 << uint(7-(dp&7))
			}
		}
	}
	cp(2, 0, 112)
	cp(130, 112, 16)
	words := make([]uint32, 8)
	for i := 0; i < 8; i++ {
		words[i] = uint32(page[i*4])<<24 | uint32(page[i*4+1])<<16 | uint32(page[i*4+2])<<8 | uint32(page[i*4+3])
	}
	frame.StampGalileoINAVCRC(words)
	return words
}

// inavSetBits packs v's low n bits (MSB-first) at bit offset off of a 128-bit
// content blob.
func inavSetBits(content []byte, off int, v uint64, n int) {
	for i := 0; i < n; i++ {
		if v&(1<<uint(n-1-i)) != 0 {
			p := off + i
			content[p>>3] |= 1 << uint(7-(p&7))
		}
	}
}

// inavWordN builds a minimal I/NAV word of the given type with a matching
// IODnav (words 1–4; word type at bit 0, IODnav at bit 6 per GAL-OS-SIS-ICD-2.2
// Tables 42–45), with extra fields applied by mutate before packing.
func inavWordN(wordType, iod int, mutate func(content []byte)) []uint32 {
	content := make([]byte, 16)
	inavSetBits(content, 0, uint64(wordType), 6)
	inavSetBits(content, 6, uint64(iod), 10)
	if mutate != nil {
		mutate(content)
	}
	return inavContentWords(content)
}

func galileoFrame(svid int, words []uint32, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{GnssID: gnss.Galileo, SvID: svid, SigID: 0, Source: "obs-inav", Recv: recv, Words: words}
}

// TestFeedGalileoGSTWnMismatch guards the I/NAV word-5 GST WN — dead
// since its regression fix decode — must feed the regression fix broadcast-vs-receiver week
// gate ON THE GST AXIS (GST week = GPS week − 1024, regression fix): a correct
// broadcast serves wn_mismatch=false, a wrong-week broadcast (replay/SV time
// fault) serves true, and comparing on the wrong axis would fail both halves
// of this test at once. The F/NAV page-1 twin (closing the regression fix residual)
// runs the same gate on the independent @3 entry.
func TestFeedGalileoGSTWnMismatch(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gstWeek, ok := gnsstime.WeekAt(gnsstime.SysGalileo, float64(now.Unix()), 18)
	if !ok {
		t.Fatal("WeekAt(SysGalileo) not ok")
	}
	// Sanity-pin the axis itself: GST week must be exactly 1024 behind GPS week.
	if gpsWeek, _ := gnsstime.WeekAt(gnsstime.SysGPS, float64(now.Unix()), 18); gstWeek != gpsWeek-1024 {
		t.Fatalf("GST week %d vs GPS week %d, want a 1024-week offset", gstWeek, gpsWeek)
	}

	word5 := func(wn int) []uint32 {
		return inavWordN(5, 0, func(c []byte) {
			inavSetBits(c, 73, uint64(wn), 12) // GST WN (Table 46/69)
			inavSetBits(c, 85, 300000, 20)     // mid-week TOW, away from the rollover grace
		})
	}

	s := New(4)
	const svid = 23
	// FeedSVs publishes only entries with an assembled ephemeris; the check
	// itself rides word 5 alone.
	for _, wt := range []int{1, 2, 3, 4} {
		s.Apply(galileoFrame(svid, inavWordN(wt, 3, nil), now))
	}
	s.Apply(galileoFrame(svid, word5(gstWeek&0xFFF), now))
	sv := s.FeedSVs(now)["E23@0"]
	if sv.WnMismatch == nil || *sv.WnMismatch {
		t.Fatalf("wn_mismatch = %v for the correct GST week %d, want present and false", sv.WnMismatch, gstWeek)
	}

	s.Apply(galileoFrame(svid, word5((gstWeek+5)&0xFFF), now))
	sv = s.FeedSVs(now)["E23@0"]
	if sv.WnMismatch == nil || !*sv.WnMismatch {
		t.Fatalf("wn_mismatch = %v for GST week %d (+5), want true", sv.WnMismatch, gstWeek+5)
	}

	// F/NAV @3: page 1 carries the same live GST WN (Table 30 @155).
	fnavP1 := func(wn int) []uint32 {
		buf := make([]byte, 32)
		setBits := func(off int, v uint64, n int) {
			for i := 0; i < n; i++ {
				if v&(1<<uint(n-1-i)) != 0 {
					p := off + i
					buf[p>>3] |= 1 << uint(7-(p&7))
				}
			}
		}
		setBits(0, 1, 6)
		setBits(155, uint64(wn), 12)
		setBits(167, 300000, 20)
		words := make([]uint32, 8)
		for i := 0; i < 8; i++ {
			words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
		}
		frame.StampGalileoFNAVCRC(words)
		return words
	}
	s2 := New(4)
	s2.Apply(&ingest.RawFrame{GnssID: gnss.Galileo, SvID: svid, SigID: 3, Source: "obs-fnav", Recv: now, Words: fnavP1(gstWeek & 0xFFF)})
	// Page 1 alone assembles no ephemeris; read the shard state directly.
	key := Key{G: gnss.Galileo, Sv: svid, Sig: 3}
	st := s2.shardFor(key).m[key]
	if st == nil || !st.haveWN || st.wnMismatch {
		t.Fatalf("F/NAV correct GST week: haveWN/wnMismatch = %v/%v, want true/false", st != nil && st.haveWN, st != nil && st.wnMismatch)
	}
	s2.Apply(&ingest.RawFrame{GnssID: gnss.Galileo, SvID: svid, SigID: 3, Source: "obs-fnav", Recv: now, Words: fnavP1((gstWeek + 7) & 0xFFF)})
	if st = s2.shardFor(key).m[key]; !st.wnMismatch {
		t.Fatal("F/NAV wrong GST week (+7) not flagged")
	}
}

// inavWithOSNMA pokes a 40-bit OSNMA pattern into an already-built I/NAV page
// (page bits 146..185, outside the nav-word content) and re-stamps the CRC,
// which protects the field.
func inavWithOSNMA(words []uint32, v uint64) []uint32 {
	page := make([]byte, 32)
	for i := 0; i < 8; i++ {
		page[i*4] = byte(words[i] >> 24)
		page[i*4+1] = byte(words[i] >> 16)
		page[i*4+2] = byte(words[i] >> 8)
		page[i*4+3] = byte(words[i])
	}
	for i := 0; i < 40; i++ {
		p := 146 + i
		if v&(1<<uint(39-i)) != 0 {
			page[p>>3] |= 1 << uint(7-(p&7))
		} else {
			page[p>>3] &^= 1 << uint(7-(p&7))
		}
	}
	for i := 0; i < 8; i++ {
		words[i] = uint32(page[i*4])<<24 | uint32(page[i*4+1])<<16 | uint32(page[i*4+2])<<8 | uint32(page[i*4+3])
	}
	frame.StampGalileoINAVCRC(words)
	return words
}

// TestFeedGalileoOSNMA guards state/feed half: a live (nonzero)
// OSNMA field serves osnma=true; once the SV transmits only all-zero fields
// for longer than the live window, the flag decays to false (the on→off
// transition the osnma_change detector consumes); an SV with no I/NAV nominal
// page at all serves no flag.
func TestFeedGalileoOSNMA(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const svid, iod = 19, 55

	// Assembled entry with a live OSNMA field on one of its pages.
	s.Apply(galileoFrame(svid, inavWithOSNMA(inavWordN(1, iod, nil), 0xDEADBEEF01), now))
	s.Apply(galileoFrame(svid, inavWordN(2, iod, nil), now))
	s.Apply(galileoFrame(svid, inavWordN(3, iod, nil), now))
	s.Apply(galileoFrame(svid, inavWordN(4, iod, nil), now))

	sv, ok := s.FeedSVs(now)["E19@0"]
	if !ok {
		t.Fatal("E19@0 missing from svs feed")
	}
	if sv.Osnma == nil || !*sv.Osnma {
		t.Fatalf("osnma = %v after live field, want true", sv.Osnma)
	}

	// Zero-field pages keep arriving (word 5 here) but nothing live: within the
	// window the flag holds true, past it it decays to false — served false,
	// not absent, because the field IS observed (the SV just isn't in the
	// distributing subset any more).
	later := now.Add(2 * time.Minute)
	s.Apply(galileoFrame(svid, inavWordN(1, iod, nil), later))
	sv = s.FeedSVs(later)["E19@0"]
	if sv.Osnma == nil || *sv.Osnma {
		t.Fatalf("osnma = %v two minutes past the last live field, want false", sv.Osnma)
	}

	// An SV that never carried an OSNMA observation serves no flag at all: the
	// F/NAV-only @3 entry (E5a carries no OSNMA — it is an E1-B-only field).
	s2 := New(4)
	for _, pt := range []int{1, 2, 3, 4} {
		s2.Apply(&ingest.RawFrame{
			GnssID: gnss.Galileo, SvID: 14, SigID: 3, Source: "obs-fnav", Recv: now,
			Words: fnavPageWords(pt, 7),
		})
	}
	if sv := s2.FeedSVs(now)["E14@3"]; sv.Osnma != nil {
		t.Errorf("F/NAV @3 entry serves osnma = %v, want absent (E1-B-only field)", sv.Osnma)
	}
}

// TestFeedGalileoGGTO guards state/feed half: an I/NAV word 10's
// GST-GPS conversion set must reach the served entry as the raw a0g/a1g/t0g/
// wn0g quartet plus gps_offset_ns evaluated at the feed instant (with a1g=0 the
// evaluation is exactly a0g, independent of the epoch math), and a subsequent
// §5.1.8 all-ones withdrawal must clear all five fields — a stale offset never
// outlives its broadcast withdrawal.
func TestFeedGalileoGGTO(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const svid, iod = 12, 33

	// A minimal served entry needs an assembled ephemeris (FeedSVs skips
	// eph-less SVs); GGTO itself rides word 10, outside that set.
	s.Apply(galileoFrame(svid, inavWordN(1, iod, nil), now))
	s.Apply(galileoFrame(svid, inavWordN(2, iod, nil), now))
	s.Apply(galileoFrame(svid, inavWordN(3, iod, nil), now))
	s.Apply(galileoFrame(svid, inavWordN(4, iod, nil), now))

	word10 := func(mutate func(c []byte)) []uint32 {
		content := make([]byte, 16)
		inavSetBits(content, 0, 10, 6)
		if mutate != nil {
			mutate(content)
		}
		return inavContentWords(content)
	}
	s.Apply(galileoFrame(svid, word10(func(c []byte) {
		inavSetBits(c, 86, 0xFFEC, 16) // A0G raw = −20 → −20×2⁻³⁵ s ≈ −0.58 ns
		inavSetBits(c, 114, 10, 8)     // t0G = 36000 s
		inavSetBits(c, 122, 5, 6)      // WN0G = 5 (truncated)
	}), now))

	sv, ok := s.FeedSVs(now)["E12@0"]
	if !ok {
		t.Fatal("E12@0 missing from svs feed")
	}
	wantA0 := -20.0 / float64(uint64(1)<<35)
	if sv.A0G == nil || *sv.A0G != wantA0 {
		t.Fatalf("a0g = %v, want %v", sv.A0G, wantA0)
	}
	if sv.A1G == nil || *sv.A1G != 0 {
		t.Errorf("a1g = %v, want 0", sv.A1G)
	}
	if sv.T0G == nil || *sv.T0G != 36000 {
		t.Errorf("t0g = %v, want 36000", sv.T0G)
	}
	if sv.WN0G == nil || *sv.WN0G != 5 {
		t.Errorf("wn0g = %v, want 5", sv.WN0G)
	}
	// a1g = 0 ⇒ the evaluated offset is exactly a0g, whatever the epoch.
	if sv.GpsOffsetNs == nil || *sv.GpsOffsetNs != wantA0*1e9 {
		t.Errorf("gps_offset_ns = %v, want %v", sv.GpsOffsetNs, wantA0*1e9)
	}

	// The all-ones withdrawal clears the whole surface.
	s.Apply(galileoFrame(svid, word10(func(c []byte) {
		inavSetBits(c, 86, 0xFFFF, 16)
		inavSetBits(c, 102, 0xFFF, 12)
		inavSetBits(c, 114, 0xFF, 8)
		inavSetBits(c, 122, 0x3F, 6)
	}), now))
	sv = s.FeedSVs(now)["E12@0"]
	if sv.GpsOffsetNs != nil || sv.A0G != nil || sv.A1G != nil || sv.T0G != nil || sv.WN0G != nil {
		t.Errorf("withdrawn GGTO still served: off=%v a0g=%v a1g=%v t0g=%v wn0g=%v",
			sv.GpsOffsetNs, sv.A0G, sv.A1G, sv.T0G, sv.WN0G)
	}
}

// TestFeedGalileoSISANAPAServesAccIndex guards regression fix (the Galileo sibling of
// URA-15 rule): an SV broadcasting SISA index 255 — "No Accuracy
// Prediction Available (NAPA) … an indicator of a potential anomalous SIS"
// (GAL-OS-SIS-ICD-2.2 §5.1.12 Table 91) — must not serve as a healthy entry
// with the accuracy silently absent, indistinguishable from "word 3 not decoded
// yet". The feed serves sisa_valid=false with no sisa_m (there is no metres
// value) but exposes acc_index=255, which is what routes the SV into the
// detector's no_accuracy classification so the transition into NAPA fires a
// sisa_change event.
func TestFeedGalileoSISANAPAServesAccIndex(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const svid, iod = 11, 42

	apply := func(sisa int) {
		s.Apply(galileoFrame(svid, inavWordN(1, iod, nil), now))
		s.Apply(galileoFrame(svid, inavWordN(2, iod, nil), now))
		s.Apply(galileoFrame(svid, inavWordN(3, iod, func(c []byte) {
			inavSetBits(c, 120, uint64(sisa), 8) // SISA(E1,E5b) @120, Table 44
		}), now))
		s.Apply(galileoFrame(svid, inavWordN(4, iod, nil), now))
	}

	apply(255)
	sv, ok := s.FeedSVs(now)["E11@0"]
	if !ok {
		t.Fatal("E11@0 missing from svs feed")
	}
	if sv.SISAValid || sv.SISAM != nil {
		t.Errorf("NAPA served as a real accuracy: sisa_valid=%v sisa_m=%v", sv.SISAValid, sv.SISAM)
	}
	if sv.AccIndex == nil || *sv.AccIndex != 255 {
		t.Fatalf("acc_index = %v, want 255 (the NAPA sentinel itself must be served)", sv.AccIndex)
	}

	// A nominal index (same SV, new IODnav so the set re-assembles) serves both
	// the metres value and the raw index: 107 → 2 m + 7×16 cm = 3.12 m (Table 91
	// band 100–125).
	s2 := New(4)
	s2.Apply(galileoFrame(svid, inavWordN(1, iod, nil), now))
	s2.Apply(galileoFrame(svid, inavWordN(2, iod, nil), now))
	s2.Apply(galileoFrame(svid, inavWordN(3, iod, func(c []byte) {
		inavSetBits(c, 120, 107, 8)
	}), now))
	s2.Apply(galileoFrame(svid, inavWordN(4, iod, nil), now))
	sv = s2.FeedSVs(now)["E11@0"]
	if !sv.SISAValid || sv.SISAM == nil {
		t.Fatalf("SISA 107: sisa_valid=%v sisa_m=%v, want a decoded metres value", sv.SISAValid, sv.SISAM)
	}
	if got := *sv.SISAM; got < 3.119 || got > 3.121 {
		t.Errorf("sisa_m = %v, want 3.12 (index 107, Table 91)", got)
	}
	if sv.AccIndex == nil || *sv.AccIndex != 107 {
		t.Errorf("acc_index = %v, want 107", sv.AccIndex)
	}
}
