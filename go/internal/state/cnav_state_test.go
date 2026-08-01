package state

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
)

// cnavStateWords builds a CRC-valid CNAV message for the state-layer tests:
// header PRN (ICD bits 9–14, 0-indexed 8) and message type (15–20, 0-indexed
// 14), then caller-supplied fields, then the regression fix preamble+CRC stamp.
func cnavStateWords(prn, msgType int, fields func(buf []byte)) []uint32 {
	buf := make([]byte, 40)
	setAbsBits(buf, 8, 6, uint64(prn))
	setAbsBits(buf, 14, 6, uint64(msgType))
	if fields != nil {
		fields(buf)
	}
	buf[0] = 0x8B
	setAbsBits(buf, 276, 24, uint64(frame.CRC24QBits(buf, 0, 276)))
	words := make([]uint32, 10)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	return words
}

func cnavStateFrame(id gnss.GNSSID, svid, sig int, words []uint32, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{Recv: recv, Source: "test", GnssID: id, SvID: svid, SigID: sig, Words: words}
}

// cnavMT10 builds an MT10 with the given 13-bit WN, 3-bit L1/L2/L5 health,
// signed 5-bit URA_ED, and raw toe (×300 s).
func cnavMT10(prn, wn, health, uraED int, toeRaw uint64) []uint32 {
	return cnavStateWords(prn, 10, func(buf []byte) {
		setAbsBits(buf, 38, 13, uint64(wn))
		setAbsBits(buf, 51, 3, uint64(health))
		setAbsBits(buf, 65, 5, uint64(uraED)&0x1F)
		setAbsBits(buf, 70, 11, toeRaw)
	})
}

// TestCNAVStateWiring guards the state layer previously decoded L2C/L5
// CNAV and dropped everything (health, URA_ED, WN, ephemeris, clock) under a
// "P6 will consume it" comment that P6 had overtaken. A CNAV signal must now be
// a first-class per-signal entry — the B-CNAV2/F-NAV pattern — carrying the
// only per-signal health GPS broadcasts (IS-GPS-200N §30.3.3.1.1.2), the
// signed URA_ED, the header alert flag, and an independently assembled
// ephemeris/clock whose af0 is served only once a clock message has decoded.
func TestCNAVStateWiring(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0) // GPS week 2288
	const svid, sig = 5, 3            // GPS L2C CM

	// MT10 alone: buffers health/acc evidence but no ephemeris — not yet served.
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavMT10(svid, 2288, 0, 2, 400), t0))
	if _, ok := s.FeedSVs(t0)["G05@3"]; ok {
		t.Fatal("MT10 alone served an entry (no assembled ephemeris)")
	}

	// MT11 with the same toe completes the pair.
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavStateWords(svid, 11, func(buf []byte) {
		setAbsBits(buf, 38, 11, 400)
	}), t0))
	sv, ok := s.FeedSVs(t0)["G05@3"]
	if !ok {
		t.Fatal("G05@3 missing after MT10+MT11 assembly")
	}
	if sv.HealthCode != 1 || sv.HealthIssueLevel != 0 {
		t.Errorf("health = %d/%d, want 1/0 (L2 carrier bit clear)", sv.HealthCode, sv.HealthIssueLevel)
	}
	if sv.AccIndex == nil || *sv.AccIndex != 2 {
		t.Errorf("acc_index = %v, want 2 (URA_ED)", sv.AccIndex)
	}
	if !sv.SISAValid || sv.SISAM == nil || math.Abs(*sv.SISAM-4.0) > 1e-9 {
		t.Errorf("sisa = %v/%v, want valid 4.0 m (URA_ED 2 ⇒ 2^(1+2/2))", sv.SISAValid, sv.SISAM)
	}
	if sv.Alert == nil || *sv.Alert {
		t.Errorf("alert = %v, want present and false", sv.Alert)
	}
	if sv.WnMismatch == nil || *sv.WnMismatch {
		t.Errorf("wn_mismatch = %v, want present and false (13-bit WN 2288 == wall-clock week)", sv.WnMismatch)
	}
	if sv.Af0 != nil {
		t.Errorf("af0 = %v served with no clock message decoded, want absent (haveClk gate)", *sv.Af0)
	}

	// The assembled CNAV ephemeris propagates to a sane orbit radius.
	s.Propagate(t0)
	sv = s.FeedSVs(t0)["G05@3"]
	if sv.XM == nil {
		t.Fatal("no position propagated from the assembled CNAV ephemeris")
	}
	if r := math.Sqrt(*sv.XM**sv.XM + *sv.YM**sv.YM + *sv.ZM**sv.ZM); r < 26.0e6 || r > 27.2e6 {
		t.Errorf("CNAV orbit radius = %.0f m, want ~26560 km", r)
	}

	// A coherent MT30 (toc == toe, regression fix) attaches the clock; af0 now serves.
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavStateWords(svid, 30, func(buf []byte) {
		setAbsBits(buf, 60, 11, 400)
		setAbsBits(buf, 71, 26, 12345)
	}), t0))
	sv = s.FeedSVs(t0)["G05@3"]
	if sv.Af0 == nil || math.Abs(*sv.Af0-12345.0/(1<<20)/(1<<15)) > 1e-15 {
		t.Errorf("af0 = %v after MT30, want 12345×2⁻³⁵", sv.Af0)
	}

	// A re-broadcast MT10 flagging the L2 carrier (health bit L2=1, no toe
	// change) must flip per-signal health freshest-wins: this entry IS the L2C
	// signal, so a set carrier bit is do-not-use (§30.3.3.1.1.2).
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavMT10(svid, 2288, 0b010, 2, 400), t0))
	sv = s.FeedSVs(t0)["G05@3"]
	if sv.HealthCode != 3 || sv.HealthSubcode != 1 {
		t.Errorf("L2-flagged health = code %d subcode %d, want 3/1", sv.HealthCode, sv.HealthSubcode)
	}

	// URA_ED −16 is the signed "no accuracy prediction" sentinel: raw index
	// served, no metres value (regression fix applied to the CNAV table).
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavMT10(svid, 2288, 0, -16, 400), t0))
	sv = s.FeedSVs(t0)["G05@3"]
	if sv.SISAValid || sv.SISAM != nil {
		t.Errorf("URA_ED −16 served as a real accuracy: %v/%v", sv.SISAValid, sv.SISAM)
	}
	if sv.AccIndex == nil || *sv.AccIndex != -16 {
		t.Errorf("acc_index = %v, want −16", sv.AccIndex)
	}
}

// TestCNAVClockAttachDoesNotRefreshEphAt checks that attaching a late coherent
// MT30 to an unchanged ephemeris must not re-stamp ephAt — otherwise the wall-clock serving cap restarts from
// the clock-attach time and a days-old extended-ops ephemeris can be served
// past the ±half-week wrap, the exact window regression fix closes.
func TestCNAVClockAttachDoesNotRefreshEphAt(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	const svid, sig = 5, 3
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavMT10(svid, 2288, 0, 2, 400), t0))
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavStateWords(svid, 11, func(buf []byte) {
		setAbsBits(buf, 38, 11, 400)
	}), t0))

	// The clock arrives 2 h later (same toe): attach-only, no new ephemeris.
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavStateWords(svid, 30, func(buf []byte) {
		setAbsBits(buf, 60, 11, 400)
		setAbsBits(buf, 71, 26, 12345)
	}), t0.Add(2*time.Hour)))

	// 73 h after the EPHEMERIS was applied (71 h after the clock attach): the
	// regression fix cap must key on the ephemeris apply time, so no position serves.
	late := t0.Add(73 * time.Hour)
	s.Propagate(late)
	if sv := s.FeedSVs(late)["G05@3"]; sv.XM != nil {
		t.Error("position served 73 h past the ephemeris apply time: clock attach re-stamped ephAt")
	}
}

// TestCNAVMislabeledPRNIgnored checks the PRN gate before the MT10
// freshest-wins scalars (health/URA_ED/WN) and the alert flag apply earlier — a
// frame whose header PRN disagrees with the receiver's svId label (mislabeled
// or corrupt) must not stamp another SV's indications onto this entry.
func TestCNAVMislabeledPRNIgnored(t *testing.T) {
	s := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	const svid, sig = 5, 3
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavMT10(svid, 2288, 0, 2, 400), t0))
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavStateWords(svid, 11, func(buf []byte) {
		setAbsBits(buf, 38, 11, 400)
	}), t0))
	if sv := s.FeedSVs(t0)["G05@3"]; sv.HealthCode != 1 {
		t.Fatalf("setup: health = %d, want 1", sv.HealthCode)
	}

	// A frame labeled svId 5 but carrying PRN 6's MT10 with the L2 carrier
	// flagged bad: must be rejected before any state mutation.
	s.Apply(cnavStateFrame(gnss.GPS, svid, sig, cnavMT10(6, 2288, 0b010, 2, 400), t0))
	sv := s.FeedSVs(t0)["G05@3"]
	if sv.HealthCode != 1 {
		t.Errorf("mislabeled PRN-6 frame flipped G05@3 health to %d, want 1 (rejected)", sv.HealthCode)
	}
}

// TestCNAVCarrierBitSelection pins cnavCarrierHealth's sigId→carrier map to the
// dispatch table (docs/CONSTELLATIONS.md §2.1): GPS 3/4 & QZSS 4/5 read the L2
// bit, GPS 6/7 & QZSS 8/9 read the L5 bit, from the (L1,L2,L5) MSB-first field.
func TestCNAVCarrierBitSelection(t *testing.T) {
	const l2Only, l5Only = 0b010, 0b001
	for _, sig := range []int{3, 4} {
		if got := cnavCarrierHealth(gnss.GPS, sig, l2Only); got != 1 {
			t.Errorf("GPS sig %d with L2 flagged: bit = %d, want 1", sig, got)
		}
		if got := cnavCarrierHealth(gnss.GPS, sig, l5Only); got != 0 {
			t.Errorf("GPS sig %d with L5 flagged: bit = %d, want 0 (not this carrier)", sig, got)
		}
	}
	for _, sig := range []int{6, 7} {
		if got := cnavCarrierHealth(gnss.GPS, sig, l5Only); got != 1 {
			t.Errorf("GPS sig %d with L5 flagged: bit = %d, want 1", sig, got)
		}
	}
	for _, sig := range []int{4, 5} {
		if got := cnavCarrierHealth(gnss.QZSS, sig, l2Only); got != 1 {
			t.Errorf("QZSS sig %d with L2 flagged: bit = %d, want 1", sig, got)
		}
	}
	for _, sig := range []int{8, 9} {
		if got := cnavCarrierHealth(gnss.QZSS, sig, l5Only); got != 1 {
			t.Errorf("QZSS sig %d with L5 flagged: bit = %d, want 1", sig, got)
		}
	}
}

// sf1WordsClk is sf1Words with an explicit two-bit IODC high field and af0, so a
// test can broadcast a clock-only data-set refresh: IODC high bits change, low
// 8 (== the IODE) unchanged (IS-GPS-200N §20.3.4.4).
func sf1WordsClk(iodcHi, iodcLo, af0 int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 1)
	setField(buf, 3, 1, 10, 240)
	setField(buf, 3, 13, 4, 4)
	setField(buf, 3, 23, 2, iodcHi)
	setField(buf, 8, 1, 8, iodcLo)
	setField(buf, 8, 9, 16, 27000)
	setField(buf, 10, 1, 22, af0)
	return packWords(buf)
}

// TestClockRefreshUnderUnchangedIODE guards regression fix (the GPS twin of regression fix,
// completing regression fix): a data set whose IODC changes only in its two high bits
// (low-8 IODE unchanged — legal once §20.3.4.4's six-hour no-repeat horizon has
// passed) was dropped whole by the IODE gate, so its refreshed af0/af1/af2
// never reached the served clock. The clock now keys on the full 10-bit IODC.
func TestClockRefreshUnderUnchangedIODE(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(gpsFrame(sf1WordsClk(0, 85, 214748), now))
	s.Apply(gpsFrame(sf2Words(85, 205075516), now))
	s.Apply(gpsFrame(sf3Words(85), now))

	sv := s.FeedSVs(now)["G05@0"]
	if sv.Af0 == nil || math.Abs(*sv.Af0-214748.0/(1<<31)) > 1e-15 {
		t.Fatalf("initial af0 = %v, want 214748×2⁻³¹", sv.Af0)
	}

	// IODC 341 (high bits 01, low 8 still 85): a clock-only refresh. The old
	// gate saw IODE 85 == IODE 85 and returned before ever reaching st.clk.
	s.Apply(gpsFrame(sf1WordsClk(1, 85, 300000), now))
	sv = s.FeedSVs(now)["G05@0"]
	if sv.Af0 == nil || math.Abs(*sv.Af0-300000.0/(1<<31)) > 1e-15 {
		t.Errorf("af0 after IODC-high-bits refresh = %v, want 300000×2⁻³¹ (refresh dropped)", sv.Af0)
	}
	if sv.IOD == nil || *sv.IOD != 85 {
		t.Errorf("served IOD = %v, want 85 (ephemeris set unchanged)", sv.IOD)
	}
	if sv.TimeDiscoNs != nil {
		t.Errorf("clock-only refresh computed a time-disco (%v): not a changeover", *sv.TimeDiscoNs)
	}
}

// sf1WordsWN is sf1Words with an explicit 10-bit broadcast WN (word 3 bits 1-10).
func sf1WordsWN(wn, iodcLo int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 1)
	setField(buf, 3, 1, 10, wn)
	setField(buf, 3, 13, 4, 4)
	setField(buf, 8, 1, 8, iodcLo)
	setField(buf, 8, 9, 16, 27000)
	setField(buf, 10, 1, 22, 214748)
	return packWords(buf)
}

// TestBroadcastWNCrossCheck guards the decoded broadcast week number
// was populated on both signal paths and never compared to anything —
// DisambiguateWeek had zero production callers, so an SV transmitting a wrong
// week (upload error, clock fault, replayed/spoofed signal) was invisible. The
// state layer now records wn_mismatch per SV, tolerating only the legitimate
// data-set lag across a week rollover (previous week, within the grace window
// of the new week — IS-GPS-200N §30.3.3.1.1.1 data-set-scoped WN semantics).
func TestBroadcastWNCrossCheck(t *testing.T) {
	// Unix 1_700_000_000 is GPS week 2288 (t0 sits mid-week); 2288 mod 1024 = 240.
	t0 := time.Unix(1_700_000_000, 0)
	const wkStartUnix = 1_699_747_182 // start of GPS week 2288 (2288×604800 + GPS epoch − 18 leap s)

	apply := func(wn int64, at time.Time) *bool {
		s := New(4)
		s.Apply(gpsFrame(sf1WordsWN(wn, 85), at))
		s.Apply(gpsFrame(sf2Words(85, 205075516), at))
		s.Apply(gpsFrame(sf3Words(85), at))
		return s.FeedSVs(at)["G05@0"].WnMismatch
	}

	if m := apply(240, t0); m == nil || *m {
		t.Errorf("matching WN 240 (week 2288): wn_mismatch = %v, want present and false", m)
	}
	if m := apply(100, t0); m == nil || !*m {
		t.Errorf("wrong WN 100: wn_mismatch = %v, want true", m)
	}

	// Rollover grace: one hour into a new week, a data set cut before the
	// rollover still broadcasts the previous week's WN — not an anomaly.
	early := time.Unix(wkStartUnix+3600, 0)
	if m := apply(239, early); m == nil || *m {
		t.Errorf("previous-week WN 1 h into the new week: wn_mismatch = %v, want false (grace)", m)
	}
	// The same previous-week WN half a day into the week IS an anomaly.
	late := time.Unix(wkStartUnix+12*3600, 0)
	if m := apply(239, late); m == nil || !*m {
		t.Errorf("previous-week WN 12 h into the new week: wn_mismatch = %v, want true", m)
	}
}

// TestBroadcastWNCrossCheckSpoolReplay guards the WN check is an
// ABSOLUTE wall-clock comparison, so it must run on the frame's forensic
// reception stamp (f.Recv), not the collector-local drain stamp. A multi-day
// feeder outage spanning the GPS week rollover replays week-W frames while the
// collector sits in week W+1 (recvReplayHorizon admits up to 7 d): with the
// local stamp those healthy frames fired false SevCritical wn_mismatch for
// every replayed WN-bearing page. A genuinely wrong WN on a replayed frame must
// still flag — the feeder's stamp is its true reception time.
func TestBroadcastWNCrossCheckSpoolReplay(t *testing.T) {
	// Reception mid-week 2288 (WN 240); drained 5 days later — 12 h into week
	// 2289, past the 4 h rollover grace that let the plain rollover case pass.
	const wkStartUnix = 1_699_747_182                      // start of GPS week 2288 (see TestBroadcastWNCrossCheck)
	recvAt := time.Unix(wkStartUnix+3*24*3600, 0)          // week 2288, mid-week
	drainAt := time.Unix(wkStartUnix+7*24*3600+12*3600, 0) // week 2289 + 12 h

	apply := func(wn int64) *bool {
		s := New(4)
		for _, w := range [][]uint32{sf1WordsWN(wn, 85), sf2Words(85, 205075516), sf3Words(85)} {
			s.Apply(&ingest.RawFrame{
				Recv: recvAt, RecvLocal: drainAt, Source: "test",
				GnssID: gnss.GPS, SvID: 5, SigID: 0, Words: w,
			})
		}
		return s.FeedSVs(drainAt)["G05@0"].WnMismatch
	}

	// The frame's WN matches its RECEPTION week: healthy replay, no mismatch.
	if m := apply(240); m == nil || *m {
		t.Errorf("replayed week-2288 frame with WN 240 drained in week 2289: wn_mismatch = %v, want false ", m)
	}
	// A genuinely wrong WN on the same replayed frame still flags.
	if m := apply(100); m == nil || !*m {
		t.Errorf("replayed frame with wrong WN 100: wn_mismatch = %v, want true", m)
	}
}
