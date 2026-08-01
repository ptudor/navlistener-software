package state

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// bcnav2Frame builds a synthetic, CRC-valid 288-bit B-CNAV2 message (9-word
// buffer) with the given PRN/MesType/SOW (seconds, must be a multiple of 3 —
// the wire field is SOW/3), letting fill set any additional fields (via the
// shared setAbsBits helper from beidou_d1_test.go) before the CRC-24Q is
// stamped over the leading 264 bits.
func bcnav2Frame(prn, mesType, sow int, fill func(buf []byte)) []uint32 {
	buf := make([]byte, 36) // 9 words = 288 bits
	setAbsBits(buf, 0, 6, uint64(prn))
	setAbsBits(buf, 6, 6, uint64(mesType))
	setAbsBits(buf, 12, 18, uint64(sow/3))
	if fill != nil {
		fill(buf)
	}
	crc := frame.CRC24Q(buf[:33])
	buf[33] = byte(crc >> 16)
	buf[34] = byte(crc >> 8)
	buf[35] = byte(crc)

	words := make([]uint32, 9)
	for i := 0; i < 9; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestApplyBeiDouBCNAV2ClockOnlyRefreshUpdatesSameIODE guards before the
// fix, applyBeiDouBCNAV2 gated the entire update (including the clock) on the
// type-10/11 IODE, so a same-IODE type-30 clock refresh (a new af0 under an
// unchanged ephemeris) was silently dropped forever. It must now update the
// served clock while leaving the ephemeris IODE untouched.
func TestApplyBeiDouBCNAV2ClockOnlyRefreshUpdatesSameIODE(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 20

	m10 := bcnav2Frame(prn, 10, 100002, func(buf []byte) {
		setAbsBits(buf, 30, 13, 1)  // WN (arbitrary)
		setAbsBits(buf, 53, 8, 7)   // IODE = 7
		setAbsBits(buf, 61, 11, 10) // Toe (arbitrary)
		setAbsBits(buf, 72, 2, 3)   // SatType = MEO
	})
	m11 := bcnav2Frame(prn, 11, 100002, nil)
	clk1 := bcnav2Frame(prn, 30, 100005, func(buf []byte) {
		setAbsBits(buf, 111, 10, 3)                  // IODC = 3
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw)
	})

	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m10})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: m11})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: clk1})

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph {
		t.Fatalf("ephemeris not assembled: %+v", st)
	}
	af0First := st.clk.Af0
	if af0First == 0 {
		t.Fatalf("test setup: initial af0 must be nonzero to detect a later change")
	}

	// A same-IODE, new-af0/new-IODC type-30 refresh must still update st.clk.
	clk2 := bcnav2Frame(prn, 30, 100008, func(buf []byte) {
		setAbsBits(buf, 111, 10, 4)                  // IODC 3 -> 4
		setAbsBits(buf, 53, 25, uint64(int64(2000))) // Af0 changed
	})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: clk2})

	if st.clk.Af0 == af0First {
		t.Errorf("same-IODE clock refresh did not update served af0: still %v", st.clk.Af0)
	}
	if st.iod != 7 {
		t.Errorf("ephemeris IODE must be unchanged by a clock-only refresh: %d", st.iod)
	}
}

// TestApplyBeiDouBCNAV2StaleClockAtIODEChangeover guards the regression fix follow-up: an
// IODE changeover assembled while the cached type-30/34 is stale (dropped by the
// assembler) must keep serving the previously applied clock — not install the
// zero model over it — and must not report a time-disco (there is no fresh clock
// to difference against; absent, not zero).
func TestApplyBeiDouBCNAV2StaleClockAtIODEChangeover(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 21

	apply := func(words []uint32, at time.Time) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: at, Words: words})
	}
	apply(bcnav2Frame(prn, 10, 100002, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)   // IODE = 7
		setAbsBits(buf, 61, 11, 10) // Toe (raw)
		setAbsBits(buf, 72, 2, 3)   // SatType = MEO
	}), now)
	apply(bcnav2Frame(prn, 11, 100002, nil), now)
	apply(bcnav2Frame(prn, 30, 100005, func(buf []byte) {
		setAbsBits(buf, 111, 10, 3)                  // IODC = 3
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw, nonzero)
	}), now)

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph || st.clk.Af0 == 0 {
		t.Fatalf("setup: ephemeris+clock not applied: %+v", st)
	}
	af0 := st.clk.Af0

	// One hour later the IODE changes; the cached type-30's SOW is now well past
	// bcnavClkStaleSOW, so the assembler drops it and returns the zero model.
	later := now.Add(time.Hour)
	apply(bcnav2Frame(prn, 10, 103602, func(buf []byte) {
		setAbsBits(buf, 53, 8, 8)   // IODE 7 -> 8
		setAbsBits(buf, 61, 11, 11) // Toe advances one step
		setAbsBits(buf, 72, 2, 3)
	}), later)
	apply(bcnav2Frame(prn, 11, 103602, nil), later)

	if st.iod != 8 {
		t.Fatalf("IODE changeover not applied: iod=%d, want 8", st.iod)
	}
	if st.clk.Af0 != af0 {
		t.Errorf("stale clock zeroed the served clock at IODE changeover: Af0=%v, want %v kept", st.clk.Af0, af0)
	}
	if st.timeDiscoValid {
		t.Errorf("time-disco reported with no fresh clock to difference against: %v ns", st.timeDiscoNs)
	}
}

// TestApplyBeiDouBCNAV2StaleClockIODCNotLatched guards the regression fix follow-up's
// second half: when the assembler drops the cached type-30/34 as stale, its IODC
// was never applied and must not be latched — otherwise a later fresh type-30
// carrying the same IODC reads as "unchanged" and its clock is never served.
func TestApplyBeiDouBCNAV2StaleClockIODCNotLatched(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 22

	apply := func(words []uint32, at time.Time) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: at, Words: words})
	}
	// The type-30 arrives first (IODC=7), before any 10/11 pair completes.
	apply(bcnav2Frame(prn, 30, 100005, func(buf []byte) {
		setAbsBits(buf, 111, 10, 7)                  // IODC = 7
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw, nonzero)
	}), now)

	// An hour later the 10/11 pair assembles; the cached type-30 is stale and
	// dropped, so its IODC=7 must not be latched as applied.
	later := now.Add(time.Hour)
	apply(bcnav2Frame(prn, 10, 103602, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)   // IODE = 7
		setAbsBits(buf, 61, 11, 11) // Toe (raw)
		setAbsBits(buf, 72, 2, 3)   // SatType = MEO
	}), later)
	apply(bcnav2Frame(prn, 11, 103602, nil), later)

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.haveEph {
		t.Fatalf("setup: ephemeris not assembled: %+v", st)
	}
	if st.clk.Af0 != 0 {
		t.Fatalf("setup: stale clock should not have been applied, Af0=%v", st.clk.Af0)
	}

	// A fresh type-30 rebroadcasting the same IODC=7 must now be recognized and
	// applied — the stale message's IODC was never latched.
	apply(bcnav2Frame(prn, 30, 103605, func(buf []byte) {
		setAbsBits(buf, 111, 10, 7)                  // same IODC = 7
		setAbsBits(buf, 53, 25, uint64(int64(1000))) // Af0 (raw, nonzero)
	}), later)

	if st.clk.Af0 == 0 {
		t.Errorf("fresh type-30 with the stale message's IODC was never applied: served clock still zero")
	}
}

// TestFeedBeiDouBDGIMServed guards B-CNAV2 half: MT30's BDGIM α1..α9
// (B2a Table 7-10: α1 10-bit unsigned, α2 signed, α3/α4 unsigned, α5 unsigned
// with the NEGATIVE −2⁻³ scale — the table's trap — α6..α9 signed, all in TECu
// at 2⁻³ resolution) must be folded and served raw as bdgim.
func TestFeedBeiDouBDGIMServed(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 28
	apply := func(words []uint32) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: words})
	}
	apply(bcnav2Frame(prn, 10, 252801, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)
		setAbsBits(buf, 61, 11, 10)
		setAbsBits(buf, 72, 2, 3)
	}))
	apply(bcnav2Frame(prn, 11, 252801, nil))
	apply(bcnav2Frame(prn, 30, 252804, func(buf []byte) {
		setAbsBits(buf, 111, 10, 3)       // IODC
		setAbsBits(buf, 145, 10, 20)      // α1 raw 20 (×2⁻³ = 2.5)
		setAbsBits(buf, 155, 8, (1<<8)-4) // α2 raw −4 (signed, ×2⁻³ = −0.5)
		setAbsBits(buf, 179, 8, 12)       // α5 raw 12 (unsigned, ×−2⁻³ = −1.5)
		setAbsBits(buf, 187, 8, (1<<8)-8) // α6 raw −8 (signed, ×2⁻³ = −1.0)
	}))
	sv := s.FeedSVs(now)["C28@8"]
	if sv.Bdgim == nil {
		t.Fatalf("bdgim not served: %+v", sv)
	}
	g := *sv.Bdgim
	if g[0] != 2.5 || g[1] != -0.5 || g[4] != -1.5 || g[5] != -1.0 {
		t.Errorf("bdgim = %v, want α1=2.5 α2=−0.5 α5=−1.5 (the −2⁻³ trap) α6=−1.0", g)
	}
	if sv.KlobAlpha != nil {
		t.Errorf("klob_alpha served on a B-CNAV2 entry: %v", sv.KlobAlpha)
	}
}

// TestApplyBeiDouBCNAV2PRNMismatchDropped guards regression fix/a CRC-valid
// B-CNAV2 message whose in-payload PRN (Figure 6-1 — the leading 6 bits,
// inside the CRC-24Q boundary; §7.1 effective range 1–63) disagrees with the
// transport SFRBX svId is mis-attributed and must be dropped under its own
// metric label — never folded into the (wrong) SV's state. PRN 0 is
// outside §7.1's effective range, so the degenerate PRN == svId == 0 match is
// rejected too.
func TestApplyBeiDouBCNAV2PRNMismatchDropped(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	counter := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(gnss.BeiDou)), "prn_mismatch")
	before := testutil.ToFloat64(counter)

	// The payload says PRN 30; the transport header says svId 31.
	m34 := bcnav2Frame(30, 34, 252804, func(buf []byte) {
		setAbsBits(buf, 30, 2, 1) // HS = 1 — must NOT reach C31's state
		setAbsBits(buf, 133, 10, 3)
	})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: 31, SigID: 8, Recv: now, Words: m34})

	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("prn_mismatch delta = %v, want 1", got)
	}
	for _, sv := range []int{30, 31} {
		key := Key{G: gnss.BeiDou, Sv: sv, Sig: 8}
		if st := s.shardFor(key).m[key]; st != nil {
			t.Errorf("mis-tagged frame built state for C%02d: %+v", sv, st)
		}
	}

	// PRN 0 == svId 0 satisfies bare equality, but §7.1's effective
	// range is 1–63 — a crafted frame must not mint a "C00@8" state.
	before = testutil.ToFloat64(counter)
	m34zero := bcnav2Frame(0, 34, 252804, func(buf []byte) {
		setAbsBits(buf, 133, 10, 3)
	})
	s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: 0, SigID: 8, Recv: now, Words: m34zero})
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("prn_mismatch delta for PRN==svId==0 = %v, want 1", got)
	}
	key0 := Key{G: gnss.BeiDou, Sv: 0, Sig: 8}
	if st := s.shardFor(key0).m[key0]; st != nil {
		t.Errorf("degenerate PRN==svId==0 frame built state: %+v", st)
	}
}

// TestBeiDouDeferredSignalLabels guards the known-but-undecoded BeiDou
// sigIds (docs/CONSTELLATIONS.md §2.1 — D2 on 1/3, B2I D1 on 2, B1C on 5/6,
// B2a pilot on 7) must each count under a stable per-signal deferral label
// (the NavIC idiom), leaving "unsupported" for genuinely unknown sigIds.
func TestBeiDouDeferredSignalLabels(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		sig   int
		label string
	}{
		{1, "bds_d2_deferred"}, {3, "bds_d2_deferred"},
		{2, "bds_b2i_deferred"},
		{5, "bds_b1c_deferred"}, {6, "bds_b1c_deferred"},
		{7, "bds_b2a_pilot_deferred"},
		{9, "unsupported"}, // an unknown sigId still lands in the generic bucket
	}
	for _, c := range cases {
		counter := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(gnss.BeiDou)), c.label)
		before := testutil.ToFloat64(counter)
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: 1, SigID: c.sig, Recv: now, Words: make([]uint32, 10)})
		if got := testutil.ToFloat64(counter) - before; got != 1 {
			t.Errorf("sigId %d: %s delta = %v, want 1", c.sig, c.label, got)
		}
	}
}

// TestFeedBeiDouIntegrityFlagsAndHSRider guards the B2a per-signal
// integrity flags (DIF/SIF/AIF, Table 7-23) + SISMAI must be folded
// freshest-wins from every decoded message type and served; and the rider —
// HS from MT30/34/40 (not just MT11) must reach served health, so an HS flip
// riding a clock message is not invisible until the next MT11.
func TestFeedBeiDouIntegrityFlagsAndHSRider(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 27
	apply := func(words []uint32) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: words})
	}
	apply(bcnav2Frame(prn, 10, 252801, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)
		setAbsBits(buf, 61, 11, 10)
		setAbsBits(buf, 72, 2, 3)
	}))
	apply(bcnav2Frame(prn, 11, 252801, nil)) // HS=0, flags clear
	sv := s.FeedSVs(now)["C27@8"]
	if sv.Dif == nil || *sv.Dif || sv.Sif == nil || *sv.Sif || sv.Aif == nil || *sv.Aif {
		t.Fatalf("clear flag block not served as decoded-false: dif=%v sif=%v aif=%v", sv.Dif, sv.Sif, sv.Aif)
	}
	if sv.HealthCode != 1 {
		t.Fatalf("initial health_code = %d, want 1", sv.HealthCode)
	}

	// An MT30 raises DIF and carries SISMAI 9 (flag block at bit 32).
	apply(bcnav2Frame(prn, 30, 252804, func(buf []byte) {
		setAbsBits(buf, 32, 1, 1)   // DIF(B2a)
		setAbsBits(buf, 35, 4, 9)   // SISMAI
		setAbsBits(buf, 111, 10, 3) // IODC
	}))
	sv = s.FeedSVs(now)["C27@8"]
	if sv.Dif == nil || !*sv.Dif || sv.Sismai == nil || *sv.Sismai != 9 {
		t.Errorf("MT30 flag raise not served: dif=%v sismai=%v", sv.Dif, sv.Sismai)
	}

	// An MT34 with HS=1 (no MT11 in sight) must flip served health — the HS
	// rider — and its clear flag block replaces the MT30's raised DIF.
	apply(bcnav2Frame(prn, 34, 252807, func(buf []byte) {
		setAbsBits(buf, 30, 2, 1) // HS = 1
		setAbsBits(buf, 133, 10, 3)
	}))
	sv = s.FeedSVs(now)["C27@8"]
	if sv.HealthCode != 3 {
		t.Errorf("health_code after MT34 HS=1 = %d, want 3 (HS folded from every HS-bearing type)", sv.HealthCode)
	}
	if sv.Dif == nil || *sv.Dif {
		t.Errorf("freshest-wins flag fold: MT34's clear block must replace MT30's DIF, got %v", sv.Dif)
	}
}

// TestFeedBeiDouSISAIRaw guards MT34/MT40's SISAI raw indices must be
// stored (accSISAIRaw, packed oe<<11|ocb<<6|oc1<<3|oc2 with oe sticky across
// MT34 updates) and served as acc_index with sisa_valid=false — the B2a ICD
// v1.0 publishes no index→metres table, so no metres value may ever appear.
func TestFeedBeiDouSISAIRaw(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 26
	apply := func(words []uint32) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: words})
	}
	apply(bcnav2Frame(prn, 10, 252801, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)
		setAbsBits(buf, 61, 11, 10)
		setAbsBits(buf, 72, 2, 3)
	}))
	apply(bcnav2Frame(prn, 11, 252801, nil))
	// MT34: SISAIoc only (top excluded from the packed index by design).
	apply(bcnav2Frame(prn, 34, 252804, func(buf []byte) {
		setAbsBits(buf, 42, 11, 1234) // SISAItop — must NOT affect acc_index
		setAbsBits(buf, 53, 5, 21)    // SISAIocb
		setAbsBits(buf, 58, 3, 5)     // SISAIoc1
		setAbsBits(buf, 61, 3, 2)     // SISAIoc2
		setAbsBits(buf, 133, 10, 3)   // IODC
	}))
	sv := s.FeedSVs(now)["C26@8"]
	want := 21<<6 | 5<<3 | 2
	if sv.AccIndex == nil || *sv.AccIndex != want {
		t.Fatalf("acc_index = %v, want %d (packed ocb/oc1/oc2)", sv.AccIndex, want)
	}
	if sv.SISAValid || sv.SISAM != nil {
		t.Errorf("sisa_valid=%v sisa_m=%v, want false/absent — no metres table exists (ICD §7.16 defers it)", sv.SISAValid, sv.SISAM)
	}
	if !sv.AccIndexRawOnly {
		t.Errorf("AccIndexRawOnly = false, want true (detector discriminator)")
	}

	// MT40 supplies SISAIoe; the packed index gains the oe<<11 component.
	apply(bcnav2Frame(prn, 40, 252807, func(buf []byte) {
		setAbsBits(buf, 42, 5, 17)   // SISAIoe
		setAbsBits(buf, 47, 11, 999) // SISAItop
		setAbsBits(buf, 58, 5, 21)   // SISAIocb
		setAbsBits(buf, 63, 3, 5)    // SISAIoc1
		setAbsBits(buf, 66, 3, 2)    // SISAIoc2
	}))
	want = 17<<11 | 21<<6 | 5<<3 | 2
	if sv := s.FeedSVs(now)["C26@8"]; sv.AccIndex == nil || *sv.AccIndex != want {
		t.Fatalf("acc_index after MT40 = %v, want %d (oe folded in)", sv.AccIndex, want)
	}

	// A later MT34 (no oe field) must keep the last-known oe — not zero it.
	apply(bcnav2Frame(prn, 34, 252810, func(buf []byte) {
		setAbsBits(buf, 53, 5, 21)
		setAbsBits(buf, 58, 3, 5)
		setAbsBits(buf, 61, 3, 2)
		setAbsBits(buf, 133, 10, 3)
	}))
	if sv := s.FeedSVs(now)["C26@8"]; sv.AccIndex == nil || *sv.AccIndex != want {
		t.Fatalf("acc_index after second MT34 = %v, want %d (oe sticky)", sv.AccIndex, want)
	}
}

// TestFeedBeiDouBDTUTC guards an MT34's BDT-UTC set must be stored
// freshest-wins, served as utc_offset_ns (Eq. 7-25 at the feed instant, with the
// Eq. 7-29 ΔtLSF arm once the WNLSF/DN event is past) plus the raw leap
// schedule, and cross-checked against the configured GPS−UTC leap count
// (broadcast ΔtLS + 14 == gpsUTCOffset — BDT = GPST − 14 s).
func TestFeedBeiDouBDTUTC(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0) // GPS 1_384_035_218 s → BDT week 932, tow 252_804
	const prn = 24
	apply := func(words []uint32) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: now, Words: words})
	}
	// Ephemeris pair so the entry publishes (haveEph gate).
	apply(bcnav2Frame(prn, 10, 252801, func(buf []byte) {
		setAbsBits(buf, 53, 8, 7)   // IODE
		setAbsBits(buf, 61, 11, 10) // Toe
		setAbsBits(buf, 72, 2, 3)   // MEO
	}))
	apply(bcnav2Frame(prn, 11, 252801, nil))

	m34 := func(dtLS, wnlsf, dn, dtLSF int) []uint32 {
		return bcnav2Frame(prn, 34, 252804, func(buf []byte) {
			setAbsBits(buf, 133, 10, 3)             // IODC
			setAbsBits(buf, 143, 16, (1<<16)-2)     // A0UTC raw −2 (≈ −0.058 ns)
			setAbsBits(buf, 179, 8, uint64(dtLS))   // ΔtLS
			setAbsBits(buf, 187, 16, 252800/16)     // tot
			setAbsBits(buf, 203, 13, 932)           // WNot (current BDT week)
			setAbsBits(buf, 216, 13, uint64(wnlsf)) // WNLSF
			setAbsBits(buf, 229, 3, uint64(dn))     // DN
			setAbsBits(buf, 232, 8, uint64(dtLSF))  // ΔtLSF
		})
	}
	// Future leap event (week 933): the ΔtLS arm applies. 4 + 14 == 18 → no mismatch.
	apply(m34(4, 933, 6, 5))
	sv, ok := s.FeedSVs(now)["C24@8"]
	if !ok || sv.UtcOffsetNs == nil || sv.DtLS == nil || sv.LeapMismatch == nil {
		t.Fatalf("BDT-UTC fields not served: %+v", sv)
	}
	// Offset ≈ ΔtLS·1e9 with only the tiny A0 term (−2·2⁻³⁵ s ≈ −0.058 ns) on top.
	if got := *sv.UtcOffsetNs; got < 4e9-1 || got > 4e9 {
		t.Errorf("utc_offset_ns = %v, want ≈ 4e9 (ΔtLS=4 arm)", got)
	}
	if *sv.DtLS != 4 || *sv.DtLSF != 5 || *sv.WnLSF != 933 || *sv.Dn != 6 {
		t.Errorf("leap schedule = %d/%d/%d/%d, want 4/5/933/6", *sv.DtLS, *sv.DtLSF, *sv.WnLSF, *sv.Dn)
	}
	if *sv.LeapMismatch {
		t.Errorf("leap_mismatch = true, want false (ΔtLS 4 + 14 == configured 18)")
	}

	// Past leap event (week 900): the ΔtLSF arm applies — served offset and the
	// cross-check must both use ΔtLSF, so a pre-event ΔtLS of 3 raises no alarm.
	apply(m34(3, 900, 6, 4))
	sv = s.FeedSVs(now)["C24@8"]
	if got := *sv.UtcOffsetNs; got < 4e9-1 || got > 4e9 {
		t.Errorf("utc_offset_ns = %v, want ≈ 4e9 (ΔtLSF arm after past WNLSF/DN)", got)
	}
	if *sv.LeapMismatch {
		t.Errorf("leap_mismatch = true, want false (applicable ΔtLSF 4 + 14 == 18)")
	}

	// A broadcast leap count disagreeing with the configured GPS−UTC (	// missing alarm): 5 + 14 != 18 → mismatch.
	apply(m34(5, 933, 6, 5))
	sv = s.FeedSVs(now)["C24@8"]
	if sv.LeapMismatch == nil || !*sv.LeapMismatch {
		t.Errorf("leap_mismatch not raised for broadcast ΔtLS=5 vs configured 18")
	}
}

func TestApplyBeiDouBCNAV2MT34CarriesMT30TGD(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	const prn = 23
	apply := func(words []uint32, at time.Time) {
		s.Apply(&ingest.RawFrame{GnssID: gnss.BeiDou, SvID: prn, SigID: 8, Recv: at, Words: words})
	}
	m10 := func(iode, toe, sow int) []uint32 {
		return bcnav2Frame(prn, 10, sow, func(buf []byte) {
			setAbsBits(buf, 53, 8, uint64(iode))
			setAbsBits(buf, 61, 11, uint64(toe))
			setAbsBits(buf, 72, 2, 3) // MEO
		})
	}
	m11 := func(sow int) []uint32 { return bcnav2Frame(prn, 11, sow, nil) }
	m30 := func(iodc, sow int) []uint32 {
		return bcnav2Frame(prn, 30, sow, func(buf []byte) {
			setAbsBits(buf, 111, 10, uint64(iodc))
			setAbsBits(buf, 121, 12, (1<<12)-137) // TGD_B2ap raw −137 (×2⁻³⁴ ≈ −8 ns), two's complement
			setAbsBits(buf, 133, 12, 59)          // ISC_B2ad raw +59 (×2⁻³⁴ ≈ +3.4 ns)
		})
	}
	m34 := func(iodc, sow int) []uint32 {
		return bcnav2Frame(prn, 34, sow, func(buf []byte) {
			setAbsBits(buf, 133, 10, uint64(iodc))
		})
	}

	apply(m10(1, 10, 100002), now)
	apply(m11(100002), now)
	apply(m30(1, 100005), now)

	key := Key{G: gnss.BeiDou, Sv: prn, Sig: 8}
	st := s.shardFor(key).m[key]
	if st == nil || !st.clkHasBcTGD || st.clk.TGD == 0 {
		t.Fatalf("MT30 TGD not applied: %+v", st)
	}
	// the served group delay is the tracked B2a DATA component's
	// eq. 7-5 sum TGD_B2ap + ISC_B2ad (raw −137 + 59 = −78 at 2⁻³⁴ s/LSB),
	// not the pilot-only TGD_B2ap.
	if want := -78.0 / (1 << 30) / 16; st.clk.TGD != want {
		t.Fatalf("MT30 clock TGD = %g, want TGD_B2ap+ISC_B2ad = %g", st.clk.TGD, want)
	}
	wantTGD := st.clk.TGD

	// MT34 wins the next IODC race. Its clock polynomial must carry the last
	// decoded MT30 TGD rather than install a decoded-looking zero.
	second := now.Add(5 * time.Minute)
	apply(m34(2, 100299), second)
	apply(m10(2, 11, 100302), second)
	apply(m11(100302), second)
	if st.clk.TGD != wantTGD || !st.clkHasBcTGD {
		t.Fatalf("MT34 lost MT30 TGD: got %g known=%v, want %g", st.clk.TGD, st.clkHasBcTGD, wantTGD)
	}
	// A same-IODC MT30 supplies the authoritative field without creating a
	// separate ephemeris changeover.
	apply(m30(2, 100305), second)
	if st.clk.TGD != wantTGD || !st.clkHasBcTGD {
		t.Fatalf("same-IODC MT30 did not preserve TGD: got %g known=%v", st.clk.TGD, st.clkHasBcTGD)
	}

	third := now.Add(10 * time.Minute)
	apply(m34(3, 100599), third)
	apply(m10(3, 12, 100602), third)
	apply(m11(100602), third)
	if !st.timeDiscoValid || st.timeDiscoNs >= 2.5 {
		t.Errorf("continuous MT34 changeover time disco = %g ns valid=%v, want <2.5 ns", st.timeDiscoNs, st.timeDiscoValid)
	}
}
