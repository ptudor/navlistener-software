package state

import (
	"testing"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/navlistener/internal/ingest"
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
		s.shardFor(key).m[key] = &svState{key: key, lastSeen: now, seenBy: seen, havePos: true, pos: gnss.ECEF{X: 29600000}, posAt: now,
			haveEph: true, eph: kepler.Ephemeris{Toe: towFor(key.G, now)}, ephAt: now, haveWN: true, posOrbitAt: now}
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

func TestMonitoringReplayCannotRefreshOrRewindWitness(t *testing.T) {
	s := New(1)
	now := time.Now()
	apply := func(stamp, arrival time.Time, stamped bool) {
		s.Apply(&ingest.RawFrame{Source: "station", Recv: stamp, RecvLocal: arrival, RecvStamped: stamped,
			GnssID: gnss.GPS, SvID: 5, Words: sf1Words(85)})
	}
	apply(now.Add(-12*time.Hour), now, true)
	if len(s.MonitoringSatellites(now)[0].WitnessTimes) != 0 {
		t.Fatal("historical observation became current on arrival")
	}
	apply(now, now, true)
	apply(now.Add(-12*time.Hour), now.Add(time.Second), true)
	got := s.MonitoringSatellites(now.Add(time.Second))[0].WitnessTimes
	if len(got) != 1 || got[0] != now.Unix() {
		t.Fatalf("old replay rewound newer evidence: %v", got)
	}
	apply(now, now.Add(61*time.Second), true)
	apply(now.Add(61*time.Second), now.Add(61*time.Second), false)
	if len(s.MonitoringSatellites(now.Add(61 * time.Second))[0].WitnessTimes) != 0 {
		t.Fatal("retry or unstamped receipt renewed an expired witness")
	}
}

func TestMonitoringOldOrbitCannotBecomeCurrentByReceptionOrPropagation(t *testing.T) {
	now := time.Date(2026, 10, 17, 10, 0, 0, 0, time.UTC)
	cases := []string{
		"old rebroadcast", "week rollover", "unknown week", "wrong week",
		"unpropagated changeover", "reused issue before propagation",
		"old GLONASS", "GLONASS changeover before propagation", "day rollover",
	}
	for _, name := range cases {
		t.Run(name, func(t *testing.T) {
			s := New(1)
			key := Key{G: gnss.BeiDou, Sv: 6}
			st := &svState{key: key, haveEph: true, ephAt: now, eph: kepler.Ephemeris{Toe: towFor(key.G, now)},
				haveWN: true, havePos: true, pos: gnss.ECEF{X: 29600000}, posAt: now, posOrbitAt: now,
				seenBy: map[string]time.Time{"station": now}}
			switch name {
			case "old rebroadcast":
				st.eph.Toe = towFor(key.G, now.Add(-78*time.Hour))
			case "week rollover":
				st.ephAt = now.Add(-7 * 24 * time.Hour)
				st.posOrbitAt = st.ephAt
			case "unknown week":
				st.haveWN = false
			case "wrong week":
				st.wnMismatch = true
			case "unpropagated changeover":
				st.iod = 1
			case "reused issue before propagation":
				st.posOrbitAt = now.Add(-time.Minute)
			case "old GLONASS", "GLONASS changeover before propagation", "day rollover":
				key.G, st.key.G = gnss.GLONASS, gnss.GLONASS
				st.haveEph, st.haveGloEph = false, true
				st.gloTbAt = now
				st.posOrbitAt = now
				st.gloEph = glonass.Ephemeris{Tb: gloTOD(now.Add(-time.Hour))}
				switch name {
				case "GLONASS changeover before propagation":
					st.gloEph.Tb = gloTOD(now)
					st.posOrbitAt = now.Add(-time.Minute)
				case "day rollover":
					st.gloTbAt = now.Add(-24 * time.Hour)
					st.posOrbitAt = st.gloTbAt
					st.gloEph.Tb = gloTOD(now)
				}
			}
			s.shardFor(key).m[key] = st
			got := s.MonitoringSatellites(now)
			if len(got) != 1 || len(got[0].WitnessTimes) != 1 || got[0].Position != nil {
				t.Fatalf("old geometry became current or fresh witness vanished: %+v", got)
			}
		})
	}
}

func TestMonitoringUsesFreshSignalWhenPrimaryOrbitIsOld(t *testing.T) {
	now := time.Now()
	s := New(1)
	for _, sig := range []int{0, 8} {
		key := Key{G: gnss.BeiDou, Sv: 6, Sig: sig}
		toe := towFor(key.G, now)
		if sig == 0 {
			toe = towFor(key.G, now.Add(-78*time.Hour))
		}
		s.shardFor(key).m[key] = &svState{key: key, haveEph: true, ephAt: now, eph: kepler.Ephemeris{Toe: toe},
			haveWN: true, havePos: true, pos: gnss.ECEF{X: 29600000, Y: float64(sig)}, posAt: now, posOrbitAt: now,
			seenBy: map[string]time.Time{"station": now}}
	}
	got := s.MonitoringSatellites(now)
	if len(got) != 1 || got[0].Position == nil || got[0].Position[1] != 8 || len(got[0].WitnessTimes) != 1 {
		t.Fatalf("fresh alternate signal was not selected: %+v", got)
	}
}
