package state

import "time"

// Snapshot is the loopback /debug/state view of live SV state — a subset of the
// future native v2 svs feed, used to verify the pipeline before that feed exists.
type Snapshot struct {
	Time    string             `json:"time"`
	LiveSVs int                `json:"live_svs"`
	SVs     map[string]SVEntry `json:"svs"`
}

// SVEntry is one satellite×signal's live state. Optional fields use pointers so an
// unknown/not-yet-computable value is absent from the JSON rather than a sentinel
// (docs/OUTPUT.md §1.1).
type SVEntry struct {
	Name       string   `json:"name"`
	GnssID     int      `json:"gnssid"`
	SvID       int      `json:"svid"`
	SigID      int      `json:"sigid"`
	Health     int      `json:"health"`
	IOD        int      `json:"iod"`
	XM         *float64 `json:"x_m,omitempty"`
	YM         *float64 `json:"y_m,omitempty"`
	ZM         *float64 `json:"z_m,omitempty"`
	OrbitDisco *float64 `json:"orbit_disco_m,omitempty"`
	TimeDisco  *float64 `json:"time_disco_ns,omitempty"`
	LastSeenS  int      `json:"last_seen_s"`
}

// Snapshot builds the current view as of now.
func (s *Store) Snapshot(now time.Time) Snapshot {
	snap := Snapshot{Time: now.UTC().Format(time.RFC3339), SVs: map[string]SVEntry{}}
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			e := SVEntry{
				Name:      st.key.Name(),
				GnssID:    int(st.key.G),
				SvID:      st.key.Sv,
				SigID:     st.key.Sig,
				Health:    st.health,
				IOD:       st.iod,
				LastSeenS: int(now.Sub(st.lastSeen).Seconds()),
			}
			if st.havePos {
				x, y, z := st.pos.X, st.pos.Y, st.pos.Z
				e.XM, e.YM, e.ZM = &x, &y, &z
			}
			if st.orbitDiscoValid {
				v := st.orbitDisco
				e.OrbitDisco = &v
			}
			if st.timeDiscoValid {
				v := st.timeDiscoNs
				e.TimeDisco = &v
			}
			snap.SVs[e.Name] = e
			if st.haveEph {
				snap.LiveSVs++
			}
		}
		sh.mu.Unlock()
	}
	return snap
}
