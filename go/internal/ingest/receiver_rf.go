package ingest

import (
	"encoding/binary"
	"slices"
)

// SignalSecurity is the receiver's SEC-SIG assessment. A frequency absent from
// Centers was not assessed. Multiple signals can share the same center.
type SignalSecurity struct {
	Version      byte       `json:"version"`
	JamEnabled   bool       `json:"jamming_detector_enabled"`
	JamState     byte       `json:"jamming_state"`
	SpoofEnabled bool       `json:"spoofing_detector_enabled"`
	SpoofState   byte       `json:"spoofing_state"`
	Centers      []RFCenter `json:"centers"`
}

type RFCenter struct {
	KHz    uint32 `json:"center_khz"`
	Jammed bool   `json:"jammed"`
}

type SecurityEvent struct {
	// Age is relative to the SIGLOG message. The receiver uses 0xffffffff
	// for an event older than 45 days.
	AgeSeconds uint32 `json:"age_s"`
	Detection  byte   `json:"detection_type"`
	Event      byte   `json:"event_type"`
}

type RFSpectrum struct {
	Path         byte    `json:"path"`
	Paths        byte    `json:"path_count"`
	CenterHz     uint32  `json:"center_hz"`
	SpanHz       uint32  `json:"span_hz"`
	ResolutionHz uint32  `json:"resolution_hz"`
	PGADB        byte    `json:"pga_db"`
	Bins         []byte  `json:"bins_quarter_db_base64"`
	PeakHz       int64   `json:"peak_hz"`
	PeakMedianDB float64 `json:"peak_above_median_db"`
}

// JammingStats v1 carries the MON-RF/HW counters. V2 carries a single type
// of receiver evidence in a separate sample. The RF detector continues to
// consume the v1 counters once per observation.
func decodeRFStats(b []byte) (*RawRF, error) {
	if len(b) < 2 || b[0] != 2 {
		bands, err := decodeJammingStats(b)
		if err != nil {
			return nil, err
		}
		return &RawRF{Bands: bands}, nil
	}
	le := binary.LittleEndian.Uint32
	switch b[1] {
	case 0x51:
		if len(b) != 3 || b[2] > 1 {
			return nil, ErrBadTelemetry
		}
		high := b[2] == 1
		return &RawRF{HighSensitivity: &high}, nil
	case 2, 3:
		if len(b) < 4 || len(b) != 4+4*int(b[3]) {
			return nil, ErrBadTelemetry
		}
		flags := b[2]
		s := &SignalSecurity{Version: b[1], JamEnabled: flags&1 != 0, JamState: flags >> 1 & 3,
			SpoofEnabled: flags&8 != 0, SpoofState: flags >> 4 & 7, Centers: make([]RFCenter, int(b[3]))}
		for i := range s.Centers {
			word := le(b[4+4*i:])
			s.Centers[i] = RFCenter{KHz: word & 0xffffff, Jammed: word&(1<<24) != 0}
		}
		return &RawRF{SignalSecurity: s}, nil
	case 0x10:
		if len(b) < 10 || b[2] != 1 || b[3] > 16 || len(b) != 10+8*int(b[3]) {
			return nil, ErrBadTelemetry
		}
		events := make([]SecurityEvent, int(b[3]))
		for i := range events {
			row := b[10+8*i:]
			events[i] = SecurityEvent{AgeSeconds: le(row), Detection: row[4], Event: row[5]}
		}
		return &RawRF{SecurityLog: &events}, nil
	case 0x31:
		if len(b) != 276 || b[2] >= b[3] {
			return nil, ErrBadTelemetry
		}
		bins := slices.Clone(b[4:260])
		s := &RFSpectrum{Path: b[2], Paths: b[3], SpanHz: le(b[260:]), ResolutionHz: le(b[264:]),
			CenterHz: le(b[268:]), PGADB: b[272], Bins: bins}
		// X20 HPG 2.02 section 3.14.8 places the center at bin 127.
		peak := slices.Max(bins)
		i := slices.Index(bins, peak)
		s.PeakHz = int64(s.CenterHz) + int64(s.SpanHz)*int64(i-127)/256
		var sorted [256]byte
		copy(sorted[:], bins)
		slices.Sort(sorted[:])
		s.PeakMedianDB = (2*float64(peak) - float64(sorted[127]) - float64(sorted[128])) / 8
		return &RawRF{Spectrum: s}, nil
	default:
		return nil, ErrBadTelemetry
	}
}
