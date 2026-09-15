package ingest

import "encoding/binary"

// BoardTiming compares external pulses with the ESP APB clock. It is not a
// UTC accuracy estimate; the RTC and GNSS phases are arbitrary modulo one second.
type BoardTiming struct {
	Clock          string        `json:"clock"`
	ResolutionHz   uint32        `json:"resolution_hz"`
	StartedMS      uint64        `json:"started_uptime_ms"`
	ElapsedMS      uint64        `json:"elapsed_ms"`
	QueueDropped   uint32        `json:"capture_queue_dropped"`
	RTCState       string        `json:"rtc_square_wave_state"`
	RTCControl     uint8         `json:"rtc_control"`
	RTCTrim        uint8         `json:"rtc_trim_raw"`
	PhaseTicks     *int32        `json:"rtc_minus_gnss_phase_ticks"`
	PhaseNS        *float64      `json:"rtc_minus_gnss_phase_ns"`
	TimepulseFlags *uint8        `json:"next_timepulse_flags"`
	TimepulseRef   uint8         `json:"next_timepulse_reference"`
	TimepulseMS    uint64        `json:"timepulse_received_uptime_ms"`
	GNSS           TimingChannel `json:"gnss"`
	RTC            TimingChannel `json:"rtc"`
}
type TimingChannel struct {
	Flags                  uint32   `json:"flags"`
	PeriodTicks            uint32   `json:"period_ticks"`
	WidthTicks             uint32   `json:"width_ticks"`
	MinTicks               uint32   `json:"min_period_ticks"`
	MaxTicks               uint32   `json:"max_period_ticks"`
	Discontinuities        uint32   `json:"discontinuities"`
	MissingEstimate        uint64   `json:"missing_pulse_estimate"`
	Captured               uint64   `json:"captured_rising_edges"`
	Physical               uint64   `json:"hardware_pulses"`
	SpanTicks              uint64   `json:"span_ticks"`
	SpanIntervals          uint64   `json:"span_intervals"`
	LastRiseMS             uint64   `json:"last_rise_uptime_ms"`
	CounterDiscontinuities uint32   `json:"counter_discontinuities"`
	PeriodNS               *float64 `json:"period_ns"`
	WidthNS                *float64 `json:"width_ns"`
	SpanPhaseNS            *float64 `json:"span_phase_ns"`
	PeriodErrorPPM         *float64 `json:"period_error_ppm"`
}

func decodeBoardTiming(v []byte, uptime uint64) (*BoardTiming, error) {
	if len(v) != 196 || v[0] != 1 || v[1] != 1 || v[2] > 4 || v[5] > 3 || v[6] > 63 {
		return nil, ErrBadTelemetry
	}
	u32, u64 := binary.BigEndian.Uint32, binary.BigEndian.Uint64
	hz := u32(v[8:])
	if hz > 200000000 || u64(v[16:]) > uptime || u64(v[28:]) > uptime {
		return nil, ErrBadTelemetry
	}
	d := &BoardTiming{Clock: "esp_apb", ResolutionHz: hz, StartedMS: u64(v[16:]),
		QueueDropped: u32(v[12:]), RTCState: []string{"unknown", "enabled_1hz", "oscillator_stopped", "alarm_or_coarse_trim_conflict", "io_error"}[v[2]],
		RTCControl: v[3], RTCTrim: v[4], TimepulseRef: v[7], TimepulseMS: u64(v[28:])}
	d.ElapsedMS = uptime - d.StartedMS
	if v[5]&2 != 0 {
		flags := v[6]
		d.TimepulseFlags = &flags
	} else if v[6] != 0 || v[7] != 0 || d.TimepulseMS != 0 {
		return nil, ErrBadTelemetry
	}
	channels := []*TimingChannel{&d.GNSS, &d.RTC}
	for i, c := range channels {
		p := v[36+80*i:]
		*c = TimingChannel{Flags: u32(p), PeriodTicks: u32(p[4:]), WidthTicks: u32(p[8:]), MinTicks: u32(p[12:]), MaxTicks: u32(p[16:]),
			Discontinuities: u32(p[20:]), MissingEstimate: u64(p[24:]), Captured: u64(p[32:]), Physical: u64(p[40:]),
			SpanTicks: u64(p[48:]), SpanIntervals: u64(p[56:]), LastRiseMS: u64(p[64:]), CounterDiscontinuities: u32(p[72:])}
		if c.Flags > 63 || u32(p[76:]) != 0 || c.LastRiseMS > uptime || c.MinTicks > c.MaxTicks ||
			(c.Flags != 0 && (c.Flags&1 == 0 || hz == 0)) || (c.Flags&2 != 0 && c.CounterDiscontinuities != 0) ||
			c.SpanIntervals > c.Captured || c.SpanIntervals > uptime/500+1 ||
			(c.SpanIntervals == 0 && c.SpanTicks != 0) ||
			(c.SpanIntervals > 0 && (float64(c.SpanTicks)/float64(c.SpanIntervals) < float64(hz)/2 || float64(c.SpanTicks)/float64(c.SpanIntervals) >= float64(hz)*1.5)) {
			return nil, ErrBadTelemetry
		}
		if c.Flags&8 != 0 {
			if c.Flags&4 == 0 || c.PeriodTicks < hz/2 || uint64(c.PeriodTicks)*2 >= uint64(hz)*3 {
				return nil, ErrBadTelemetry
			}
			value := float64(c.PeriodTicks) * 1e9 / float64(hz)
			c.PeriodNS = &value
		} else if c.PeriodTicks != 0 {
			return nil, ErrBadTelemetry
		}
		if c.Flags&16 != 0 {
			if c.Flags&4 == 0 || c.WidthTicks == 0 || uint64(c.WidthTicks)*2 >= uint64(hz)*3 {
				return nil, ErrBadTelemetry
			}
			value := float64(c.WidthTicks) * 1e9 / float64(hz)
			c.WidthNS = &value
		} else if c.WidthTicks != 0 {
			return nil, ErrBadTelemetry
		}
		if c.Flags&32 != 0 {
			if c.Flags&12 != 12 || c.SpanIntervals == 0 {
				return nil, ErrBadTelemetry
			}
			phase := (float64(c.SpanTicks) - float64(c.SpanIntervals)*float64(hz)) * 1e9 / float64(hz)
			ppm := (float64(c.SpanTicks)/float64(c.SpanIntervals)/float64(hz) - 1) * 1e6
			c.SpanPhaseNS = &phase
			c.PeriodErrorPPM = &ppm
		}
	}
	phase := int32(u32(v[24:]))
	if v[5]&1 != 0 {
		if hz == 0 || int64(phase) < -int64(hz)/2 || int64(phase) >= int64(hz)/2 || d.GNSS.Flags&12 != 12 || d.RTC.Flags&12 != 12 {
			return nil, ErrBadTelemetry
		}
		d.PhaseTicks = &phase
		ns := float64(phase) * 1e9 / float64(hz)
		d.PhaseNS = &ns
	} else if phase != 0 {
		return nil, ErrBadTelemetry
	}
	return d, nil
}
