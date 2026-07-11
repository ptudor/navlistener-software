package state

import (
	"sort"
	"time"

	"github.com/ptudor/navlistener/internal/ingest"
)

// PNT-defense Tier-0 (docs/DEFENSE-PNT.md): the collector turns the RF-environment
// telemetry the fleet's receivers already produce (MON-RF/MON-HW AGC + jamming, NAV-SAT
// C/N₀ + elevation) into per-station detection metrics — jamming departures from a
// learned baseline and the C/N₀-vs-elevation spoofing gate. The metrics are computed
// here (like the SV integrity signals); the debounced detector classifies them into
// station-scoped events (internal/detect). Thresholds are observational in v1 — we
// publish the metrics and learn per-station quiet-time baselines; alerting stays
// conservative until real distributions exist (docs/DEFENSE-PNT.md §4).

// rfBaselineRing bounds the per-band quiet-time AGC sample window whose median is the
// station's clear-sky baseline; agcLearnBand is how far a sample may sit from the
// current baseline and still be treated as quiet (so a jammer's departure cannot poison
// the baseline it is measured against — docs/DEFENSE-PNT.md §2).
const (
	rfBaselineRing = 64
	agcLearnBand   = 400 // AGC counts; departures beyond this don't update the baseline
	rfStaleAfter   = 5 * time.Minute
	cn0MinSats     = 5 // fewest tracked SVs before the C/N₀-vs-elevation gate is meaningful
)

// rfBand is one RF path's learned state at a station: the latest receiver numbers plus
// the quiet-time AGC baseline learned as a robust median.
type rfBand struct {
	block                            int
	agc, noise, cwSuppress, jamState int
	antStatus                        int
	baseline                         float64
	haveBaseline                     bool
	quiet                            []int // recent quiet-time AGC samples (median → baseline)
	lastSeen                         time.Time
}

// rfStation is one observer's RF-environment state: per-band front-end state and the
// derived C/N₀-vs-elevation residual (the spoofing gate).
type rfStation struct {
	id       string
	lastSeen time.Time
	bands    map[int]*rfBand

	haveCn0     bool
	cn0Mean     float64
	cn0Resid    float64 // variance of C/N₀ after removing the elevation trend
	cn0NumSats  int
	cn0LastSeen time.Time // last NAV-SAT sample; ages the spoof gate independently of MON-RF
}

// applyRF folds one RF-telemetry sample into the per-station RF state, updating the
// per-band AGC baselines and the C/N₀-vs-elevation residual. Station identity is the
// ingest source (dial) or observer id (push).
func (s *Store) applyRF(f *ingest.RawFrame) {
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	st := s.rf[f.Source]
	if st == nil {
		st = &rfStation{id: f.Source, bands: map[int]*rfBand{}}
		s.rf[f.Source] = st
	}
	st.lastSeen = f.Recv

	for _, b := range f.RF.Bands {
		band := st.bands[b.Block]
		if band == nil {
			band = &rfBand{block: b.Block}
			st.bands[b.Block] = band
		}
		band.agc, band.noise, band.cwSuppress = b.AGC, b.NoiseLevel, b.CWSuppress
		band.jamState, band.antStatus = b.JamState, b.AntStatus
		band.lastSeen = f.Recv
		band.learn(b.AGC)
	}
	if len(f.RF.Sats) > 0 {
		st.cn0Mean, st.cn0Resid, st.cn0NumSats = cn0ElevationResidual(f.RF.Sats)
		st.haveCn0 = st.cn0NumSats >= cn0MinSats
		st.cn0LastSeen = f.Recv
	}
}

// learn folds an AGC sample into the quiet-time baseline. A sample far from the current
// baseline (a jamming departure) is measured but not learned, so the baseline stays the
// clear-sky reference (docs/DEFENSE-PNT.md §2). Before a baseline exists, every sample
// seeds it.
func (b *rfBand) learn(agc int) {
	if b.haveBaseline && absInt(agc-int(b.baseline)) > agcLearnBand {
		return // a departure: don't let it poison its own reference
	}
	b.quiet = append(b.quiet, agc)
	if len(b.quiet) > rfBaselineRing {
		b.quiet = b.quiet[len(b.quiet)-rfBaselineRing:]
	}
	b.baseline = medianInt(b.quiet)
	b.haveBaseline = true
}

// cn0ElevationResidual fits C/N₀ against elevation across the tracked SVs and returns the
// mean C/N₀, the residual variance about that linear trend, and the SV count. A genuine
// sky spreads C/N₀ with elevation and SV-to-SV; a single-transmitter spoofer drives the
// residual variance toward zero at an unnaturally uniform, high C/N₀ (docs/DEFENSE-PNT.md
// §3). Only SVs the receiver is actually tracking (C/N₀ > 0) count.
func cn0ElevationResidual(sats []ingest.SatCN0) (mean, residVar float64, n int) {
	var xs, ys []float64
	for _, s := range sats {
		// UBX-NAV-SAT elevation is valid only in [0,90]; > 90 is the
		// "elevation unknown" sentinel (91 dial-mode, clamped to 90 by the feeder's
		// GNF1 telemetry encode -- both must be excluded here, the one choke point
		// covering both paths), typical for a freshly-acquired SV. Feeding an
		// unknown elevation into the regression as if it were a real data point
		// biases the slope/residual this single-transmitter spoof gate depends on.
		if s.Cn0 <= 0 || s.ElevDeg < 0 || s.ElevDeg > 90 {
			continue // untracked, below-horizon, or elevation-unknown sentinel
		}
		xs = append(xs, float64(s.ElevDeg))
		ys = append(ys, float64(s.Cn0))
	}
	n = len(xs)
	if n == 0 {
		return 0, 0, 0
	}
	var sy float64
	for _, y := range ys {
		sy += y
	}
	mean = sy / float64(n)
	if n < 2 {
		return mean, 0, n
	}
	// Least-squares line C/N₀ = a + b·elev; residual variance is the spread the trend
	// cannot explain — the discriminator that collapses under a flat spoofer.
	var sx, sxx, sxy float64
	for i := range xs {
		sx += xs[i]
		sxx += xs[i] * xs[i]
		sxy += xs[i] * ys[i]
	}
	fn := float64(n)
	denom := fn*sxx - sx*sx
	var a, slope float64
	if denom != 0 {
		slope = (fn*sxy - sx*sy) / denom
		a = (sy - slope*sx) / fn
	} else {
		a = mean // all SVs at one elevation: fall back to the mean model
	}
	var ss float64
	for i := range xs {
		r := ys[i] - (a + slope*xs[i])
		ss += r * r
	}
	return mean, ss / fn, n
}

// StationRF is one observer's RF-defense read model (docs/OUTPUT.md §1.3 / DEFENSE-PNT
// §6). Derived metrics are pointers so an unlearned/absent one is omitted, never a
// sentinel. RFTrust is how much this station's votes are currently down-weighted
// (1 = fully trusted).
type StationRF struct {
	ID       string          `json:"id"`
	LastSeen int64           `json:"last_seen"`
	Bands    []StationRFBand `json:"bands,omitempty"`
	Cn0Mean  *float64        `json:"cn0_mean_db_hz,omitempty"`
	Cn0Resid *float64        `json:"cn0_elev_resid_var,omitempty"`
	NumSats  int             `json:"num_sats"`
	RFTrust  float64         `json:"rf_trust"`
}

// StationRFBand is one RF path's published metrics (docs/DEFENSE-PNT.md §6): the raw
// numbers plus the derived AGC departure from the learned baseline.
type StationRFBand struct {
	Block        int      `json:"block"`
	AGC          int      `json:"agc"`
	AGCDeparture *float64 `json:"agc_departure,omitempty"` // baseline − current; +ve ⇒ gain cut
	CWSuppress   int      `json:"cw_suppress"`
	NoiseLevel   int      `json:"noise_level"`
	JamState     int      `json:"jam_state"`
	AntStatus    int      `json:"ant_status"`
}

// FeedStationRF builds the per-station RF-defense read model as of now, for the
// observers feed and the detector. rf_trust falls when a band shows a sustained AGC
// departure or the C/N₀-vs-elevation residual collapses — an untrustworthy RF
// environment down-weights the station's integrity votes (docs/DEFENSE-PNT.md §2).
func (s *Store) FeedStationRF(now time.Time) map[string]StationRF {
	out := make(map[string]StationRF)
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	for id, st := range s.rf {
		if now.Sub(st.lastSeen) > rfStaleAfter {
			continue
		}
		entry := StationRF{
			ID:       id,
			LastSeen: st.lastSeen.Unix(),
			RFTrust:  1.0,
		}
		// MON-RF and NAV-SAT arrive as independent frames and jointly keep
		// the station-level lastSeen fresh; without its own staleness check, a
		// C/N₀ residual computed once and never refreshed (NAV-SAT stopped while
		// MON-RF kept the station alive) would feed a tripped spoof gate forever
		// -- it could confirm spoofing_suspected once and never clear.
		if st.haveCn0 && now.Sub(st.cn0LastSeen) <= rfStaleAfter {
			m, r := st.cn0Mean, st.cn0Resid
			entry.Cn0Mean, entry.Cn0Resid = &m, &r
			entry.NumSats = st.cn0NumSats
		}
		blocks := make([]int, 0, len(st.bands))
		for b := range st.bands {
			blocks = append(blocks, b)
		}
		sort.Ints(blocks)
		for _, bn := range blocks {
			b := st.bands[bn]
			// symmetrically, a band whose own telemetry has stopped must
			// not keep contributing its last-ever (possibly jammed) agc/jamState
			// to the jamming classification forever.
			if now.Sub(b.lastSeen) > rfStaleAfter {
				continue
			}
			sb := StationRFBand{
				Block: b.block, AGC: b.agc, CWSuppress: b.cwSuppress,
				NoiseLevel: b.noise, JamState: b.jamState, AntStatus: b.antStatus,
			}
			if b.haveBaseline {
				dep := b.baseline - float64(b.agc)
				sb.AGCDeparture = &dep
				if dep > agcLearnBand/2 { // a real departure: down-weight this station
					entry.RFTrust = 0.3
				}
			}
			entry.Bands = append(entry.Bands, sb)
		}
		out[id] = entry
	}
	return out
}

func medianInt(v []int) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]int(nil), v...)
	sort.Ints(c)
	n := len(c)
	if n%2 == 1 {
		return float64(c[n/2])
	}
	return float64(c[n/2-1]+c[n/2]) / 2
}

func absInt(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
