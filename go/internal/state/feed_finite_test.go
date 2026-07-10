package state

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

// TestFeedSVsNonFiniteOmitted guards a non-finite orbit-disco or position
// must never reach the feed. Before the fix, json.Marshal would fail on the whole
// envelope (NaN/Inf are not valid JSON numbers), and serve/serve.go's refresh
// keeps the previous cache bytes on a marshal error — freezing the svs feed for
// every consumer, indefinitely, because of one bad SV.
func TestFeedSVsNonFiniteOmitted(t *testing.T) {
	s := New(4)
	now := time.Now()
	key := Key{G: gnss.GPS, Sv: 1, Sig: 0}
	sh := s.shardFor(key)
	sh.mu.Lock()
	sh.m[key] = &svState{
		key:             key,
		lastSeen:        now,
		haveEph:         true,
		pos:             gnss.ECEF{X: math.NaN(), Y: math.NaN(), Z: math.NaN()},
		havePos:         true,
		orbitDiscoValid: true,
		orbitDisco:      math.NaN(),
		discoAt:         now,
		timeDiscoValid:  true,
		timeDiscoNs:     math.Inf(1),
	}
	sh.mu.Unlock()

	svs := s.FeedSVs(now)
	e, ok := svs[key.Name()]
	if !ok {
		t.Fatalf("expected an entry for %s", key.Name())
	}
	if e.XM != nil || e.YM != nil || e.ZM != nil {
		t.Errorf("XM/YM/ZM = %v/%v/%v, want nil (non-finite position must be absent)", e.XM, e.YM, e.ZM)
	}
	if e.OrbitDiscoM != nil {
		t.Errorf("OrbitDiscoM = %v, want nil (NaN must be absent)", *e.OrbitDiscoM)
	}
	if e.TimeDiscoNs != nil {
		t.Errorf("TimeDiscoNs = %v, want nil (+Inf must be absent)", *e.TimeDiscoNs)
	}

	if _, err := json.Marshal(svs); err != nil {
		t.Fatalf("json.Marshal failed on a feed containing a formerly-NaN SV: %v", err)
	}
}

// TestFeedSVsRefreshDoesNotFreezeOnBadSV is the refresh-regression guard: a store
// with one non-finite SV among healthy ones must still marshal cleanly and update
// on every call, rather than serve.go's refresh() keeping stale cached bytes.
func TestFeedSVsRefreshDoesNotFreezeOnBadSV(t *testing.T) {
	s := New(4)
	now := time.Now()

	goodKey := Key{G: gnss.GPS, Sv: 2, Sig: 0}
	badKey := Key{G: gnss.GPS, Sv: 3, Sig: 0}
	for _, e := range []struct {
		key  Key
		disc float64
	}{
		{goodKey, 1.5},
		{badKey, math.NaN()},
	} {
		sh := s.shardFor(e.key)
		sh.mu.Lock()
		sh.m[e.key] = &svState{
			key: e.key, lastSeen: now, haveEph: true,
			orbitDiscoValid: true, orbitDisco: e.disc, discoAt: now,
		}
		sh.mu.Unlock()
	}

	svs := s.FeedSVs(now)
	body, err := json.Marshal(svs)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	if svs[goodKey.Name()].OrbitDiscoM == nil || *svs[goodKey.Name()].OrbitDiscoM != 1.5 {
		t.Errorf("good SV's OrbitDiscoM = %v, want 1.5", svs[goodKey.Name()].OrbitDiscoM)
	}
	if svs[badKey.Name()].OrbitDiscoM != nil {
		t.Errorf("bad SV's OrbitDiscoM = %v, want nil", *svs[badKey.Name()].OrbitDiscoM)
	}
	if len(body) == 0 {
		t.Fatal("marshaled body is empty")
	}
}
