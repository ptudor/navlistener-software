package state

import "time"

// Snapshot is the compact /debug/state view of live SV state. It intentionally
// remains a diagnostic subset of the richer native v2 svs feed.
//
// regression fix (recorded design fact, not a defect): this is an in-RAM debug view
// only — there is NO cross-restart state persistence anywhere in the daemon.
// Every restart rebuilds the registry from live ingest (positions absent until
// each SV re-broadcasts a full set; discos need a second post-restart ephemeris,
// correctly gated on prior haveEph), consistent with the "re-decode from raw
// frames" design: the historian's nav_frames hypertable is the durable record
// and enables offline replay. If a restart blackout ever matters operationally
// (a disco spanning a deploy), the fix is a warm-start replay of the last few
// minutes of nav_frames at boot — not a snapshot file bolted onto this view.
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
	HaveHealth bool     `json:"have_health"` // health is meaningful only when true
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
				Name:       st.key.Name(),
				GnssID:     int(st.key.G),
				SvID:       st.key.Sv,
				SigID:      st.key.Sig,
				Health:     st.health,
				HaveHealth: st.haveHealth,
				IOD:        st.iod,
				LastSeenS:  int(now.Sub(st.lastSeen).Seconds()),
			}
			// mirror the feed's posFresh gate  — a position frozen by a
			// repeatedly-failing propagate tick (havePos stays true, posAt stops advancing)
			// must not linger in the debug view forever misleading an operator.
			if st.havePos && finiteECEF(st.pos) && !st.posAt.IsZero() && now.Sub(st.posAt) <= posStaleBound {
				x, y, z := st.pos.X, st.pos.Y, st.pos.Z
				e.XM, e.YM, e.ZM = &x, &y, &z
			}
			if st.orbitDiscoValid && finite(st.orbitDisco) {
				v := st.orbitDisco
				e.OrbitDisco = &v
			}
			if st.timeDiscoValid && finite(st.timeDiscoNs) {
				v := st.timeDiscoNs
				e.TimeDisco = &v
			}
			snap.SVs[e.Name] = e
			if st.haveEph || st.haveGloEph {
				snap.LiveSVs++
			}
		}
		sh.mu.Unlock()
	}
	return snap
}
