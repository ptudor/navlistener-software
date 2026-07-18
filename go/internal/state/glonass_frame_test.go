package state

import (
	"fmt"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// setSignMag writes a sign-magnitude n-bit field (top bit sign, remaining n-1
// bits magnitude) at absolute bit offset start — the encoding gnss/frame's
// BitReader.SignMag decodes, used by GLONASS strings 1-3's coord/vel/accel.
func setSignMag(buf []byte, start, n int, value int64) {
	var sign, mag uint64
	if value < 0 {
		sign, mag = 1, uint64(-value)
	} else {
		mag = uint64(value)
	}
	v := (sign << uint(n-1)) | (mag & ((1 << uint(n-1)) - 1))
	setAbsBits(buf, start, n, v)
}

// glonassStringWords builds the 4-word raw form DecodeGLONASSString expects for
// one string (number 1/2/3), packing the same block-bit offsets the decoder
// reads (gnss/frame/glonass_string.go: string number @1, coord @50, vel @21,
// accel @45; string 2 additionally: health/Bn @5, tb @9). coord/vel/accel are
// raw quantized units (pre-scale-factor) — this test only needs distinct,
// round-trippable values, not physically realistic ones. Built on gloWords so
// the §4.7 check bits are always stamped (A4).
func glonassStringWords(number int, coord, vel, accel int64, health, tb int) []uint32 {
	return gloWords(number, func(buf []byte) {
		setSignMag(buf, 50, 27, coord)
		setSignMag(buf, 21, 24, vel)
		setSignMag(buf, 45, 5, accel)
		if number == 2 {
			setAbsBits(buf, 5, 3, uint64(health))
			setAbsBits(buf, 9, 7, uint64(tb))
		}
	})
}

func glonassStringFrame(svID, number int, coord, vel, accel int64, health, tb int, recv time.Time) *ingest.RawFrame {
	return &ingest.RawFrame{
		Recv: recv, Source: "test", GnssID: gnss.GLONASS, SvID: svID, SigID: 0, FreqID: 7,
		Words: glonassStringWords(number, coord, vel, accel, health, tb),
	}
}

// TestGLONASSChangeoverDoesNotMixEpochs guards strings 1/2/3 are coherent
// only within one ~30s frame; applyGLONASS previously reassembled unconditionally
// on every string arrival, so a fresh string arriving at a tb changeover paired
// with the other two strings still cached from up to ~30 minutes earlier —
// mixing X from the new epoch with Y/Z from the old. The reception-time window
//  must reject that and only accept a fully coherent (temporally close)
// triple.
func TestGLONASSChangeoverDoesNotMixEpochs(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)

	// First, fully coherent triple, all within the same instant.
	st.Apply(glonassStringFrame(7, 1, 1000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 2000, 20, 2, 0, 450, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 3000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	firstEph := sh.m[key].gloEph
	haveEph := sh.m[key].haveGloEph
	sh.mu.Unlock()
	if !haveEph {
		t.Fatal("first coherent triple did not assemble")
	}
	if firstEph.X != 1000*gloPosScale {
		t.Fatalf("first eph.X = %v, want %v (sanity check on the encoder)", firstEph.X, 1000*gloPosScale)
	}

	// Changeover: a fresh string 1 (shifted coords, a new epoch) arrives ~30
	// minutes later, alone — strings 2/3 are still the stale cached ones from t0.
	tChangeover := t0.Add(30 * time.Minute)
	st.Apply(glonassStringFrame(7, 1, 9000, 10, 1, 0, 0, tChangeover))

	sh.mu.Lock()
	afterEph := sh.m[key].gloEph
	sh.mu.Unlock()
	if afterEph != firstEph {
		t.Errorf("gloEph changed from an incoherent triple (fresh string 1 + stale strings 2/3): %+v -> %+v",
			firstEph, afterEph)
	}

	// The coherent triple completes once strings 2/3 arrive within the same
	// window as the new string 1 — now the changeover must take effect.
	st.Apply(glonassStringFrame(7, 2, 9500, 20, 2, 0, 450, tChangeover.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 9800, 30, 3, 0, 0, tChangeover.Add(4*time.Second)))

	sh.mu.Lock()
	finalEph := sh.m[key].gloEph
	sh.mu.Unlock()
	if finalEph == firstEph {
		t.Error("coherent changeover triple did not update gloEph")
	}
	if finalEph.X != 9000*gloPosScale {
		t.Errorf("final eph.X = %v, want %v (the new epoch's X)", finalEph.X, 9000*gloPosScale)
	}
}

// TestGLONASSL2OFDispatched guards L2OF (SigID 2) strings share the byte-identical
// 85-bit format with L1OF and must decode into the same per-SV state (keyed at Sig 0), not
// be dropped as "unsupported". A coherent L2OF triple must assemble an ephemeris.
func TestGLONASSL2OFDispatched(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	l2of := func(number int, coord, vel, accel int64, health, tb int, recv time.Time) *ingest.RawFrame {
		f := glonassStringFrame(9, number, coord, vel, accel, health, tb, recv)
		f.SigID = 2 // L2OF
		return f
	}
	st.Apply(l2of(1, 20000000, 10, 1, 0, 0, t0))
	st.Apply(l2of(2, 20000000, 20, 2, 0, 450, t0.Add(2*time.Second)))
	st.Apply(l2of(3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	key := Key{G: gnss.GLONASS, Sv: 9, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	have := sh.m[key] != nil && sh.m[key].haveGloEph
	sh.mu.Unlock()
	if !have {
		t.Fatal("L2OF (SigID 2) triple did not assemble — dispatched as unsupported?")
	}
}

// TestGLONASSDiscoOnTbChangeover guards a GLONASS tb changeover with an offset
// position/clock must produce orbit_disco_m and time_disco_ns in FeedSVs (the metric was
// structurally absent for GLONASS before this fix).
func TestGLONASSDiscoOnTbChangeover(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	const tauRaw = -100000
	// First set: large (valid) positions, small velocity, tb encoded 450.
	st.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 450, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))
	st.Apply(glonassString4Frame(7, tauRaw, 5, t0.Add(6*time.Second)))

	// Second set ~15 min later: adjacent tb (451), an offset X and a changed clock.
	t1 := t0.Add(15 * time.Minute)
	st.Apply(glonassStringFrame(7, 1, 20500000, 10, 1, 0, 0, t1))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 451, t1.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t1.Add(4*time.Second)))
	st.Apply(glonassString4Frame(7, tauRaw+50000, 5, t1.Add(6*time.Second)))

	sv, ok := st.FeedSVs(t1.Add(6 * time.Second))["R07@0"]
	if !ok {
		t.Fatal("R07@0 missing from svs feed")
	}
	if sv.OrbitDiscoM == nil {
		t.Error("orbit_disco_m absent for a GLONASS tb changeover")
	}
	if sv.TimeDiscoNs == nil {
		t.Error("time_disco_ns absent for a GLONASS tb changeover with a clock offset")
	}
}

// TestGLONASSDiscoGatedOnAdjacentTb guards adjacency gate: disco measures
// broadcast continuity, which is only defined between ADJACENT tb sets (≤ 60 min
// apart, the maximum P1 update interval, GLO-ICD-5.1 Table 4.3). After missed
// changeovers the outgoing model would be extrapolated far past its ±15 min
// validity and the model error charged to orbit_disco_m as a phantom jump — the
// disco must be absent (never a sentinel), while the ephemeris itself still
// updates.
func TestGLONASSDiscoGatedOnAdjacentTb(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	// First set at tb index 45.
	st.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 45, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	// Second set at tb index 50 — Δtb = 5×900 s = 75 min > the 60 min adjacency
	// bound (changeovers were missed), arriving well inside discoTrustAge.
	t1 := t0.Add(20 * time.Minute)
	st.Apply(glonassStringFrame(7, 1, 20500000, 10, 1, 0, 0, t1))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 50, t1.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t1.Add(4*time.Second)))

	sv, ok := st.FeedSVs(t1.Add(4 * time.Second))["R07@0"]
	if !ok {
		t.Fatal("R07@0 missing from svs feed")
	}
	if sv.OrbitDiscoM != nil {
		t.Errorf("orbit_disco_m = %v for a 75 min tb gap, want absent (non-adjacent sets)", *sv.OrbitDiscoM)
	}
	if sv.TimeDiscoNs != nil {
		t.Errorf("time_disco_ns = %v for a 75 min tb gap, want absent", *sv.TimeDiscoNs)
	}
	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	tb := sh.m[key].gloEph.Tb
	sh.mu.Unlock()
	if tb != 50*900 {
		t.Errorf("gloEph.Tb = %v, want %v — the ephemeris itself must still update", tb, 50*900)
	}
}

// TestGLONASSUnknownSlotRejected guards u-blox emits SFRBX with svId 255
// for a GLONASS satellite whose slot is not yet identified (UBX-PROTOCOL), and every
// unknown-slot satellite aliases into that one key — so accepting it both fabricates
// SV "R255" and lets strings from two different satellites assemble a chimera
// ephemeris. Frames with svId outside the real slot range 1..24 (GLO-ICD-5.1 §5.1)
// must be parked under the glo_unknown_slot metric, never keyed into state.
func TestGLONASSUnknownSlotRejected(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	counter := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(gnss.GLONASS)), "glo_unknown_slot")
	before := testutil.ToFloat64(counter)

	for _, sv := range []int{0, 25, 255} {
		st.Apply(glonassStringFrame(sv, 1, 20000000, 10, 1, 0, 0, t0))
		st.Apply(glonassStringFrame(sv, 2, 20000000, 20, 2, 0, 45, t0.Add(2*time.Second)))
		st.Apply(glonassStringFrame(sv, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))
		key := Key{G: gnss.GLONASS, Sv: sv, Sig: 0}
		sh := st.shardFor(key)
		sh.mu.Lock()
		_, exists := sh.m[key]
		sh.mu.Unlock()
		if exists {
			t.Errorf("svId %d keyed into state, want rejected (1..24 only)", sv)
		}
	}
	if got := testutil.ToFloat64(counter) - before; got != 9 {
		t.Errorf("glo_unknown_slot delta = %v, want 9 (3 strings × 3 bad svIds)", got)
	}

	// Boundary slots 1 and 24 must still assemble normally.
	for _, sv := range []int{1, 24} {
		st.Apply(glonassStringFrame(sv, 1, 20000000, 10, 1, 0, 0, t0))
		st.Apply(glonassStringFrame(sv, 2, 20000000, 20, 2, 0, 45, t0.Add(2*time.Second)))
		st.Apply(glonassStringFrame(sv, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))
		key := Key{G: gnss.GLONASS, Sv: sv, Sig: 0}
		sh := st.shardFor(key)
		sh.mu.Lock()
		have := sh.m[key] != nil && sh.m[key].haveGloEph
		sh.mu.Unlock()
		if !have {
			t.Errorf("slot %d did not assemble", sv)
		}
	}
}

// TestGLONASSFreqChServedAndValidated guards regression fix (the FDMA channel k — "the"
// FDMA identifier per docs/CONSTELLATIONS.md §5 — must be served on GLONASS svs
// entries) and state boundary (a frame with freqId outside 0..13 is
// parked under glo_bad_freq, never stored as identity metadata).
func TestGLONASSFreqChServedAndValidated(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	st.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 45, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	sv, ok := st.FeedSVs(t0.Add(4 * time.Second))["R07@0"]
	if !ok {
		t.Fatal("R07@0 missing from svs feed")
	}
	if sv.FreqCh == nil || *sv.FreqCh != 0 {
		t.Errorf("freq_ch = %v, want 0 (freqId 7 − 7)", sv.FreqCh)
	}

	// Out-of-plan freqId: parked with a metric, no state entry.
	counter := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(gnss.GLONASS)), "glo_bad_freq")
	before := testutil.ToFloat64(counter)
	bad := glonassStringFrame(9, 1, 20000000, 10, 1, 0, 0, t0)
	bad.FreqID = 200
	st.Apply(bad)
	if got := testutil.ToFloat64(counter) - before; got != 1 {
		t.Errorf("glo_bad_freq delta = %v, want 1", got)
	}
	key := Key{G: gnss.GLONASS, Sv: 9, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	_, exists := sh.m[key]
	sh.mu.Unlock()
	if exists {
		t.Error("freqId 200 frame keyed into state, want parked")
	}
}

// glonassLnFrame builds a string carrying the ℓn fast malfunction flag (regression fix;
// GLO-ICD-5.1 Table 4.6: string 3 → block offset 20, odd strings 5–15 → offset 76).
func glonassLnFrame(svID, number, ln int, recv time.Time) *ingest.RawFrame {
	off := 20
	if number != 3 {
		off = 76
	}
	words := gloWords(number, func(buf []byte) { setAbsBits(buf, off, 1, uint64(ln)) })
	return &ingest.RawFrame{
		Recv: recv, Source: "test", GnssID: gnss.GLONASS, SvID: svID, SigID: 0, FreqID: 7,
		Words: words,
	}
}

// TestGLONASSLnHealth guards state wiring: the ℓn fast malfunction flag
// (≤10 s latency by design, GLO-ICD-5.1 §5.3 note) must reach served health the
// moment any carrying string decodes — up to ~50 s before Bn catches up — and a
// cleared ℓn must restore health. The served health_subcode is the packed
// Bn | ℓn<<3 (docs/OUTPUT.md §2.2), so the Bn-vs-ℓn disagreement window is visible.
func TestGLONASSLnHealth(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	// Coherent healthy frame: strings 1/2/3 (Bn=0, ℓn=0 rides string 3 clear).
	st.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 45, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	sv, ok := st.FeedSVs(t0.Add(4 * time.Second))["R07@0"]
	if !ok {
		t.Fatal("R07@0 missing from svs feed")
	}
	if sv.HealthCode != 1 || sv.HealthSubcode != 0 {
		t.Fatalf("healthy frame: health_code=%d subcode=%d, want 1/0", sv.HealthCode, sv.HealthSubcode)
	}

	// ℓn=1 arrives on string 5 (the fast path, no string 2 needed): served health
	// must flip to not-ok immediately, subcode showing the packed ℓn bit.
	st.Apply(glonassLnFrame(7, 5, 1, t0.Add(8*time.Second)))
	sv = st.FeedSVs(t0.Add(8 * time.Second))["R07@0"]
	if sv.HealthCode != 2 {
		t.Errorf("ℓn=1: health_code = %d, want 2 (the fast flag must gate health)", sv.HealthCode)
	}
	if sv.HealthSubcode != 8 {
		t.Errorf("ℓn=1: health_subcode = %d, want 8 (packed ℓn bit)", sv.HealthSubcode)
	}

	// ℓn clears on the next carrying string: health restored, subcode zeroed.
	st.Apply(glonassLnFrame(7, 7, 0, t0.Add(10*time.Second)))
	sv = st.FeedSVs(t0.Add(10 * time.Second))["R07@0"]
	if sv.HealthCode != 1 || sv.HealthSubcode != 0 {
		t.Errorf("ℓn cleared: health_code=%d subcode=%d, want 1/0", sv.HealthCode, sv.HealthSubcode)
	}

	// regression fix discipline: for a fresh SV with no Bn yet, ℓn=0 alone must NOT claim
	// health OK (half the Table 5.1 evidence), but ℓn=1 alone must claim not-ok.
	st.Apply(glonassLnFrame(9, 5, 0, t0))
	key := Key{G: gnss.GLONASS, Sv: 9, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	have := sh.m[key].haveHealth
	sh.mu.Unlock()
	if have {
		t.Error("ℓn=0 with no Bn decoded claimed haveHealth (would serve OK off half the evidence)")
	}
	st.Apply(glonassLnFrame(9, 5, 1, t0.Add(2*time.Second)))
	sh.mu.Lock()
	have, health := sh.m[key].haveHealth, sh.m[key].health
	sh.mu.Unlock()
	if !have || health != 8 {
		t.Errorf("ℓn=1 with no Bn: haveHealth=%v health=%d, want true/8", have, health)
	}
}

// gloPosScale mirrors gnss/frame's unexported gloPos (2^-11 km) scale factor —
// duplicated here (not imported; frame's constant is unexported) purely to
// translate this test's raw encoded units back to the decoded km for assertions.
const gloPosScale = 1.0 / (1 << 11)

// glonassString4Frame builds a string-4 frame carrying the SV clock terms
// (regression fix; ICD Ed. 5.1 Table 4.6: τn at block offset 5 width 22, Δτn at 27
// width 5, both sign-magnitude, raw pre-scale units).
func glonassString4Frame(svID int, tauRaw, dtauRaw int64, recv time.Time) *ingest.RawFrame {
	words := gloWords(4, func(buf []byte) {
		setSignMag(buf, 5, 22, tauRaw)
		setSignMag(buf, 27, 5, dtauRaw)
	})
	return &ingest.RawFrame{
		Recv: recv, Source: "test", GnssID: gnss.GLONASS, SvID: svID, SigID: 0, FreqID: 7,
		Words: words,
	}
}

// TestGLONASSClockFromString4 guards state wiring: a same-frame string 4
// refreshes the assembled ephemeris with τn/Δτn (ClockKnown), and a stale
// string 4 from a previous frame must not attach to a fresh 1/2/3 triple — the
// same temporal rule regression fix applies to the position strings.
func TestGLONASSClockFromString4(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	const tauRaw = -123456

	// Broadcast order 1,2,3,4 at ~2s spacing: assembly first happens at string 3
	// (clockless — string 4 hasn't arrived), then string 4 completes the frame.
	st.Apply(glonassStringFrame(7, 1, 1000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 2000, 20, 2, 0, 450, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 3000, 30, 3, 0, 0, t0.Add(4*time.Second)))

	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	eph, have := sh.m[key].gloEph, sh.m[key].haveGloEph
	sh.mu.Unlock()
	if !have {
		t.Fatal("triple did not assemble")
	}
	if eph.ClockKnown {
		t.Error("ClockKnown = true before any string 4 arrived")
	}

	st.Apply(glonassString4Frame(7, tauRaw, 5, t0.Add(6*time.Second)))
	sh.mu.Lock()
	eph = sh.m[key].gloEph
	sh.mu.Unlock()
	if !eph.ClockKnown {
		t.Fatal("same-frame string 4 did not attach the clock")
	}
	if want := float64(tauRaw) / (1 << 30); eph.TauN != want {
		t.Errorf("TauN = %v, want %v", eph.TauN, want)
	}

	// A tb changeover 30 minutes later: fresh strings 1/2/3, but the cached
	// string 4 is from the old frame — the new set must assemble clockless
	// rather than pair the stale τn with the new epoch.
	t1 := t0.Add(30 * time.Minute)
	st.Apply(glonassStringFrame(7, 1, 9000, 10, 1, 0, 0, t1))
	st.Apply(glonassStringFrame(7, 2, 9500, 20, 2, 0, 450, t1.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 9800, 30, 3, 0, 0, t1.Add(4*time.Second)))

	sh.mu.Lock()
	eph = sh.m[key].gloEph
	sh.mu.Unlock()
	if eph.X != 9000*gloPosScale {
		t.Fatalf("changeover triple did not assemble: X = %v", eph.X)
	}
	if eph.ClockKnown {
		t.Error("stale string 4 from the previous frame attached to a fresh triple")
	}
}
