package integrity

import (
	"fmt"
	"time"
)

// checkInfos is every check this build knows, by name.
var checkInfos = map[string]Info{}

func init() {
	for _, info := range []Info{
		staticPositionInfo, stationaryVelocityInfo, motionBoundInfo, positionVelocityInfo,
		clockBiasDriftInfo, clockDriftRateInfo, utcOffsetInfo, ppsRTCPhaseInfo,
		cn0UniformityInfo, agcInfo, receiverSpoofingInfo,
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

	pos   *positionChecks
	clock clockChecks
	pps   ppsRTCPhase

	haveFix    bool
	lastFixOK  bool
	lastFixAt  time.Time
	lastStart  uint32
	haveStatus bool
}

// NewStation builds a station evaluator. Both profiles are validated.
func NewStation(p Profile, station StationProfile) (*Station, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if err := station.Validate(); err != nil {
		return nil, err
	}
	s := &Station{profile: p, station: station, trackers: map[string]*tracker{}, pos: newPositionChecks(station)}
	infos := append(s.pos.infos(),
		clockBiasDriftInfo, clockDriftRateInfo, utcOffsetInfo, ppsRTCPhaseInfo,
		cn0UniformityInfo, agcInfo, receiverSpoofingInfo)
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
// duplicate or out-of-order epoch is ignored.
func (s *Station) ApplySolution(sol Solution) {
	if !s.pos.accept(sol) {
		return
	}
	s.haveFix, s.lastFixOK, s.lastFixAt = true, sol.FixOK && !sol.InvalidLLH, sol.Received
	for name, v := range s.pos.evaluate(sol, s.profile) {
		s.update(name, v, sol.Received)
	}
	s.update(CheckUTCOffset, utcOffset(sol, s.profile.UTCOffset), sol.Received)
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
// restart, which breaks the clock history.
func (s *Station) ApplyStatus(st ReceiverStatus) {
	if st.HaveStart {
		if s.haveStatus && st.SinceStartMS < s.lastStart {
			s.clock.reset()
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

// ApplyRF evaluates the AGC check for one front-end report.
func (s *Station) ApplyRF(rf RFSample) {
	s.update(CheckAGC, agc(rf, s.profile.AGC), rf.Received)
}

// Assessment is a station's served assurance: the fused state, every check's result,
// and what produced them. It carries no coordinates.
type Assessment struct {
	Fusion
	EvaluatedAt int64    `json:"evaluated_at"`
	Engine      int      `json:"engine_version"`
	ConfigHash  string   `json:"config_hash"`
	Mode        Mode     `json:"mode,omitempty"`
	Surveyed    bool     `json:"surveyed_position"`
	MaxSpeedMPS float64  `json:"max_speed_mps,omitempty"`
	Checks      []Result `json:"checks"`
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
		Fusion:      Fuse(results, s.profile.Weights),
		EvaluatedAt: now.Unix(),
		Engine:      EngineVersion,
		ConfigHash:  s.hash,
		Mode:        s.station.Mode,
		Surveyed:    s.station.Position != nil,
		MaxSpeedMPS: s.station.MaxSpeedMPS,
		Checks:      results,
	}
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
	return fmt.Sprintf("mode=%s surveyed=%t max_speed_mps=%g", mode, s.Position != nil, s.MaxSpeedMPS)
}
