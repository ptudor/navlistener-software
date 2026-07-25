// Package ingest reads raw broadcast nav frames from receivers we control and
// forwards them, undecoded, to the central decode+state stage. Each source is a
// thin connector (docs/DESIGN.md §1): read a local feed → frame → emit. All
// decoding and orbit math is central — the edge is dumb.
//
// Dial connectors open receiver/caster TCP streams, while PushServer accepts
// authenticated GNF1 feeder streams. Both normalize their input into RawFrame
// so the downstream decoder and state store do not depend on transport mode.
package ingest

import (
	"encoding/binary"
	"time"

	"github.com/ptudor/gnss"
)

// RawFrame is one raw broadcast nav frame lifted off a receiver, tagged with just
// enough to dispatch it to the right decoder. Word-oriented frames (GPS/QZSS/
// BeiDou/GLONASS/SBAS from UBX) carry Words; byte-oriented content (RTCM messages
// and SBF blocks) carries Bytes. RTCM/SBF are currently capture-only: they reach
// the historian but do not update live constellation state. Word-oriented
// dispatch is selected by (GnssID, SigID).
type RawFrame struct {
	Recv    time.Time   // reception time (receiver/host clock; push path: the FEEDER's stamp)
	Source  string      // ingest source name
	GnssID  gnss.GNSSID // constellation (u-blox numbering)
	SvID    int         // PRN/slot within constellation
	SigID   int         // signal id (0 = primary civil signal)
	FreqID  int         // GLONASS FDMA channel carrier (k = FreqID − 7)
	MsgType int         // SBF block number / RTCM message number (byte-oriented sources)
	Words   []uint32    // 30-bit (or native) nav words, right-aligned
	Bytes   []byte      // raw frame bytes (for byte-oriented sources)
	Obs     *RawObs     // raw observables (RXM-RAWX telemetry), nil for nav frames
	RF      *RawRF      // RF-environment telemetry (MON-RF/MON-HW/NAV-SAT), nil for nav frames

	// Seq is the feeder's GNF1 global sequence, set only for push-path frames
	// (HasSeq true). On feeder reconnect, DATA frames past the last ack are
	// replayed — decode/live-state tolerate the duplicate, but the historian must
	// not : Seq is the dedup key store.Store uses to drop replayed rows.
	// Dial-mode frames have no such sequence and always persist.
	Seq    uint64
	HasSeq bool

	// RecvLocal is the collector-local receipt instant, stamped from THIS host's
	// clock (carrying Go's monotonic reading) at the push ingest boundary
	//. On that path Recv is the feeder's wall-clock stamp — the
	// forensic reception time, preserved for the historian and accepted with up
	// to recvTimestampSlack of skew or recvReplayHorizon of spool-replay age —
	// so subtracting it from this host's time.Now() compares two machines'
	// clocks: a feeder lagging near the 5 min slack would sit permanently at the
	// equally-sized liveness/RF staleness windows, and a spool replay would
	// rewind lastSeen/ephAt days backward mid-stream. All elapsed-time math in
	// live state (SV TTL expiry, live-receiver windows, RF/SBAS staleness, the
	// disco trust gates, GLONASS frame-coherence windows) must therefore read
	// LocalRecv(), never Recv. Dial-mode connectors stamp Recv from this host's
	// clock already and leave RecvLocal zero.
	RecvLocal time.Time
}

// LocalRecv returns the collector-local receipt time for elapsed-time math
// (staleness, expiry, trust windows — regression fix). A zero RecvLocal means the
// frame came from a dial-mode connector (or a test) whose Recv was already
// stamped from this host's clock, so Recv is the correct local instant there.
func (f *RawFrame) LocalRecv() time.Time {
	if !f.RecvLocal.IsZero() {
		return f.RecvLocal
	}
	return f.Recv
}

// RawRF is one RF-environment telemetry sample from a receiver: the jamming/AGC
// front-end state (UBX-MON-RF on F9+, UBX-MON-HW legacy) and/or the per-SV C/N₀ +
// elevation the spoofing gates need (UBX-NAV-SAT). It is station-scoped (keyed by the
// ingest source), not per-SV, and feeds the PNT-defense layer (docs/DEFENSE-PNT.md §1);
// the collector derives the detection metrics centrally — the edge only forwards.
type RawRF struct {
	Bands []RFBand // per-RF-path AGC/noise/CW/jamming/antenna (MON-RF/MON-HW)
	Sats  []SatCN0 // per-SV C/N₀ and elevation (NAV-SAT), for the C/N₀-vs-elevation gate
}

// RFBand is one RF path's front-end state (UBX-MON-RF block, or the single MON-HW
// path). Fields are the raw receiver numbers; the collector learns the per-station
// baseline and derives departures (docs/DEFENSE-PNT.md §2) — we do not threshold here.
type RFBand struct {
	Block      int // RF block index (0 = L1, 1 = L2/L5, …); MON-HW is always 0
	AGC        int // AGC monitor (0..8191); lower ⇒ the front-end cut gain (broadband energy)
	NoiseLevel int // noise level indicator
	CWSuppress int // CW-suppression / jamming indicator (0..255); high ⇒ a narrowband tone
	JamState   int // receiver's own jamming state (0 unknown/disabled, 1 ok, 2 warning, 3 critical)
	AntStatus  int // antenna status: 0 init, 1 unknown, 2 ok, 3 short, 4 open
}

// SatCN0 is one satellite's carrier-to-noise density and elevation as the receiver
// reports it (UBX-NAV-SAT), the input to the C/N₀-vs-elevation spoofing gate
// (docs/DEFENSE-PNT.md §3): a constellation of identical, elevation-independent C/N₀ is
// the classic single-transmitter spoofer signature.
type SatCN0 struct {
	GnssID  int
	SvID    int
	Cn0     int // dB-Hz
	ElevDeg int // −90..+90; degrees
	Used    bool
}

// RawObs is one raw observable measurement (UBX-RXM-RAWX / SBF MeasEpoch): the
// pseudorange/carrier/Doppler tuple for one SV signal at one receiver epoch.
// Dual-frequency pairs of these feed the geometry-free measured ionosphere
// (docs/MATH.md §7.4); Doppler feeds the delta-Hz integrity signal.
type RawObs struct {
	RcvTow     float64 // receiver time of week, seconds (receiver clock)
	Week       int     // week number of RcvTow
	PrM        float64 // pseudorange, metres
	CpCyc      float64 // carrier phase, cycles
	DoHz       float64 // Doppler, Hz (positive approaching)
	LockTimeMs int     // carrier lock time, ms — a reset signals a cycle slip
	Cn0        int     // dB-Hz
	CpValid    bool    // carrier-phase measurement valid (trkStat bit 1)
	CycleSlip  bool    // receiver reported a half-cycle/discontinuity condition
	ArcBreak   bool    // parser rejected carrier data; estimator must reset continuity
}

// RawBytes returns the frame's untouched bytes for the forensic record: the words
// serialized big-endian, or the byte payload for byte-oriented sources.
func (f *RawFrame) RawBytes() []byte {
	if len(f.Bytes) > 0 {
		return f.Bytes
	}
	b := make([]byte, len(f.Words)*4)
	for i, w := range f.Words {
		binary.BigEndian.PutUint32(b[i*4:], w)
	}
	return b
}

// NavType returns the GNF1 nav message type byte for this frame's (gnssId, sigId)
// (docs/CONSTELLATIONS.md §6), or 0 if unmapped.
func (f *RawFrame) NavType() int {
	switch f.GnssID {
	case gnss.GPS:
		switch f.SigID {
		case 0:
			return 0x10 // GpsLnav
		case 3, 4, 6, 7:
			return 0x11 // GpsCnav
		default:
			return 0
		}
	case gnss.QZSS:
		switch f.SigID {
		case 0:
			return 0x50 // QzsLnav
		case 4, 5, 8, 9:
			return 0x51 // QzsCnav
		default:
			return 0 // L1S/L1C-CNAV2/L6 are planned, not supported mappings
		}
	case gnss.Galileo:
		switch f.SigID {
		case 0, 1, 5, 6:
			return 0x20 // GalInav
		case 3, 4:
			return 0x21 // GalFnav
		default:
			return 0
		}
	case gnss.BeiDou:
		switch f.SigID {
		case 0:
			return 0x30 // BdsD1 (B1I — the shipped, capture-verified decoder)
		case 8:
			return 0x33 // BdsCnav2 (B2a data — shipped, capture-verified)
		default:
			// D2 (1/3), B2I D1 (2), B-CNAV1 (5/6), and the B2a companion
			// (7) have no shipped decoder and no capture-verified ID mapping —
			// do not label them as supported families (the same deferral
			// contract as NavIC/QZSS; BdsD2/BdsCnav1 stay reserved).
			return 0
		}
	case gnss.GLONASS:
		if f.SigID == 0 || f.SigID == 2 {
			return 0x40 // GloNav
		}
		return 0
	case gnss.NavIC:
		return 0 // planned: no NavIC decoder; do not advertise false live support
	case gnss.SBAS:
		if f.SigID == 0 {
			return 0x70 // SbasL1
		}
		// 0x71 SbasL5 (DFMC, a different 250-bit layout with its own
		// message types — docs/CONSTELLATIONS.md §6.1) is doc-reserved, not
		// shipped. Mirror the regression fix rule: never advertise a nav type that
		// wasn't verified, or a future L5-capable receiver's frames would be
		// persisted as L1 messages and re-decoded through the wrong header
		// layout on replay.
		return 0
	default:
		return 0
	}
}
