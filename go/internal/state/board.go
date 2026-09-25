package state

import (
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"time"
)

// BoardSample separates receipt time from a feeder wall clock. A nil SampleTime
// means no accepted wall-clock stamp; uptime still orders samples within a boot.
//
// HardwareTrust is what the collector verified from the hardware evidence of the
// session that delivered the sample (docs/COMMISSIONING.md). It sits beside
// Details, never inside it: Details — including update.trust_profile — is the
// device's own account, and this is the only field here that is not.
type BoardSample struct {
	ReceivedAt    time.Time              `json:"received_at"`
	SampleTime    *time.Time             `json:"sample_time"`
	Session       string                 `json:"session"`
	Sequence      uint64                 `json:"sequence"`
	HardwareTrust identity.HardwareTrust `json:"hardware_trust"`
	Details       ingest.ObserverDetails `json:"details"`
}
type BoardEventContext struct {
	Before   *BoardSample `json:"before,omitempty"`
	Snapshot BoardSample  `json:"snapshot"`
}
type StationBoard struct {
	Latest           *BoardSample       `json:"latest,omitempty"`
	Stale            bool               `json:"stale"`
	LastInterference *BoardEventContext `json:"last_interference,omitempty"`
	Timing           *BoardSample       `json:"timing,omitempty"`
	Update           *BoardSample       `json:"update,omitempty"`
	UpdateStale      bool               `json:"update_stale"`
	TimingStale      bool               `json:"timing_stale"`
}
type boardStation struct {
	latest *BoardSample
	last   BoardSample // ordering across independently paced board/timing records
	update *BoardSample
	timing *BoardSample
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
		if f.Session == old.last.Session &&
			((f.HasSeq && f.Seq <= old.last.Sequence) || f.Details.UptimeMS < old.last.Details.UptimeMS) {
			return
		}
		if f.LocalRecv().Before(old.last.ReceivedAt) {
			return
		}
	}
	// Authenticated station identifiers are bounded independently of report size.
	if old == nil && len(s.boards) >= 10000 {
		return
	}
	sample := BoardSample{ReceivedAt: f.LocalRecv(), Session: f.Session, Sequence: f.Seq, Details: *f.Details,
		HardwareTrust: f.Observer.HardwareTrust}
	if sample.HardwareTrust == "" { // dial and programmatic frames carry no session evidence
		sample.HardwareTrust = identity.HardwareTrustNone
	}
	if f.BoardSampleStamped {
		stamp := f.Recv
		sample.SampleTime = &stamp
	}
	if old == nil {
		old = &boardStation{}
		s.boards[f.Source] = old
	}
	if old.last.Session != f.Session {
		old.latest = nil
		old.timing = nil
		old.update = nil
		old.event = nil
	}
	old.last = sample
	if f.Details.Update != nil {
		update := sample
		old.update = &update
	}
	if f.Details.Timing != nil {
		timing := sample
		old.timing = &timing
		if f.Details.Environment == nil && f.Details.Heater == nil && f.Details.RTC == nil && f.Details.ATECC == nil && f.Details.EEPROM == nil && f.Details.Resources == nil && f.Details.Receiver == nil && f.Details.Firmware == "" {
			return
		}
	}
	if f.Details.EventCount != 0 && (old.latest == nil || old.latest.Details.EventCount != f.Details.EventCount) {
		event := &BoardEventContext{Snapshot: sample}
		if old.latest != nil && old.latest.Session == f.Session && !old.latest.ReceivedAt.IsZero() {
			previous := *old.latest
			event.Before = &previous
		}
		old.event = event
	}
	old.latest = &sample
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
		stale := func(sample *BoardSample, after time.Duration) bool {
			return sample == nil || now.Sub(sample.ReceivedAt) > after || (sample.SampleTime != nil && now.Sub(*sample.SampleTime) > after)
		}
		out[id] = StationBoard{Latest: st.latest, Stale: stale(st.latest, 11*time.Minute), LastInterference: st.event,
			Update: st.update, UpdateStale: stale(st.update, 5*time.Second), Timing: st.timing, TimingStale: stale(st.timing, 5*time.Second)}
	}
	return out
}
