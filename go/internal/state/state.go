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
	// BeiDou B2a B-CNAV2 message assembly buffers (types 10/11 ephemeris, 30/34 clock).
	bc10, bc11, bc30 *frame.BeiDouBCNAV2
	// Measured-iono tracks per ingest source (dual-frequency observables).
	ionoBySource map[string]*ionoTrack
	// GLONASS string assembly buffers + Cartesian ephemeris (RK4, not kepler).
	gloS1, gloS2, gloS3 *frame.GLONASSString
	gloEph              glonass.Ephemeris
	gloFreqID           int
	haveGloEph          bool
	// Buffered first string of an almanac pair (6/8/10/12/14), awaiting its second
	// (7/9/11/13/15) from the same transmitting satellite.
	gloAlmFirst    []uint32
	gloAlmFirstNum int

	eph     kepler.Ephemeris
	clk     clock.Model
	haveEph bool
	iod     int
	health  int
	ura     int

	// Broadcast accuracy index and the table it decodes with (accNone/accURA/
	// accSISA) — backs the sisa_valid/sisa_m feed fields. BeiDou also carries an
	// age-of-clock / age-of-ephemeris pair, surfaced for that constellation only.
	accIdx     int
	accKind    uint8
	aodc, aode int
	haveAOD    bool

	pos     gnss.ECEF
	havePos bool

	orbitDisco      float64
	orbitDiscoValid bool
	timeDiscoNs     float64
	timeDiscoValid  bool
	discoAt         time.Time
}

// accuracy-table selectors for accKind (docs/MATH.md §6).
const (
	accNone uint8 = 0 // no accuracy captured for this constellation/signal
	accURA  uint8 = 1 // GPS/QZSS/NavIC/BeiDou-B1I URA step table
	accSISA uint8 = 2 // Galileo SISA linear bands
)

type shard struct {
	mu sync.Mutex
	m  map[Key]*svState
}

// sbasState is the per-GEO SBAS augmentation health tracked for the sbas feed
// (docs/OUTPUT.md §1.5): the last message type seen, the last type-0 (do-not-use)
// timestamp, and the provider derived from the PRN.
type sbasState struct {
	prn       int
	provider  string
	lastType  int
	lastSeen  time.Time
	lastType0 time.Time
	haveType0 bool
	doNotUse  bool
}

// Store is the sharded live SV state plus the SBAS health map and the GLONASS almanac
// (the broadcast almanac names every slot, so it carries out-of-view SVs the ephemeris
// store cannot — docs/OUTPUT.md §1.4).
type Store struct {
	shards []*shard

	sbasMu sync.Mutex
	sbas   map[int]*sbasState

	// GLONASS almanac, keyed by subject slot (nA), plus the frame day-number NA. Any
	// satellite's frame carries the whole constellation's almanac (strings 6–15), so
	// this is a store-global map, separate from the per-SV ephemeris shards.
	gloAlmMu   sync.Mutex
	gloAlmanac map[int]frame.GLONASSAlmanacEntry
	gloNA      int

	// Per-station RF-environment state for the PNT-defense layer (docs/DEFENSE-PNT.md):
	// station-scoped (keyed by ingest source / observer id), not per-SV.
	rfMu sync.Mutex
	rf   map[string]*rfStation

	// Per-station capability fingerprint (docs/CONSTELLATIONS.md §7, INTEGRITY §6): the set
	// of (gnssId, sigId) each observer has actually produced nav frames on, so the integrity
	// layer knows what a node *should* be reporting. declared is the tudorgps-declared set
	// (what the silicon *can* produce), set once at startup; both are guarded by capMu.
	capMu    sync.Mutex
	caps     map[string]*capStation
	declared map[string][]CapSignal
}

// New builds a Store with n shards (n >= 1).
func New(n int) *Store {
	if n < 1 {
		n = 1
	}
	s := &Store{
		shards:     make([]*shard, n),
		sbas:       make(map[int]*sbasState),
		gloAlmanac: make(map[int]frame.GLONASSAlmanacEntry),
		rf:         make(map[string]*rfStation),
		caps:       make(map[string]*capStation),
	}
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
	if f.RF != nil {
		s.applyRF(f)
		return
	}
	// A decoded nav frame (Words/Bytes) is the canonical evidence that this station tracks
	// this (gnssId, sigId); record it into the capability fingerprint before dispatch.
	if f.Source != "" && (f.Words != nil || f.Bytes != nil) {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}
	if f.Obs != nil {
		s.applyObservation(f)
		return
	}
	switch {
	case (f.GnssID == gnss.GPS || f.GnssID == gnss.QZSS) && f.SigID == 0:
		s.applyGPSLNAV(f)
	case f.GnssID == gnss.Galileo && (f.SigID == 0 || f.SigID == 1): // E1-B I/NAV
		s.applyGalileoINAV(f)
	case f.GnssID == gnss.BeiDou && f.SigID == 0: // B1I D1 NAV
		s.applyBeiDouD1(f)
	case f.GnssID == gnss.BeiDou && f.SigID == 8: // B2a data component, B-CNAV2
		s.applyBeiDouBCNAV2(f)
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

// applySBAS decodes an SBAS L1 message and folds it into the per-PRN augmentation
// health map that backs the sbas feed (docs/OUTPUT.md §1.5): the last message type,
// the provider from the PRN, and the last type-0 (do-not-use) transition.
func (s *Store) applySBAS(f *ingest.RawFrame) {
	m, err := frame.DecodeSBASL1(f.SvID, f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "sbas").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "sbas").Inc()

	s.sbasMu.Lock()
	defer s.sbasMu.Unlock()
	st := s.sbas[f.SvID]
	if st == nil {
		st = &sbasState{prn: f.SvID, provider: m.Provider}
		s.sbas[f.SvID] = st
	}
	st.lastSeen = f.Recv
	st.lastType = m.Type
	st.doNotUse = m.DoNotUse
	if m.DoNotUse {
		st.lastType0, st.haveType0 = f.Recv, true
	}
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
	st.accKind, st.accIdx = accURA, st.sf1.URAIndex
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

	// Word type 5 carries E1B health and BGD (not part of the ephemeris set); fold
	// its health in as it arrives (docs/CONSTELLATIONS.md §2.2).
	if w.Type == 5 {
		st.health = w.Health
		return
	}
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
	st.accKind, st.accIdx = accSISA, st.galW[3].SISA
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
	st.accKind, st.accIdx = accURA, st.bd1.URAI
	st.aodc, st.aode, st.haveAOD = st.bd1.AODC, st.bd1.AODE, true
}

func (s *Store) applyBeiDouBCNAV2(f *ingest.RawFrame) {
	m, err := frame.DecodeBeiDouBCNAV2(f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "bcnav2").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "bcnav2").Inc()

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

	switch m.MesType {
	case 10:
		st.bc10 = m
	case 11:
		st.bc11 = m
	case 30, 34:
		st.bc30 = m
	default:
		return // types 31/32/33/40 (almanac/EOP/BGTO) not consumed here
	}
	if st.bc10 == nil || st.bc11 == nil {
		return
	}
	eph, clk, err := frame.AssembleBeiDouBCNAV2(f.SvID, st.bc10, st.bc11, st.bc30)
	if err != nil {
		return // types 10/11 not broadcast-adjacent; wait for a fresh pair
	}
	if st.haveEph && st.bc10.IODE == st.iod {
		return
	}
	if st.haveEph {
		s.computeDisco(st, eph, clk, f.Recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, st.bc10.IODE, true
	st.health = st.bc11.HS
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

	switch {
	case str.Number == 1:
		st.gloS1 = str
	case str.Number == 2:
		st.gloS2 = str
		st.health = str.Health
	case str.Number == 3:
		st.gloS3 = str
	case str.Number == 5: // time string: carries the frame day-number NA
		if na, err := frame.DecodeGLONASSFrameNA(f.Words); err == nil {
			s.setGloNA(na)
		}
		return
	case str.Number >= 6 && str.Number <= 14 && str.Number%2 == 0: // first of an almanac pair
		st.gloAlmFirst = append(st.gloAlmFirst[:0], f.Words...)
		st.gloAlmFirstNum = str.Number
		return
	case str.Number >= 7 && str.Number <= 15 && str.Number%2 == 1: // second of an almanac pair
		if st.gloAlmFirst != nil && str.Number == st.gloAlmFirstNum+1 {
			s.applyGloAlmanac(st.gloAlmFirst, f.Words)
		}
		st.gloAlmFirst = nil
		return
	default:
		return // string 4 (reserved here)
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

// setGloNA records the frame day-number NA (the day the almanac elements refer to),
// validated against its ICD range (1..1461 days within the four-year interval).
func (s *Store) setGloNA(na int) {
	if na < 1 || na > 1461 {
		return
	}
	s.gloAlmMu.Lock()
	s.gloNA = na
	s.gloAlmMu.Unlock()
}

// applyGloAlmanac decodes one satellite's almanac from its two-string pair and stores it
// by subject slot. Decoding is pure and done outside the lock; only the map write is
// guarded. Called with the transmitting SV's shard lock held (ordering shard→gloAlm).
func (s *Store) applyGloAlmanac(first, second []uint32) {
	s.gloAlmMu.Lock()
	na := s.gloNA
	s.gloAlmMu.Unlock()

	a, err := frame.DecodeGLONASSAlmanac(first, second, na)
	if err != nil || a.Alm.Slot < 1 || a.Alm.Slot > 24 {
		return
	}
	s.gloAlmMu.Lock()
	s.gloAlmanac[a.Alm.Slot] = a
	s.gloAlmMu.Unlock()
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
