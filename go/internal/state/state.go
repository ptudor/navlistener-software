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
	"errors"
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

func countDecodeFailure(f *ingest.RawFrame, kind string, err error) {
	if errors.Is(err, frame.ErrBadCRC) || errors.Is(err, frame.ErrBadPreamble) ||
		errors.Is(err, frame.ErrBadTLMPreamble) || errors.Is(err, frame.ErrBadBCH) ||
		errors.Is(err, frame.ErrGLONASSHamming) {
		metrics.NavCRCFailTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), fmt.Sprint(f.SigID)).Inc()
		return
	}
	metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), kind).Inc()
}

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

	// propagateMaxEphAge (regression fix, the documented regression fix remainder) caps how long
	// past its wall-clock apply time (svState.ephAt) a Kepler-family ephemeris may
	// keep being propagated into served positions. The propagation age tk is
	// derived from gnsstime.EphAge, which wraps to ±half-week (302400 s): past
	// ~3.5 days every derived signal self-defeats simultaneously — the propagated
	// position is thousands of km wrong but finite (so posAt keeps advancing and
	// posStaleBound never fires), eph_age_m wraps back toward zero, and the
	// eph_aged detector consumes that wrapped near-zero value — silent wrong
	// output stamped fresh. Reachable via the regression fix window: RAWX observables keep
	// an SV lastSeen-fresh for days while its nav decode is dead. 72 h sits
	// safely below the 302400 s wrap (12 h margin), so a served position can
	// never come from a wrapped ephemeris and the SOW-based eph_age_m stays
	// truthful over the whole served regime; routine hours-past-fit extrapolation
	// (surfaced via eph_age_m / eph_aged) is untouched by design. Past the cap,
	// Propagate skips the SV, posAt stops advancing, and posStaleBound expires
	// the served position naturally (position_unknown then fires). The GLONASS
	// twin of this cap is gloPropagateMaxEphAge.
	propagateMaxEphAge = 72 * time.Hour

	// gloPropagateMaxEphAge (regression fix, the GLONASS twin of propagateMaxEphAge) caps
	// how long past its wall-clock apply time (svState.gloEphAt) a GLONASS
	// immediate ephemeris keeps being propagated into served positions. GLONASS is
	// categorically sharper than the Kepler family here: the broadcast state
	// vector is referred to the MIDDLE of its tb interval and updated every
	// 30/45/60 min (GLO-ICD-5.1 §4.4, Table 4.3), the simplified J₂+constant-
	// luni-solar model is characterized by the ICD only over that ±15-min span,
	// and the RK4's error COMPOUNDS with distance instead of staying a closed-form
	// conic. Worse, the propagation interval tk comes from gnsstime.EphAgeDay,
	// which wraps at ±half a DAY (GLONASS has no week/TOW): past +12 h the served
	// eph_age_m flips negative (un-firing eph_aged on a worsening SV) and the RK4
	// integrates the frozen state ~12 h BACKWARD — a position on the wrong side of
	// the orbit, stamped fresh. 60 min = the maximum broadcast tb update interval
	// (Table 4.3): one full missed changeover of margin over the fit interval,
	// far below the 12 h wrap horizon, and deliberately NOT the 4 h discoTrustAge
	// (that gate bounds disco comparisons, not position validity). Past the cap,
	// Propagate skips the SV (posAt stops advancing, posStaleBound retires the
	// served position, position_unknown fires) while the entry keeps serving
	// health/metadata, and feedSV switches eph_age_m to the un-wrappable
	// wall-clock age so eph_aged latches and stays latched.
	gloPropagateMaxEphAge = 60 * time.Minute

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
	// GPS/QZSS L2C/L5 CNAV message assembly buffers, keyed like BeiDou
	// B-CNAV2's bc10/bc11/bcClk: gcClk is the last clock-bearing MT30–37.
	gc10, gc11, gcClk *frame.GPSCNAV
	// Galileo I/NAV word assembly buffers, indexed by word type 1–5 (word 5 carries
	// BGD/health, not part of the ephemeris set; index 0 unused).
	galW [6]*frame.GalileoINAV
	// Galileo E5a F/NAV page assembly buffers, indexed by page type 1–4 (index 0 unused).
	// Kept separate from galW because F/NAV is a distinct signal (E5a, u-blox sigId 3/4) with
	// its own svState entry, mirroring BeiDou B-CNAV2 (sigId 8) rather than overwriting the
	// E1-B I/NAV set. regression fix; see applyGalileoFNAV (lightly tested on real hardware 2026-07-12).
	fnav [5]*frame.GalileoFNAV
	// BeiDou D1 subframe assembly buffers.
	bd1, bd2, bd3 *frame.BeiDouSubframe
	// BeiDou B2a B-CNAV2 message assembly buffers. bcClk is the last clock-bearing
	// MT30/34; bc30 is retained separately because only MT30 carries TGD_B2ap.
	bc10, bc11, bc30, bcClk *frame.BeiDouBCNAV2
	// Measured-iono tracks per ingest source (dual-frequency observables).
	ionoBySource map[string]*ionoTrack
	// GLONASS string assembly buffers + Cartesian ephemeris (RK4, not kepler).
	// gloS{1,2,3}At are each string's reception time : strings 1/2/3 carry
	// x/y/z of one PZ-90 state valid only within one ~30 s frame, and unlike
	// GPS/Galileo/BeiDou they carry no per-changeover tag, so recency is the only
	// coherence guard — assembly is gated on all three having arrived within one
	// frame window of each other.
	gloS1, gloS2, gloS3, gloS4         *frame.GLONASSString
	gloS1At, gloS2At, gloS3At, gloS4At time.Time
	gloEph                             glonass.Ephemeris
	gloFreqID                          int
	haveGloEph                         bool
	// gloEphAt is the wall-clock apply time of the current GLONASS ephemeris,
	// the analog of ephAt: the GLONASS disco staleness gate uses it.
	gloEphAt time.Time
	// GLONASS time-disco is deferred : the tb changeover is detected at the string-3
	// (clockless) assembly, but the incoming clock (τn/γn) rides string 4, which completes
	// the same-tb set ~2 s later. At the changeover we retain the OUTGOING clock and the
	// awaited tb, then complete the time-disco when string 4 supplies the new clock.
	discoPendClk                           bool
	discoPendTb                            float64
	discoOldTau, discoOldGamma, discoOldTb float64
	// Buffered first string of an almanac pair (6/8/10/12/14), awaiting its second
	// (7/9/11/13/15) from the same transmitting satellite.
	gloAlmFirst    []uint32
	gloAlmFirstNum int
	// gloAlmFirstAt is the reception time of the buffered even string : the odd
	// string must arrive within one frame window, or the pair is a cross-frame chimera
	// (each frame's strings 6/7 describe a DIFFERENT subject satellite) and must be dropped.
	gloAlmFirstAt time.Time
	// gloFrameBaseSlot is the subject slot of the current frame's first almanac pair
	// (strings 6/7), used to detect frame 5. Frame 5 carries almanac only for slots
	// 21–24 (strings 6–13); its strings 14/15 are B1/B2/KP UT1/leap data, NOT almanac, so a
	// base slot ≥ 21 means strings 14/15 must not be paired as an almanac. Reset to 0 when a
	// new frame's string 6 is buffered so a stale frame-5 base can't linger.
	gloFrameBaseSlot int

	eph     kepler.Ephemeris
	clk     clock.Model
	haveEph bool
	// haveClk (regression fix, the regression fix/regression fix discipline applied to the clock): st.clk
	// is meaningful only when true. The LNAV/I-NAV/F-NAV/D1 paths always decode a
	// clock with the ephemeris, but the CNAV and B-CNAV2 message families carry
	// the clock in separate messages — an ephemeris can assemble clockless, and
	// serving the zero model's af0=0 as if decoded would be a fabricated sentinel.
	haveClk bool
	iod     int
	// lnavIODC (regression fix, the GPS twin of bcIODC) is the full 10-bit IODC
	// of the last applied LNAV data set. The 8-bit IODE equals the IODC's low 8
	// bits (IS-GPS-200N §20.3.4.4), so an IODE-keyed gate alone drops a data set
	// whose IODC changed only in its two high bits — reachable after a >6 h
	// decode gap, since §20.3.4.4's no-repeat rule only spans six hours — and
	// its refreshed af0/af1/af2/Toc/TGD with it. Clock refresh keys on the full
	// IODC; disco stays keyed on IODE (a changeover is what disco measures).
	lnavIODC     int
	haveLnavIODC bool
	// ephAt is the wall-clock instant this ephemeris was applied. computeDisco's
	// staleness gate must use it, not gnsstime.EphAge(tstar, Toe): EphAge wraps its result
	// to ±half-week, so an outgoing ephemeris ~1 week stale reads as fresh and produces the
	// exact phantom critical disco the regression fix gate exists to prevent. Wall-clock cannot wrap.
	ephAt time.Time
	// bcIODC is BeiDou B-CNAV2's type-30/34 clock IODC, tracked separately
	// from iod (the type-10/11 ephemeris IODE) so a clock-only refresh (same
	// ephemeris, new af0/af1/af2) is not silently dropped by the ephemeris IODE
	// gate below — the two message families change independently.
	bcIODC    int
	haveBcIOD bool
	// TGD_B2ap is a quasi-static data-set property sourced only from MT30. Track
	// its provenance separately so MT34 clocks never turn an unknown TGD into a
	// decoded zero or create a false time discontinuity.
	bcTGD                  float64
	haveBcTGD, clkHasBcTGD bool
	health                 int
	// haveHealth is health_code 0 ("unknown") is the correct answer until
	// this SV's own health bits have actually been decoded (e.g. a Galileo SV
	// with only word types 1-4 assembled -- health arrives on word 5) or an
	// iono-only SV  nothing is known about at all. Without this flag,
	// st.health's zero value is indistinguishable from a genuinely decoded
	// "healthy" and healthFor silently (and wrongly) reports OK.
	haveHealth bool
	ura        int
	// alert is the GPS/QZSS broadcast URA-alert flag : LNAV HOW
	// bit 18 / CNAV header bit 38 — the SV's own "URA may be worse than indicated,
	// use at own risk" declaration (IS-GPS-200N §20.3.3.2, §6.4.6.3). Freshest-wins
	// like health (it rides every subframe/message, outside any IOD-gated set);
	// haveAlert follows the regression fix unknown-until-decoded discipline.
	alert     bool
	haveAlert bool
	// wnMismatch : the decoded broadcast week number (LNAV 10-bit /
	// CNAV 13-bit), rollover-disambiguated, disagrees with the collector
	// wall-clock GPS week — see checkBroadcastWN. haveWN follows regression fix.
	wnMismatch bool
	haveWN     bool

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
	accNone  uint8 = 0 // no accuracy captured for this constellation/signal
	accURA   uint8 = 1 // GPS/QZSS/NavIC/BeiDou-B1I URA step table
	accSISA  uint8 = 2 // Galileo SISA linear bands
	accURAED uint8 = 3 // GPS/QZSS CNAV signed URA_ED (IS-GPS-200N §30.3.3.1.1.4), regression fix
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
		metrics.CapturedOnlyTotal.WithLabelValues(f.Source, "byte_frame").Inc()
		return
	}
	switch {
	case (f.GnssID == gnss.GPS || f.GnssID == gnss.QZSS) && f.SigID == 0:
		s.applyGPSLNAV(f)
	case f.GnssID == gnss.Galileo && (f.SigID == 0 || f.SigID == 1): // E1-B I/NAV
		s.applyGalileoINAV(f)
	case f.GnssID == gnss.Galileo && (f.SigID == 3 || f.SigID == 4): // E5a F/NAV (regression fix; validated 2026-07-12 vs I/NAV — see applyGalileoFNAV)
		s.applyGalileoFNAV(f)
	case f.GnssID == gnss.BeiDou && f.SigID == 0: // B1I D1 NAV
		s.applyBeiDouD1(f)
	case f.GnssID == gnss.BeiDou && f.SigID == 8: // B2a data component, B-CNAV2
		s.applyBeiDouBCNAV2(f)
	case f.GnssID == gnss.GLONASS && (f.SigID == 0 || f.SigID == 2): // L1OF/L2OF strings
		// L2OF (6,2) carries the byte-identical 85-bit string format to L1OF and is
		// contract-shipped (0x40 GloNav in all three frame tables). applyGLONASS keys GLONASS
		// state at Sig:0, so L1OF and L2OF strings for one SV merge into one state entry —
		// they carry the same navigation data. Without this, every L2OF string on an
		// L2-tracking receiver was dropped as "unsupported".
		s.applyGLONASS(f)
	case (f.GnssID == gnss.GPS || f.GnssID == gnss.QZSS) && isCNAVSignal(f.GnssID, f.SigID):
		s.applyGPSCNAV(f)
	case f.GnssID == gnss.SBAS && f.SigID == 0: // L1 C/A message stream
		s.applySBAS(f)
	case f.GnssID == gnss.NavIC:
		// NavIC (IRNSS SPS) — STUB / TRACKED DEFERRAL, not decoded.
		// Unlike Galileo F/NAV (already decoded, this pass just wired dispatch), NavIC's L5 SPS
		// frame is a from-scratch ICD format AND we have no live NavIC stream to validate a
		// decoder against — NavIC is below the horizon from our stations, so a real feed must be
		// sourced in Asia (see navlistener.toml.example). Wiring a decoder we cannot test would
		// be untestable guesswork, so it stays a stub (gnss/frame/navic.go) until such a source
		// exists. Counted under its own label so the deferral is visible in /metrics instead of
		// being hidden in the generic "unsupported" bucket.
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "navic_deferred").Inc()
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

// cnavCarrierHealth extracts the tracked carrier's own 1-bit health from the
// CNAV MT10 3-bit L1/L2/L5 field (bits 52–54 in broadcast order L1, L2, L5 —
// IS-GPS-200N §30.3.3.1.1.2; QZSS-PNT-006 §4.3.2 mirrors the layout): the
// dispatch map (isCNAVSignal, docs/CONSTELLATIONS.md §2.1) says GPS sigId 3/4
// and QZSS sigId 4/5 are L2C, GPS 6/7 and QZSS 8/9 are L5. Storing only the
// tracked carrier's bit keeps healthFor's per-signal arm a plain 0/1 test.
func cnavCarrierHealth(id gnss.GNSSID, sig, h3 int) int {
	l2, l5 := (h3>>1)&1, h3&1
	if id == gnss.GPS {
		if sig == 3 || sig == 4 {
			return l2
		}
		return l5
	}
	if sig == 4 || sig == 5 { // QZSS
		return l2
	}
	return l5
}

// applyGPSCNAV  decodes a GPS/QZSS L2C/L5 CNAV message and folds it
// into a SEPARATE per-signal SV state keyed on the frame's own sigId — the same
// secondary-signal pattern as Galileo E5a F/NAV (Sig:3) and BeiDou B-CNAV2
// (Sig:8) — so L2C/L5 surface as their own name@sigid feed entries carrying the
// ONLY per-signal health GPS broadcasts (MT10's L1/L2/L5 bits, §30.3.3.1.1.2),
// the signed URA_ED, the header alert flag, the 13-bit WN cross-check
//, and an independently-assembled CNAV ephemeris/clock. This replaces
// the decode-and-drop stub whose "consumed in the integrity pass (P6)" comment
// had been overtaken by P6 landing without it: the LNAV-vs-CNAV cross-signal
// comparison consumes these entries exactly the way the E1-B-vs-E5a pair
// already does — two independently decoded positions/clocks for one SV.
func (s *Store) applyGPSCNAV(f *ingest.RawFrame) {
	m, err := frame.DecodeGPSCNAV(f.GnssID, f.Words)
	if err != nil {
		countDecodeFailure(f, "cnav", err)
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "cnav").Inc()
	recv := f.LocalRecv() // collector-local clock for staleness/expiry math 
	// capability evidence only after a structurally valid decode.
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
	}
	// Only the ephemeris (10/11) and clock (30–37) messages build SV state; the
	// almanac/EOP/UTC types are capability evidence only — the B-CNAV2
	// types-31/32/33/40 precedent.
	if m.MsgType != 10 && m.MsgType != 11 && (m.MsgType < 30 || m.MsgType > 37) {
		return
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
	st.lastSeen = recv
	// the alert flag rides every CNAV header — freshest-wins, as LNAV.
	st.alert, st.haveAlert = m.Alert, true

	switch {
	case m.MsgType == 10:
		st.gc10 = m
		// regression fix freshest-wins, BEFORE the toe-changeover gate below: per-carrier
		// health, signed URA_ED, and the broadcast-WN cross-check reach live
		// state on every MT10, not only at data-set cutovers (Alert/URA-class
		// fields are the ICD's NOTE1 "CEI refinement" set — they may change
		// without a toe change, IS-GPS-200N Table 6-I-1).
		st.health, st.haveHealth = cnavCarrierHealth(f.GnssID, f.SigID, m.Health), true
		st.accKind, st.accIdx = accURAED, m.URAED
		st.checkBroadcastWN(recv, m.WN, 13) // regression fix
	case m.MsgType == 11:
		st.gc11 = m
	default: // MT30–37 all carry the common clock block
		st.gcClk = m
	}
	if st.gc10 == nil || st.gc11 == nil {
		return
	}
	eph, clk, clkOK, err := frame.AssembleGPSCNAV(f.GnssID, f.SvID, st.gc10, st.gc11, st.gcClk)
	if err != nil {
		return // MT10/MT11 from different data sets (toe mismatch); wait for a coherent pair
	}
	// CNAV carries no IODE/IODC: toe IS the data-set key (IS-GPS-200N §30.3.4.4
	// — updates to curve-fit parameters "shall prompt changes in toe/toc"; the
	// BeiDou D1 toe-keyed precedent). The clock shares that key (toc == toe when
	// clkOK, regression fix), so the only same-toe refresh to catch is a coherent clock
	// arriving after a clockless assembly.
	ephChanged := !st.haveEph || int(eph.Toe) != st.iod
	clkAttached := clkOK && !st.haveClk
	if !ephChanged && !clkAttached {
		return
	}
	if ephChanged && st.haveEph {
		s.computeDisco(st, eph, clk, recv)
		// Time-disco needs a coherent decoded clock on BOTH sides (the regression fix
		// absent-not-zero rule); computeDisco differenced the zero model
		// otherwise. Orbit-disco stands either way.
		if !clkOK || !st.haveClk {
			st.timeDiscoValid = false
		}
	}
	if !clkOK {
		clk = st.clk // keep the previously applied clock; haveClk gates serving it
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, int(eph.Toe), true
	st.ephAt = recv // regression fix (collector-local, regression fix)
	if clkOK {
		st.haveClk = true
	}
}

// applySBAS decodes an SBAS L1 message and folds it into the per-PRN augmentation
// health map that backs the sbas feed (docs/OUTPUT.md §1.5): the last message type,
// the provider from the PRN, and the last type-0 (do-not-use) transition.
func (s *Store) applySBAS(f *ingest.RawFrame) {
	m, err := frame.DecodeSBASL1(f.SvID, f.Words)
	if err != nil {
		countDecodeFailure(f, "sbas", err)
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "sbas").Inc()
	// all recency below is the collector-local clock — staleness ages
	// (sbasStaleAfter, capability windows) must never absorb feeder clock skew.
	recv := f.LocalRecv()
	// the capability fingerprint is decoded-nav-frame evidence and durable
	// by design; record it only once decode has actually succeeded, never before.
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
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
	st.lastSeen = recv
	st.lastType = m.Type
	st.doNotUse = m.DoNotUse
	if m.DoNotUse {
		st.lastType0, st.haveType0 = recv, true
	}
}

func (s *Store) applyGPSLNAV(f *ingest.RawFrame) {
	sf, err := frame.DecodeGPSLNAV(f.Words)
	if err != nil {
		countDecodeFailure(f, "lnav", err)
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "lnav").Inc()
	recv := f.LocalRecv() // collector-local clock for all staleness/expiry/disco-age math 
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
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
	st.lastSeen = recv
	// the HOW alert flag rides EVERY subframe (1–5) and belongs to no
	// IODC-gated data set — apply freshest-wins before the subframe switch, so
	// even an almanac page's HOW keeps it current (the regression fix discipline).
	st.alert, st.haveAlert = sf.Alert, true

	switch sf.SubframeID {
	case 1:
		st.sf1 = sf
		// cross-check the broadcast 10-bit WN against the wall-clock week.
		st.checkBroadcastWN(recv, sf.WN, 10)
		// apply health/URA at subframe-1 arrival, BEFORE the IOD gate below, so a
		// health-bit flip that arrives under an unchanged IODC (a re-broadcast subframe 1,
		// or an IODC bump confined to its two high bits that leaves the low-8 IODE
		// unchanged) reaches live state, the feeds, and the detector promptly instead of
		// waiting for the next full ephemeris cutover. Trust comes from receiver-validated,
		// D30*-resolved SFRBX data plus DecodeGPSLNAV's fixed-preamble structural check;
		// freshest-wins health semantics do not wait for a full set. The eph-gated assignment below stays
		// (it is now a no-op for these scalars) so the ephemeris/clock IOD gating is
		// untouched.
		st.health, st.haveHealth, st.ura = sf.Health, true, sf.URAIndex
		st.accKind, st.accIdx = accURA, sf.URAIndex
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
	// regression fix (the GPS twin of regression fix, finishing what regression fix started for the
	// non-clock scalars): an ephemeris changeover keys on the 8-bit IODE, but the
	// clock scalars live in subframe 1 under the full 10-bit IODC — a data set
	// whose IODC changes only in its two high bits (same low-8 IODE, legal after
	// the §20.3.4.4 six-hour no-repeat horizon) must still refresh the served
	// af0/af1/af2/Toc/TGD instead of being dropped by the IODE gate forever.
	newIOD := st.sf2.IODE
	ephChanged := !st.haveEph || newIOD != st.iod
	clkChanged := !st.haveLnavIODC || st.sf1.IODC != st.lnavIODC
	if !ephChanged && !clkChanged {
		return // same data set, nothing new
	}
	if ephChanged {
		// A new ephemeris (new IODE): compute the changeover discontinuities
		// against the outgoing set before replacing it (docs/INTEGRITY.md §3).
		// A clock-only IODC refresh is not a changeover and computes no disco.
		if st.haveEph {
			s.computeDisco(st, eph, clk, recv)
		}
		st.eph, st.iod, st.haveEph = eph, newIOD, true
		st.ephAt = recv // collector-local apply time for the disco staleness gate 
	}
	// The clock comes wholly from subframe 1 (af0/af1/af2/Toc/TGD), so it is
	// self-coherent even on a clock-only refresh; residual (recorded): after a
	// >6 h gap an IODE-repeating data set's *ephemeris* still waits for the next
	// IODE change — a distinct, narrower staleness window than the clock one
	// this fixes.
	st.clk, st.haveClk = clk, true
	st.lnavIODC, st.haveLnavIODC = st.sf1.IODC, true
	st.health, st.haveHealth, st.ura = st.sf1.Health, true, st.sf1.URAIndex
	st.accKind, st.accIdx = accURA, st.sf1.URAIndex
}

func (s *Store) applyGalileoINAV(f *ingest.RawFrame) {
	w, err := frame.DecodeGalileoINAV(f.Words)
	if err != nil {
		countDecodeFailure(f, "inav", err)
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "inav").Inc()
	recv := f.LocalRecv() // regression fix
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
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
	st.lastSeen = recv

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
		s.computeDisco(st, eph, clk, recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, newIOD, true
	st.haveClk = true // this format's ephemeris set always carries the clock
	st.ephAt = recv   // regression fix (collector-local, regression fix)
	st.accKind, st.accIdx = accSISA, st.galW[3].SISA
}

// applyGalileoFNAV decodes a Galileo E5a F/NAV page (u-blox sigId 3 = E5a-I; 4 = E5a-Q, the
// dataless pilot, mapped here for parity with rawframe.go's NavType) and folds it into a
// SEPARATE per-signal SV state keyed on E5a (Sig:3) — the same secondary-signal pattern as
// BeiDou B-CNAV2 (Sig:8), so E5a surfaces as its own name@3 feed entry instead of overwriting
// the E1-B I/NAV (Sig:0) set. NB: E5b-I (sigId 5) also carries I/NAV, not F/NAV — do not route
// it here. F/NAV carries the same ephemeris as I/NAV on the E5a signal;
// decoding it here is "wire F/NAV" half and produces the cross-signal (I/NAV-vs-F/NAV)
// evidence the P6 integrity pass consumes (docs/CONSTELLATIONS.md §2.2).
//
// ⚠️ LIGHTLY TESTED ON REAL HARDWARE. First validated 2026-07-12 against a live E5a-I
// stream from a u-blox on capture-station → this collector on collector-host: 136 F/NAV pages decoded with
// ZERO decode errors, and the assembled @3 ephemerides matched the independently-decoded E1-B
// I/NAV (@0) positions bit-for-bit for every SV (E07/E13/…: identical x,y,z, same IODnav) — the
// cross-signal agreement the design wants. Caveats still open: a single receiver over a single
// session (not soaked across many IODnav changeovers / health flips); E5a-Q (sigId 4) is a
// dataless pilot never exercised; the F/NAV GST week/TOW is not frame-decoded, so a served wn
// derives from system time, not the page; and E5b-I I/NAV (sigId 5) is still undispatched.
func (s *Store) applyGalileoFNAV(f *ingest.RawFrame) {
	w, err := frame.DecodeGalileoFNAV(f.Words)
	if err != nil {
		countDecodeFailure(f, "fnav", err)
		return
	}
	if w.PageType == 63 {
		metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "fnav_dummy").Inc()
		return
	}
	// regression fix (IODnav-completeness): a right-length frame whose page type is outside the F/NAV
	// nominal set (1..6) is not decode evidence — count it as an error, mirroring the
	// structural-id gate the other decoders use, so a mis-tagged or corrupt E5a frame never
	// installs the durable capability fingerprint or counts as a healthy decode.
	if w.PageType < 1 || w.PageType > 6 {
		metrics.DecodeErrorsTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "fnav").Inc()
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "fnav").Inc()
	recv := f.LocalRecv() // regression fix
	// record the capability fingerprint only after a structurally valid decode.
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
	}

	// Only the ephemeris/clock pages (1..4) assemble a nav set; almanac pages 5/6 are
	// capability evidence only (like the BeiDou/GLONASS almanac message types, which the
	// dispatch above also decodes-and-drops without folding into the ephemeris).
	if w.PageType > 4 {
		return
	}

	key := Key{G: f.GnssID, Sv: f.SvID, Sig: f.SigID} // E5a keyed on its own sigId (3/4)
	sh := s.shardFor(key)
	sh.mu.Lock()
	defer sh.mu.Unlock()

	st := sh.m[key]
	if st == nil {
		st = &svState{key: key}
		sh.m[key] = st
	}
	st.lastSeen = recv

	st.fnav[w.PageType] = w
	// Page 1 carries SISA + the E5a Signal Health Status outside the IODnav-matched eph set;
	// fold them in as they arrive (mirrors the I/NAV word-5 health/SISA pattern).
	if w.PageType == 1 {
		st.health, st.haveHealth = w.E5aHS, true
		st.accKind, st.accIdx = accSISA, w.SISA
	}
	if st.fnav[1] == nil || st.fnav[2] == nil || st.fnav[3] == nil || st.fnav[4] == nil {
		return
	}
	eph, clk, err := frame.AssembleGalileoFNAV(f.SvID, st.fnav[1], st.fnav[2], st.fnav[3], st.fnav[4])
	if err != nil {
		return // pages from different IODnav; wait for a mutually consistent set
	}
	newIOD := st.fnav[1].IODnav
	if st.haveEph && newIOD == st.iod {
		return // same data set, nothing new
	}
	if st.haveEph {
		s.computeDisco(st, eph, clk, recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, newIOD, true
	st.haveClk = true // this format's ephemeris set always carries the clock
	st.ephAt = recv   // regression fix (collector-local, regression fix)
	st.accKind, st.accIdx = accSISA, st.fnav[1].SISA
}

func (s *Store) applyBeiDouD1(f *ingest.RawFrame) {
	sf, err := frame.DecodeBeiDouD1(f.Words)
	if err != nil {
		countDecodeFailure(f, "d1", err)
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "d1").Inc()
	recv := f.LocalRecv() // regression fix
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
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
	st.lastSeen = recv

	switch sf.FraID {
	case 1:
		st.bd1 = sf
		// apply health/URA/AOD at subframe-1 arrival, BEFORE the toe-changeover
		// gate below, so a SatH1 flip in a re-broadcast subframe 1 (unchanged toe) reaches
		// live state instead of being dropped for up to an hour. DecodeBeiDouD1 verifies
		// every delivered BCH(15,11,1) block before this freshest-wins update. The
		// eph-gated assignment below stays (now a no-op for these).
		st.health, st.haveHealth = sf.Health, true
		st.accKind, st.accIdx = accURA, sf.URAI
		st.aodc, st.aode, st.haveAOD = sf.AODC, sf.AODE, true
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
		s.computeDisco(st, eph, clk, recv)
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, newIOD, true
	st.haveClk = true // this format's ephemeris set always carries the clock
	st.ephAt = recv   // regression fix (collector-local, regression fix)
	st.health, st.haveHealth = st.bd1.Health, true
	st.accKind, st.accIdx = accURA, st.bd1.URAI
	st.aodc, st.aode, st.haveAOD = st.bd1.AODC, st.bd1.AODE, true
}

func (s *Store) applyBeiDouBCNAV2(f *ingest.RawFrame) {
	m, err := frame.DecodeBeiDouBCNAV2(f.Words)
	if err != nil {
		countDecodeFailure(f, "bcnav2", err)
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "bcnav2").Inc()
	recv := f.LocalRecv() // regression fix
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
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
	st.lastSeen = recv

	switch m.MesType {
	case 10:
		st.bc10 = m
	case 11:
		st.bc11 = m
		// apply health at type-11 arrival, BEFORE the IODE/IODC gate below, so an HS
		// flip in a re-broadcast type-11 (unchanged IODE, no fresh type-30) reaches live
		// state. Type 11 is CRC-checked by DecodeBeiDouBCNAV2. The eph-gated assignment
		// below stays (now a no-op).
		st.health, st.haveHealth = m.HS, true
	case 30:
		st.bc30, st.bcClk = m, m
		st.bcTGD, st.haveBcTGD = m.TGDB2ap, true
	case 34:
		st.bcClk = m
	default:
		return // types 31/32/33/40 (almanac/EOP/BGTO) not consumed here
	}
	if st.bc10 == nil || st.bc11 == nil {
		return
	}
	eph, clk, clkOK, err := frame.AssembleBeiDouBCNAV2(f.SvID, st.bc10, st.bc11, st.bcClk)
	if err != nil {
		return // types 10/11 not broadcast-adjacent (a stale clock no longer fails assembly)
	}
	// an MT34 clock has no TGD_B2ap field. Carry the most recently
	// decoded MT30 value as the quasi-static data-set property; until MT30 has
	// ever arrived, keep its provenance unknown rather than treating zero as
	// decoded. A later same-IODC MT30 is therefore an actionable TGD refresh.
	nextClkHasTGD := st.clkHasBcTGD
	if clkOK {
		nextClkHasTGD = st.bcClk.MesType == 30 || st.haveBcTGD
		if nextClkHasTGD {
			clk.TGD = st.bcTGD
		}
	}
	// an ephemeris changeover (IODE) and a clock changeover (IODC) are
	// independent events. Gating the whole update on IODE (as before) silently
	// dropped legitimate same-IODE clock refreshes (a new af0/af1/af2 under the
	// same ephemeris) forever, since the function returned before ever reaching
	// the st.clk assignment below. disco stays keyed on IODE alone (an ephemeris
	// changeover is what "disco" measures); a clock-only refresh must still
	// update st.clk so served af0/af1/af2 don't run stale between IODE changes.
	//
	// When the assembler dropped the cached type-30/34 as stale (clkOK false),
	// the returned clock is the zero model, not a decoded one: keep serving the
	// previously applied clock (BeiDou clock and ephemeris changeovers are
	// independent, so the old af0/af1/af2 remain the best estimate until a fresh
	// type-30/34 decodes), skip the time-disco (there is no fresh clock to
	// difference against — absent, not zero), and do not latch the stale
	// message's IODC as applied (a later fresh type-30 carrying that IODC must
	// still be recognized as a change).
	ephChanged := !st.haveEph || st.bc10.IODE != st.iod
	clkChanged := clkOK && (!st.haveBcIOD || st.bcClk.IODC != st.bcIODC)
	tgdRefresh := clkOK && nextClkHasTGD && (!st.clkHasBcTGD || clk.TGD != st.clk.TGD)
	if !ephChanged && !clkChanged && !tgdRefresh {
		return
	}
	if !clkOK {
		clk = st.clk
	}
	if ephChanged && st.haveEph {
		if clkOK && st.clkHasBcTGD == nextClkHasTGD {
			s.computeDisco(st, eph, clk, recv)
		} else {
			// A clock comparison across differing TGD provenance would turn an
			// unknown group delay into a false clock jump.
			st.timeDiscoValid = false
		}
	}
	st.eph, st.clk, st.iod, st.haveEph = eph, clk, st.bc10.IODE, true
	st.ephAt = recv // regression fix (collector-local, regression fix)
	if clkOK {
		st.bcIODC, st.haveBcIOD = st.bcClk.IODC, true
		st.clkHasBcTGD = nextClkHasTGD
		st.haveClk = true // regression fix family: af0/af1/af2 serve only once a type-30/34 has decoded
	}
	st.health, st.haveHealth = st.bc11.HS, true
}

func (s *Store) applyGLONASS(f *ingest.RawFrame) {
	str, err := frame.DecodeGLONASSString(f.Words)
	if err != nil {
		countDecodeFailure(f, "glo", err)
		return
	}
	metrics.DecodeTotal.WithLabelValues(fmt.Sprint(int(f.GnssID)), "glo").Inc()
	// regression fix — TWO clock domains in this function, deliberately:
	//   recv (collector-local) — staleness/expiry/disco-age math (lastSeen,
	//   gloEphAt, discoAt, almanac-slot recency): elapsed time against this
	//   host's own clock, immune to feeder skew and spool-replay rewinds.
	//   f.Recv (feeder stamp) — the regression fix/regression fix frame-coherence windows
	//   (gloS1At..gloS4At, gloAlmFirstAt): those guard BROADCAST adjacency of
	//   tag-less strings, and only the feeder's stamp preserves the on-air
	//   spacing. A spool replay (or a feeder draining a backlog burst) delivers
	//   strings milliseconds apart on the collector clock, so a local-clock
	//   window would pass strings from DIFFERENT frames across a tb changeover
	//   and assemble a chimera (X new, Y/Z old — AssembleGLONASS has no
	//   cross-string tb tag to catch it), firing a phantom multi-thousand-km
	//   orbit-disco. Feeder stamps from one station are self-consistent, which
	//   is all adjacency needs; the cross-station-skew wrong-REJECT this keeps
	//   is the conservative failure for an integrity monitor, the wrong-ACCEPT
	//   is not.
	recv := f.LocalRecv()
	if f.Source != "" {
		s.recordCapability(f.Source, f.GnssID, f.SigID, recv)
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
	st.lastSeen = recv
	st.gloFreqID = f.FreqID

	switch {
	case str.Number == 1:
		st.gloS1, st.gloS1At = str, f.Recv // feeder stamp: broadcast adjacency (regression fix, above)
	case str.Number == 2:
		st.gloS2, st.gloS2At = str, f.Recv
		st.health, st.haveHealth = str.Health, true
	case str.Number == 3:
		st.gloS3, st.gloS3At = str, f.Recv
	case str.Number == 4: // SV clock: τn/Δτn; joins the frame-window assembly below
		st.gloS4, st.gloS4At = str, f.Recv
	case str.Number == 5: // time string: carries the frame day-number NA
		if na, err := frame.DecodeGLONASSFrameNA(f.Words); err == nil {
			s.setGloNA(na)
		}
		return
	case str.Number >= 6 && str.Number <= 14 && str.Number%2 == 0: // first of an almanac pair
		st.gloAlmFirst = append(st.gloAlmFirst[:0], f.Words...)
		st.gloAlmFirstNum = str.Number
		st.gloAlmFirstAt = f.Recv // feeder stamp: broadcast adjacency (regression fix, above)
		if str.Number == 6 {
			st.gloFrameBaseSlot = 0 // new frame's first almanac; base slot set on pairing
		}
		return
	case str.Number >= 7 && str.Number <= 15 && str.Number%2 == 1: // second of an almanac pair
		// require the odd string within one frame window of its even mate. Without
		// it, a stale even string (from a fade a frame or more ago) pairs with a later
		// frame's odd string — but strings 6/7 of different frames describe DIFFERENT
		// subject satellites, so the merge is a chimera almanac stored under the wrong slot.
		if st.gloAlmFirst != nil && str.Number == st.gloAlmFirstNum+1 &&
			f.Recv.Sub(st.gloAlmFirstAt) <= glonassFrameWindow {
			// in frame 5 (base slot ≥ 21), strings 14/15 carry B1/B2/KP UT1/leap
			// data, not almanac — decoding them as an almanac pair stores garbage (a stable
			// misread of B1's bits) under a wrong slot, flip-flopping that slot every
			// superframe. Skip the pair there; frames 1–4 (base 1..16) pair normally, and an
			// unknown base (string 6 lost) conservatively still pairs (the slot-range guard
			// in applyGloAlmanac remains the backstop).
			if !(st.gloAlmFirstNum == 14 && st.gloFrameBaseSlot >= 21) {
				slot := s.applyGloAlmanac(st.gloAlmFirst, f.Words, recv)
				if st.gloAlmFirstNum == 6 && slot > 0 {
					st.gloFrameBaseSlot = slot
				}
			}
		}
		st.gloAlmFirst = nil
		return
	default:
		return
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
	// string 4 (τn/Δτn) belongs to the same immediate-data frame as
	// strings 1–3 (broadcast order 1,2,3,4, ~2 s apart). Include it only when it
	// falls inside the same frame window — a stale string 4 from a previous
	// frame must not pair with fresh strings 1–3, exactly the regression fix rule. When
	// excluded the set assembles clockless (ClockKnown false) and the clock
	// arrives ~2 s later when string 4 completes the frame and reassembles.
	s4 := st.gloS4
	if s4 != nil {
		lo, hi := oldest, newest
		if st.gloS4At.Before(lo) {
			lo = st.gloS4At
		}
		if st.gloS4At.After(hi) {
			hi = st.gloS4At
		}
		if hi.Sub(lo) > glonassFrameWindow {
			s4 = nil
		}
	}
	eph, err := frame.AssembleGLONASS(f.SvID, st.gloFreqID, st.gloS1, st.gloS2, st.gloS3, s4)
	if err != nil {
		return
	}
	// compute the orbit/time discontinuity across a tb changeover before replacing
	// the outgoing set — the same integrity metric the Kepler family gets from computeDisco.
	// An identical tb (e.g. an L2OF string re-assembling the same set, regression fix) is not a
	// changeover and does no disco work.
	if st.haveGloEph && eph.Tb != st.gloEph.Tb {
		s.computeGloDisco(st, eph, recv)
	}
	// complete a deferred time-disco once the incoming set's clock (string 4)
	// arrives for the tb we recorded at the changeover.
	if st.discoPendClk && eph.ClockKnown && eph.Tb == st.discoPendTb {
		tkOld := gnsstime.EphAgeDay(eph.Tb, st.discoOldTb)
		oldOff := st.discoOldTau - st.discoOldGamma*tkOld
		if d := math.Abs(eph.TauN-oldOff) * 1e9; finite(d) {
			st.timeDiscoNs = d
			st.timeDiscoValid = true
			st.discoAt = recv
		}
		st.discoPendClk = false
	}
	st.gloEph, st.haveGloEph = eph, true
	st.gloEphAt = recv // collector-local, as ephAt
}

// computeGloDisco records the orbit/time discontinuity across a GLONASS tb changeover
//, mirroring computeDisco for the Kepler family. It propagates the outgoing and
// incoming ephemerides to the incoming tb and differences position (orbit_disco_m) and the
// SV clock model (time_disco_ns). Guards match computeDisco: the outgoing set must be
// fresher than discoTrustAge by wall clock (which cannot wrap, regression fix), and both propagations
// must be finite; time-disco is skipped (only) when either side lacks a decoded clock
// (ClockKnown false), mirroring the regression fix B-CNAV2 handling. Called before st.gloEph is
// replaced, with the shard lock held. Reuses st.orbitDisco*/timeDisco*/discoAt so the feed
// and detector consume it unchanged.
func (s *Store) computeGloDisco(st *svState, newEph glonass.Ephemeris, now time.Time) {
	st.discoAt = now
	st.orbitDiscoValid = false
	st.timeDiscoValid = false
	if st.gloEphAt.IsZero() || now.Sub(st.gloEphAt) >= discoTrustAge {
		return
	}
	// Propagate the outgoing set forward by the day-wrapped interval from its tb to the new
	// tb; the incoming set sits at tk=0 (its own reference epoch).
	tkOld := gnsstime.EphAgeDay(newEph.Tb, st.gloEph.Tb)
	oldPos, e1 := glonass.Propagate(st.gloEph, tkOld)
	newPos, e2 := glonass.Propagate(newEph, 0)
	if e1 == nil && e2 == nil {
		if d := newPos.Sub(oldPos).Norm(); finite(d) {
			st.orbitDisco = d
			st.orbitDiscoValid = true
		}
	}
	// Time-disco: difference the two SV clock corrections at the common epoch (the new tb).
	// The old model evaluated at the new tb is τn − γn·(tb_new − tb_old); the new model at
	// its own tb is τn (the γ term vanishes). The outgoing set must carry a clock to compare
	// against; otherwise time-disco is genuinely unknowable and skipped (the regression fix rule).
	st.discoPendClk = false
	if !st.gloEph.ClockKnown {
		return
	}
	if newEph.ClockKnown {
		oldOff := st.gloEph.TauN - st.gloEph.GammaN*tkOld
		if d := math.Abs(newEph.TauN-oldOff) * 1e9; finite(d) {
			st.timeDiscoNs = d
			st.timeDiscoValid = true
		}
		return
	}
	// The incoming set is clockless at the changeover (string 4 not yet in this frame): retain
	// the outgoing clock and defer the time-disco until string 4 completes the same-tb set.
	st.discoPendClk = true
	st.discoPendTb = newEph.Tb
	st.discoOldTau, st.discoOldGamma, st.discoOldTb = st.gloEph.TauN, st.gloEph.GammaN, st.gloEph.Tb
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
// SV's shard lock held (ordering shard→gloAlm). Returns the stored subject slot (1..24), or
// 0 if nothing was stored (used by frame-5 detection).
func (s *Store) applyGloAlmanac(first, second []uint32, recv time.Time) int {
	s.gloAlmMu.Lock()
	na := s.gloNA
	s.gloAlmMu.Unlock()
	// gloNA starts at its zero value until string 5 has actually been
	// decoded (setGloNA only ever writes a validated 1..1461); storing an
	// almanac pair against na==0 (outside that range) mis-epochs
	// PropagateAlmanacECEF for every entry decoded before the first string 5.
	if na == 0 {
		return 0
	}

	a, err := frame.DecodeGLONASSAlmanac(first, second, na)
	if err != nil || a.Alm.Slot < 1 || a.Alm.Slot > 24 {
		return 0
	}
	s.gloAlmMu.Lock()
	s.gloAlmanac[a.Alm.Slot] = gloAlmSlot{entry: a, lastSeen: recv}
	s.gloAlmMu.Unlock()
	return a.Alm.Slot
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
	//
	//
	// EphAge alone is insufficient — it wraps to ±half-week, so an outgoing set
	// whose Toe is ~1 week older than tstar reads as < 4 h and passes this gate, is then
	// propagated at tk ≈ 0 (its position a week ago, thousands of km off), banded crit, and
	// confirmed as a phantom disco. regression fix newly enables that window (RAWX observables keep
	// lastSeen fresh with no nav decode for a week). Add a wall-clock age gate (now − ephAt,
	// which cannot wrap) alongside the EphAge propagation-distance gate: skip the disco if
	// EITHER says the outgoing set is stale.
	//
	// regression fix residual (known, accepted): ephAt is the COLLECTOR-local apply time, so a
	// spool replay applies old sets seconds apart and this wall gate passes — correct in
	// general (the replayed changeover is real and should be measured), but if two
	// consecutive replayed sets for one SV are 7 d ± 4 h apart in Toe, EphAge aliases
	// under 4 h too and one phantom disco can slip through. That needs a ≥6 d 20 h gap
	// between consecutive ephemerides inside one ≤7 d spool — a razor-thin tail of an
	// already-degenerate capture; not worth a third gate.
	if math.Abs(gnsstime.EphAge(tstar, st.eph.Toe)) >= discoTrustAge.Seconds() ||
		st.ephAt.IsZero() || now.Sub(st.ephAt) >= discoTrustAge {
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
	// regression fix / docs/INTEGRITY.md §3: a changeover discontinuity compares the
	// satellite clock polynomial plus relativity. Group delay is a signal bias,
	// not the SV clock, so a routine TGD/BGD revision must not appear as a jump.
	oldDiscoClk, newDiscoClk := st.clk, newClk
	oldDiscoClk.TGD, newDiscoClk.TGD = 0, 0
	oOff, e3 := clock.OffsetFor(oldDiscoClk, st.eph, tstar)
	nOff, e4 := clock.OffsetFor(newDiscoClk, newEph, tstar)
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
				// refuse to serve positions from a GLONASS ephemeris past its
				// wall-clock validity cap (see gloPropagateMaxEphAge). Without this gate
				// the frozen state was re-integrated and re-stamped fresh on every tick
				// forever — through the km-wrong 0.5–2 h regime (reachable from a plain
				// reception gap: any decoded string refreshes lastSeen, so choppy
				// reception keeps the entry alive without completing a 1/2/3 set) and
				// past the ±12 h EphAgeDay wrap into backward integration. Wall clock
				// (now − gloEphAt) cannot wrap. Skipping (not zeroing) lets posStaleBound
				// expire the previously-served position naturally.
				if st.gloEphAt.IsZero() || now.Sub(st.gloEphAt) > gloPropagateMaxEphAge {
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
			// refuse to serve positions from an ephemeris past the wall-clock
			// cap (see propagateMaxEphAge) — beyond it the half-week EphAge wrap turns
			// the propagation into garbage-but-finite output while blinding every
			// staleness signal at once. Wall clock (now − ephAt) cannot wrap, the same
			// reasoning as computeDisco's regression fix gate. Skipping (not zeroing) lets
			// posStaleBound expire the previously-served position naturally.
			if st.ephAt.IsZero() || now.Sub(st.ephAt) > propagateMaxEphAge {
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

// stationEvictAfter is how long a filtered-stale sbas/rf entry stays in RAM
// before ExpireStations deletes it : a generous 12× the 5 min
// serving-staleness window, so a briefly-dark station re-appearing keeps its
// learned AGC baselines, while a renamed/mistyped source or a decommissioned
// GEO eventually leaves RAM entirely instead of parking a filtered entry
// forever. GLONASS almanac slots use their own (already multi-day) bound.
const stationEvictAfter = 12 * rfStaleAfter

// ExpireStations deletes sbas, rf, and GLONASS-almanac entries whose last
// sample is far past their serving-staleness windows. The feeds
// already FILTER stale entries (rfStaleAfter / sbasStaleAfter /
// gloAlmanacStaleAfter) — this makes the "dropped" language true in RAM too.
// The maps are bounded in practice (SBAS PRNs, 24 GLONASS slots, fleet-sized
// station ids), so this is residue hygiene, not a leak fix. The capability
// fingerprint (s.caps) is deliberately NOT evicted: durability is its design
// (capability.go) — a demonstrated signal gone silent must stay detectable.
// Runs on the same tick as Expire.
func (s *Store) ExpireStations(now time.Time) {
	s.sbasMu.Lock()
	for prn, st := range s.sbas {
		if now.Sub(st.lastSeen) > stationEvictAfter {
			delete(s.sbas, prn)
		}
	}
	s.sbasMu.Unlock()

	s.rfMu.Lock()
	for id, st := range s.rf {
		if now.Sub(st.lastSeen) > stationEvictAfter {
			delete(s.rf, id)
		}
	}
	s.rfMu.Unlock()

	s.gloAlmMu.Lock()
	for slot, a := range s.gloAlmanac {
		if now.Sub(a.lastSeen) > gloAlmanacStaleAfter {
			delete(s.gloAlmanac, slot)
		}
	}
	s.gloAlmMu.Unlock()
}

// wnRolloverGraceS tolerates the legitimate broadcast-WN lag across the weekly
// rollover : the transmitted WN is that of the START of the data set's
// transmission interval (IS-GPS-200N §30.3.3.1.1.1; the LNAV WN is likewise
// data-set-scoped — it can only change with an IODC cutover, §20.3.4.4), so a
// set cut over shortly before Saturday-midnight GPS time keeps broadcasting the
// previous week into the new one for up to its transmission interval —
// nominally 2 h (Table 20-XII normal operations); 4 h here for margin. An
// extended-operations set (the CS unable to upload for a day+) can exceed the
// grace and fire wn_mismatch — accepted: extended ops is itself monitor-worthy,
// and silently widening the window would blunt the replay-detection value.
const wnRolloverGraceS = 4 * 3600

// checkBroadcastWN  compares a decoded broadcast week number
// (truncated to the field's width) against the collector wall-clock GPS week —
// the cheapest time-domain integrity check there is, and DisambiguateWeek's
// intended production caller (it previously had none: both WN fields were
// decoded and never read). A mismatch means an upload error, an SV time fault,
// or a replayed/spoofed signal carrying a plausible TOW under a wrong week
// (docs/DEFENSE-PNT.md's time-plausibility gate). GPS and QZSS share GPS week
// numbering. Called with the shard lock held; the result feeds wn_mismatch →
// the detector's debounced wn_mismatch event.
func (st *svState) checkBroadcastWN(recv time.Time, wn, bits int) {
	full := gnsstime.DisambiguateWeek(gnsstime.SysGPS, wn, bits, float64(recv.Unix()))
	expect := int((recv.Unix() - gpsEpochUnix + gpsUTCOffset) / weekSeconds)
	mismatch := full != expect
	if mismatch && full == expect-1 && gpsTOW(recv) < wnRolloverGraceS {
		mismatch = false // data-set WN lagging across the rollover: designed behavior
	}
	st.wnMismatch, st.haveWN = mismatch, true
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

// gloNTDay returns the current GLONASS day number NT within the four-year interval
// (1..1461), derived from wall clock on the GLONASS time scale (MT = UTC+3h). The
// four-year intervals begin at the leap years 1996, 2000, 2004, …; NT = 1 on 1 Jan of
// that leap year.
//
// the almanac feed must propagate every out-of-view slot to *today*, not to the
// broadcast NA. NA is only the day the almanac ELEMENTS are referenced to; the CS
// re-references the almanac roughly daily but not at 00:00 MT sharp, so NA lags the
// calendar. Using NA as the propagation target day evaluates the position for
// (NA-day, current-tod) — up to tens of thousands of km off along-track when NA ≠ today.
// The current day needs no broadcast: it is the wall-clock MT calendar day, computed here.
func gloNTDay(now time.Time) int {
	mt := now.UTC().Add(3 * time.Hour) // civil MT fields (Year/YearDay) read off a UTC time
	year := mt.Year()
	cycleStart := year - ((year - 1996) % 4) // most recent four-year-interval start (leap year 1996+4k)
	start := time.Date(cycleStart, 1, 1, 0, 0, 0, 0, time.UTC)
	days := int(mt.Sub(start) / (24 * time.Hour))
	return days + 1 // NT is 1-based
}
