package state

import (
	"fmt"
	"sort"
	"time"
)

// MonitoringSatellite reports observation evidence even before a complete
// ephemeris is assembled. Witnesses are distinct sources across all signals,
// never reference-file records. Source identities remain inside this audience.
// A position comes from a fresh broadcast ephemeris when one exists, otherwise
// from a decoded almanac, which also places satellites no station hears.
type MonitoringSatellite struct {
	Name           string      `json:"name"`
	GNSS           int         `json:"gnssid"`
	WitnessTimes   []int64     `json:"witness_times"`
	Position       *[3]float64 `json:"ecef_m,omitempty"`
	PositionSource string      `json:"position_source,omitempty"` // "ephemeris" or "almanac"
}

func (s *Store) MonitoringSatellites(now time.Time) []MonitoringSatellite {
	byName := map[string]*MonitoringSatellite{}
	witnesses := map[string]map[string]time.Time{}
	bestSig := map[string]int{}
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			if st.seenBy == nil {
				continue
			}
			name := fmt.Sprintf("%c%02d", st.key.G.Letter(), st.key.Sv)
			sv := byName[name]
			if sv == nil {
				sv = &MonitoringSatellite{Name: name, GNSS: int(st.key.G), WitnessTimes: []int64{}}
				byName[name] = sv
				witnesses[name] = map[string]time.Time{}
			}
			for id, at := range st.seenBy {
				if !at.After(now) && now.Sub(at) <= freshReceiverWindow && at.After(witnesses[name][id]) {
					witnesses[name][id] = at
				}
			}
			if sig, ok := bestSig[name]; !ok || st.key.Sig < sig {
				if st.havePos && !st.posAt.IsZero() && !st.posAt.After(now) && now.Sub(st.posAt) <= posStaleBound && finiteECEF(st.pos) {
					sv.Position = &[3]float64{st.pos.X, st.pos.Y, st.pos.Z}
					sv.PositionSource = "ephemeris"
					bestSig[name] = st.key.Sig
				}
			}
		}
		sh.mu.Unlock()
	}
	for _, a := range s.almanacPositions(now) {
		sv := byName[a.name]
		if sv == nil {
			sv = &MonitoringSatellite{Name: a.name, GNSS: int(a.g), WitnessTimes: []int64{}}
			byName[a.name] = sv
		}
		if sv.Position == nil {
			sv.Position = &[3]float64{a.pos.X, a.pos.Y, a.pos.Z}
			sv.PositionSource = "almanac"
		}
	}
	s.monitoringMu.Lock()
	for name, g := range s.monitoringRoster {
		if byName[name] == nil {
			byName[name] = &MonitoringSatellite{Name: name, GNSS: g, WitnessTimes: []int64{}}
		}
	}
	s.monitoringMu.Unlock()
	out := make([]MonitoringSatellite, 0, len(byName))
	for name, sv := range byName {
		for _, at := range witnesses[name] {
			sv.WitnessTimes = append(sv.WitnessTimes, at.Unix())
		}
		sort.Slice(sv.WitnessTimes, func(i, j int) bool { return sv.WitnessTimes[i] > sv.WitnessTimes[j] })
		out = append(out, *sv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
