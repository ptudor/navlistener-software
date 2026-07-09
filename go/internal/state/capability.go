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

// CapSignal is one (gnssId, sigId) a station is declared capable of producing — the tudorgps
// fingerprint the integrity layer checks the observed set against (docs/INTEGRITY.md §6).
type CapSignal struct {
	Gnss int `json:"gnss"`
	Sig  int `json:"sig"`
}

// CapabilityDiff compares a station's observed signal set against its declared (tudorgps) set
// and returns the two mismatches an operator watches (docs/INTEGRITY.md §6): unexpected =
// observed but NOT declared (a signal the silicon should not be able to produce — the
// capability_impossible input); missing = declared but never observed (a signal the node
// should produce but has not). Both are nil when no declared set exists — there is nothing to
// compare against. Deterministically ordered by (gnss, sig).
func CapabilityDiff(observed []StationCapability, declared []CapSignal) (unexpected, missing []CapSignal) {
	if len(declared) == 0 {
		return nil, nil
	}
	declSet := make(map[CapSignal]bool, len(declared))
	for _, c := range declared {
		declSet[c] = true
	}
	obsSet := make(map[CapSignal]bool, len(observed))
	for _, o := range observed {
		k := CapSignal{Gnss: o.Gnss, Sig: o.Sig}
		obsSet[k] = true
		if !declSet[k] {
			unexpected = append(unexpected, k)
		}
	}
	for _, c := range declared {
		if !obsSet[c] {
			missing = append(missing, c)
		}
	}
	sortCapSignals(unexpected)
	sortCapSignals(missing)
	return unexpected, missing
}

func sortCapSignals(cs []CapSignal) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Gnss != cs[j].Gnss {
			return cs[i].Gnss < cs[j].Gnss
		}
		return cs[i].Sig < cs[j].Sig
	})
}

// SetDeclaredCapabilities installs each station's declared (tudorgps) capability set, keyed by
// station id. Called once at startup from config; a station with no entry simply has no
// declared set, and only the observed-only detectors (signal-lost) apply to it.
func (s *Store) SetDeclaredCapabilities(decl map[string][]CapSignal) {
	s.capMu.Lock()
	defer s.capMu.Unlock()
	m := make(map[string][]CapSignal, len(decl))
	for id, sigs := range decl {
		cp := make([]CapSignal, len(sigs))
		copy(cp, sigs)
		m[id] = cp
	}
	s.declared = m
}

// StationCapReport is the detector's per-station capability read model (docs/INTEGRITY.md §6):
// how recently the station produced any nav frame (is it otherwise alive?), the observed
// signal set with recency, and the declared set to check against. Built by
// FeedCapabilityReports for every station that has observed or declared capabilities.
type StationCapReport struct {
	ID              string
	StationLastSeen int64
	Observed        []StationCapability
	Declared        []CapSignal
}

// FeedCapabilityReports builds the per-station capability read model for the plausibility
// detector: the union of stations with an observed fingerprint and stations with only a
// declared set (a node that has produced nothing yet is still checkable for a never-delivered
// declared signal). now is accepted for signature symmetry with the other Feed* methods.
func (s *Store) FeedCapabilityReports(now time.Time) map[string]StationCapReport {
	observed := s.FeedStationCapabilities(now)
	s.capMu.Lock()
	defer s.capMu.Unlock()
	out := make(map[string]StationCapReport, len(s.caps))
	for id, st := range s.caps {
		out[id] = StationCapReport{
			ID:              id,
			StationLastSeen: st.lastSeen.Unix(),
			Observed:        observed[id],
			Declared:        s.declared[id],
		}
	}
	for id, decl := range s.declared {
		if _, ok := out[id]; ok {
			continue
		}
		out[id] = StationCapReport{ID: id, Declared: decl} // declared but nothing observed yet
	}
	return out
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
