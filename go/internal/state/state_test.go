package state

import (
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// --- synthetic LNAV builders (mirror the frame package's, using the exported
// GPSParity as the parity oracle) ---

func fieldOff(w, a int) int { return (w-1)*24 + (a - 1) }

func setField(buf []byte, w, a, n int, val int64) {
	start := fieldOff(w, a)
	u := uint64(val) & ((1 << uint(n)) - 1)
	for i := 0; i < n; i++ {
		if u&(1<<uint(n-1-i)) != 0 {
			pos := start + i
			buf[pos>>3] |= 1 << uint(7-(pos&7))
		}
	}
}

func setSplit32(buf []byte, wHi, aHi, wLo int, val int64) {
	u := uint64(uint32(val))
	setField(buf, wHi, aHi, 8, int64(u>>24))
	setField(buf, wLo, 1, 24, int64(u&0xFFFFFF))
}

func read24(buf []byte, wordIdx int) uint32 {
	var v uint32
	for i := 0; i < 24; i++ {
		pos := wordIdx*24 + i
		b := (buf[pos>>3] >> uint(7-(pos&7))) & 1
		v = (v << 1) | uint32(b)
	}
	return v
}

// packWords lays the intended 24 data bits per word into bits 29..6, the way
// u-blox delivers them (receiver-validated, un-inverted); DecodeGPSLNAV reads
// those directly.
func packWords(buf []byte) []uint32 {
	words := make([]uint32, 10)
	for i := 0; i < 10; i++ {
		words[i] = read24(buf, i) << 6
	}
	return words
}

func sf1Words(iodcLo int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 1)
	setField(buf, 3, 1, 10, 2200)
	setField(buf, 3, 13, 4, 4)
	setField(buf, 8, 1, 8, iodcLo)
	setField(buf, 8, 9, 16, 27000)
	setField(buf, 10, 1, 22, 214748)
	return packWords(buf)
}

func sf2Words(iode, m0 int64) []uint32 { return sf2WordsToe(iode, m0, 27000) }

// sf2WordsToe is sf2Words with an explicit Toe (word 10, seconds/16 scale), so
// tests can control the ephemeris changeover reference epoch directly (// computeDisco's staleness gate is keyed on the gap between two Toe values).
func sf2WordsToe(iode, m0, toe int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 2)
	setField(buf, 3, 1, 8, iode)
	setField(buf, 3, 9, 16, -640)
	setField(buf, 4, 1, 16, 12596)
	setSplit32(buf, 4, 17, 5, m0)
	setField(buf, 6, 1, 16, 537)
	setSplit32(buf, 6, 17, 7, 42949673)
	setField(buf, 8, 1, 16, 4295)
	setSplit32(buf, 8, 17, 9, 2702199603)
	setField(buf, 10, 1, 16, toe)
	return packWords(buf)
}

func sf3Words(iode int64) []uint32 {
	buf := make([]byte, 30)
	setField(buf, 2, 20, 3, 3)
	setField(buf, 3, 1, 16, 54)
	setSplit32(buf, 3, 17, 4, -341830285)
	setField(buf, 5, 1, 16, -107)
	setSplit32(buf, 5, 17, 6, 656335797)
	setField(buf, 7, 1, 16, 8000)
	setSplit32(buf, 7, 17, 8, 478536844)
	setField(buf, 9, 1, 24, -22679)
	setField(buf, 10, 1, 8, iode)
	setField(buf, 10, 9, 14, 279)
	return packWords(buf)
}

func gpsFrame(words []uint32, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{Recv: recv, Source: "test", GnssID: gnss.GPS, SvID: 5, SigID: 0, Words: words}
}

func TestStoreEndToEnd(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	st.Propagate(now)

	snap := st.Snapshot(now)
	e, ok := snap.SVs["G05@0"]
	if !ok {
		t.Fatalf("G05@0 not in snapshot: %+v", snap.SVs)
	}
	if e.XM == nil || e.YM == nil || e.ZM == nil {
		t.Fatal("G05@0 has no propagated position")
	}
	radius := math.Sqrt((*e.XM)*(*e.XM) + (*e.YM)*(*e.YM) + (*e.ZM)*(*e.ZM))
	if radius < 26.0e6 || radius > 27.2e6 {
		t.Errorf("propagated radius = %.0f m, want ~26560 km", radius)
	}
	if snap.LiveSVs != 1 {
		t.Errorf("live_svs = %d, want 1", snap.LiveSVs)
	}
}

func TestStoreDiscoOnIODChange(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	// First data set (IODE 85).
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	// A new data set (IODE 86) with a slightly shifted M0 → a small orbit-disco.
	st.Apply(gpsFrame(sf1Words(86), now))
	st.Apply(gpsFrame(sf2Words(86, 205075516+2000), now))
	st.Apply(gpsFrame(sf3Words(86), now))

	snap := st.Snapshot(now)
	e := snap.SVs["G05@0"]
	if e.OrbitDisco == nil {
		t.Fatal("orbit_disco not computed on IOD change")
	}
	if *e.OrbitDisco <= 0 || *e.OrbitDisco > 5000 {
		t.Errorf("orbit_disco = %v m, want a small positive jump", *e.OrbitDisco)
	}
	if e.TimeDisco == nil {
		t.Error("time_disco not computed on IOD change")
	}
}

// TestStoreDiscoGatedOnStaleOutgoingEphemeris guards computeDisco must not
// trust a disco computed against an outgoing ephemeris whose Toe is more than
// discoTrustAge (4h) away from the new changeover epoch — an SV unseen for hours
// and then refreshed would otherwise propagate an arbitrarily stale outgoing set,
// producing a physically meaningless (but detector-triggering) discontinuity.
func TestStoreDiscoGatedOnStaleOutgoingEphemeris(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	// First data set: Toe raw 27000 (×16 scale = 432000s).
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2WordsToe(85, 205075516, 27000), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	// Second data set: Toe raw 28000 (448000s) — a 16000s (4.44h) gap, over the 4h
	// discoTrustAge, so the disco must be gated to absent rather than computed.
	st.Apply(gpsFrame(sf1Words(86), now))
	st.Apply(gpsFrame(sf2WordsToe(86, 205075516+2000, 28000), now))
	st.Apply(gpsFrame(sf3Words(86), now))

	snap := st.Snapshot(now)
	e := snap.SVs["G05@0"]
	if e.OrbitDisco != nil {
		t.Errorf("orbit_disco = %v, want absent (outgoing ephemeris is stale relative to the changeover epoch)", *e.OrbitDisco)
	}
	if e.TimeDisco != nil {
		t.Errorf("time_disco = %v, want absent", *e.TimeDisco)
	}
}

// TestFeedAlmanacDeterministicSignalPick guards FeedAlmanac must pick one
// signal's position per SV deterministically (lowest SigID = primary signal), not
// whichever signal Go's randomized map/shard iteration happens to visit first.
func TestFeedAlmanacDeterministicSignalPick(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)

	insert := func(sig int, pos gnss.ECEF) {
		key := Key{G: gnss.Galileo, Sv: 14, Sig: sig}
		sh := st.shardFor(key)
		sh.mu.Lock()
		sh.m[key] = &svState{key: key, pos: pos, havePos: true, posAt: now}
		sh.mu.Unlock()
	}
	// Insert the non-primary signal first so a naive "first wins" would pick it.
	insert(1, gnss.ECEF{X: 999, Y: 999, Z: 999})
	insert(0, gnss.ECEF{X: 111, Y: 222, Z: 333})

	alm := st.FeedAlmanac(now)
	e, ok := alm["E14"]
	if !ok {
		t.Fatalf("E14 not in almanac: %+v", alm)
	}
	if e.EcefXM != 111 || e.EcefYM != 222 || e.EcefZM != 333 {
		t.Errorf("E14 almanac position = (%v,%v,%v), want the SigID=0 (primary) signal's (111,222,333)",
			e.EcefXM, e.EcefYM, e.EcefZM)
	}
	if e.T != int(now.Unix()) {
		t.Errorf("E14 almanac T = %d, want propagation epoch %d", e.T, now.Unix())
	}
	if got := st.FeedAlmanac(now.Add(posStaleBound + time.Second)); len(got) != 0 {
		t.Errorf("stale precise position still published in almanac: %+v", got)
	}
}

// TestStoreQZSS confirms that QZSS L1 C/A (gnssId 5) uses the shared GPS LNAV
// path and surfaces as J03@0 with a propagated position.
func TestStoreQZSS(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	qzss := func(words []uint32) *ingest.RawFrame {
		return &ingest.RawFrame{Recv: now, Source: "test", GnssID: gnss.QZSS, SvID: 3, SigID: 0, Words: words}
	}
	st.Apply(qzss(sf1Words(85)))
	st.Apply(qzss(sf2Words(85, 205075516)))
	st.Apply(qzss(sf3Words(85)))
	st.Propagate(now)

	e, ok := st.Snapshot(now).SVs["J03@0"]
	if !ok {
		t.Fatalf("J03@0 (QZSS) not in snapshot")
	}
	if e.GnssID != int(gnss.QZSS) || e.XM == nil {
		t.Errorf("QZSS entry wrong: %+v", e)
	}
	radius := math.Sqrt((*e.XM)*(*e.XM) + (*e.YM)*(*e.YM) + (*e.ZM)*(*e.ZM))
	if radius < 26.0e6 || radius > 27.2e6 {
		t.Errorf("QZSS radius = %.0f m (ephemeris is GPS-like here)", radius)
	}
}

func TestStoreExpire(t *testing.T) {
	st := New(2)
	now := time.Unix(1_700_000_000, 0)
	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	st.Expire(now.Add(3*time.Hour), 2*time.Hour)
	if len(st.Snapshot(now.Add(3*time.Hour)).SVs) != 0 {
		t.Error("stale SV should have been expired")
	}
}

// TestApplyByteFrameSkipsLNAVDispatch guards a byte-oriented frame
// (RTCM/SBF, Words nil, Bytes only) carries the RawFrame zero values for
// GnssID/SigID (GPS/0), matching the GPS LNAV dispatch case -- without a
// guard, DecodeGPSLNAV(nil) fails on every single RTCM/SBF message (e.g. once
// per second on a typical MSM stream), incrementing the lnav error counter and
// burying real LNAV decode errors under a permanently-red metric.
func TestApplyByteFrameSkipsLNAVDispatch(t *testing.T) {
	lnavBefore := testutil.ToFloat64(metrics.DecodeErrorsTotal.WithLabelValues("0", "lnav"))
	byteFrameBefore := testutil.ToFloat64(metrics.DecodeErrorsTotal.WithLabelValues("0", "byte_frame"))

	st := New(1)
	st.Apply(&ingest.RawFrame{MsgType: 1074, Bytes: []byte{0xDE, 0xAD, 0xBE, 0xEF}})

	if got := testutil.ToFloat64(metrics.DecodeErrorsTotal.WithLabelValues("0", "lnav")); got != lnavBefore {
		t.Errorf("lnav error counter = %v, want unchanged %v (a byte frame must not be dispatched to DecodeGPSLNAV)", got, lnavBefore)
	}
	if got := testutil.ToFloat64(metrics.DecodeErrorsTotal.WithLabelValues("0", "byte_frame")); got != byteFrameBefore+1 {
		t.Errorf("byte_frame error counter = %v, want %v", got, byteFrameBefore+1)
	}
	if len(st.Snapshot(time.Now()).SVs) != 0 {
		t.Error("a byte frame must not create an svState")
	}
}
