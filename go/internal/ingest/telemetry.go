package ingest

import (
	"encoding/binary"
	"errors"
)

// GNF1 telemetry records (docs/CONSTELLATIONS.md §6.2). Receiver-side metadata — RF
// front-end state and per-SV C/N₀ — is not a broadcast nav frame, but the PNT-defense
// Tier-0 detector needs it from every fleet station, not only the LAN-dialled ones. So
// telemetry rides the SAME GNF1 DATA frame as raw-nav records (spool/ack/replay/zstd
// unchanged); the record's frame_type byte discriminates: telemetry types are < 0x10,
// raw-nav frame types (§6.1) are ≥ 0x10. The record's gnssId/svId/sigId/freqId header
// fields are unused for telemetry (the sample is station-scoped, keyed by the observer),
// and the body is carried verbatim in the record's raw region.
//
// Only the two types the RF detector consumes are transported today; the remaining §6.2
// types (RFData, ObserverPosition, …) are follow-ons and get their own body codecs when
// their features land.
const (
	TelemReceptionData = 0x01 // UBX-NAV-SAT: per-SV C/N₀ + elevation → RawRF.Sats (spoof gate)
	TelemJammingStats  = 0x05 // UBX-MON-RF / MON-HW: AGC/jamming/antenna → RawRF.Bands (jam gate)
)

// telemBodyVersion prefixes every telemetry body. The GNF1 magic versions the stream, but
// telemetry schemas evolve independently of the ICD-fixed nav frames (DEFENSE-PNT stages
// SEC-SIG/clock-jump gates that extend these records), so a per-body version lets the
// collector reject an unknown layout cleanly instead of misparsing one.
const telemBodyVersion = 1

// maxTelemSats bounds a ReceptionData body so it fits the feeder's fixed record buffer
// (feeder/navfeeder.c MAX_RAW): 3 + 5·200 = 1003 ≤ 1024. A multi-band receiver tracks a
// few dozen SVs — 200 is headroom the real sky never reaches. The feeder and this codec
// agree on the cap so the C↔Go cross-oracle stays byte-exact.
const maxTelemSats = 200

// ErrBadTelemetry is returned when a telemetry body is truncated, over-long, or carries an
// unrecognised version — the untrusted-input discipline (docs/INTEGRITY.md §9), applied to
// the push path exactly as the dial-mode UBX parsers apply it.
var ErrBadTelemetry = errors.New("ingest: malformed telemetry body")

// IsTelemetryType reports whether a GNF1 frame_type byte is a telemetry record (§6.2)
// rather than a raw-nav frame (§6.1). frame_type 0 is unmapped/unknown, not telemetry.
func IsTelemetryType(t int) bool { return t > 0 && t < 0x10 }

// clampU16 saturates a receiver counter (AGC 0..8191, noise) into the 16-bit wire field.
func clampU16(v int) uint16 {
	if v < 0 {
		return 0
	}
	if v > 0xFFFF {
		return 0xFFFF
	}
	return uint16(v)
}

// clampU8 saturates a small field (block/cw/jamState/antStatus/cn0) into a byte.
func clampU8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 0xFF {
		return 0xFF
	}
	return byte(v)
}

const (
	jammingBandLen  = 8 // block + agc(2) + noise(2) + cw + jamState + antStatus
	receptionSatLen = 5 // gnssId + svId + cn0 + elev(i8) + flags
)

// EncodeJammingStats serializes RF bands (MON-RF blocks, or the single MON-HW path) into a
// JammingStats (0x05) body: [1B version][1B nBands] then per band
// [1B block][2B BE agc][2B BE noise][1B cw][1B jamState][1B antStatus]. nBands is a byte —
// a receiver has a handful of RF paths, never 256.
func EncodeJammingStats(bands []RFBand) []byte {
	n := len(bands)
	if n > 0xFF {
		n = 0xFF
	}
	buf := make([]byte, 2+n*jammingBandLen)
	buf[0] = telemBodyVersion
	buf[1] = byte(n)
	for i := 0; i < n; i++ {
		b := bands[i]
		o := 2 + i*jammingBandLen
		buf[o] = clampU8(b.Block)
		binary.BigEndian.PutUint16(buf[o+1:], clampU16(b.AGC))
		binary.BigEndian.PutUint16(buf[o+3:], clampU16(b.NoiseLevel))
		buf[o+5] = clampU8(b.CWSuppress)
		buf[o+6] = clampU8(b.JamState)
		buf[o+7] = clampU8(b.AntStatus)
	}
	return buf
}

// decodeJammingStats parses a JammingStats body back into RF bands. Every length is
// validated before indexing; a short or over-versioned body returns ErrBadTelemetry.
func decodeJammingStats(b []byte) ([]RFBand, error) {
	if len(b) < 2 || b[0] != telemBodyVersion {
		return nil, ErrBadTelemetry
	}
	n := int(b[1])
	// exact length, not a lower bound. The feeder encoder emits exactly
	// 2+n*jammingBandLen bytes; trailing junk after the declared bands is a feeder-encoder
	// bug and must be rejected (ErrBadTelemetry's contract), keeping the C↔Go cross-oracle
	// byte-exact rather than silently accepting over-long bodies.
	if len(b) != 2+n*jammingBandLen {
		return nil, ErrBadTelemetry
	}
	bands := make([]RFBand, n)
	for i := 0; i < n; i++ {
		o := 2 + i*jammingBandLen
		bands[i] = RFBand{
			Block:      int(b[o]),
			AGC:        int(binary.BigEndian.Uint16(b[o+1:])),
			NoiseLevel: int(binary.BigEndian.Uint16(b[o+3:])),
			CWSuppress: int(b[o+5]),
			JamState:   int(b[o+6]),
			AntStatus:  int(b[o+7]),
		}
	}
	return bands, nil
}

// EncodeReceptionData serializes per-SV C/N₀ + elevation (NAV-SAT) into a ReceptionData
// (0x01) body: [1B version][2B BE nSats] then per sat
// [1B gnssId][1B svId][1B cn0][1B int8 elev][1B flags] (flags bit 0 = used-in-solution).
// At most maxTelemSats sats are emitted so the body fits the feeder's record buffer.
func EncodeReceptionData(sats []SatCN0) []byte {
	n := len(sats)
	if n > maxTelemSats {
		n = maxTelemSats
	}
	buf := make([]byte, 3+n*receptionSatLen)
	buf[0] = telemBodyVersion
	binary.BigEndian.PutUint16(buf[1:], uint16(n))
	for i := 0; i < n; i++ {
		s := sats[i]
		o := 3 + i*receptionSatLen
		buf[o] = clampU8(s.GnssID)
		buf[o+1] = clampU8(s.SvID)
		buf[o+2] = clampU8(s.Cn0)
		buf[o+3] = byte(int8(clampElev(s.ElevDeg)))
		if s.Used {
			buf[o+4] = 0x01
		}
	}
	return buf
}

// clampElev holds an elevation to the signed-byte wire range. The nominal valid
// range is −90..+90 (NAV-SAT reports I1 degrees), but UBX-NAV-SAT's
// out-of-range "elevation unknown" sentinel (91, typical for a freshly-acquired
// SV) is deliberately let through rather than clamped down to a
// plausible-looking 90 — clamping it made the push path indistinguishable from
// a genuine zenith satellite, defeating cn0ElevationResidual's exclusion of
// out-of-range elevations (state/rf.go). Only bound at the wire's actual int8
// range (127) so the byte(int8(...)) conversion above can't wrap.
func clampElev(deg int) int {
	if deg < -90 {
		return -90
	}
	if deg > 127 {
		return 127
	}
	return deg
}

// decodeReceptionData parses a ReceptionData body back into per-SV samples, bounds-checking
// the sat count against the body length before indexing.
func decodeReceptionData(b []byte) ([]SatCN0, error) {
	if len(b) < 3 || b[0] != telemBodyVersion {
		return nil, ErrBadTelemetry
	}
	n := int(binary.BigEndian.Uint16(b[1:]))
	// the encoder never emits more than maxTelemSats (200), so a body
	// claiming more is either a misbehaving feeder or a bug -- reject before the
	// length check below, which alone would still accept up to 65535 sats
	// (~328 KiB, inside the 1 MiB frame limit) into the RF/spoofing detector.
	if n > maxTelemSats {
		return nil, ErrBadTelemetry
	}
	// exact length, not a lower bound — trailing junk after the declared sats is a
	// feeder-encoder bug, rejected to keep the C↔Go cross-oracle byte-exact.
	if len(b) != 3+n*receptionSatLen {
		return nil, ErrBadTelemetry
	}
	sats := make([]SatCN0, n)
	for i := 0; i < n; i++ {
		o := 3 + i*receptionSatLen
		sats[i] = SatCN0{
			GnssID:  int(b[o]),
			SvID:    int(b[o+1]),
			Cn0:     int(b[o+2]),
			ElevDeg: int(int8(b[o+3])),
			Used:    b[o+4]&0x01 != 0,
		}
	}
	return sats, nil
}
