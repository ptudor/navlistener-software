package state

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

const gloVelScale = 1.0 / float64(1<<20) // 2⁻²⁰ km/s, the string 1–3 velocity LSB

// gloRelayFrame is glonassStringFrame for slot 7 under an explicit relay
// (source, session "boot"), with X=Y=Z=coord and zero velocity.
func gloRelayFrame(source string, number int, coord int64, tb int, recv time.Time) *ingest.RawFrame {
	f := glonassStringFrame(7, number, coord, 0, 0, 0, tb, recv)
	f.Source, f.Session = source, "boot"
	return f
}

// gloStateFrame is one immediate-data string of slot 7 under an explicit relay
// carrying one axis of a state vector: string 1 → X/Vx, 2 → Y/Vy (+ tb), 3 → Z/Vz.
func gloStateFrame(source string, number int, coord, vel int64, tb int, recv time.Time) *ingest.RawFrame {
	f := glonassStringFrame(7, number, coord, vel, 0, 0, tb, recv)
	f.Source, f.Session = source, "boot"
	return f
}

func gloEphOf(t *testing.T, st *Store, slot int) (glonass.Ephemeris, bool) {
	t.Helper()
	key := Key{G: gnss.GLONASS, Sv: slot, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	s := sh.m[key]
	if s == nil {
		return glonass.Ephemeris{}, false
	}
	return s.gloEph, s.haveGloEph
}

// gloRawSet is one broadcast immediate-data set in the raw string units.
type gloRawSet struct {
	x, y, z, vx, vy, vz int64
	tb                  int
}

func (r gloRawSet) matches(e glonass.Ephemeris) bool {
	return e.X == float64(r.x)*gloPosScale && e.Y == float64(r.y)*gloPosScale && e.Z == float64(r.z)*gloPosScale &&
		e.Vx == float64(r.vx)*gloVelScale && e.Vy == float64(r.vy)*gloVelScale && e.Vz == float64(r.vz)*gloVelScale &&
		e.Tb == float64(r.tb)*900
}

func (r gloRawSet) ephemeris() glonass.Ephemeris {
	return glonass.Ephemeris{
		X: float64(r.x) * gloPosScale, Y: float64(r.y) * gloPosScale, Z: float64(r.z) * gloPosScale,
		Vx: float64(r.vx) * gloVelScale, Vy: float64(r.vy) * gloVelScale, Vz: float64(r.vz) * gloVelScale,
		Tb: float64(r.tb) * 900, TodKnown: true, Slot: 7,
	}
}

// continuedSet propagates r by 900 s and quantizes the result into the next
// broadcast set, so two consecutive fixtures describe one continuous orbit.
func continuedSet(t *testing.T, r gloRawSet) gloRawSet {
	t.Helper()
	e := r.ephemeris()
	at, err := glonass.Propagate(e, 900)
	if err != nil {
		t.Fatal(err)
	}
	before, err := glonass.Propagate(e, 899.5)
	if err != nil {
		t.Fatal(err)
	}
	after, err := glonass.Propagate(e, 900.5)
	if err != nil {
		t.Fatal(err)
	}
	pos := func(m float64) int64 { return int64(math.Round(m / 1000 / gloPosScale)) }
	vel := func(a, b float64) int64 { return int64(math.Round((b - a) / 1000 / gloVelScale)) }
	return gloRawSet{
		x: pos(at.X), y: pos(at.Y), z: pos(at.Z),
		vx: vel(before.X, after.X), vy: vel(before.Y, after.Y), vz: vel(before.Z, after.Z),
		tb: r.tb + 1,
	}
}

func applyGloSet(st *Store, source string, set gloRawSet, at time.Time, numbers ...int) {
	for _, n := range numbers {
		var f *ingest.RawFrame
		switch n {
		case 1:
			f = gloStateFrame(source, 1, set.x, set.vx, 0, at)
		case 2:
			f = gloStateFrame(source, 2, set.y, set.vy, set.tb, at.Add(2*time.Second))
		case 3:
			f = gloStateFrame(source, 3, set.z, set.vz, 0, at.Add(4*time.Second))
		}
		st.Apply(f)
	}
}

// TestGLONASSImmediateDataBufferedPerRelay: strings 1–4 are buffered per relay
// (source, session, signal), like the almanac pairs, because the frame window
// compares feeder stamps and only one relay's stamps are mutually comparable.
// Two stations hear slot 7 with feeder clocks 25 s apart. At a tb changeover,
// station a's new-frame string 1 (T0+30) lands after station b's old-frame
// strings 2/3 (T0+27, T0+29): with one shared slot per SV those three passed
// the 8 s window and assembled a chimera (X new, Y/Z and tb old) that replaced
// the served set as a same-tb reassembly, and a's genuine strings 2/3 then
// differenced against it as a multi-thousand-km orbit-disco. Per relay, the
// served set is unchanged until a's own strings 2/3 arrive, every assembled
// set is one broadcast set, and the changeover disco is the genuine sub-10 m
// one (set N+1 is set N propagated 900 s, so the orbit is continuous).
func TestGLONASSImmediateDataBufferedPerRelay(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	setN := gloRawSet{x: 52224000, vy: 2097152, tb: 48} // 25,500 km on X, 2 km/s along Y
	setN1 := continuedSet(t, setN)
	if setN1.x == setN.x || setN1.y == setN.y {
		t.Fatalf("fixture: the continued set did not move: %+v -> %+v", setN, setN1)
	}
	served := func(when string) glonass.Ephemeris {
		t.Helper()
		eph, ok := gloEphOf(t, st, 7)
		if !ok {
			t.Fatalf("%s: no GLONASS set assembled", when)
		}
		if !setN.matches(eph) && !setN1.matches(eph) {
			t.Fatalf("%s: served set is neither broadcast set (a chimera): %+v", when, eph)
		}
		return eph
	}

	// Frame N (tb 48) from a at T0, and from b whose feeder clock runs 25 s ahead.
	applyGloSet(st, "a", setN, t0, 1, 2, 3)
	if eph := served("after a's frame N"); !setN.matches(eph) {
		t.Fatalf("frame N = %+v, want set N", eph)
	}
	applyGloSet(st, "b", setN, t0.Add(25*time.Second), 1, 2, 3)
	if eph := served("after b's frame N"); !setN.matches(eph) {
		t.Fatalf("b's identical frame N changed the set: %+v", eph)
	}

	// Changeover: a's frame N+1 string 1 (T0+30) arrives after b's frame-N
	// strings 2/3 (T0+27, T0+29) — the shared-slot chimera window.
	applyGloSet(st, "a", setN1, t0.Add(30*time.Second), 1)
	if eph := served("after a's new string 1"); !setN.matches(eph) {
		t.Fatalf("a's new string 1 alone changed the served set (chimera with b's strings 2/3): %+v", eph)
	}
	if sv := st.FeedSVs(t0.Add(30 * time.Second))["R07@0"]; sv.OrbitDiscoM != nil {
		t.Fatalf("orbit_disco_m = %v before any changeover completed", *sv.OrbitDiscoM)
	}

	// a's own strings 2/3 complete the genuine new set.
	applyGloSet(st, "a", setN1, t0.Add(30*time.Second), 2, 3)
	if eph := served("after a's frame N+1"); !setN1.matches(eph) {
		t.Fatalf("frame N+1 = %+v, want set N+1", eph)
	}
	sv := st.FeedSVs(t0.Add(34 * time.Second))["R07@0"]
	if sv.OrbitDiscoM == nil {
		t.Fatal("orbit_disco_m absent after the genuine changeover")
	}
	if *sv.OrbitDiscoM >= 10 {
		t.Fatalf("orbit_disco_m = %v m, want the genuine sub-10 m changeover of a continuous orbit", *sv.OrbitDiscoM)
	}

	// b's frame N+1 (25 s later on its clock) reassembles the same set: no
	// disco work, still one broadcast set.
	applyGloSet(st, "b", setN1, t0.Add(55*time.Second), 1, 2, 3)
	if eph := served("after b's frame N+1"); !setN1.matches(eph) {
		t.Fatalf("b's identical frame N+1 changed the set: %+v", eph)
	}
	if sv := st.FeedSVs(t0.Add(59 * time.Second))["R07@0"]; sv.OrbitDiscoM == nil || *sv.OrbitDiscoM >= 10 {
		t.Fatalf("orbit_disco_m = %v after b's same-tb reassembly, want the unchanged sub-10 m value", sv.OrbitDiscoM)
	}
}

// TestGLONASSDrainDoesNotBlockLiveRelay: a station draining a day-old spool
// (forensic stamps a day behind, collector stamps now) interleaved string by
// string with a live station must neither block the live relay's assembly
// (the shared-slot window used to see a 24 h span) nor replace the live set
// with the replayed one (ordered by reception time, counted as stale_replay).
func TestGLONASSDrainDoesNotBlockLiveRelay(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	const liveX, drainX = 20000000, 21000000
	stale := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(gnss.GLONASS)), "stale_replay")
	before := testutil.ToFloat64(stale)
	live := func(number int, at time.Duration) *ingest.RawFrame {
		f := gloRelayFrame("a", number, liveX, 48, now.Add(at))
		f.RecvLocal = now.Add(at)
		return f
	}
	drain := func(number int, at time.Duration) *ingest.RawFrame {
		f := gloRelayFrame("b", number, drainX, 48, now.Add(-24*time.Hour).Add(at))
		f.RecvLocal = now.Add(at)
		return f
	}
	st.Apply(live(1, 0))
	st.Apply(drain(1, time.Second))
	st.Apply(live(2, 2*time.Second))
	st.Apply(drain(2, 3*time.Second))
	st.Apply(live(3, 4*time.Second))
	eph, ok := gloEphOf(t, st, 7)
	if !ok || eph.X != liveX*gloPosScale {
		t.Fatalf("live relay did not assemble between drained strings: %+v (ok=%v)", eph, ok)
	}
	st.Apply(drain(3, 5*time.Second))
	eph, _ = gloEphOf(t, st, 7)
	if eph.X != liveX*gloPosScale {
		t.Fatalf("day-old drained set replaced the live set: %+v", eph)
	}
	if got := testutil.ToFloat64(stale) - before; got != 1 {
		t.Errorf("stale_replay delta = %v, want 1 (the drained set assembled, then was refused)", got)
	}
	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	recvAt, tbAt := sh.m[key].gloEphRecvAt, sh.m[key].gloTbAt
	sh.mu.Unlock()
	// Both stamps carry the live relay's completing string-3 frame (now+4 s),
	// not the drained frames' day-old reception time.
	if !recvAt.Equal(now.Add(4*time.Second)) || !tbAt.Equal(now.Add(4*time.Second)) {
		t.Errorf("forensic stamps moved to the drained frames: gloEphRecvAt %v gloTbAt %v", recvAt, tbAt)
	}
}

// TestGLONASSSameTbReassemblyKeepsClock: every frame's string 3 arrives ~28 s
// after the previous frame's string 4, so each same-tb reassembly at string 3
// is clockless. The set it replaces describes the same tb, hence the same
// τn/Δτn, so the clock is carried forward; a string 4 lost right before a
// changeover then still leaves the outgoing set clocked and the time-disco
// for that changeover computable, instead of "unknowable".
func TestGLONASSSameTbReassemblyKeepsClock(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0)
	const tau1Raw, tau2Raw = -100000, -60000

	// Set 1 (tb 48) with its clock.
	st.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t0))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 48, t0.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t0.Add(4*time.Second)))
	st.Apply(glonassString4Frame(7, tau1Raw, 5, t0.Add(6*time.Second)))

	// Frame 2, same tb, strings 1–3 only: its string 4 is lost in a fade.
	t1 := t0.Add(30 * time.Second)
	st.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, t1))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 48, t1.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t1.Add(4*time.Second)))
	eph, _ := gloEphOf(t, st, 7)
	tau1 := float64(tau1Raw) / (1 << 30)
	if !eph.ClockKnown || eph.TauN != tau1 {
		t.Fatalf("same-tb reassembly without string 4 dropped the known clock: %+v", eph)
	}

	// Changeover (tb 49): strings 1–3, then the new set's string 4.
	t2 := t0.Add(15 * time.Minute)
	st.Apply(glonassStringFrame(7, 1, 20500000, 10, 1, 0, 0, t2))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, 49, t2.Add(2*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, t2.Add(4*time.Second)))
	eph, _ = gloEphOf(t, st, 7)
	if eph.Tb != 49*900 || eph.ClockKnown {
		t.Fatalf("changeover set = %+v, want tb 49 adopted clockless (no clock carried across a tb change)", eph)
	}
	st.Apply(glonassString4Frame(7, tau2Raw, 5, t2.Add(6*time.Second)))
	sv, ok := st.FeedSVs(t2.Add(6 * time.Second))["R07@0"]
	if !ok {
		t.Fatal("R07@0 missing from svs feed")
	}
	if sv.TimeDiscoNs == nil {
		t.Fatal("time_disco_ns absent: the outgoing set's clock was known for its tb and must have been carried")
	}
	want := math.Abs(float64(tau2Raw)/(1<<30)-tau1) * 1e9 // γn is zero in these fixtures
	if math.Abs(*sv.TimeDiscoNs-want) > 1e-6 {
		t.Errorf("time_disco_ns = %v, want %v", *sv.TimeDiscoNs, want)
	}
}

// TestStaleReplayDoesNotRegressDataSet: data-set replacement is ordered by
// forensic reception time. A live GPS set (IODE 85, received now) followed by a
// full older set (IODE 80) drained from a spool with a reception stamp six hours
// back must leave the served set, its clock and its forensic stamp untouched,
// compute no disco, and count the refusal under stale_replay; the next live set
// then applies normally.
func TestStaleReplayDoesNotRegressDataSet(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	stale := metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(gnss.GPS)), "stale_replay")
	before := testutil.ToFloat64(stale)

	st.Apply(gpsFrame(sf1Words(85), now))
	st.Apply(gpsFrame(sf2Words(85, 205075516), now))
	st.Apply(gpsFrame(sf3Words(85), now))
	key := Key{G: gnss.GPS, Sv: 5, Sig: 0}
	sh := st.shardFor(key)
	read := func() (iod, iodc int, recvAt time.Time, discoOK bool) {
		sh.mu.Lock()
		defer sh.mu.Unlock()
		s := sh.m[key]
		return s.iod, s.lnavIODC, s.ephRecvAt, s.orbitDiscoValid
	}
	if iod, _, recvAt, discoOK := read(); iod != 85 || !recvAt.Equal(now) || discoOK {
		t.Fatalf("setup: iod %d recvAt %v disco %v", iod, recvAt, discoOK)
	}

	replayed := now.Add(-6 * time.Hour)
	for _, words := range [][]uint32{sf1Words(80), sf2Words(80, 205075516+2000), sf3Words(80)} {
		f := gpsFrame(words, replayed)
		f.RecvLocal = now
		st.Apply(f)
	}
	iod, iodc, recvAt, discoOK := read()
	if iod != 85 || iodc != 85 {
		t.Fatalf("replayed IODE 80 set regressed the live set: iod %d iodc %d", iod, iodc)
	}
	if discoOK {
		t.Error("a disco was computed against a replayed older set")
	}
	if !recvAt.Equal(now) {
		t.Errorf("ephRecvAt = %v, want the live stamp %v", recvAt, now)
	}
	if got := testutil.ToFloat64(stale) - before; got != 1 {
		t.Errorf("stale_replay delta = %v, want 1", got)
	}

	// A genuinely newer set still applies, with its changeover disco.
	later := now.Add(30 * time.Second)
	st.Apply(gpsFrame(sf1Words(86), later))
	st.Apply(gpsFrame(sf2Words(86, 205075516+2000), later))
	st.Apply(gpsFrame(sf3Words(86), later))
	if iod, _, recvAt, discoOK := read(); iod != 86 || !recvAt.Equal(later) || !discoOK {
		t.Fatalf("live IODE 86 set did not apply: iod %d recvAt %v disco %v", iod, recvAt, discoOK)
	}
}

// TestGLONASSStaleReplayKeepsLiveSet is the GLONASS twin: a day-old replayed
// set whose tb lies within 30 min of the live tb's time of day (a station
// reconnecting after a one-day outage) must not replace the live set, move
// its forensic stamps (which would stop Propagate serving it), or produce an
// orbit-disco between a day-old state vector and the live one.
func TestGLONASSStaleReplayKeepsLiveSet(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	// tb index 5 lands ~100 s from now's Moscow time-of-day (see
	// TestGLONASSPropagateEphAgeCap), so the live set serves a position.
	const liveTb, replayTb = 5, 4
	st.Apply(glonassStringFrame(7, 1, 20000000, 10, 1, 0, 0, now.Add(-6*time.Second)))
	st.Apply(glonassStringFrame(7, 2, 20000000, 20, 2, 0, liveTb, now.Add(-4*time.Second)))
	st.Apply(glonassStringFrame(7, 3, 20000000, 30, 3, 0, 0, now.Add(-2*time.Second)))
	st.Propagate(now)
	if sv := st.FeedSVs(now)["R07@0"]; sv.XM == nil {
		t.Fatal("setup: live GLONASS set does not serve a position")
	}

	dayAgo := now.Add(-24 * time.Hour)
	for _, f := range []*ingest.RawFrame{
		glonassStringFrame(7, 1, 21000000, 10, 1, 0, 0, dayAgo),
		glonassStringFrame(7, 2, 21000000, 20, 2, 0, replayTb, dayAgo.Add(2*time.Second)),
		glonassStringFrame(7, 3, 21000000, 30, 3, 0, 0, dayAgo.Add(4*time.Second)),
	} {
		f.RecvLocal = now
		st.Apply(f)
	}
	eph, _ := gloEphOf(t, st, 7)
	if eph.Tb != liveTb*900 || eph.X != 20000000*gloPosScale {
		t.Fatalf("day-old replayed set replaced the live set: %+v", eph)
	}
	st.Propagate(now)
	sv := st.FeedSVs(now)["R07@0"]
	if sv.XM == nil {
		t.Error("live position stopped serving after the replayed set")
	}
	if sv.OrbitDiscoM != nil {
		t.Errorf("orbit_disco_m = %v between a day-old replay and the live set, want absent", *sv.OrbitDiscoM)
	}
}
