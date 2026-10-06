package state

import (
	"math"
	"sort"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/integrity"
)

// PNT-defense Tier-0 (docs/DEFENSE-PNT.md): the collector turns the RF-environment
// telemetry the fleet's receivers already produce (MON-RF/MON-HW AGC + jamming, NAV-SAT
// C/N₀ + elevation) into per-station detection metrics — jamming departures from a
// learned baseline and the C/N₀-vs-elevation spoofing gate. The metrics are computed
// here (like the SV integrity signals); the debounced detector classifies them into
// station-scoped events (internal/detect). Thresholds are observational in v1 — we
// publish the metrics and learn per-station quiet-time baselines; alerting stays
// conservative until real distributions exist (docs/DEFENSE-PNT.md §4).

// The AGC baseline (docs/DEFENSE-PNT.md §2) is each band's clear-sky level: the median
// of per-minute AGC medians over agcBaselineWindow. A sample further than agcLearnBand
// from the baseline is measured but not learned, so a jammer's departure cannot poison
// the baseline it is measured against. No baseline is served until agcWarmupMinutes of
// quiet minutes exist, so a station is never measured against its first sample. Once
// established, the baseline moves at most agcDriftPerHour, so a jammer that ramps up
// more slowly than the learn band still cannot drag it along. A band persistently
// quieter than its baseline (gain above it by more than the learn band) for
// agcRelearnAfter learned its baseline under interference; it starts learning again.
const (
	agcLearnBand        = 400 // AGC counts; departures beyond this don't update the baseline
	agcMinuteMinSamples = 3   // a minute needs this many quiet samples to count
	agcMinuteMaxSamples = 600 // bound on one minute's samples (UBX MON-RF is 1 Hz by default)
	agcWarmupMinutes    = 10
	agcBaselineWindow   = 6 * time.Hour
	agcDriftPerHour     = 150.0 // AGC counts
	agcRelearnAfter     = 15 * time.Minute
	// agcRestoreMaxAge bounds how old a checkpointed baseline may be when restored.
	agcRestoreMaxAge = 24 * time.Hour
	// rfStaleAfter is THE staleness operating point for station-scoped feeds —
	// and, through the feed.go alias `sbasStaleAfter = rfStaleAfter`, for the
	// served SBAS feed as well. two other constants are pinned to this
	// value and neither is reachable from here by grep, so retuning it silently
	// desynchronizes them:
	//   - state.sbasStaleAfter (feed.go) — the alias; moves automatically.
	//   - detect.SBASSilentThreshold (300.0 s) — a duplicated literal that does
	//     NOT move (detect depends on state, not the reverse, so it cannot
	//     import this). It exists so sbas_lost confirms one debounce after the
	//     PRN leaves the served feed; if the two diverge, either a PRN vanishes
	//     from the feed with no darkness event, or the event fires while the
	//     feed still serves it.
	// Retune all three together, or state the divergence deliberately.
	rfStaleAfter = 5 * time.Minute
	cn0MinSats   = 5 // fewest tracked SVs before the C/N₀-vs-elevation gate is meaningful
)

// rfBand is one RF path's learned state at a station: the latest receiver numbers plus
// the quiet-time AGC baseline.
type rfBand struct {
	block                            int
	agc, noise, cwSuppress, jamState int
	antStatus                        int
	lastSeen                         time.Time
	// departedSince starts the current run at or beyond the jamming AGC departure;
	// zero while the band is within it or has no baseline.
	departedSince time.Time
	agcBaseline
}

// departure is the band's AGC below its learned baseline; ok is false while no
// baseline is learned.
func (b *rfBand) departure() (dep float64, ok bool) {
	if !b.haveBaseline {
		return 0, false
	}
	return b.baseline - float64(b.agc), true
}

// AGCMinute is one completed minute's median of quiet AGC samples.
type AGCMinute struct {
	Start  time.Time `json:"start"`
	Median float64   `json:"median"`
}

// agcBaseline learns one band's clear-sky AGC level (see the constants above).
type agcBaseline struct {
	baseline     float64
	haveBaseline bool
	baselineAt   time.Time   // when the baseline was last moved
	minutes      []AGCMinute // completed minutes within agcBaselineWindow, oldest first
	minuteStart  time.Time
	current      []int     // quiet samples of the minute in progress
	aboveSince   time.Time // start of a continuous run above baseline + agcLearnBand
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
	cn0ByGNSS   map[int]Cn0Stats
	cn0LastSeen time.Time // last NAV-SAT sample; ages the spoof gate independently of MON-RF
	cn0DropAt   time.Time // last NAV-SAT at which the cn0_drop check served a drop
}

// applyRF folds one RF-telemetry sample into the per-station RF state, updating the
// per-band AGC baselines and the C/N₀-vs-elevation residual. Station identity is the
// ingest source (dial) or observer id (push).
func (s *Store) applyRF(f *ingest.RawFrame) {
	// RF staleness (rfStaleAfter) and the live-receiver window are
	// elapsed times against this collector's clock, so recency must be stamped
	// from it — a feeder lagging near the 5 min push timestamp slack would
	// otherwise sit permanently at the equally-sized staleness bound.
	recv := f.LocalRecv()
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	st := s.rf[f.Source]
	if st == nil {
		st = &rfStation{id: f.Source, bands: map[int]*rfBand{}}
		s.rf[f.Source] = st
	}
	st.lastSeen = recv

	for _, b := range f.RF.Bands {
		band := st.bands[b.Block]
		if band == nil {
			band = &rfBand{block: b.Block}
			if restored, ok := s.agcRestore[f.Source][b.Block]; ok {
				band.agcBaseline = restored
				delete(s.agcRestore[f.Source], b.Block)
			}
			st.bands[b.Block] = band
		}
		band.agc, band.noise, band.cwSuppress = b.AGC, b.NoiseLevel, b.CWSuppress
		band.jamState, band.antStatus = b.JamState, b.AntStatus
		band.lastSeen = recv
		band.learn(b.AGC, recv)
		if dep, ok := band.departure(); ok && dep >= integrity.DefaultAGCDeparture {
			if band.departedSince.IsZero() {
				band.departedSince = recv
			}
		} else {
			band.departedSince = time.Time{}
		}
	}
	if len(f.RF.Sats) > 0 {
		st.cn0Mean, st.cn0Resid, st.cn0NumSats = cn0ElevationResidual(f.RF.Sats)
		st.cn0ByGNSS = cn0ElevationResidualByConstellation(f.RF.Sats)
		st.haveCn0 = st.cn0NumSats >= cn0MinSats
		st.cn0LastSeen = recv
	}
	s.integrityRF(st, f, recv)
}

// cn0ElevationResidualByConstellation keeps the single-constellation transmitter
// signature visible in a mixed-GNSS sky. SBAS GEOs are excluded because one
// fixed-elevation satellite cannot form a constellation trend.
func cn0ElevationResidualByConstellation(sats []ingest.SatCN0) map[int]Cn0Stats {
	grouped := make(map[int][]ingest.SatCN0)
	for _, sat := range sats {
		if gnss.GNSSID(sat.GnssID) == gnss.SBAS {
			continue
		}
		grouped[sat.GnssID] = append(grouped[sat.GnssID], sat)
	}
	out := make(map[int]Cn0Stats)
	for id, group := range grouped {
		mean, resid, n := cn0ElevationResidual(group)
		if n >= cn0MinSats {
			out[id] = Cn0Stats{Mean: mean, Resid: resid, NumSats: n}
		}
	}
	return out
}

// learn folds an AGC sample received at at into the quiet-time baseline.
func (b *agcBaseline) learn(agc int, at time.Time) {
	if b.haveBaseline {
		switch d := float64(agc) - b.baseline; {
		case d < -agcLearnBand:
			// A gain cut: a jamming departure, measured but never learned.
			b.aboveSince = time.Time{}
			return
		case d > agcLearnBand:
			// Quieter than the clear-sky reference. Brief excursions are not
			// learned; a sustained one means the reference was learned under
			// interference, so learning starts again.
			if b.aboveSince.IsZero() {
				b.aboveSince = at
			}
			if at.Sub(b.aboveSince) < agcRelearnAfter {
				return
			}
			*b = agcBaseline{}
		default:
			b.aboveSince = time.Time{}
		}
	}
	if b.minuteStart.IsZero() {
		b.minuteStart = at
	}
	if at.Sub(b.minuteStart) >= time.Minute {
		b.closeMinute(at)
	}
	if len(b.current) < agcMinuteMaxSamples {
		b.current = append(b.current, agc)
	}
}

// closeMinute completes the minute in progress, drops minutes older than the window,
// and moves the baseline toward the median of the remaining minutes.
func (b *agcBaseline) closeMinute(at time.Time) {
	if len(b.current) >= agcMinuteMinSamples {
		b.minutes = append(b.minutes, AGCMinute{Start: b.minuteStart, Median: medianInt(b.current)})
	}
	b.current, b.minuteStart = b.current[:0], at
	n := 0
	for n < len(b.minutes) && at.Sub(b.minutes[n].Start) > agcBaselineWindow {
		n++
	}
	b.minutes = b.minutes[n:]
	b.update(at)
}

// update moves the baseline toward the median of the learned minutes, establishing it
// after the warm-up and then at most agcDriftPerHour.
func (b *agcBaseline) update(at time.Time) {
	if len(b.minutes) == 0 {
		return
	}
	medians := make([]float64, len(b.minutes))
	for i, m := range b.minutes {
		medians[i] = m.Median
	}
	target := medianFloat(medians)
	switch {
	case !b.haveBaseline:
		if len(b.minutes) >= agcWarmupMinutes {
			b.baseline, b.haveBaseline, b.baselineAt = target, true, at
		}
	default:
		limit := agcDriftPerHour * at.Sub(b.baselineAt).Hours()
		b.baseline += math.Max(-limit, math.Min(limit, target-b.baseline))
		b.baselineAt = at
	}
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
		// "elevation unknown" sentinel (91), typical for a freshly-acquired SV.
		// The GNF1 telemetry encode preserves it (clampElev bounds at the wire
		// field's int8 range, not 90), so this one choke point covers both the
		// dial and push paths. Feeding an unknown elevation into the regression
		// as if it were a real data point biases the slope/residual this
		// single-transmitter spoof gate depends on.
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
	// Cn0Drop reports a simultaneous C/N₀ drop across the station's signals: served
	// now by the integrity cn0_drop check (held through its recovery period), or
	// served at some point during a band's current AGC departure, so it lasts as long
	// as that departure. The jamming classifier takes it as corroboration
	// (docs/DEFENSE-PNT.md §2); the station's assessment serves the check itself.
	Cn0Drop bool `json:"-"`
}

// Cn0Stats is one constellation's elevation-fit result. The integrity
// cn0_uniformity check evaluates each one beside the aggregate fit (integrity.go).
type Cn0Stats struct {
	Mean    float64
	Resid   float64
	NumSats int
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
// observers feed and the station RF classifiers. rf_trust falls when a band shows a
// sustained AGC departure — an untrustworthy RF environment down-weights the station's
// integrity votes (docs/DEFENSE-PNT.md §2). The C/N₀-vs-elevation residual is served as
// cn0_resid and evaluated by the integrity cn0_uniformity check, but is deliberately NOT
// folded into rf_trust, and nothing in detect consumes rf_trust; this comment matches the code.
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
			RFTrust:  st.rfTrust(now),
		}
		// MON-RF and NAV-SAT arrive as independent frames and jointly keep
		// the station-level lastSeen fresh; without its own staleness check, a
		// C/N₀ residual computed once and never refreshed (NAV-SAT stopped while
		// MON-RF kept the station alive) would be served as current forever. The
		// integrity cn0_uniformity check ages its own input the same way.
		if st.haveCn0 && now.Sub(st.cn0LastSeen) <= rfStaleAfter {
			m, r := st.cn0Mean, st.cn0Resid
			entry.Cn0Mean, entry.Cn0Resid = &m, &r
			entry.NumSats = st.cn0NumSats
		}
		entry.Cn0Drop = s.cn0DropCorroboration(st, now)
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
			if dep, ok := b.departure(); ok {
				sb.AGCDeparture = &dep
			}
			entry.Bands = append(entry.Bands, sb)
		}
		out[id] = entry
	}
	return out
}

// rfTrust is how much the station's votes count given its front end: 0.3 while a
// fresh band shows a real AGC departure, otherwise 1. The caller holds rfMu.
func (st *rfStation) rfTrust(now time.Time) float64 {
	for _, b := range st.bands {
		if now.Sub(b.lastSeen) > rfStaleAfter {
			continue
		}
		if dep, ok := b.departure(); ok && dep > agcLearnBand/2 {
			return 0.3
		}
	}
	return 1
}

// sourceVoteWeights returns, as of now, each station whose testimony counts for less
// than a full vote toward a satellite's weighted corroboration (FeedSV.ConfWeighted):
// its RF trust, lowered further by its assessment, which maps the integrity levels
// to weights (assured 1, inconsistent ½, unassured 0) and gives a station whose
// evidence indicates spoofing no weight. A station with no RF or integrity evidence
// is absent and weighs 1: lacking evidence is not distrust.
func (s *Store) sourceVoteWeights(now time.Time) map[string]float64 {
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	out := map[string]float64{}
	lower := func(id string, w float64) {
		if prev, ok := out[id]; ok {
			w = min(w, prev)
		}
		if w < 1 {
			out[id] = w
		}
	}
	for id, st := range s.rf {
		if now.Sub(st.lastSeen) <= rfStaleAfter {
			lower(id, st.rfTrust(now))
		}
	}
	for id, is := range s.integrity {
		if now.Sub(is.lastInput) > integrityEvictAfter {
			continue
		}
		switch a := is.eval.Assess(now); {
		case a.SpoofingIndicated || a.State == integrity.Unassured:
			lower(id, 0)
		case a.State == integrity.Inconsistent:
			lower(id, 0.5)
		}
	}
	return out
}

// cn0DropCorroboration reports a simultaneous C/N₀ drop that corroborates the
// station's front-end evidence as of now: one the cn0_drop check serves now, or one
// it served during a fresh band's current AGC departure. The drop marks a jammer's
// onset and C/N₀ then stays low without dropping again, so a drop seen during a
// departure keeps corroborating it until the departure ends. The caller holds rfMu.
func (s *Store) cn0DropCorroboration(st *rfStation, now time.Time) bool {
	if is := s.integrity[st.id]; is != nil {
		if served, ok := is.eval.Served(integrity.CheckCn0Drop, now); ok && degradedState(served) {
			return true
		}
	}
	if st.cn0DropAt.IsZero() {
		return false
	}
	for _, b := range st.bands {
		if now.Sub(b.lastSeen) <= rfStaleAfter && !b.departedSince.IsZero() && !st.cn0DropAt.Before(b.departedSince) {
			return true
		}
	}
	return false
}

func medianFloat(v []float64) float64 {
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	n := len(c)
	if n%2 == 1 {
		return c[n/2]
	}
	return (c[n/2-1] + c[n/2]) / 2
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

// AGCBaseline is one band's durable AGC baseline: what a checkpoint stores and a
// restart restores, so a collector restart does not repeat the warm-up.
type AGCBaseline struct {
	Block      int         `json:"block"`
	Baseline   float64     `json:"baseline"`
	BaselineAt time.Time   `json:"baseline_at"`
	Minutes    []AGCMinute `json:"minutes"`
}

// AGCBaselines returns every station's established AGC baselines, bands in block order.
func (s *Store) AGCBaselines() map[string][]AGCBaseline {
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	out := map[string][]AGCBaseline{}
	for id, st := range s.rf {
		blocks := make([]int, 0, len(st.bands))
		for block, b := range st.bands {
			if b.haveBaseline {
				blocks = append(blocks, block)
			}
		}
		sort.Ints(blocks)
		for _, block := range blocks {
			b := st.bands[block]
			out[id] = append(out[id], AGCBaseline{Block: block, Baseline: b.baseline, BaselineAt: b.baselineAt,
				Minutes: append([]AGCMinute(nil), b.minutes...)})
		}
	}
	return out
}

// RestoreAGCBaselines queues a station's checkpointed baselines. Each is installed
// when its band first reports, so a restored station is not live until it is heard
// from. A baseline older than agcRestoreMaxAge, out of range, or for a band that has
// already reported is not restored; the count restored is returned.
func (s *Store) RestoreAGCBaselines(id string, bands []AGCBaseline, now time.Time) int {
	s.rfMu.Lock()
	defer s.rfMu.Unlock()
	restored := 0
	for _, b := range bands {
		if b.Block < 0 || b.Block > 255 || math.IsNaN(b.Baseline) || b.Baseline < 0 || b.Baseline > 65535 ||
			now.Sub(b.BaselineAt) > agcRestoreMaxAge || b.BaselineAt.After(now) {
			continue
		}
		if st := s.rf[id]; st != nil && st.bands[b.Block] != nil {
			continue
		}
		baseline := agcBaseline{baseline: b.Baseline, haveBaseline: true, baselineAt: b.BaselineAt}
		for _, m := range b.Minutes {
			if !math.IsNaN(m.Median) && now.Sub(m.Start) <= agcBaselineWindow && !m.Start.After(now) {
				baseline.minutes = append(baseline.minutes, m)
			}
		}
		if s.agcRestore[id] == nil {
			s.agcRestore[id] = map[int]agcBaseline{}
		}
		s.agcRestore[id][b.Block] = baseline
		restored++
	}
	return restored
}
