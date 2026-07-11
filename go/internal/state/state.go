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

// gpsEpochUnix is 1980-01-06T00:00:00Z.
const (
	gpsEpochUnix  = 315964800
	weekSeconds   = 604800
	discoTrustAge = 4 * time.Hour // ephemerides older than this aren't trusted for disco

	// glonassFrameWindow bounds how far apart strings 1/2/3's reception times may be
	// and still be treated as one coherent frame. Real broadcast spacing is
	// ~2s per string (~4s end to end); this is generous margin over normal jitter
	// while comfortably rejecting a stale string left over from ~30 minutes prior.
	glonassFrameWindow = 8 * time.Second

	// posStaleBound  is how old a stored propagation epoch (svState.posAt)
	// may be before the feed omits the position/tow/wn entirely rather than serve a
	// solution frozen at a repeatedly-failing propagate tick. Generous over any
	// sane [state].propagate_interval (default 1s) while still catching "this SV's
	// propagation has been silently failing for a couple of minutes" (each failure
	// leaves havePos/pos untouched, so without this bound a stale-but-finite
	// position would otherwise be served forever with an ever-fresher-looking tow).
	posStaleBound = 120 * time.Second
)

// gpsUTCOffset is the current GPS−UTC (ΔtLS), the leap-second count applied to
// every wall-clock→GPS/BDT time-of-week conversion (gpsTOW, towFor, weekFor)
// and served as the global feed's leap_seconds. the design mandates
// "transcribe, don't invent" and the broadcast UTC-parameter decode is a stated
// follow-up, but until that lands this compiled-in default is the only source —
// a real leap event would otherwise shift every conversion by 1s (~3.9 km)
// until a rebuild. SetLeapSeconds lets [state].leap_seconds override it as an
// interim fix; it must be called during startup config wiring, before any
// ingest/propagate goroutines start (it is a plain package var, not
// synchronized for concurrent use).
var gpsUTCOffset int64 = 18

// SetLeapSeconds overrides gpsUTCOffset. n <= 0 is a no-op (keeps the
// compiled-in default); config.go's Validate bounds any nonzero override to the
// ICD-plausible 10..30s range before this is ever called.
func SetLeapSeconds(n int) {
	if n > 0 {
		gpsUTCOffset = int64(n)
	}
}

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
	// Galileo I/NAV word assembly buffers, indexed by word type 1–5 (word 5 carries
	// BGD/health, not part of the ephemeris set; index 0 unused).
	galW [6]*frame.GalileoINAV
	// BeiDou D1 subframe assembly buffers.
	bd1, bd2, bd3 *frame.BeiDouSubframe
	// BeiDou B2a B-CNAV2 message assembly buffers (types 10/11 ephemeris, 30/34 clock).
	bc10, bc11, bc30 *frame.BeiDouBCNAV2
	// Measured-iono tracks per ingest source (dual-frequency observables).
	ionoBySource map[string]*ionoTrack
	// GLONASS string assembly buffers + Cartesian ephemeris (RK4, not kepler).
	// gloS{1,2,3}At are each string's reception time : strings 1/2/3 carry
	// x/y/z of one PZ-90 state valid only within one ~30 s frame, and unlike
	// GPS/Galileo/BeiDou they carry no per-changeover tag, so recency is the only
	// coherence guard — assembly is gated on all three having arrived within one
	// frame window of each other.
	gloS1, gloS2, gloS3       *frame.GLONASSString
	gloS1At, gloS2At, gloS3At time.Time
	gloEph                    glonass.Ephemeris
	gloFreqID                 int
	haveGloEph                bool
	// Buffered first string of an almanac pair (6/8/10/12/14), awaiting its second
	// (7/9/11/13/15) from the same transmitting satellite.
	gloAlmFirst    []uint32
	gloAlmFirstNum int

	eph     kepler.Ephemeris
	clk     clock.Model
	haveEph bool
	iod     int
	// bcIODC is BeiDou B-CNAV2's type-30/34 clock IODC, tracked separately
	// from iod (the type-10/11 ephemeris IODE) so a clock-only refresh (same
	// ephemeris, new af0/af1/af2) is not silently dropped by the ephemeris IODE
	// gate below — the two message families change independently.
	bcIODC    int
	haveBcIOD bool
	health    int
	// haveHealth is health_code 0 ("unknown") is the correct answer until
	// this SV's own health bits have actually been decoded (e.g. a Galileo SV
	// with only word types 1-4 assembled -- health arrives on word 5) or an
	// iono-only SV  nothing is known about at all. Without this flag,
	// st.health's zero value is indistinguishable from a genuinely decoded
	// "healthy" and healthFor silently (and wrongly) reports OK.
	haveHealth bool
	ura        int

	// Broadcast accuracy index and the table it decodes with (accNone/accURA/
	// accSISA) — backs the sisa_valid/sisa_m feed fields. BeiDou also carries an
	// age-of-clock / age-of-ephemeris pair, surfaced for that constellation only.
	accIdx     int
	accKind    uint8
	aodc, aode int
	haveAOD    bool

	pos     gnss.ECEF
	havePos bool
	// posAt is the propagation epoch that produced pos : the exact instant
	// Propagate passed to kepler.Propagate/glonass.Propagate, not "now" at feed
	// build time. tow/wn must be served from this epoch, not recomputed later, or
	// the (tow, position) pair served can disagree by the SV's orbital motion over
	// the skew between the tick and the feed build (up to km).
	posAt time.Time

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
	gloAlmanac map[int]gloAlmSlot
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
		gloAlmanac: make(map[int]gloAlmSlot),
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
	if f.Obs != nil {
		s.applyObservation(f)
		return
	}
	// byte-oriented frames (RTCM messages, SBF blocks) carry Bytes only
	// and no word-oriented Words -- but they also carry the RawFrame zero values
	// for GnssID/SigID (GPS/0), which matches the LNAV dispatch case below.
	// Without this guard, DecodeGPSLNAV(nil) fails on every single RTCM/SBF
	// message (e.g. once per second on a typical MSM stream), burying real LNAV
	// decode errors under a permanently-red counter.
	if f.Words == nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "byte_frame").Inc()
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
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}
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
	// the capability fingerprint is decoded-nav-frame evidence and durable
	// by design; record it only once decode has actually succeeded, never before.
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}
	// PreambleOK previously gated nothing — even a message whose preamble
	// didn't match one of the three ICD-mandated SBAS values (0x53/0x9A/0xC6) still
	// updated doNotUse/lastType. Now (with CRC-24Q check in DecodeSBASL1
	// already ruling out most corruption) this is defense in depth: a structurally
	// self-consistent but non-standard-preamble message is still not trusted for
	// the do-not-use alarm.
	if !m.PreambleOK {
		return
	}

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
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}

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
	st.health, st.haveHealth, st.ura = st.sf1.Health, true, st.sf1.URAIndex
	st.accKind, st.accIdx = accURA, st.sf1.URAIndex
}

func (s *Store) applyGalileoINAV(f *ingest.RawFrame) {
	w, err := frame.DecodeGalileoINAV(f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "inav").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "inav").Inc()
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}

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

	// Word type 5 carries E1B health and BGD (not part of the IODnav-matched
	// ephemeris set); fold its health in as it arrives (docs/CONSTELLATIONS.md
	// §2.2) and refresh the already-assembled clock's TGD without treating this as
	// a new ephemeris (no IODnav change, so no disco recompute).
	if w.Type == 5 {
		st.health, st.haveHealth = w.Health, true
		st.galW[5] = w
		if st.haveEph && st.galW[1] != nil && st.galW[2] != nil && st.galW[3] != nil && st.galW[4] != nil {
			if _, clk, err := frame.AssembleGalileo(f.SvID, st.galW[1], st.galW[2], st.galW[3], st.galW[4], st.galW[5]); err == nil {
				st.clk.TGD = clk.TGD
			}
		}
		return
	}
	if w.Type < 1 || w.Type > 4 {
		return // only word types 1–4 assemble the ephemeris
	}
	st.galW[w.Type] = w
	if st.galW[1] == nil || st.galW[2] == nil || st.galW[3] == nil || st.galW[4] == nil {
		return
	}

	eph, clk, err := frame.AssembleGalileo(f.SvID, st.galW[1], st.galW[2], st.galW[3], st.galW[4], st.galW[5])
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
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}

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
	st.health, st.haveHealth = st.bd1.Health, true
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
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}

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
		return // types 10/11 not broadcast-adjacent, or the clock is too stale 
	}
	// an ephemeris changeover (IODE) and a clock changeover (IODC) are
	// independent events. Gating the whole update on IODE (as before) silently
	// dropped legitimate same-IODE clock refreshes (a new af0/af1/af2 under the
	// same ephemeris) forever, since the function returned before ever reaching
	// the st.clk assignment below. disco stays keyed on IODE alone (an ephemeris
	// changeover is what "disco" measures); a clock-only refresh must still
	// update st.clk so served af0/af1/af2 don't run stale between IODE changes.
	ephChanged := !st.haveEph || st.bc10.IODE != st.iod
	clkChanged := st.bc30 != nil && (!st.haveBcIOD || st.bc30.IODC != st.bcIODC)
	if !ephChanged && !clkChanged {
		return
	}
	if ephChanged && st.haveEph {
		s.computeDisco(st, eph, clk, f.Recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, st.bc10.IODE, true
	if st.bc30 != nil {
		st.bcIODC, st.haveBcIOD = st.bc30.IODC, true
	}
	st.health, st.haveHealth = st.bc11.HS, true
}

func (s *Store) applyGLONASS(f *ingest.RawFrame) {
	str, err := frame.DecodeGLONASSString(f.Words)
	if err != nil {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "glo").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "glo").Inc()
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, f.Recv)
	}

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
		st.gloS1, st.gloS1At = str, f.Recv
	case str.Number == 2:
		st.gloS2, st.gloS2At = str, f.Recv
		st.health, st.haveHealth = str.Health, true
	case str.Number == 3:
		st.gloS3, st.gloS3At = str, f.Recv
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
			s.applyGloAlmanac(st.gloAlmFirst, f.Words, f.Recv)
		}
		st.gloAlmFirst = nil
		return
	default:
		return // string 4 (reserved here)
	}
	if st.gloS1 == nil || st.gloS2 == nil || st.gloS3 == nil {
		return
	}
	// strings 1/2/3 are coherent only within one ~30s frame (broadcast order
	// 1,2,3, each ~2s apart); reassembling on every arrival with no temporal guard
	// mixes epochs at every tb changeover (a fresh string arriving pairs with the
	// other two still-cached, up-to-30-minutes-old strings). Gate on all three
	// having arrived within one frame window of each other.
	oldest, newest := st.gloS1At, st.gloS1At
	for _, t := range []time.Time{st.gloS2At, st.gloS3At} {
		if t.Before(oldest) {
			oldest = t
		}
		if t.After(newest) {
			newest = t
		}
	}
	if newest.Sub(oldest) > glonassFrameWindow {
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

// gloAlmSlot is one GLONASS almanac subject slot's decoded entry plus the wall-clock
// time it was last (re)broadcast. the entry itself carries no wall-clock recency
// (Alm.NA is a broadcast day-number, not a receive timestamp), so without lastSeen a
// decommissioned slot's last-ever almanac would be served forever, propagated out to an
// ever-more-speculative position at the current day number.
type gloAlmSlot struct {
	entry    frame.GLONASSAlmanacEntry
	lastSeen time.Time
}

// gloAlmanacStaleAfter bounds how long a GLONASS almanac slot may go un-rebroadcast
// before it is dropped from the feed. Every operating satellite retransmits the
// whole constellation's almanac roughly every ~2.5h broadcast cycle, so this is generous
// margin over normal operation while still catching a slot the constellation's ground
// control has actually retired (which stops appearing in any satellite's broadcast
// almanac table within a few cycles, not indefinitely).
const gloAlmanacStaleAfter = 3 * 24 * time.Hour

// applyGloAlmanac decodes one satellite's almanac from its two-string pair and stores it
// by subject slot, alongside recv as the slot's last-(re)broadcast time. Decoding is pure
// and done outside the lock; only the map write is guarded. Called with the transmitting
// SV's shard lock held (ordering shard→gloAlm).
func (s *Store) applyGloAlmanac(first, second []uint32, recv time.Time) {
	s.gloAlmMu.Lock()
	na := s.gloNA
	s.gloAlmMu.Unlock()
	// gloNA starts at its zero value until string 5 has actually been
	// decoded (setGloNA only ever writes a validated 1..1461); storing an
	// almanac pair against na==0 (outside that range) mis-epochs
	// PropagateAlmanacECEF for every entry decoded before the first string 5.
	if na == 0 {
		return
	}

	a, err := frame.DecodeGLONASSAlmanac(first, second, na)
	if err != nil || a.Alm.Slot < 1 || a.Alm.Slot > 24 {
		return
	}
	s.gloAlmMu.Lock()
	s.gloAlmanac[a.Alm.Slot] = gloAlmSlot{entry: a, lastSeen: recv}
	s.gloAlmMu.Unlock()
}

// finite reports whether v is neither NaN nor ±Inf — encoding/json.Marshal fails
// the whole envelope on either, so a non-finite float must never reach a feed
// (the "absent = unknown, never a sentinel" contract: treat it as unset, not 0).
func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}

// finiteECEF reports whether every component of an ECEF position is finite.
func finiteECEF(p gnss.ECEF) bool {
	return finite(p.X) && finite(p.Y) && finite(p.Z)
}

// computeDisco propagates the outgoing and incoming ephemerides to the new
// reference epoch and records orbit-disco (metres) and time-disco (ns). Guards
// require both propagations to succeed; this is not the first ephemeris
// (guaranteed by the caller). A violated guard leaves the field invalid (absent in
// the feed), never a sentinel.
func (s *Store) computeDisco(st *svState, newEph kepler.Ephemeris, newClk clock.Model, now time.Time) {
	tstar := newEph.Toe

	// don't trust a disco computed against an outgoing ephemeris that's gone
	// stale. docs/INTEGRITY.md §3 requires "both ephemerides are ephAge < 4h old" at
	// the changeover epoch tstar; newEph's age there is 0 by construction (tstar ==
	// newEph.Toe), so this reduces to checking the outgoing set's age. An SV unseen
	// for hours and then refreshed would otherwise propagate an arbitrarily stale
	// outgoing ephemeris out to tstar, producing a physically meaningless (but
	// detector-triggering) discontinuity.
	if math.Abs(gnsstime.EphAge(tstar, st.eph.Toe)) >= discoTrustAge.Seconds() {
		st.orbitDiscoValid = false
		st.timeDiscoValid = false
		st.discoAt = now
		return
	}

	oldPos, e1 := kepler.Propagate(st.eph, tstar)
	newPos, e2 := kepler.Propagate(newEph, tstar)
	if e1 == nil && e2 == nil {
		disco := newPos.Sub(oldPos).Norm()
		if !math.IsNaN(disco) && !math.IsInf(disco, 0) {
			st.orbitDisco = disco
			st.orbitDiscoValid = true
		} else {
			st.orbitDiscoValid = false
		}
	} else {
		st.orbitDiscoValid = false
	}
	oOff, e3 := clock.OffsetFor(st.clk, st.eph, tstar)
	nOff, e4 := clock.OffsetFor(newClk, newEph, tstar)
	if e3 == nil && e4 == nil {
		disco := math.Abs(nOff-oOff) * 1e9
		if !math.IsNaN(disco) && !math.IsInf(disco, 0) {
			st.timeDiscoNs = disco
			st.timeDiscoValid = true
		} else {
			st.timeDiscoValid = false
		}
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
				if pos, err := glonass.Propagate(st.gloEph, tk); err == nil && finiteECEF(pos) {
					st.pos, st.havePos, st.posAt = pos, true, now
				}
				counts["glonass"]++
				continue
			}
			if !st.haveEph {
				continue
			}
			tow := towFor(st.key.G, now)
			if pos, err := kepler.Propagate(st.eph, tow); err == nil && finiteECEF(pos) {
				st.pos, st.havePos, st.posAt = pos, true, now
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
