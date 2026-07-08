package frame

import (
	"encoding/binary"
	"errors"
)

// BeiDou B2a B-CNAV2 decoding — DELIBERATELY UNIMPLEMENTED (compliance gap).
//
// ┌─ COMPLIANCE GAP: BDS-SIS-ICD-B2a §6.2 (message types 10 & 11) ────────────┐
// │ The B-CNAV2 ephemeris bit layout could not be fully reverse-engineered    │
// │ from captured frames alone, so this decoder is a flagged placeholder. A   │
// │ future review MUST treat it as non-compliant and complete it from the     │
// │ ICD's message-10/11 field tables (§6.2), then validate against the real   │
// │ capture (go/internal/ingest/testdata/f9t_capture.ubx, gnssId 3 sigId 8)   │
// │ by cross-checking the decoded position against the D1 decoder — they must │
// │ agree to metres (see TestRealBeiDouD1 for the pattern).                    │
// └───────────────────────────────────────────────────────────────────────────┘
//
// Confirmed anchors (validated against the capture — seed the ICD work with these):
//
//	header:  PRN@0(6), MesType@6(6), SOW@12(18); ephemeris in message types 10 & 11.
//	type 10: toe@61 (11 bits, ×300 s → 288000, exact), SatType@72(2),
//	         ΔA@74 (26 bits signed, 2^-9 m; A = A_ref + ΔA, A_ref = 27906100 m MEO).
//	type 11: i0@75 (33 bits signed, 2^-32 semicircle × π; exact match to D1).
//
// UNRESOLVED — and confirmed intractable from the capture (2026-07-08): a definitive
// search matched the constant orbital parameters e and ω (which MUST equal the D1
// values, and do match on the LNAV/CNAV/D1/F-NAV signals) against EVERY message type,
// bit offset, field width (30–34), and plausible scale in the captured B-CNAV2 frames
// — no position works, even though toe@61 and i0@75 match exactly. That split (some
// fields land, most don't) is the signature of u-blox delivering B2a with the
// LDPC(96,48) channel-coding parity still INTERLEAVED: i0 happens to sit on an
// information bit, e straddles a coding boundary. The information-bit layout therefore
// cannot be recovered from the frames — it must come from the BDS-SIS-ICD-B2a §6.2
// message spec together with §5 channel coding. Do NOT guess: a wrong offset silently
// yields a plausible-but-wrong orbit. (Contrast GPS L2C CNAV, frame/gps_cnav.go, which
// u-blox delivers as clean information bits and which IS implemented + validated — its
// ΔA/message-10-11 structure is the model to follow once the B2a coding is in hand.)

// ErrBCNAV2Unimplemented is returned by DecodeBeiDouBCNAV2 until the §6.2 layout is
// completed. It is intentionally explicit so the frame is dropped rather than
// mis-decoded into fake ephemeris, and so telemetry and code review surface the gap.
var ErrBCNAV2Unimplemented = errors.New("frame: BeiDou B-CNAV2 (B2a) decode unimplemented — needs BDS-SIS-ICD-B2a §6.2 message-10/11 bit table")

// BeiDouBCNAV2 is the (partial) decode of a B2a B-CNAV2 message. Only MesType is
// reliably read today; the ephemeris is pending the ICD (see the package doc above).
type BeiDouBCNAV2 struct {
	MesType int // confirmed: message type at bits 6..11 (10/11 = ephemeris)
}

// DecodeBeiDouBCNAV2 reads the reliably-known header (message type) of a B2a
// B-CNAV2 message and returns ErrBCNAV2Unimplemented — the ephemeris body decode
// is the compliance gap documented above. It never fabricates an ephemeris.
func DecodeBeiDouBCNAV2(words []uint32) (*BeiDouBCNAV2, error) {
	if len(words) < 9 {
		return nil, ErrShortFrame
	}
	buf := make([]byte, 36) // 9 words × 32 bits = 288 bits
	for i := 0; i < 9; i++ {
		binary.BigEndian.PutUint32(buf[i*4:], words[i])
	}
	r := NewBitReaderN(buf, 288)
	mt, _ := r.Bits(6, 6)
	return &BeiDouBCNAV2{MesType: int(mt)}, ErrBCNAV2Unimplemented
}
