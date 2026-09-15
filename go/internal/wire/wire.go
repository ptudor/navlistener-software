// Package wire implements GNF1, the length-prefixed framing spoken between a
// navfeeder edge feeder and the collector over TLS (docs/DESIGN.md §2). It carries
// raw broadcast nav frames — the feeder forwards, the collector decodes — and is
// the same resilient shape as radiolistener's RLF1, authored fresh (it is not
// galmon's navmon.proto, which stays entirely inside the optional GPL bridge).
//
// This package defines the framing, the handshake messages, and the raw-record
// envelope. The authenticated PushServer and the C navfeeder share it; dial-mode
// ingest reads receiver bytes directly and does not use GNF1.
package wire

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"

	"github.com/ptudor/gnss"
)

// Magic prefixes a GNF1 stream, sent once after the TLS handshake.
var Magic = [4]byte{'G', 'N', 'F', '1'}

// MaxFrameLen bounds a single frame's payload (RLF1 parity). Every length prefix
// is validated against it before allocation to bound reads and memory use
// (docs/INTEGRITY.md §9).
const MaxFrameLen = 1 << 20 // 1 MiB

// FrameType is the 1-byte GNF1 frame discriminator.
type FrameType uint8

const (
	Hello   FrameType = 0x01 // feeder→collector: JSON HelloMsg
	Welcome FrameType = 0x02 // collector→feeder: JSON WelcomeMsg
	// Data carries [8B seq][raw record]. Assigned sequences start at 1 — both
	// reference feeders emit ++seq — and 0 is therefore NOT a member of the
	// sequence space. A collector MUST treat a DATA frame with sequence 0 as a
	// protocol error and close the connection : it can never be
	// acked (the durable watermark has no value below it to report), so accepting
	// one applies and stores a record whose spool copy the feeder can never
	// retire, replayed on every reconnect forever. A second implementer must not
	// emit sequence 0 to mean "unsequenced".
	Data FrameType = 0x03 // feeder→collector: [8B seq][raw record], seq ≥ 1
	// Ack (regression fix, revised by regression fix 2026-07-31): [8B seq] the DURABLE
	// watermark for this session — the highest sequence N such that every
	// sequenced frame ≤ N the collector RECEIVED has been durably resolved:
	// committed by the historian (or deduped as a replay of an
	// already-committed ledger claim), quarantined as unfixable poison, or
	// classified never-persistable (telemetry, malformed body). The feeder
	// prunes its spool up to the acked seq, so before this revision ACK meant
	// "queued in RAM" and a DB outage after ACK permanently erased the raw
	// evidence — now the ack simply stalls until the store commits, and the
	// feeder's spool (plus its ack-stall reconnect watchdog) carries the
	// outage. A collector running WITHOUT a historian acks on receipt — the
	// explicit live-only mode. Two regression fix properties survive unchanged: the
	// collector never waits for a RECEPTION gap to fill (a sequence that never
	// arrived is skipped past — a second implementer must not wait for
	// contiguity over unreceived sequences), and reconnect replay (the feeder
	// resends everything after the last ack) makes any duplicate harmless.
	Ack           FrameType = 0x04 // collector→feeder: [8B seq] durable watermark (live-only mode: highest received)
	Ping          FrameType = 0x05 // keepalive
	Pong          FrameType = 0x06 // keepalive
	UpdateControl FrameType = 0x09 // collector to device: versioned 36-byte command
	SignedData    FrameType = 0x07 // hardware tier (vNext): Data batch + ATECC ECDSA
)

var (
	// ErrBadMagic is returned when a stream does not begin with the GNF1 magic.
	ErrBadMagic = errors.New("wire: bad GNF1 magic")
	// ErrFrameTooLarge is returned when a length prefix exceeds MaxFrameLen.
	ErrFrameTooLarge = errors.New("wire: frame exceeds MaxFrameLen")
	// ErrShortRecord is returned when a DATA payload is too short for the envelope.
	ErrShortRecord = errors.New("wire: DATA record too short")
)

// HelloMsg is the feeder's opening handshake (JSON). feed ∈ {ubx, rtcm} : the push
// path deliberately rejects sbf (the GNF1 frame_type can't carry SBF block numbers, regression fix)
// and nmea is unimplemented — a second feeder implementer mirroring this file must not build
// an sbf/nmea pusher that can never authenticate.
type HelloMsg struct {
	Token   string `json:"token"`
	Station string `json:"station"`
	Feed    string `json:"feed"`
	SW      string `json:"sw"`
	Zstd    bool   `json:"zstd,omitempty"`
	// Session is the feeder's boot/session identity, REQUIRED —
	// contract revision 2026-07-31: a HELLO without a ValidSession value is
	// rejected before WELCOME (Error "missing or invalid session"); there are
	// no legacy GNF1 peers to grandfather (design decision, cumulative July fix
	// pass). It is an opaque token minted fresh whenever the feeder's DATA
	// sequence space restarts from zero, and REUSED whenever that space
	// continues: the C feeder stores it in its disk-spool header so a
	// spool-recovering restart resumes session and sequence together, while
	// the ESP32's RAM-only ring mints a new one every boot. The collector's
	// replay-dedup identity is (canonical authenticated observer, session,
	// seq) — session is NOT trusted as observer identity, it only partitions
	// one observer's sequence spaces. Without it, a feeder restart that reset
	// seq to 0 collided with the durable ledger's old rows and fresh
	// post-reboot frames were silently discarded as replays — the product's
	// worst failure class (silent loss of forensic raw frames).
	Session string `json:"session"`
}

// SessionMaxLen bounds HelloMsg.Session. 64 comfortably covers the reference
// implementations (32 hex chars) while keeping the pre-auth HELLO small.
const SessionMaxLen = 64

// ValidSession reports whether s is an acceptable GNF1 session identity:
// 1..SessionMaxLen bytes of [A-Za-z0-9._-]. The charset mirrors the observer-id
// discipline (config.ValidObserverID) so a session can never smuggle JSON/SQL
// metacharacters into logs or the historian ledger.
func ValidSession(s string) bool {
	if len(s) == 0 || len(s) > SessionMaxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// WelcomeMsg is the collector's handshake reply (JSON).
type WelcomeMsg struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	AckIntervalMS int    `json:"ack_interval_ms,omitempty"`
	Zstd          bool   `json:"zstd,omitempty"`
	DurableACK    bool   `json:"durable_ack,omitempty"`
}

// RawRecord is the envelope inside a DATA frame: just enough for the collector to
// dispatch without decoding (docs/DESIGN.md §2). The raw bytes are the untouched
// broadcast nav frame.
type RawRecord struct {
	RecvUnixNs int64       // observer/RTC reception time
	GnssID     gnss.GNSSID // constellation (u-blox numbering)
	SvID       uint8       // PRN within constellation
	SigID      uint8       // signal id
	FreqID     uint8       // GLONASS FDMA channel (k = freqId − 7); 0 for other constellations
	FrameType  uint8       // GNF1 nav message type (docs/CONSTELLATIONS.md §6)
	Raw        []byte      // the broadcast nav frame, verbatim
}

const recordHeaderLen = 8 + 1 + 1 + 1 + 1 + 1 // recv_unix_ns + gnssId + svId + sigId + freqId + frame_type

// WriteMagic writes the GNF1 stream prefix. Call once, before any frame.
func WriteMagic(w io.Writer) error {
	_, err := w.Write(Magic[:])
	return err
}

// ReadMagic reads and validates the GNF1 stream prefix.
func ReadMagic(r io.Reader) error {
	var m [4]byte
	if _, err := io.ReadFull(r, m[:]); err != nil {
		return err
	}
	if m != Magic {
		return ErrBadMagic
	}
	return nil
}

// WriteFrame writes one framed message: [1B type][4B BE len][payload].
func WriteFrame(w io.Writer, ft FrameType, payload []byte) error {
	if len(payload) > MaxFrameLen {
		return ErrFrameTooLarge
	}
	var hdr [5]byte
	hdr[0] = byte(ft)
	binary.BigEndian.PutUint32(hdr[1:], uint32(len(payload)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame reads one framed message, validating the length against MaxFrameLen
// before allocating (the pre-allocation length check).
func ReadFrame(r io.Reader) (FrameType, []byte, error) {
	return ReadFrameMax(r, MaxFrameLen)
}

// ReadFrameMax is ReadFrame with a caller-supplied length cap instead of the
// package-wide MaxFrameLen -- a reception-side policy for callers that want a
// tighter bound in a specific context (the collector's pre-auth
// handshake caps HELLO far below MaxFrameLen, since a real HELLO is ~150 bytes
// and there is no reason to let an unauthenticated connection pin 1 MiB).
// This is not a wire change: the frame format is identical, only the maximum
// this particular read will accept is smaller.
func ReadFrameMax(r io.Reader, maxLen uint32) (FrameType, []byte, error) {
	var hdr [5]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint32(hdr[1:])
	if n > maxLen {
		return 0, nil, ErrFrameTooLarge
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return FrameType(hdr[0]), payload, nil
}

// WriteHello / WriteWelcome marshal the handshake messages to their frames.
func WriteHello(w io.Writer, h HelloMsg) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	return WriteFrame(w, Hello, b)
}

func WriteWelcome(w io.Writer, m WelcomeMsg) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return WriteFrame(w, Welcome, b)
}

// ParseHello unmarshals a HELLO frame payload into a HelloMsg.
func ParseHello(payload []byte) (HelloMsg, error) {
	var h HelloMsg
	err := json.Unmarshal(payload, &h)
	return h, err
}

// MarshalWelcome encodes a WelcomeMsg to its JSON frame payload.
//
// regression fix — NORMATIVE: the payload MUST be the compact `encoding/json` spelling, with no
// space after the ':' and no pretty-printing. The C feeder and the ESP32 firmware accept the
// handshake by matching the exact byte sequences `"ok":true` and `"zstd":true` with strstr
// (feeder/navfeeder.c, esp32/components/gnf1/src/gnf1.c) rather than carrying a JSON parser
// into a binary that has to fit on an OpenWrt router and an ESP32-C6. That is a deliberate
// size trade, so the constraint belongs here, at the write site: reformatting this to
// `json.MarshalIndent`, hand-rolling the JSON with a space after the colon, or interposing a
// proxy that re-serializes the payload PERMANENTLY breaks authentication and zstd negotiation
// for the whole fleet — the feeder would reconnect forever, never seeing an accepted welcome.
// json.Marshal's output is compact by contract, so this line satisfies the requirement today;
// the requirement is on the WIRE, and is stated in docs/DESIGN.md §GNF1 for any second
// implementation. TestNavfeeder* (the real C feeder against this collector) is the
// regression guard.
func MarshalWelcome(m WelcomeMsg) ([]byte, error) {
	return json.Marshal(m)
}

// EncodeData builds a DATA frame payload: [8B seq][record header][raw bytes].
func EncodeData(seq uint64, rec RawRecord) []byte {
	buf := make([]byte, 8+recordHeaderLen+len(rec.Raw))
	binary.BigEndian.PutUint64(buf[0:], seq)
	binary.BigEndian.PutUint64(buf[8:], uint64(rec.RecvUnixNs))
	buf[16] = byte(rec.GnssID)
	buf[17] = rec.SvID
	buf[18] = rec.SigID
	buf[19] = rec.FreqID
	buf[20] = rec.FrameType
	copy(buf[21:], rec.Raw)
	return buf
}

// DecodeData parses a DATA frame payload back into its sequence number and record.
// The raw bytes alias the payload slice; copy them if they must outlive it.
func DecodeData(payload []byte) (seq uint64, rec RawRecord, err error) {
	if len(payload) < 8+recordHeaderLen {
		return 0, RawRecord{}, ErrShortRecord
	}
	seq = binary.BigEndian.Uint64(payload[0:])
	rec.RecvUnixNs = int64(binary.BigEndian.Uint64(payload[8:]))
	rec.GnssID = gnss.GNSSID(payload[16])
	rec.SvID = payload[17]
	rec.SigID = payload[18]
	rec.FreqID = payload[19]
	rec.FrameType = payload[20]
	rec.Raw = payload[21:]
	return seq, rec, nil
}

// EncodeAck / DecodeAck carry the collector's ACK watermark: the highest
// DURABLY RESOLVED sequence (regression fix; receipt-based only in documented
// live-only mode), never-received sequences skipped per regression fix -- see the Ack
// FrameType comment, which is authoritative.
func EncodeAck(seq uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, seq)
	return b
}

func DecodeAck(payload []byte) (uint64, error) {
	if len(payload) < 8 {
		return 0, ErrShortRecord
	}
	return binary.BigEndian.Uint64(payload), nil
}
