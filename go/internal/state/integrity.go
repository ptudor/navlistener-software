package state

import (
	"fmt"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
)

// Station integrity assurance (docs/proposals/STATION-ASSURANCE.md). Each station's
// receiver solutions, clock and status, board pulse timing, NAV-SAT fits and MON-RF
// reports feed one integrity.Station, which filters, holds and fuses its checks.
// FeedStationIntegrity serves the assessments to the observers feed and the
// detector. All of it is guarded by rfMu, with the other station-scoped state.

// IntegrityConfig is a validated operating profile and the configured
// installations, by station id. It is immutable, so every store can share one.
type IntegrityConfig struct {
	profile  integrity.Profile
	stations map[string]integrity.StationProfile
}

// NewIntegrityConfig validates a profile and installations into a shareable config.
func NewIntegrityConfig(profile integrity.Profile, stations map[string]integrity.StationProfile) (*IntegrityConfig, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	copied := make(map[string]integrity.StationProfile, len(stations))
	for id, sp := range stations {
		if err := sp.Validate(); err != nil {
			return nil, fmt.Errorf("integrity station %s: %w", id, err)
		}
		if sp.Position != nil {
			pos := *sp.Position
			sp.Position = &pos
		}
		copied[id] = sp
	}
	return &IntegrityConfig{profile: profile, stations: copied}, nil
}

const (
	// integrityEvictAfter drops a station's checks after this long without input,
	// bounding memory for stations that have gone away; a returning station
	// starts its checks afresh.
	integrityEvictAfter = time.Hour
	// maxIntegrityStations bounds evaluators like the board map; authenticated
	// station identifiers are bounded independently of report size.
	maxIntegrityStations = 10000
	// statusPreferredFor is how long an epoch-rate receiver status block
	// supersedes the low-rate spoofing state in board reports.
	statusPreferredFor = 5 * time.Minute
)

type integrityStation struct {
	eval       *integrity.Station
	lastInput  time.Time
	lastStatus time.Time
}

// StationConfigHash is the configuration hash a station's assessments carry under
// this configuration (integrity.ConfigHash), for tools that report it.
func (c *IntegrityConfig) StationConfigHash(id string) (string, error) {
	eval, err := integrity.NewStation(c.profile, c.stations[id])
	if err != nil {
		return "", err
	}
	return eval.ConfigHash(), nil
}

// SetIntegrity installs an integrity configuration. Per-station state evaluated
// under any previous configuration is discarded. A store with no configuration
// evaluates every station with the default profile and no installation profile.
func (s *Store) SetIntegrity(cfg *IntegrityConfig) {
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	s.integrityCfg = cfg
	s.integrity = make(map[string]*integrityStation)
}

// integrityFor returns a station's evaluator, creating it on first input. It returns
// nil at the station ceiling. The caller holds rfMu.
func (s *Store) integrityFor(id string, at time.Time) *integrityStation {
	st := s.integrity[id]
	if st == nil {
		if len(s.integrity) >= maxIntegrityStations {
			return nil
		}
		profile, stationProfile := integrity.DefaultProfile(), integrity.StationProfile{}
		if s.integrityCfg != nil {
			profile, stationProfile = s.integrityCfg.profile, s.integrityCfg.stations[id]
		}
		eval, err := integrity.NewStation(profile, stationProfile)
		if err != nil {
			// NewIntegrityConfig validated both profiles, and the defaults are
			// covered by tests: this is unreachable without a programming error.
			panic(fmt.Sprintf("state: integrity station %s: %v", id, err))
		}
		st = &integrityStation{eval: eval}
		s.integrity[id] = st
	}
	if at.After(st.lastInput) {
		st.lastInput = at
	}
	return st
}

// applySolution folds one receiver-solution epoch: position and UTC first, so the
// clock and status that follow see this epoch's fix state.
func (s *Store) applySolution(f *ingest.RawFrame) {
	if f.Source == "" {
		return
	}
	recv := f.LocalRecv()
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	st := s.integrityFor(f.Source, recv)
	if st == nil {
		return
	}
	sol := f.Solution
	if p := sol.PVT; p != nil {
		in := integrity.Solution{
			Received: recv, TOW: p.TOWMS,
			FixType: int(p.FixType), FixOK: p.FixFlags&ingest.FixFlagOK != 0,
			InvalidLLH: p.PosFlags&ingest.PosFlagInvalidLLH != 0, NumSV: int(p.NumSV),
			LatDeg: float64(p.LatE7) / 1e7, LonDeg: float64(p.LonE7) / 1e7, HeightM: float64(p.HeightMM) / 1e3,
			HAccM: float64(p.HAccMM) / 1e3, VAccM: float64(p.VAccMM) / 1e3,
			VelN: float64(p.VelNMMS) / 1e3, VelE: float64(p.VelEMMS) / 1e3, VelD: float64(p.VelDMMS) / 1e3,
			SAccMPS: float64(p.SAccMMS) / 1e3,
		}
		in.UTC, in.UTCValid = p.UTC()
		// An unstamped push frame's arrival time includes any spool backlog and
		// is no reference for the receiver's UTC (RawFrame.WallClockStamp).
		if stamp, ok := f.WallClockStamp(); ok {
			in.HostStamp = stamp
		}
		st.eval.ApplySolution(in)
	}
	if c := sol.Clock; c != nil {
		st.eval.ApplyClock(integrity.ClockSample{
			Received: recv, TOW: c.TOWMS, BiasNs: float64(c.BiasNS), DriftNsPerS: float64(c.DriftNSS),
			TAccNs: float64(c.TAccNS), FAccPsPerS: float64(c.FAccPSS),
		})
	}
	if status := sol.Status; status != nil {
		st.lastStatus = recv
		st.eval.ApplyStatus(integrity.ReceiverStatus{
			Received: recv, SpoofState: int(status.SpoofState), SinceStartMS: status.SinceStart, HaveStart: true,
		})
	}
}

// integrityRF folds a MON-RF or NAV-SAT update into the station's checks after
// applyRF has refreshed the baselines and fits. The caller holds rfMu.
func (s *Store) integrityRF(st *rfStation, f *ingest.RawFrame, recv time.Time) {
	is := s.integrityFor(st.id, recv)
	if is == nil {
		return
	}
	if len(f.RF.Bands) > 0 {
		sample := integrity.RFSample{Received: recv, Cn0Drop: s.cn0DropCorroboration(st, recv)}
		for _, b := range st.bands {
			if recv.Sub(b.lastSeen) > rfStaleAfter {
				continue
			}
			band := integrity.RFBand{Block: b.block, CWSuppress: b.cwSuppress, JamState: b.jamState, AntStatus: b.antStatus}
			if dep, ok := b.departure(); ok {
				band.Departure = &dep
			}
			sample.Bands = append(sample.Bands, band)
		}
		is.eval.ApplyRF(sample)
	}
	if len(f.RF.Sats) > 0 {
		fit := integrity.Cn0Fit{Received: recv, Mean: st.cn0Mean, ResidVar: st.cn0Resid, NumSats: st.cn0NumSats,
			PerConstellation: make(map[int]integrity.Cn0Group, len(st.cn0ByGNSS))}
		for id, g := range st.cn0ByGNSS {
			fit.PerConstellation[id] = integrity.Cn0Group{Mean: g.Mean, ResidVar: g.Resid, NumSats: g.NumSats}
		}
		is.eval.ApplyCn0(fit)
		snap := integrity.Cn0Snapshot{Received: recv, Signals: make([]integrity.Cn0Signal, len(f.RF.Sats))}
		for i, sat := range f.RF.Sats {
			snap.Signals[i] = integrity.Cn0Signal{GnssID: sat.GnssID, SvID: sat.SvID, Cn0: sat.Cn0, Used: sat.Used}
		}
		is.eval.ApplyCn0Snapshot(snap)
		if served, _ := is.eval.Served(integrity.CheckCn0Drop, recv); degradedState(served) {
			st.cn0DropAt = recv
		}
	}
}

// degradedState reports an inconsistent or unassured served state.
func degradedState(s integrity.State) bool {
	return s == integrity.Inconsistent || s == integrity.Unassured
}

// integrityBoard folds an accepted board report: its pulse timing, and the
// receiver's spoofing state when no epoch-rate status block is arriving. The caller
// holds rfMu.
func (s *Store) integrityBoard(f *ingest.RawFrame, recv time.Time) {
	d := f.Details
	if d.Timing == nil && d.Receiver == nil {
		return
	}
	st := s.integrityFor(f.Source, recv)
	if st == nil {
		return
	}
	if t := d.Timing; t != nil {
		st.eval.ApplyTiming(integrity.TimingSample{
			Received: recv, UptimeMS: d.UptimeMS, PhaseNs: t.PhaseNS,
			RTCRunning: t.RTCState == "enabled_1hz", RTCTrim: t.RTCTrim,
		})
	}
	if r := d.Receiver; r != nil && recv.Sub(st.lastStatus) > statusPreferredFor {
		st.eval.ApplyStatus(integrity.ReceiverStatus{Received: recv, SpoofState: int(r.Spoofing)})
	}
}

// FeedStationIntegrity returns each station's assessment as of now, evicting
// stations without input for integrityEvictAfter.
func (s *Store) FeedStationIntegrity(now time.Time) map[string]integrity.Assessment {
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	out := make(map[string]integrity.Assessment, len(s.integrity))
	for id, st := range s.integrity {
		if now.Sub(st.lastInput) > integrityEvictAfter {
			delete(s.integrity, id)
			continue
		}
		out[id] = st.eval.Assess(now)
	}
	return out
}
