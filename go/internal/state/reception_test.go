package state

import (
	"math"
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/gnss/physconst"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/reception"
)

func forecastFixture(t *testing.T) (*Store, reception.Site, *svState, time.Time) {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	c := reception.Config{Stations: []reception.Site{{Observer: "edge", Position: []float64{0, 0, 0}, Signals: []string{"0:0"}}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	s := New(1)
	key := Key{G: gnss.GPS, Sv: 1, Sig: 0}
	tow := towFor(gnss.GPS, now)
	// A circular equatorial orbit over the survey position at t0, safely above
	// the mask for the entire forecast. This tests geometry, not canned counts.
	st := &svState{key: key, haveHealth: true, haveEph: true, ephAt: now, ephRecvAt: now, lastSeen: now,
		seenBy: map[string]time.Time{"edge": now, "witness": now},
		eph:    kepler.Ephemeris{ID: gnss.GPS, SVID: 1, SqrtA: math.Sqrt(26560000), Toe: tow, Omega0: physconst.MustFor(gnss.GPS).OmegaE * tow}}
	s.shards[0].m[key] = st
	s.Propagate(now)
	return s, c.Stations[0], st, now
}

func TestReceptionForecastGeometryAndWitness(t *testing.T) {
	s, site, st, now := forecastFixture(t)
	good := s.ReceptionForecast(site, now)
	if !good.Valid() || len(good.Entries) != 1 || good.Entries[0].Slots != 31 || good.Entries[0].Signal != reception.Satellite {
		t.Fatalf("overhead forecast: %+v", good)
	}
	if other := s.ReceptionForecast(site, now); other.ID != good.ID {
		t.Fatal("same forecast changed identity")
	}
	site.Position[1] = 180
	if len(s.ReceptionForecast(site, now).Entries) != 0 {
		t.Fatal("satellite below opposite horizon included")
	}
	site.Position[1] = 0
	site.PerSignal = true
	if e := s.ReceptionForecast(site, now); len(e.Entries) != 1 || e.Entries[0].Signal != 0 {
		t.Fatal("signal expectation lost")
	}
	delete(st.seenBy, "witness")
	if len(s.ReceptionForecast(site, now).Entries) != 0 {
		t.Fatal("assessed station became its own independent witness")
	}
	for _, at := range []time.Time{now.Add(-61 * time.Second), now.Add(time.Second)} {
		st.seenBy["witness"] = at
		if len(s.ReceptionForecast(site, now).Entries) != 0 {
			t.Fatal("stale/future witness included")
		}
	}
	st.seenBy["witness"] = now
	st.health = 1
	if len(s.ReceptionForecast(site, now).Entries) != 0 {
		t.Fatal("unhealthy satellite included")
	}
	st.health = 0
	st.ephRecvAt = now.Add(-7 * 24 * time.Hour)
	if len(s.ReceptionForecast(site, now).Entries) != 0 {
		t.Fatal("week-old replay included")
	}
}

func TestForecastDoesNotBorrowOtherAudience(t *testing.T) {
	s, site, _, now := forecastFixture(t)
	if len(s.ReceptionForecast(site, now).Entries) == 0 {
		t.Fatal("fixture")
	}
	if len(New(1).ReceptionForecast(site, now).Entries) != 0 {
		t.Fatal("empty audience borrowed global orbits")
	}
}

func TestReceptionBoardHistoryAndIndependentCheck(t *testing.T) {
	s := New(1)
	now := time.Now()
	apply := func(seq uint64, d *ingest.ObserverDetails) {
		s.Apply(&ingest.RawFrame{Source: "edge", Session: "boot", Seq: seq, HasSeq: true, Recv: now, RecvLocal: now, Details: d, ReceptionCheck: &reception.Check{Alarm: 1}})
	}
	apply(1, &ingest.ObserverDetails{UptimeMS: 1, Firmware: "test"})
	apply(2, &ingest.ObserverDetails{UptimeMS: 2, Reception: &reception.Sample{Alarm: 1, Valid: 1}})
	apply(3, &ingest.ObserverDetails{UptimeMS: 3, ReceptionEvent: &reception.Sample{Boot: 1, Event: 7, Alarm: 1}})
	apply(4, &ingest.ObserverDetails{UptimeMS: 4, ReceptionEvent: &reception.Sample{Boot: 1, Event: 7, Alarm: 1}})
	apply(5, &ingest.ObserverDetails{UptimeMS: 5, ReceptionPowerEvent: &reception.PowerEvent{Boot: 1, Event: 8}})
	b := s.FeedStationBoards(now)["edge"]
	if b.Latest.Sequence != 1 || b.Reception.Sequence != 2 || b.Reception.ReceptionCheck.Alarm != 1 || len(b.ReceptionEvents) != 1 ||
		len(b.ReceptionPowerEvents) != 1 || b.ReceptionPowerEvents[0].Details.ReceptionPowerEvent.Event != 8 {
		t.Fatalf("independent streams: %+v", b)
	}
	if !s.FeedStationBoards(now.Add(16 * time.Second))["edge"].ReceptionStale {
		t.Fatal("reception never stale")
	}
	if s.LiveReceivers(now) != 0 {
		t.Fatal("assessment invented live RF")
	}
	for event := uint64(10); event <= 50; event++ {
		apply(event, &ingest.ObserverDetails{UptimeMS: event, ReceptionEvent: &reception.Sample{Boot: 1, Event: event}})
	}
	apply(51, &ingest.ObserverDetails{UptimeMS: 51, ReceptionEvent: &reception.Sample{Boot: 1, Event: 1}})
	b = s.FeedStationBoards(now)["edge"]
	if len(b.ReceptionEvents) != 32 || b.ReceptionEvents[0].Details.ReceptionEvent.Event != 19 || b.ReceptionEvents[31].Details.ReceptionEvent.Event != 50 {
		t.Fatal("reconnect replay evicted newer journal evidence")
	}
}
