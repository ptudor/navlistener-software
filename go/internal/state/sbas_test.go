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

// TestSBASDetectRetainsStaleEntries guards the DETECTOR's view must keep a
// dark PRN observable (with its climbing last_seen_s) after the served feed drops
// it, until RAM eviction — the sbas_lost classifier has no input otherwise.
func TestSBASDetectRetainsStaleEntries(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.sbas[133] = &sbasState{prn: 133, provider: "WAAS", lastType: 1, lastSeen: now}

	at := now.Add(sbasStaleAfter + time.Minute)
	got := s.SBASDetect(at)
	ent, ok := got["133"]
	if !ok {
		t.Fatalf("stale PRN missing from SBASDetect: %+v", got)
	}
	if want := int(at.Sub(now).Seconds()); ent.LastSeenS != want {
		t.Errorf("LastSeenS = %d, want %d (the climbing age)", ent.LastSeenS, want)
	}
	// The served feed must still drop it (regression fix unchanged).
	if served := s.FeedSBAS(at); len(served) != 0 {
		t.Errorf("FeedSBAS still serves the stale PRN: %+v", served)
	}
}

// TestStationLastSeen guards station_offline read model: the union of
// nav-capability and RF recency, unfiltered, so a dark station keeps a climbing
// age (the capability half is never evicted).
func TestStationLastSeen(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.recordCapability("stnA", gnss.GPS, 0, now.Add(-10*time.Minute))
	s.rf["stnA"] = &rfStation{id: "stnA", lastSeen: now.Add(-2 * time.Minute)} // fresher RF wins
	s.recordCapability("stnB", gnss.GPS, 0, now.Add(-40*time.Minute))          // long-dark, caps only

	ages := s.StationLastSeen(now)
	if got, want := ages["stnA"], 120; got != want {
		t.Errorf("stnA age = %d, want %d (the fresher of caps/rf)", got, want)
	}
	if got, want := ages["stnB"], 2400; got != want {
		t.Errorf("stnB age = %d, want %d (retained well past every serving filter)", got, want)
	}
}

// TestFeedGlobalSBASAndZeroFill guards the global feed must count fresh SBAS PRNs
// (they live in s.sbas, not the shards) and zero-fill every per-constellation pair so a
// count of 0 is an explicit value, not an absent key.
func TestFeedGlobalSBASAndZeroFill(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.sbas[133] = &sbasState{prn: 133, provider: "WAAS", lastType: 1, lastSeen: now}

	g := s.FeedGlobal(now.Add(time.Minute))
	if g.Counts["sbas_svs"] != 1 || g.Counts["sbas_sigs"] != 1 {
		t.Errorf("sbas counts = %d/%d, want 1/1", g.Counts["sbas_svs"], g.Counts["sbas_sigs"])
	}
	for _, c := range []string{"gps", "sbas", "galileo", "beidou", "qzss", "glonass", "navic"} {
		if _, ok := g.Counts[c+"_svs"]; !ok {
			t.Errorf("%s_svs key absent (want zero-filled)", c)
		}
		if _, ok := g.Counts[c+"_sigs"]; !ok {
			t.Errorf("%s_sigs key absent (want zero-filled)", c)
		}
	}
	if g.Counts["navic_svs"] != 0 {
		t.Errorf("navic_svs = %d, want an explicit 0", g.Counts["navic_svs"])
	}
}

// TestSBASDoNotUseLatchesAcrossInterleavedMessages guards a test-mode
// SBAS provider interleaves MT0 with its normal message stream (the DO-229
// "MT0/2" pattern — the WARNING following EGNOS-SDD-OS §4.1.2 Table 4,
// canonical cite), so the served health_code must latch on
// MT0 recency (sbasType0Hold, QZSS-L1S §4.1.2.3's 60 s exclusion) rather than
// flip back to OK on the very next non-MT0 message.
func TestSBASDoNotUseLatchesAcrossInterleavedMessages(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	// MT0 followed one second later by an ordinary MT2 (fast corrections) —
	// the nominal 1 Hz interleaving of a system under test.
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 131, SigID: 0, Recv: now,
		Words: sbasRawWords(0x53, 0)})
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 131, SigID: 0, Recv: now.Add(time.Second),
		Words: sbasRawWords(0x9A, 2)})

	ent, ok := s.FeedSBAS(now.Add(2 * time.Second))["131"]
	if !ok {
		t.Fatal("SBAS entry missing")
	}
	if ent.HealthCode != 3 {
		t.Errorf("health_code after MT0→MT2 interleave = %d, want 3 (latched do-not-use)", ent.HealthCode)
	}
	if ent.LastType != 2 {
		t.Errorf("last_type = %d, want 2 (the latch must not hide the raw message type)", ent.LastType)
	}
	if ent.LastType0 == nil {
		t.Error("last_type_0 absent; consumers must see the raw MT0 recency")
	}

	// A fresh non-MT0 message keeps the entry alive past the hold window: with
	// no further MT0, the exclusion ages out and the GEO reads OK again.
	late := now.Add(sbasType0Hold + 30*time.Second)
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 131, SigID: 0, Recv: late,
		Words: sbasRawWords(0xC6, 2)})
	ent, ok = s.FeedSBAS(late.Add(time.Second))["131"]
	if !ok {
		t.Fatal("SBAS entry missing after hold expiry")
	}
	if ent.HealthCode != 1 {
		t.Errorf("health_code %v after the last MT0 = %d, want 1 (hold expired)",
			sbasType0Hold+31*time.Second, ent.HealthCode)
	}

	// A renewed MT0 re-arms the latch.
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 131, SigID: 0, Recv: late.Add(2 * time.Second),
		Words: sbasRawWords(0x53, 0)})
	if ent = s.FeedSBAS(late.Add(3 * time.Second))["131"]; ent.HealthCode != 3 {
		t.Errorf("health_code after renewed MT0 = %d, want 3", ent.HealthCode)
	}
}

// TestApplyRejectsOutOfEnvelopeSvID guards the svId arrives in the
// SFRBX/GNF1 header, OUTSIDE the nav message the CRC authenticates, so a
// corrupted/mis-set svId with an intact payload previously fabricated a fully
// served sbas-feed row (and, via RAWX, a phantom QZSS svs entry). The gate
// enforces SBAS PRN 120–158 (EGNOS-SDD-OS §5), QZSS svId 1–10 (PRN 193–202:
// QZSS-PNT-006 Table 3.2.1-1's SV-ID column is PRN−192, canonical
// cite; Table 4.2.2-5's 193–202 effective range concurs), NavIC svId 1–14
// (NAVIC-SPS-L5S Table 7).
func TestApplyRejectsOutOfEnvelopeSvID(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)

	// A CRC-valid SBAS message under a svId outside 120–158 must not create a
	// feed row; boundary PRNs 120 and 158 must.
	for _, sv := range []int{7, 119, 159, 255} {
		s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: sv, SigID: 0, Recv: now,
			Words: sbasRawWords(0x53, 2)})
	}
	if got := s.FeedSBAS(now); len(got) != 0 {
		t.Errorf("out-of-envelope SBAS svIds served: %+v", got)
	}
	for _, sv := range []int{120, 158} {
		s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: sv, SigID: 0, Recv: now,
			Words: sbasRawWords(0x53, 2)})
	}
	if got := s.FeedSBAS(now); len(got) != 2 {
		t.Errorf("boundary SBAS PRNs 120/158 not served: %+v", got)
	}

	// A RAWX observable under an impossible QZSS svId must not create a
	// phantom J77@0 svs entry (the QZSS L1 carrier IS mapped, so only the
	// envelope gate stops it).
	s.Apply(&ingest.RawFrame{GnssID: gnss.QZSS, SvID: 77, SigID: 0, Recv: now, Source: "obs1",
		Obs: &ingest.RawObs{RcvTow: 100000, PrM: 3.8e7, CpCyc: 3.8e7 / 0.19, LockTimeMs: 1000, CpValid: true}})
	if svs := s.FeedSVs(now); len(svs) != 0 {
		t.Errorf("out-of-envelope QZSS observable created svs entries: %+v", svs)
	}
	// An in-envelope QZSS observable still lands.
	s.Apply(&ingest.RawFrame{GnssID: gnss.QZSS, SvID: 3, SigID: 0, Recv: now, Source: "obs1",
		Obs: &ingest.RawObs{RcvTow: 100000, PrM: 3.8e7, CpCyc: 3.8e7 / 0.19, LockTimeMs: 1000, CpValid: true}})
	if svs := s.FeedSVs(now); len(svs) != 1 {
		t.Errorf("in-envelope QZSS observable missing from svs: %+v", svs)
	}
}

// TestSBASObservableCreatesNoSVSEntry guards only one SBAS carrier is
// mapped (L1), so a geometry-free pair can never form — an SBAS RAWX
// pseudorange (every F9-class receiver emits one for a tracked GEO) must not
// create a permanently data-less S###@0 svs entry duplicating the sbas feed.
func TestSBASObservableCreatesNoSVSEntry(t *testing.T) {
	s := New(4)
	now := time.Unix(1_700_000_000, 0)
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 131, SigID: 0, Recv: now, Source: "obs1",
		Obs: &ingest.RawObs{RcvTow: 100000, PrM: 3.8e7, CpCyc: 3.8e7 / 0.19, LockTimeMs: 1000, CpValid: true}})
	if svs := s.FeedSVs(now); len(svs) != 0 {
		t.Errorf("SBAS observable created svs entries: %+v", svs)
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
	s.Apply(&ingest.RawFrame{GnssID: gnss.SBAS, SvID: 133, SigID: 0, Recv: now, Source: "obs1",
		Words: sbasRawWords(0x00, 0)})
	if got := s.FeedSBAS(now); len(got) != 0 {
		t.Fatalf("bad-preamble message must not create SBAS state: %+v", got)
	}
	// a preamble-rejected message is not a successful decode — it must
	// not install the durable (1,0) station capability fingerprint either
	// (capStation never forgets a signal; a poisoned entry later drives the
	// capability_signal_lost / capability_impossible classifiers, regression fix).
	if caps := s.FeedStationCapabilities(now); len(caps["obs1"]) != 0 {
		t.Fatalf("bad-preamble message recorded a capability: %+v", caps["obs1"])
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
