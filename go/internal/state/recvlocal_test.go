package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
)

// regression fix guard tests: on the push path Recv is the FEEDER's wall-clock stamp
// (accepted with up to 5 min of skew, or 7 days of spool-replay age), while all
// staleness/expiry/liveness/disco-age math must run on the collector's own clock
// (RawFrame.RecvLocal, read via LocalRecv). These tests pin the state package to
// the collector clock: a feeder stamp minutes-to-hours in the past must not make
// a live, continuously-reporting station or SV read as stale.

// pushFrame is gpsFrame with the two clock domains split the way the push path
// splits them: feederStamp in Recv, collector-local in RecvLocal.
func pushFrame(words []uint32, feederStamp, local time.Time) *ingest.RawFrame {
	f := gpsFrame(words, feederStamp)
	f.RecvLocal = local
	return f
}

func TestPushSkewedFeederStampKeepsSVLive(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	stamp := now.Add(-10 * time.Minute) // a feeder clock lagging well past the 5 min liveness windows
	st.Apply(pushFrame(sf1Words(85), stamp, now))
	st.Apply(pushFrame(sf2Words(85, 205075516), stamp, now))
	st.Apply(pushFrame(sf3Words(85), stamp, now))
	st.Propagate(now)

	e, ok := st.FeedSVs(now)["G05@0"]
	if !ok {
		t.Fatal("G05@0 not in feed")
	}
	if e.LastSeenS > 1 {
		t.Errorf("last_seen_s = %d, want ≈0 (collector clock, not the 10 min feeder skew)", e.LastSeenS)
	}

	// A 5 min TTL must not expire an SV the collector heard seconds ago, no
	// matter what the feeder's clock claimed.
	st.Expire(now, 5*time.Minute)
	if _, ok := st.FeedSVs(now)["G05@0"]; !ok {
		t.Error("skewed feeder stamp expired a just-heard SV (Expire is using Recv, want LocalRecv)")
	}

	// Dial-mode fallback unchanged: with no RecvLocal, Recv IS the collector
	// clock and a genuinely old lastSeen must still expire.
	st2 := New(4)
	st2.Apply(gpsFrame(sf1Words(85), stamp))
	st2.Apply(gpsFrame(sf2Words(85, 205075516), stamp))
	st2.Apply(gpsFrame(sf3Words(85), stamp))
	st2.Expire(now, 5*time.Minute)
	if _, ok := st2.FeedSVs(now)["G05@0"]; ok {
		t.Error("dial-mode SV unseen for 10 min survived a 5 min TTL")
	}
}

func TestPushSkewedFeederStampKeepsStationRFLive(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	f := &ingest.RawFrame{
		Recv:      now.Add(-10 * time.Minute), // skewed feeder stamp, past rfStaleAfter (5 min)
		RecvLocal: now,
		Source:    "obs1",
		RF:        &ingest.RawRF{Bands: []ingest.RFBand{{Block: 0, AGC: 2500}}},
	}
	st.Apply(f)

	if _, ok := st.FeedStationRF(now)["obs1"]; !ok {
		t.Error("station with a just-received RF sample dropped as stale (RF recency is using Recv, want LocalRecv)")
	}
	if got := st.FeedGlobal(now).TotalLiveReceivers; got != 1 {
		t.Errorf("total_live_receivers = %d, want 1 (capability/RF recency must use the collector clock)", got)
	}
}

// TestDiscoTrustGateUsesCollectorClock: the regression fix wall-clock disco-trust gate
// ("outgoing ephemeris applied ≥4h ago is not trusted") measures how long ago
// THIS collector applied the outgoing set. The pinned scenario is
// live-after-replay: the outgoing set arrived via spool replay (feeder stamp
// 6 h old, accepted by the 7-day replay horizon) seconds before the live
// changeover set (fresh stamp). On feeder stamps the gate reads a 6 h apply age
// and silently suppresses the disco — and a feeder clock hours slow would
// suppress every disco from that station permanently; on the collector clock
// the outgoing set was applied 30 s ago and the changeover must be measured.
func TestDiscoTrustGateUsesCollectorClock(t *testing.T) {
	st := New(4)
	now := time.Unix(1_700_000_000, 0)
	replayStamp := now.Add(-6 * time.Hour) // outgoing set: replayed, stamp well past discoTrustAge
	// First data set (IODE 85), replay-delivered — the collector applied it 30 s ago.
	st.Apply(pushFrame(sf1Words(85), replayStamp, now.Add(-30*time.Second)))
	st.Apply(pushFrame(sf2Words(85, 205075516), replayStamp, now.Add(-30*time.Second)))
	st.Apply(pushFrame(sf3Words(85), replayStamp, now.Add(-30*time.Second)))
	// Second data set (IODE 86, shifted M0, same Toe → the EphAge gate passes),
	// live: fresh feeder stamp. Pre-regression fix the gate computed
	// recv(B) − ephAt(A) = now − (now−6h) = 6 h ≥ discoTrustAge → disco absent.
	st.Apply(pushFrame(sf1Words(86), now, now))
	st.Apply(pushFrame(sf2Words(86, 205075516+2000), now, now))
	st.Apply(pushFrame(sf3Words(86), now, now))

	e := st.Snapshot(now).SVs["G05@0"]
	if e.OrbitDisco == nil {
		t.Fatal("orbit_disco absent: the regression fix trust gate rejected an outgoing set the collector applied 30s ago (gate is using Recv, want LocalRecv)")
	}
	if e.TimeDisco == nil {
		t.Error("time_disco absent, want computed")
	}
}

// TestGLONASSFrameWindowStaysOnFeederStamp: the regression fix frame-coherence window
// guards BROADCAST adjacency of tag-less GLONASS strings, so it must keep using
// the feeder's stamp even after regression fix moved staleness math to the collector
// clock. A spool replay delivers strings milliseconds apart locally; a window on
// the collector clock would pair a fresh string 1 (new tb frame) with cached
// strings 2/3 from the PREVIOUS frame — a chimera state vector (X new, Y/Z old)
// that AssembleGLONASS cannot detect (only string 2 carries tb) and that fires a
// phantom multi-thousand-km orbit-disco at the next real changeover.
func TestGLONASSFrameWindowStaysOnFeederStamp(t *testing.T) {
	st := New(4)
	t0 := time.Unix(1_700_000_000, 0) // broadcast time of the OLD frame (feeder stamps)
	local := t0.Add(30 * time.Minute) // the replay happens now, all strings ms apart locally

	replay := func(number int, coord int64, tb int, stamp, local time.Time) *ingest.RawFrame {
		f := glonassStringFrame(7, number, coord, 10, 1, 0, tb, stamp)
		f.RecvLocal = local
		return f
	}
	// Old frame's strings 1/2/3 (broadcast-adjacent at t0), replayed back-to-back.
	st.Apply(replay(1, 1000, 0, t0, local))
	st.Apply(replay(2, 2000, 450, t0.Add(2*time.Second), local.Add(time.Millisecond)))
	st.Apply(replay(3, 3000, 0, t0.Add(4*time.Second), local.Add(2*time.Millisecond)))

	key := Key{G: gnss.GLONASS, Sv: 7, Sig: 0}
	sh := st.shardFor(key)
	sh.mu.Lock()
	firstEph, have := sh.m[key].gloEph, sh.m[key].haveGloEph
	sh.mu.Unlock()
	if !have {
		t.Fatal("replayed coherent triple did not assemble")
	}

	// The NEXT replayed frame's string 1 (new epoch, ~30 s later on air) arrives
	// 3 ms later on the collector clock. Broadcast adjacency says the cached
	// strings 2/3 belong to the previous frame — no assembly may happen until
	// the new frame's own strings 2/3 arrive.
	st.Apply(replay(1, 9000, 0, t0.Add(30*time.Second), local.Add(3*time.Millisecond)))

	sh.mu.Lock()
	afterEph := sh.m[key].gloEph
	sh.mu.Unlock()
	if afterEph != firstEph {
		t.Error("replay-compressed delivery assembled a chimera across the frame boundary (window is using the collector clock, want the feeder stamp)")
	}
}
