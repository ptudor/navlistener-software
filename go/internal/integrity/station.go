package integrity

import (
	"fmt"
	"time"
)

// checkInfos is every check this build knows, by name.
var checkInfos = map[string]Info{}

func init() {
	for _, info := range []Info{
		staticPositionInfo, stationaryVelocityInfo, motionBoundInfo, positionVelocityInfo, motionKinematicsInfo,
		clockBiasDriftInfo, clockDriftRateInfo, utcOffsetInfo, ppsRTCPhaseInfo,
		cn0UniformityInfo, cn0DropInfo, agcInfo, receiverSpoofingInfo, baselineInfo,
	} {
		checkInfos[info.Name] = info
	}
}

// fixFreshness bounds how old the latest solution may be (collector time) when a
// clock or pulse input is gated on the receiver having a valid fix.
const fixFreshness = 5 * time.Second

// Station evaluates one station's checks from its inputs. It is not safe for
// concurrent use; the owner serializes calls.
type Station struct {
	profile  Profile
	station  StationProfile
	hash     string
	order    []string
	trackers map[string]*tracker

	pos     *positionChecks
	clock   clockChecks
	pps     ppsRTCPhase
	cn0Drop cn0DropCheck

	haveFix    bool
	lastFixOK  bool
	lastFixAt  time.Time
	lastStart  uint32
	haveStatus bool
	survey     *Survey
	segment    uint64
	segments   []SegmentRecord
}

// NewStation builds a station evaluator. Both profiles are validated.
func NewStation(p Profile, station StationProfile) (*Station, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := station.Validate(); err != nil {
		return nil, err
	}
	station = station.Clone()
	s := &Station{profile: p, station: station, trackers: map[string]*tracker{}, pos: newPositionChecks(station)}
	if station.Survey != nil {
		s.survey, _ = NewSurvey(*station.Survey)
	}
	infos := s.pos.infos()
	if station.Baseline != nil {
		infos = append(infos, baselineInfo)
	}
	infos = append(infos,
		clockBiasDriftInfo, clockDriftRateInfo, utcOffsetInfo, ppsRTCPhaseInfo,
		cn0UniformityInfo, cn0DropInfo, agcInfo, receiverSpoofingInfo)
	versions := make(map[string]int, len(infos))
	for _, info := range infos {
		s.order = append(s.order, info.Name)
		s.trackers[info.Name] = newTracker(info)
		versions[info.Name] = info.Version
	}
	s.hash = ConfigHash(p, station, versions)
	return s, nil
}

// ConfigHash is the configuration hash every assessment from this station carries.
func (s *Station) ConfigHash() string { return s.hash }

func (s *Station) update(name string, v Verdict, at time.Time) {
	if t, ok := s.trackers[name]; ok {
		t.update(v, at, s.profile.Filter)
	}
}

// fixOK reports whether the receiver had a valid fix in its latest solution, if that
// solution is fresh relative to at. ok is false when no solution has been seen.
func (s *Station) fixOK(at time.Time) (fix, ok bool) {
	if !s.haveFix {
		return false, false
	}
	return s.lastFixOK && at.Sub(s.lastFixAt) <= fixFreshness && at.Sub(s.lastFixAt) >= -fixFreshness, true
}

// ApplySolution evaluates the position and UTC checks for one solution epoch. A
// duplicate or out-of-order epoch is ignored, and false is returned for it.
func (s *Station) ApplySolution(sol Solution) bool {
	if !s.pos.accept(sol) {
		return false
	}
	if s.pos.segmented {
		s.startSegment(sol.Received, ReasonSegmentGap)
	}
	if s.survey != nil {
		s.survey.Apply(sol)
	}
	s.haveFix, s.lastFixOK, s.lastFixAt = true, sol.FixOK && !sol.InvalidLLH, sol.Received
	for name, v := range s.pos.evaluate(sol, s.profile) {
		s.update(name, v, sol.Received)
	}
	s.update(CheckUTCOffset, utcOffset(sol, s.profile.UTCOffset), sol.Received)
	return true
}

// ApplyBaseline evaluates the baseline check for one epoch of this station's solution
// and its partner's solution for the same epoch, evaluated at the later of their
// arrivals. A station without a configured partner ignores it.
func (s *Station) ApplyBaseline(own, partner Solution, at time.Time) {
	if s.station.Baseline == nil {
		return
	}
	s.update(CheckBaseline, baseline(own, partner, s.station.Baseline.DistanceM, s.profile.Baseline), at)
}

// ApplyClock evaluates the receiver-clock checks for one clock epoch. A station that
// reports no solution at all is evaluated without the fix gate.
func (s *Station) ApplyClock(c ClockSample) {
	fix, known := s.fixOK(c.Received)
	verdicts := s.clock.evaluate(c, fix || !known, s.profile)
	for _, name := range []string{CheckClockBiasDrift, CheckClockDriftRate} {
		if v, ok := verdicts[name]; ok {
			s.update(name, v, c.Received)
		}
	}
}

// ApplyStatus folds the receiver's own status: its spoofing indication, and a
// restart, which breaks the clock history and the receiver-time continuity the
// position and clock epochs are ordered by — the next epoch re-anchors instead of
// being refused as out of order behind a pre-restart time.
func (s *Station) ApplyStatus(st ReceiverStatus) {
	if st.HaveStart {
		if s.haveStatus && st.SinceStartMS < s.lastStart {
			s.clock.reset()
			s.clock.started = false
			s.pos.resync()
			s.pos.started = false
		}
		s.lastStart, s.haveStatus = st.SinceStartMS, true
	}
	s.update(CheckReceiverSpoofing, receiverSpoofing(st.SpoofState), st.Received)
}

// ApplyTiming evaluates the PPS-vs-RTC phase check for one timing sample. Without a
// fresh valid fix the GNSS pulse may free-run, so the check is unavailable.
func (s *Station) ApplyTiming(t TimingSample) {
	fix, known := s.fixOK(t.Received)
	s.update(CheckPPSRTCPhase, s.pps.evaluate(t, fix && known, s.profile.PPSRTCPhase), t.Received)
}

// ApplyCn0 evaluates the C/N₀ uniformity check for one NAV-SAT fit.
func (s *Station) ApplyCn0(f Cn0Fit) {
	s.update(CheckCn0Uniformity, cn0Uniformity(f, s.profile.Cn0Uniformity), f.Received)
}

// ApplyCn0Snapshot evaluates the simultaneous C/N₀ drop check for one NAV-SAT epoch.
// A duplicate or out-of-order epoch is ignored.
func (s *Station) ApplyCn0Snapshot(snap Cn0Snapshot) {
	if v, ok := s.cn0Drop.evaluate(snap, s.profile.Cn0Drop); ok {
		s.update(CheckCn0Drop, v, snap.Received)
	}
}

// ApplyRF evaluates the AGC check for one front-end report.
func (s *Station) ApplyRF(rf RFSample) {
	s.update(CheckAGC, agc(rf, s.profile.AGC), rf.Received)
}

// Assessment is a station's served assurance: the fused state, every check's result,
// and what produced them. It carries no coordinates.
type Assessment struct {
	Fusion
	EvaluatedAt  int64           `json:"evaluated_at"`
	Engine       int             `json:"engine_version"`
	ConfigHash   string          `json:"config_hash"`
	Mode         Mode            `json:"mode,omitempty"`
	Surveyed     bool            `json:"surveyed_position"`
	MaxSpeedMPS  float64         `json:"max_speed_mps,omitempty"`
	Checks       []Result        `json:"checks"`
	Installation Installation    `json:"installation,omitempty"`
	Motion       MotionPolicy    `json:"motion"`
	Fixed        FixedPolicy     `json:"fixed"`
	Segment      uint64          `json:"segment,omitempty"`
	SegmentID    string          `json:"segment_id,omitempty"`
	Segments     []SegmentRecord `json:"segments,omitempty"`
	SurveyReady  bool            `json:"survey_candidate_ready,omitempty"`
}

// SegmentRecord keeps the preceding alarm results, without coordinates. Raw
// inputs and event history retain their normal persistence and retention rules.
type SegmentRecord struct {
	Previous uint64    `json:"previous"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
	Checks   []Result  `json:"checks"`
}

func (s *Station) startSegment(at time.Time, reason string) {
	r := SegmentRecord{Previous: s.segment, At: at, Reason: reason}
	for _, name := range []string{CheckMotionBound, CheckMotionKinematics, CheckPositionVelocity, CheckClockBiasDrift, CheckClockDriftRate, CheckPPSRTCPhase} {
		if t := s.trackers[name]; t != nil {
			r.Checks = append(r.Checks, t.result())
			s.trackers[name] = newTracker(t.info)
			s.update(name, Verdict{State: Unavailable, Reasons: []string{reason}}, at)
		}
	}
	s.segments = append(s.segments, r)
	if len(s.segments) > 8 {
		s.segments = s.segments[len(s.segments)-8:]
	}
	s.segment++
	s.clock = clockChecks{}
	s.pps = ppsRTCPhase{}
	s.haveFix = false
}

// Relocate is an explicit operator action, available only to portable test
// installations. It does not change identity, stored observations or references.
func (s *Station) Relocate(at time.Time, reason string) error {
	if s.station.Installation != PortableTest || reason == "" {
		return fmt.Errorf("integrity: relocation requires portable_test and a maintenance reason")
	}
	s.pos.resync()
	s.pos.started = false
	s.pos.haveUsable = false
	s.startSegment(at, ReasonRelocation+": "+reason)
	return nil
}

// SurveyCandidate returns a copy for an operator to review and explicitly pin.
// Candidates are never folded into the configured surveyed position.
func (s *Station) SurveyCandidate() *SurveyCandidate { return s.survey.Candidate() }

// Served returns one check's served state as of now, after ageing out stale input.
// ok is false for a check this station does not run.
func (s *Station) Served(name string, now time.Time) (State, bool) {
	t, ok := s.trackers[name]
	if !ok {
		return Unavailable, false
	}
	t.expire(now, s.profile.StaleAfter, s.profile.Filter)
	return t.served, true
}

// Fused returns the fusion of the checks' served states exactly as they stand: the
// read model a vote weight or a neighbour gate needs, without the per-check metric
// and reason clones Assess makes and without ageing anything out. It never mutates
// the station, so a read path (a per-request feed render, a per-frame neighbour
// scan) cannot drive check transitions with its own clock; staleness is applied by
// Assess and Served on the owner's evaluation cadence.
func (s *Station) Fused() Fusion {
	results := make([]Result, 0, len(s.order))
	for _, name := range s.order {
		t := s.trackers[name]
		results = append(results, Result{Check: name, Domain: t.info.Domain, State: t.served, lowerOnly: t.info.LowerOnly})
	}
	return Fuse(results, s.profile.Weights)
}

// LastEvaluation returns one check's latest raw evaluation — its state before the
// filter and hysteresis — and when it was made. ok is false for a check this
// station does not run or has never evaluated. It does not mutate the station.
// Where the served state is deliberately held (a recovery hold, or until staleness
// ages it out), this is the instant the evidence was actually seen.
func (s *Station) LastEvaluation(name string) (state State, at time.Time, ok bool) {
	t, found := s.trackers[name]
	if !found || !t.seen {
		return Unavailable, time.Time{}, false
	}
	return t.last.State, t.lastAt, true
}

// Assess ages out stale checks as of now and returns the station's assessment.
func (s *Station) Assess(now time.Time) Assessment {
	results := make([]Result, 0, len(s.order))
	for _, name := range s.order {
		t := s.trackers[name]
		t.expire(now, s.profile.StaleAfter, s.profile.Filter)
		results = append(results, t.result())
	}
	return Assessment{
		Fusion:       Fuse(results, s.profile.Weights),
		EvaluatedAt:  now.Unix(),
		Engine:       EngineVersion,
		ConfigHash:   s.hash,
		Mode:         s.station.Mode,
		Surveyed:     s.station.Position != nil,
		MaxSpeedMPS:  s.station.MaxSpeedMPS,
		Checks:       results,
		Installation: s.station.Installation, Motion: s.station.Motion, Fixed: s.station.Fixed,
		Segment: s.segment, SegmentID: s.station.SegmentID, Segments: s.segmentHistory(), SurveyReady: s.SurveyCandidate() != nil,
	}
}

func (s *Station) segmentHistory() []SegmentRecord {
	out := append([]SegmentRecord(nil), s.segments...)
	for i := range out {
		out[i].Checks = append([]Result(nil), out[i].Checks...)
		for j := range out[i].Checks {
			r := &out[i].Checks[j]
			r.Reasons = append([]string(nil), r.Reasons...)
			r.Metrics = cloneMetrics(r.Metrics)
			r.Thresholds = cloneMetrics(r.Thresholds)
		}
	}
	return out
}

func cloneMetrics(in map[string]float64) map[string]float64 {
	if in == nil {
		return nil
	}
	out := make(map[string]float64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// WiredPhysicsDomains is how many physics domains have at least one check in this
// build: the most independent domains the spoofing rule can ever count.
func WiredPhysicsDomains() int {
	domains := map[Domain]bool{}
	for _, info := range checkInfos {
		if info.Domain.Physics() {
			domains[info.Domain] = true
		}
	}
	return len(domains)
}

// CheckVersions returns every known check's version, for documentation and tooling.
func CheckVersions() map[string]int {
	out := make(map[string]int, len(checkInfos))
	for name, info := range checkInfos {
		out[name] = info.Version
	}
	return out
}

// String names the station profile for logs without its coordinates.
func (s StationProfile) String() string {
	mode := string(s.Mode)
	if mode == "" {
		mode = "unknown"
	}
	out := fmt.Sprintf("mode=%s surveyed=%t max_speed_mps=%g", mode, s.Position != nil, s.MaxSpeedMPS)
	if s.Baseline != nil {
		out += fmt.Sprintf(" baseline_partner=%s baseline_m=%g", s.Baseline.Partner, s.Baseline.DistanceM)
	}
	return out
}
