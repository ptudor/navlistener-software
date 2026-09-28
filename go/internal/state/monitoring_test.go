package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
)

func TestMonitoringWitnessesExpiryAndReset(t *testing.T) {
	s := New(2)
	now := time.Now()
	for _, sig := range []int{0, 3} {
		key := Key{G: gnss.Galileo, Sv: 14, Sig: sig}
		seen := map[string]time.Time{"same-station": now, "expired": now.Add(-61 * time.Second), "future": now.Add(time.Minute)}
		if sig == 3 {
			seen["another-station"] = now.Add(-time.Second)
		}
		s.shardFor(key).m[key] = &svState{key: key, lastSeen: now, seenBy: seen, havePos: true, pos: gnss.ECEF{X: 29600000}, posAt: now}
	}
	got := s.MonitoringSatellites(now)
	if len(got) != 1 || len(got[0].WitnessTimes) != 2 || got[0].Position == nil {
		t.Fatalf("bad distinct witness count: %+v", got)
	}
	got = s.MonitoringSatellites(now.Add(10 * time.Minute))
	if len(got) != 1 || len(got[0].WitnessTimes) != 0 || got[0].Position != nil {
		t.Fatal("stale evidence or vanished satellite")
	}
	s.Expire(now.Add(24*time.Hour), time.Hour)
	got = s.MonitoringSatellites(now.Add(24 * time.Hour))
	if len(got) != 1 || got[0].Name != "E14" || got[0].Position != nil {
		t.Fatal("expiry lost expected identity")
	}
	s.Reset()
	if len(s.MonitoringSatellites(now)) != 0 {
		t.Fatal("withdrawn identity survived audience reset")
	}
}
