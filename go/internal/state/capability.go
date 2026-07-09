package state

import (
	"sort"
	"time"

	"github.com/ptudor/gnss"
)

// The capability fingerprint (docs/CONSTELLATIONS.md §7, docs/INTEGRITY.md §6). Each observer
// is a real receiver with real silicon limits — a ZED-F9T-00B is L1+L2, a -10B is L1+L5, same
// MON-VER string — so a node's *demonstrated* signal set is what the integrity layer must know
// to reason about what it should be reporting. We learn that set empirically here: every
// decoded nav frame is evidence the station tracks that (gnssId, sigId). A node that has
// demonstrated E5a and then goes silent on GalFnav while otherwise alive is a signal (jamming,
// spoofing, or fault), not merely "quiet." This is the observed half; a node's *declared*
// capability (from tudorgps) is folded in as the follow-on so impossibility — reporting a
// signal the silicon cannot hear — is catchable too.

// sigKey identifies one signal a station can produce: the constellation and its signal id.
type sigKey struct {
	g   gnss.GNSSID
	sig int
}

// capSig is the running evidence for one (gnssId, sigId) at one station.
type capSig struct {
	firstSeen time.Time
	lastSeen  time.Time
	count     uint64
}

// capStation is one observer's demonstrated capability set. The fingerprint is durable — a
// signal a node has ever produced stays in the set (per-signal lastSeen carries recency), so
// the record survives a transient outage, which is exactly the condition we want to detect.
type capStation struct {
	id       string
	lastSeen time.Time
	sigs     map[sigKey]*capSig
}

// recordCapability folds one decoded nav frame into the station's fingerprint. Called from
// Apply for frames carrying a nav payload; station telemetry (MON-RF/NAV-SAT) and raw
// observables have no per-signal identity and are excluded by the caller.
func (s *Store) recordCapability(source string, g gnss.GNSSID, sig int, recv time.Time) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	st := s.caps[source]
	if st == nil {
		st = &capStation{id: source, sigs: map[sigKey]*capSig{}}
		s.caps[source] = st
	}
	if recv.After(st.lastSeen) {
		st.lastSeen = recv
	}
	k := sigKey{g: g, sig: sig}
	c := st.sigs[k]
	if c == nil {
		c = &capSig{firstSeen: recv}
		st.sigs[k] = c
	}
	if recv.After(c.lastSeen) {
		c.lastSeen = recv
	}
	c.count++
}

// StationCapability is one demonstrated signal in a station's fingerprint (docs/OUTPUT.md
// §1.3). Consumers key on the numeric gnss/sig ids, never on letters (docs/CONSTELLATIONS.md
// §0). first_seen/last_seen bound how long the node has been producing the signal and how
// recently — a stale last_seen against a live station is the capability-loss signal.
type StationCapability struct {
	Gnss      int   `json:"gnss"`
	Sig       int   `json:"sig"`
	FirstSeen int64 `json:"first_seen"`
	LastSeen  int64 `json:"last_seen"`
	Count     uint64 `json:"count"`
}

// FeedStationCapabilities builds the per-station capability read model, each station's signals
// sorted by (gnss, sig) for a stable feed. Unlike the RF read model, capabilities are not aged
// out — a demonstrated capability is durable by design (that is what makes a gone-silent signal
// detectable); recency lives in each signal's last_seen. now is accepted for symmetry with the
// other Feed* methods and to keep the signature stable as the declared-capability merge lands.
func (s *Store) FeedStationCapabilities(now time.Time) map[string][]StationCapability {
	out := make(map[string][]StationCapability)
	s.capMu.Lock()
	defer s.capMu.Unlock()
	for id, st := range s.caps {
		caps := make([]StationCapability, 0, len(st.sigs))
		for k, c := range st.sigs {
			caps = append(caps, StationCapability{
				Gnss:      int(k.g),
				Sig:       k.sig,
				FirstSeen: c.firstSeen.Unix(),
				LastSeen:  c.lastSeen.Unix(),
				Count:     c.count,
			})
		}
		sort.Slice(caps, func(i, j int) bool {
			if caps[i].Gnss != caps[j].Gnss {
				return caps[i].Gnss < caps[j].Gnss
			}
			return caps[i].Sig < caps[j].Sig
		})
		out[id] = caps
	}
	return out
}
