// Package ingest reads raw broadcast nav frames from receivers we control and
// forwards them, undecoded, to the central decode+state stage. Each source is a
// thin connector (docs/DESIGN.md §1): read a local feed → frame → emit. All
// decoding and orbit math is central — the edge is dumb.
//
// This build implements dial-mode connectors (the collector opens a TCP
// connection to a known receiver on the LAN); the authenticated fleet push
// endpoint is a later pass.
package ingest

import (
	"encoding/binary"
	"time"

	"github.com/ptudor/gnss"
)

// RawFrame is one raw broadcast nav frame lifted off a receiver, tagged with just
// enough to dispatch it to the right decoder. Word-oriented frames (GPS/QZSS/
// BeiDou/GLONASS/SBAS from UBX or SBF) carry Words; byte-oriented content (RTCM
// messages, SBF blocks) carries Bytes. The decode stage picks by (GnssID, SigID)
// or message type.
type RawFrame struct {
	Recv    time.Time   // reception time (receiver/host clock)
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
		if f.SigID == 0 {
			return 0x10 // GpsLnav
		}
		return 0x11 // GpsCnav
	case gnss.QZSS:
		switch f.SigID {
		case 0:
			return 0x50 // QzsLnav
		case 1:
			return 0x53 // QzsL1s
		default:
			return 0x51 // QzsCnav
		}
	case gnss.Galileo:
		if f.SigID == 3 || f.SigID == 4 {
			return 0x21 // GalFnav
		}
		return 0x20 // GalInav
	case gnss.BeiDou:
		switch f.SigID {
		case 1, 3:
			return 0x31 // BdsD2
		case 5, 6:
			return 0x32 // BdsCnav1
		case 7, 8:
			return 0x33 // BdsCnav2
		default:
			return 0x30 // BdsD1
		}
	case gnss.GLONASS:
		return 0x40 // GloNav
	case gnss.NavIC:
		return 0 // planned: no NavIC decoder; do not advertise false live support
	case gnss.SBAS:
		return 0x70 // SbasL1
	default:
		return 0
	}
}
