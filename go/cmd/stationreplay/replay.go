package main

import (
	"maps"
	"slices"
	"time"

	"github.com/ptudor/navlistener/internal/detect"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
	"github.com/ptudor/navlistener/internal/state"
)

// record is one line of the replay timeline: a change in a station's assessment, or
// a confirmed station event.
type record struct {
	Time              time.Time                  `json:"time"`
	Kind              string                     `json:"kind"`
	Station           string                     `json:"station"`
	State             string                     `json:"state,omitempty"`
	Score             *float64                   `json:"score,omitempty"`
	UnassuredDomains  []integrity.Domain         `json:"unassured_domains,omitempty"`
	SpoofingIndicated *bool                      `json:"spoofing_indicated,omitempty"`
	Checks            map[string]integrity.State `json:"checks,omitempty"`
	ConfigHash        string                     `json:"config_hash,omitempty"`
	Type              string                     `json:"type,omitempty"`
	OldValue          string                     `json:"old_value,omitempty"`
	NewValue          string                     `json:"new_value,omitempty"`
	Severity          *int                       `json:"severity,omitempty"`
	Message           string                     `json:"message,omitempty"`
}

// replayer applies stored station inputs to a fresh live state in collector receipt
// order and runs the station detectors on the collector's cadence, so the timeline is
// what the live collector would have produced from the same inputs.
type replayer struct {
	live     *state.Store
	det      *detect.Detector
	interval time.Duration
	next     time.Time
	started  bool
	out      func(record)
	last     map[string]string // station -> the assessment last emitted, as a comparable key
	events   []detect.Event
	inputs   int
}

func newReplayer(cfg *state.IntegrityConfig, interval time.Duration, out func(record)) *replayer {
	live := state.New(1)
	live.SetIntegrity(cfg)
	return &replayer{live: live, det: detect.New(0), interval: interval, out: out, last: map[string]string{}}
}

// apply runs every detector tick due before the frame's receipt, then applies it.
func (r *replayer) apply(f *ingest.RawFrame) {
	r.advance(f.LocalRecv())
	r.live.Apply(f)
	r.inputs++
}

// advance runs the detector ticks due at or before to. The first tick falls one
// interval after the first input, as the live ticker's phase is not recorded.
func (r *replayer) advance(to time.Time) {
	if !r.started {
		r.started, r.next = true, to.Add(r.interval)
		return
	}
	for !r.next.After(to) {
		r.tick(r.next)
		r.next = r.next.Add(r.interval)
	}
}

// tick evaluates every station at now, in the live detector's order.
func (r *replayer) tick(now time.Time) {
	assessments := r.live.FeedStationIntegrity(now)
	for _, id := range slices.Sorted(maps.Keys(assessments)) {
		r.emitAssessment(now, id, assessments[id])
	}
	events := r.det.TickStations(now, r.live.FeedStationRF(now))
	events = append(events, r.det.TickIntegrity(now, assessments)...)
	for _, e := range events {
		sev := e.Severity
		r.out(record{Time: e.Time, Kind: "event", Station: e.SV, Type: e.Type, OldValue: e.OldValue,
			NewValue: e.NewValue, Severity: &sev, Message: e.Message})
	}
	r.events = append(r.events, events...)
}

func (r *replayer) emitAssessment(now time.Time, id string, a integrity.Assessment) {
	checks := make(map[string]integrity.State, len(a.Checks))
	key := string(a.State)
	for _, c := range a.Checks {
		checks[c.Check] = c.State
		key += "|" + c.Check + "=" + string(c.State)
	}
	if r.last[id] == key {
		return
	}
	r.last[id] = key
	spoofing := a.SpoofingIndicated
	r.out(record{Time: now, Kind: "assessment", Station: id, State: string(a.State), Score: a.Score,
		UnassuredDomains: a.UnassuredDomains, SpoofingIndicated: &spoofing, Checks: checks, ConfigHash: a.ConfigHash})
}

// finish runs the ticks due through until.
func (r *replayer) finish(until time.Time) {
	if r.started {
		r.advance(until)
	}
}
