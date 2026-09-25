package ingest

import "encoding/binary"

// BoardHumidityHeater is the HDC humidity-sensor heater's condensation-recovery
// state. While State is not "normal" the report's HDC temperature and humidity
// are withheld (null), and the other sensors' values were taken with the heater
// on or the sensor still cooling. The humidity dwell counters use fixed
// 95/98 %RH buckets and count normal service only, since ESP boot.
type BoardHumidityHeater struct {
	State            string          `json:"state"`
	TrustedUTC       bool            `json:"trusted_utc"`
	LastRunReadable  bool            `json:"last_run_readable"`
	RunsSinceBoot    uint8           `json:"runs_since_boot"`
	AtLeast95Seconds uint32          `json:"humidity_at_least_95_s"`
	AtLeast98Seconds uint32          `json:"humidity_at_least_98_s"`
	CondensingS      uint32          `json:"condensing_streak_s"`
	LastRunUnix      *uint64         `json:"last_run_unix_seconds"`
	LatestRun        *BoardHeaterRun `json:"latest_run,omitempty"`
}

// BoardHeaterRun is the latest run of this boot. On and recovery times keep
// running while those phases last. Nil measurements were unavailable.
type BoardHeaterRun struct {
	StartUptimeMS  uint64   `json:"start_uptime_ms"`
	OnMS           uint32   `json:"on_ms"`
	RecoveryMS     uint32   `json:"recovery_ms"`
	StopReason     *string  `json:"stop_reason"`
	HumidityBefore *float64 `json:"humidity_before_percent"`
	HumidityAtStop *float64 `json:"humidity_at_stop_percent"`
	HDCBeforeC     *float64 `json:"hdc2080_before_c"`
	HDCPeakC       *float64 `json:"hdc2080_peak_c"`
	HDCEndC        *float64 `json:"hdc2080_end_c"`
	MCPBeforeC     *float64 `json:"mcp9808_before_c"`
	MCPPeakC       *float64 `json:"mcp9808_peak_c"`
	MCPEndC        *float64 `json:"mcp9808_end_c"`
}

var heaterStates = []string{"normal", "heating", "stopping", "recovering"}
var heaterStops = []string{"", "dry", "timeout", "overtemp", "bus_error", "sensor_lost"}

func decodeHumidityHeater(v []byte, uptime uint64) (*BoardHumidityHeater, error) {
	if len(v) != 58 || v[0] != 1 || v[1] > 3 || v[2] > 3 || v[40] > 5 {
		return nil, ErrBadTelemetry
	}
	u16, u32, u64 := binary.BigEndian.Uint16, binary.BigEndian.Uint32, binary.BigEndian.Uint64
	state, runs, last, stop, valid := v[1], v[3], u64(v[16:]), v[40], v[41]
	h := &BoardHumidityHeater{State: heaterStates[state], TrustedUTC: v[2]&1 != 0, LastRunReadable: v[2]&2 != 0,
		RunsSinceBoot: runs, AtLeast95Seconds: u32(v[4:]), AtLeast98Seconds: u32(v[8:]), CondensingS: u32(v[12:])}
	// An unreadable last-run record blocks automatic runs for the boot. The 98 %RH
	// intervals are a subset of the 95 %RH ones, and only normal service accumulates.
	if h.AtLeast98Seconds > h.AtLeast95Seconds || (state != 0 && h.CondensingS != 0) ||
		(v[2]&2 == 0 && (last != 0 || runs != 0)) || (last != 0 && (last < 946684800 || last >= 4102444800)) {
		return nil, ErrBadTelemetry
	}
	if last != 0 {
		h.LastRunUnix = &last
	}
	if runs == 0 {
		for _, b := range v[24:] {
			if b != 0 {
				return nil, ErrBadTelemetry
			}
		}
		if state != 0 {
			return nil, ErrBadTelemetry
		}
		return h, nil
	}
	r := &BoardHeaterRun{StartUptimeMS: u64(v[24:]), OnMS: u32(v[32:]), RecoveryMS: u32(v[36:])}
	ended := valid&(16|128) != 0
	if last == 0 || r.StartUptimeMS > uptime || uptime-r.StartUptimeMS < uint64(r.OnMS)+uint64(r.RecoveryMS) ||
		valid&(1|4) != 1|4 || (state == 1) != (stop == 0) || (state < 3 && state != 0 && r.RecoveryMS != 0) ||
		(state != 0 && ended) || (state == 0 && r.RecoveryMS == 0) {
		return nil, ErrBadTelemetry
	}
	if stop != 0 {
		reason := heaterStops[stop]
		r.StopReason = &reason
	}
	humidity := []**float64{&r.HumidityBefore, &r.HumidityAtStop}
	for i, dest := range humidity {
		raw := u16(v[42+2*i:])
		if (valid&(1<<i) == 0 && raw != 0) || raw > 10000 {
			return nil, ErrBadTelemetry
		}
		if valid&(1<<i) != 0 {
			value := float64(raw) / 100
			*dest = &value
		}
	}
	temps := []**float64{&r.HDCBeforeC, &r.HDCPeakC, &r.HDCEndC, &r.MCPBeforeC, &r.MCPPeakC, &r.MCPEndC}
	for i, dest := range temps {
		raw, bit := int16(u16(v[46+2*i:])), uint8(4<<i)
		if (valid&bit == 0 && raw != 0) || (valid&bit != 0 && (raw < -4000 || raw > 12500)) {
			return nil, ErrBadTelemetry
		}
		if valid&bit != 0 {
			value := float64(raw) / 100
			*dest = &value
		}
	}
	h.LatestRun = r
	return h, nil
}
