package state

import (
	"github.com/ptudor/navlistener/internal/ingest"
	"time"
)

// BoardSample separates receipt time from a feeder wall clock. A nil SampleTime
// means no accepted wall-clock stamp; uptime still orders samples within a boot.
type BoardSample struct {
	ReceivedAt time.Time              `json:"received_at"`
	SampleTime *time.Time             `json:"sample_time"`
	Session    string                 `json:"session"`
	Sequence   uint64                 `json:"sequence"`
	Details    ingest.ObserverDetails `json:"details"`
}
type BoardEventContext struct {
	Before   *BoardSample `json:"before,omitempty"`
	Snapshot BoardSample  `json:"snapshot"`
}
type StationBoard struct {
	Latest           BoardSample        `json:"latest"`
	Stale            bool               `json:"stale"`
	LastInterference *BoardEventContext `json:"last_interference,omitempty"`
}
type boardStation struct {
	latest BoardSample
	event  *BoardEventContext
}

func (s *Store) applyBoard(f *ingest.RawFrame) {
	if f.Source == "" {
		return
	}
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	old := s.boards[f.Source]
	if old != nil {
		if f.Session == old.latest.Session &&
			((f.HasSeq && f.Seq <= old.latest.Sequence) || f.Details.UptimeMS < old.latest.Details.UptimeMS) {
			return
		}
		if f.LocalRecv().Before(old.latest.ReceivedAt) {
			return
		}
	}
	// Authenticated station identifiers are bounded independently of report size.
	if old == nil && len(s.boards) >= 10000 {
		return
	}
	sample := BoardSample{ReceivedAt: f.LocalRecv(), Session: f.Session, Sequence: f.Seq, Details: *f.Details}
	if f.BoardSampleStamped {
		stamp := f.Recv
		sample.SampleTime = &stamp
	}
	if old == nil {
		old = &boardStation{}
		s.boards[f.Source] = old
	}
	if f.Details.EventCount != 0 && (old.latest.Session != f.Session || old.latest.Details.EventCount != f.Details.EventCount) {
		event := &BoardEventContext{Snapshot: sample}
		if old.latest.Session == f.Session && !old.latest.ReceivedAt.IsZero() {
			previous := old.latest
			event.Before = &previous
		}
		old.event = event
	} else if old.latest.Session != f.Session {
		old.event = nil
	}
	old.latest = sample
}

// FeedStationBoards returns private board context. It does not influence GNSS
// liveness, satellite counts, constellation confidence, or detector thresholds.
// These are bounded live snapshots, with the preceding report retained for the
// latest receiver interference transition; they are not a durable archive.
func (s *Store) FeedStationBoards(now time.Time) map[string]StationBoard {
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	out := make(map[string]StationBoard, len(s.boards))
	for id, st := range s.boards {
		stale := now.Sub(st.latest.ReceivedAt) > 11*time.Minute
		if st.latest.SampleTime != nil && now.Sub(*st.latest.SampleTime) > 11*time.Minute {
			stale = true
		}
		out[id] = StationBoard{Latest: st.latest, Stale: stale, LastInterference: st.event}
	}
	return out
}
