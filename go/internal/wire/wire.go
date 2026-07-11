// Package wire implements GNF1, the length-prefixed framing spoken between a
// navfeeder edge feeder and the collector over TLS (docs/DESIGN.md §2). It carries
// raw broadcast nav frames — the feeder forwards, the collector decodes — and is
// the same resilient shape as radiolistener's RLF1, authored fresh (it is not
// galmon's navmon.proto, which stays entirely inside the optional GPL bridge).
//
// This package defines the framing, the handshake messages, and the raw-record
// envelope. Both the future authenticated push listener and the C feeder share it;
// dial-mode ingest reads receiver bytes directly and does not use GNF1.
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
	Hello      FrameType = 0x01 // feeder→collector: JSON HelloMsg
	Welcome    FrameType = 0x02 // collector→feeder: JSON WelcomeMsg
	Data       FrameType = 0x03 // feeder→collector: [8B seq][raw record]
	Ack        FrameType = 0x04 // collector→feeder: [8B seq] last contiguous stored
	Ping       FrameType = 0x05 // keepalive
	Pong       FrameType = 0x06 // keepalive
	SignedData FrameType = 0x07 // hardware tier (vNext): Data batch + ATECC ECDSA
)

var (
	// ErrBadMagic is returned when a stream does not begin with the GNF1 magic.
	ErrBadMagic = errors.New("wire: bad GNF1 magic")
	// ErrFrameTooLarge is returned when a length prefix exceeds MaxFrameLen.
	ErrFrameTooLarge = errors.New("wire: frame exceeds MaxFrameLen")
	// ErrShortRecord is returned when a DATA payload is too short for the envelope.
	ErrShortRecord = errors.New("wire: DATA record too short")
)

// HelloMsg is the feeder's opening handshake (JSON). feed ∈ {ubx, sbf, rtcm, nmea}.
type HelloMsg struct {
	Token   string `json:"token"`
	Station string `json:"station"`
	Feed    string `json:"feed"`
	SW      string `json:"sw"`
	Zstd    bool   `json:"zstd,omitempty"`
}

// WelcomeMsg is the collector's handshake reply (JSON).
type WelcomeMsg struct {
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	AckIntervalMS int    `json:"ack_interval_ms,omitempty"`
	Zstd          bool   `json:"zstd,omitempty"`
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

// EncodeAck / DecodeAck carry the last contiguously-stored sequence number.
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
