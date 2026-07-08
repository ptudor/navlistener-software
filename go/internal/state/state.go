// Package state holds the live per-SV navigation state: the ephemeris store
// (current + previous set per SV), the assembled clock model, health/accuracy, the
// propagated ECEF position, and the derived integrity signals orbit-disco and
// time-disco (docs/DESIGN.md §1 stage 3, docs/INTEGRITY.md §3). It is the
// "propagate + integrity" stage — the part that is really ours.
//
// The store is sharded and each shard is mutex-guarded, so ingest goroutines
// update state concurrently and the serve/propagate side reads it race-free (the
// C1–C11 discipline: no unlocked shared map).
package state

import (
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/clock"
	"github.com/ptudor/gnss/frame"
	"github.com/ptudor/gnss/glonass"
	"github.com/ptudor/gnss/gnsstime"
	"github.com/ptudor/gnss/kepler"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/metrics"
)

// gpsEpochUnix is 1980-01-06T00:00:00Z; gpsUTCOffset is the current GPS−UTC (ΔtLS).
const (
	gpsEpochUnix  = 315964800
	gpsUTCOffset  = 18
	weekSeconds   = 604800
	discoTrustAge = 4 * time.Hour // ephemerides older than this aren't trusted for disco
)

// Key identifies a satellite×signal, the feed's name@sigid space.
type Key struct {
	G   gnss.GNSSID
	Sv  int
	Sig int
}

// Name renders the RINEX-letter SV name with signal, e.g. "G05@0", "J03@0".
func (k Key) Name() string {
	return fmt.Sprintf("%c%02d@%d", k.G.Letter(), k.Sv, k.Sig)
}

type svState struct {
	key      Key
	lastSeen time.Time

	// LNAV subframe assembly buffers (GPS/QZSS).
	sf1, sf2, sf3 *frame.GPSSubframe
	// Galileo I/NAV word assembly buffers, indexed by word type 1–4.
	galW [5]*frame.GalileoINAV
	// BeiDou D1 subframe assembly buffers.
	bd1, bd2, bd3 *frame.BeiDouSubframe
	// GLONASS string assembly buffers + Cartesian ephemeris (RK4, not kepler).
	gloS1, gloS2, gloS3 *frame.GLONASSString
	gloEph              glonass.Ephemeris
	gloFreqID           int
	haveGloEph          bool

	eph     kepler.Ephemeris
	clk     clock.Model
	haveEph bool
	iod     int
	health  int
	ura     int

	pos     gnss.ECEF
	havePos bool

	orbitDisco      float64
	orbitDiscoValid bool
	timeDiscoNs     float64
	timeDiscoValid  bool
	discoAt         time.Time
}

type shard struct {
	mu sync.Mutex
	m  map[Key]*svState
}

// Store is the sharded live SV state.
type Store struct {
	shards []*shard
}

// New builds a Store with n shards (n >= 1).
func New(n int) *Store {
	if n < 1 {
		n = 1
	}
	s := &Store{shards: make([]*shard, n)}
	for i := range s.shards {
		s.shards[i] = &shard{m: make(map[Key]*svState)}
	}
	return s
}

func (s *Store) shardFor(k Key) *shard {
	h := uint(k.G)*131 + uint(k.Sv)*17 + uint(k.Sig)
	return s.shards[h%uint(len(s.shards))]
}

// Apply decodes a raw frame and folds it into live state. Unsupported frame types
// are counted and dropped (their raw bytes are preserved upstream once the persist
// stage lands). It never panics on malformed input — decode errors are returned as
// metrics, not crashes.
func (s *Store) Apply(f *ingest.RawFrame) {
	switch {
	case (f.GnssID == gnss.GPS || f.GnssID == gnss.QZSS) && f.SigID == 0:
		s.applyGPSLNAV(f)
	case f.GnssID == gnss.Galileo && (f.SigID == 0 || f.SigID == 1): // E1-B I/NAV
		s.applyGalileoINAV(f)
	case f.GnssID == gnss.BeiDou && f.SigID == 0: // B1I D1 NAV
		s.applyBeiDouD1(f)
	case f.GnssID == gnss.GLONASS && f.SigID == 0: // L1OF strings
		s.applyGLONASS(f)
	case (f.GnssID == gnss.GPS || f.GnssID == gnss.QZSS) && isCNAVSignal(f.GnssID, f.SigID):
		s.applyGPSCNAV(f)
	case f.GnssID == gnss.SBAS && f.SigID == 0: // L1 C/A message stream
		s.applySBAS(f)
	default:
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "unsupported").Inc()
	}
}

// isCNAVSignal reports whether an (id, sigId) is a GPS/QZSS L2C or L5 CNAV signal
// (docs/CONSTELLATIONS.md §2.1): GPS L2C 3/4, L5 6/7; QZSS L2C 4/5, L5 8/9.
func isCNAVSignal(id gnss.GNSSID, sig int) bool {
	if id == gnss.GPS {
		return sig == 3 || sig == 4 || sig == 6 || sig == 7
	}
	return sig == 4 || sig == 5 || sig == 8 || sig == 9 // QZSS
}

// applyGPSCNAV decodes a GPS/QZSS CNAV message for telemetry. The CNAV ephemeris is
// redundant with LNAV for positioning; its L2C/L5 ISCs and the cross-signal
// integrity check are consumed in the integrity pass (P6).
func (s *Store) applyGPSCNAV(f *ingest.RawFrame) {
	if _, err := frame.DecodeGPSCNAV(f.GnssID, f.Words); err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "cnav").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "cnav").Inc()
}

// applySBAS decodes an SBAS L1 message for telemetry. The per-PRN sbas health feed
// (message type / do-not-use / provider) is populated in the serve pass (P5).
func (s *Store) applySBAS(f *ingest.RawFrame) {
	if _, err := frame.DecodeSBASL1(f.SvID, f.Words); err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "sbas").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "sbas").Inc()
}

func (s *Store) applyGPSLNAV(f *ingest.RawFrame) {
	sf, err := frame.DecodeGPSLNAV(f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "lnav").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "lnav").Inc()

	key := Key{G: f.GnssID, Sv: f.SvID, Sig: f.SigID}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	st := sh.m[key]
	if st == nil {
		st = &svState{key: key}
		sh.m[key] = st
	}
	st.lastSeen = f.Recv

	switch sf.SubframeID {
	case 1:
		st.sf1 = sf
	case 2:
		st.sf2 = sf
	case 3:
		st.sf3 = sf
	default:
		return // subframes 4/5 (almanac/iono) not consumed here
	}
	if st.sf1 == nil || st.sf2 == nil || st.sf3 == nil {
		return
	}

	eph, clk, err := frame.AssembleGPS(f.GnssID, f.SvID, st.sf1, st.sf2, st.sf3)
	if err != nil {
		return // inconsistent triple; wait for a matching set
	}
	newIOD := st.sf2.IODE
	if st.haveEph && newIOD == st.iod {
		return // same data set, nothing new
	}

	// A new ephemeris (new IOD): compute the changeover discontinuities against the
	// outgoing set before replacing it (docs/INTEGRITY.md §3).
	if st.haveEph {
		s.computeDisco(st, eph, clk, f.Recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, newIOD, true
	st.health, st.ura = st.sf1.Health, st.sf1.URAIndex
}

func (s *Store) applyGalileoINAV(f *ingest.RawFrame) {
	w, err := frame.DecodeGalileoINAV(f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "inav").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "inav").Inc()

	key := Key{G: f.GnssID, Sv: f.SvID, Sig: 0} // Galileo SV keyed on primary signal
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	st := sh.m[key]
	if st == nil {
		st = &svState{key: key}
		sh.m[key] = st
	}
	st.lastSeen = f.Recv

	if w.Type < 1 || w.Type > 4 {
		return // only word types 1–4 assemble the ephemeris
	}
	st.galW[w.Type] = w
	if st.galW[1] == nil || st.galW[2] == nil || st.galW[3] == nil || st.galW[4] == nil {
		return
	}

	eph, clk, err := frame.AssembleGalileo(f.SvID, st.galW[1], st.galW[2], st.galW[3], st.galW[4])
	if err != nil {
		return // words from different IODnav; wait for a consistent set
	}
	newIOD := st.galW[1].IODnav
	if st.haveEph && newIOD == st.iod {
		return
	}
	if st.haveEph {
		s.computeDisco(st, eph, clk, f.Recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, newIOD, true
	// Galileo health lives in I/NAV word type 5 (not part of the ephemeris set);
	// it is decoded and surfaced when the integrity pass consumes word 5.
}

func (s *Store) applyBeiDouD1(f *ingest.RawFrame) {
	sf, err := frame.DecodeBeiDouD1(f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "d1").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "d1").Inc()

	key := Key{G: f.GnssID, Sv: f.SvID, Sig: 0}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	st := sh.m[key]
	if st == nil {
		st = &svState{key: key}
		sh.m[key] = st
	}
	st.lastSeen = f.Recv

	switch sf.FraID {
	case 1:
		st.bd1 = sf
	case 2:
		st.bd2 = sf
	case 3:
		st.bd3 = sf
	default:
		return // subframes 4/5 (almanac/iono) not consumed here
	}
	if st.bd1 == nil || st.bd2 == nil || st.bd3 == nil {
		return
	}
	eph, clk, err := frame.AssembleBeiDou(f.SvID, st.bd1, st.bd2, st.bd3)
	if err != nil {
		return
	}
	// BeiDou has no single issue-of-data across subframes; key the changeover on
	// the ephemeris reference time toe.
	newIOD := int(eph.Toe)
	if st.haveEph && newIOD == st.iod {
		return
	}
	if st.haveEph {
		s.computeDisco(st, eph, clk, f.Recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, newIOD, true
	st.health = st.bd1.Health
}

func (s *Store) applyGLONASS(f *ingest.RawFrame) {
	str, err := frame.DecodeGLONASSString(f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "glo").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "glo").Inc()

	key := Key{G: f.GnssID, Sv: f.SvID, Sig: 0}
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	st := sh.m[key]
	if st == nil {
		st = &svState{key: key}
		sh.m[key] = st
	}
	st.lastSeen = f.Recv
	st.gloFreqID = f.FreqID

	switch str.Number {
	case 1:
		st.gloS1 = str
	case 2:
		st.gloS2 = str
		st.health = str.Health
	case 3:
		st.gloS3 = str
	default:
		return // strings 4/5 (time) and 6–15 (almanac) not assembled here
	}
	if st.gloS1 == nil || st.gloS2 == nil || st.gloS3 == nil {
		return
	}
	eph, err := frame.AssembleGLONASS(f.SvID, st.gloFreqID, st.gloS1, st.gloS2, st.gloS3)
	if err != nil {
		return
	}
	st.gloEph, st.haveGloEph = eph, true
}

// computeDisco propagates the outgoing and incoming ephemerides to the new
// reference epoch and records orbit-disco (metres) and time-disco (ns). Guards
// require both propagations to succeed; this is not the first ephemeris
// (guaranteed by the caller). A violated guard leaves the field invalid (absent in
// the feed), never a sentinel.
func (s *Store) computeDisco(st *svState, newEph kepler.Ephemeris, newClk clock.Model, now time.Time) {
	tstar := newEph.Toe
	oldPos, e1 := kepler.Propagate(st.eph, tstar)
	newPos, e2 := kepler.Propagate(newEph, tstar)
	if e1 == nil && e2 == nil {
		st.orbitDisco = newPos.Sub(oldPos).Norm()
		st.orbitDiscoValid = true
	} else {
		st.orbitDiscoValid = false
	}
	oOff, e3 := clock.OffsetFor(st.clk, st.eph, tstar)
	nOff, e4 := clock.OffsetFor(newClk, newEph, tstar)
	if e3 == nil && e4 == nil {
		st.timeDiscoNs = math.Abs(nOff-oOff) * 1e9
		st.timeDiscoValid = true
	} else {
		st.timeDiscoValid = false
	}
	st.discoAt = now
}

// Propagate re-propagates every SV with a current ephemeris to the wall-clock time
// now, updating its ECEF position, and refreshes the live-SV gauge. Called on the
// state tick.
func (s *Store) Propagate(now time.Time) {
	counts := map[string]int{}
	for _, sh := range s.shards {
		sh.mu.Lock()
		for _, st := range sh.m {
			if st.key.G == gnss.GLONASS {
				if !st.haveGloEph {
					continue
				}
				tk := gnsstime.EphAgeDay(gloTOD(now), st.gloEph.Tb)
				if pos, err := glonass.Propagate(st.gloEph, tk); err == nil {
					st.pos, st.havePos = pos, true
				}
				counts["glonass"]++
				continue
			}
			if !st.haveEph {
				continue
			}
			tow := towFor(st.key.G, now)
			if pos, err := kepler.Propagate(st.eph, tow); err == nil {
				st.pos, st.havePos = pos, true
			}
			counts[st.key.G.String()]++
		}
		sh.mu.Unlock()
	}
	for c, n := range counts {
		metrics.LiveSVs.WithLabelValues(c).Set(float64(n))
	}
}

// Expire drops SVs unseen for longer than ttl.
func (s *Store) Expire(now time.Time, ttl time.Duration) {
	for _, sh := range s.shards {
		sh.mu.Lock()
		for k, st := range sh.m {
			if now.Sub(st.lastSeen) > ttl {
				delete(sh.m, k)
				metrics.SVsExpiredTotal.Inc()
			}
		}
		sh.mu.Unlock()
	}
}

// gpsTOW returns the GPS/QZSS time-of-week (seconds) for a wall-clock instant.
// GST (Galileo) shares this time-of-week to nanoseconds.
func gpsTOW(now time.Time) float64 {
	gps := now.Unix() - gpsEpochUnix + gpsUTCOffset
	return float64(((gps % weekSeconds) + weekSeconds) % weekSeconds)
}

// towFor returns the constellation's own time-of-week for propagation. GPS/QZSS/
// Galileo share GPS SOW; BeiDou runs 14 s behind (BDT = GPST − 14 s), so its toe
// is on a shifted scale and it must be propagated at the shifted SOW.
func towFor(g gnss.GNSSID, now time.Time) float64 {
	tow := gpsTOW(now)
	if g == gnss.BeiDou {
		if tow -= 14; tow < 0 {
			tow += weekSeconds
		}
	}
	return tow
}

// gloTOD returns the GLONASS time-of-day (seconds) for a wall-clock instant.
// GLONASS time is UTC(SU)+3h, and the broadcast tb is on that scale, so the RK4
// propagation target is (UTC + 3 h) modulo the day.
func gloTOD(now time.Time) float64 {
	tod := (now.Unix() + 3*3600) % 86400
	if tod < 0 {
		tod += 86400
	}
	return float64(tod)
}
