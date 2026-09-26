package ingest

import "encoding/binary"

// BoardBarometer is the MAX board's MS5607 (tag 11): local absolute pressure without
// sea-level correction. ExtendedRange marks a reading outside the 300-1100 mbar
// full-accuracy range; any height derived from pressure is a model result.
type BoardBarometer struct {
	State         string   `json:"state"`
	TemperatureC  *float64 `json:"temperature_c"`
	PressurePa    *uint32  `json:"pressure_pa"`
	ExtendedRange bool     `json:"extended_range"`
}

// BoardThermocouple is the MAX board's MAX31856 external thermocouple input (tag 12).
// A reading is valid only for a new conversion without faults; cold-junction faults
// alone leave the thermocouple invalid and the cold junction too.
type BoardThermocouple struct {
	State         string   `json:"state"`
	Type          string   `json:"type"`
	NotchHz       int      `json:"mains_notch_hz"`
	NewConversion bool     `json:"new_conversion"`
	Faults        []string `json:"faults"`
	ThermocoupleC *float64 `json:"thermocouple_c"`
	ColdJunctionC *float64 `json:"cold_junction_c"`
}

// BoardMotion is the MAX board's ICM-45686 IMU and MMC34160PJ magnetometer summary
// (tag 13). The IMU runs at 12.5 Hz while still and at its profile's moving rate
// otherwise; RateHz is the rate in force. Vectors are in each sensor's own axes;
// mounting calibration, attitude and heading are not applied. Counters run since ESP boot.
type BoardMotion struct {
	IMUState          string             `json:"imu_state"`
	MagnetometerState string             `json:"magnetometer_state"`
	Profile           string             `json:"motion_profile"`
	Moving            bool               `json:"moving"`
	RateHz            float64            `json:"imu_rate_hz"`
	AccelRangeG       uint8              `json:"accel_range_g"`
	GyroRangeDPS      uint16             `json:"gyro_range_dps"`
	Packets           uint32             `json:"imu_packets"`
	Overflows         uint32             `json:"imu_fifo_overflows"`
	Resyncs           uint32             `json:"imu_fifo_resyncs"`
	RateChanges       uint32             `json:"imu_rate_changes"`
	Latest            *BoardIMUSample    `json:"latest,omitempty"`
	Window            *BoardMotionWindow `json:"window,omitempty"`
	Magnetometer      *BoardMagnetometer `json:"magnetometer,omitempty"`
}

// BoardIMUSample is the latest FIFO sample, raw and scaled by the reported ranges.
type BoardIMUSample struct {
	UptimeMS     uint64     `json:"uptime_ms"`
	AccelCounts  [3]int16   `json:"accel_counts"`
	GyroCounts   [3]int16   `json:"gyro_counts"`
	AccelG       [3]float64 `json:"accel_g"`
	GyroDPS      [3]float64 `json:"gyro_dps"`
	TemperatureC float64    `json:"temperature_c"`
}

// BoardMotionWindow covers every valid sample since the previous report the observer
// queued: the time they span, their time-weighted mean vectors (the mean acceleration is
// the gravity direction while the unit is not accelerating), and the extremes of the
// acceleration and rotation-rate magnitudes.
type BoardMotionWindow struct {
	Samples     uint32     `json:"samples"`
	SpanMS      uint32     `json:"span_ms"`
	AccelMeanG  [3]float64 `json:"accel_mean_g"`
	GyroMeanDPS [3]float64 `json:"gyro_mean_dps"`
	AccelMinG   float64    `json:"accel_min_g"`
	AccelMaxG   float64    `json:"accel_max_g"`
	GyroMaxDPS  float64    `json:"gyro_max_dps"`
}

// BoardMagnetometer is one SET/RESET measurement: the field with the bridge offset
// removed, and that offset.
type BoardMagnetometer struct {
	UptimeMS        uint64     `json:"uptime_ms"`
	FieldCounts     [3]int16   `json:"field_counts"`
	FieldMicrotesla [3]float64 `json:"field_microtesla"`
	OffsetCounts    [3]uint16  `json:"bridge_offset_counts"`
}

var barometerStates = []string{"absent", "calibration_rejected", "ready"}
var thermocoupleFaults = []string{"open_circuit", "over_under_voltage", "thermocouple_low", "thermocouple_high",
	"cold_junction_low", "cold_junction_high", "thermocouple_range", "cold_junction_range"}

const coldJunctionFaults = 0x80 | 0x20 | 0x10

func centi(raw int32) *float64 {
	v := float64(raw) / 100
	return &v
}

func decodeBarometer(v []byte) (*BoardBarometer, error) {
	if len(v) != 10 || v[0] != 1 || v[1] > 2 || v[2] > 1 || v[3] > 1 {
		return nil, ErrBadTelemetry
	}
	temp, pressure := int16(binary.BigEndian.Uint16(v[4:])), binary.BigEndian.Uint32(v[6:])
	b := &BoardBarometer{State: barometerStates[v[1]], ExtendedRange: v[3] != 0}
	if v[2] == 0 {
		if temp != 0 || pressure != 0 || v[3] != 0 {
			return nil, ErrBadTelemetry
		}
		return b, nil
	}
	extended := pressure < 30000 || pressure > 110000
	if v[1] != 2 || temp < -4000 || temp > 8500 || pressure < 1000 || pressure > 120000 || extended != b.ExtendedRange {
		return nil, ErrBadTelemetry
	}
	b.TemperatureC, b.PressurePa = centi(int32(temp)), &pressure
	return b, nil
}

func decodeThermocouple(v []byte) (*BoardThermocouple, error) {
	if len(v) != 12 || v[0] != 1 || v[1] > 1 || v[2] > 3 || v[3] > 1 || v[5]&0x0f != 3 || v[5]&^0x1f != 0 {
		return nil, ErrBadTelemetry
	}
	valid, fresh, fault := v[2], v[3] != 0, v[4]
	tc, cj := int32(binary.BigEndian.Uint32(v[6:])), int16(binary.BigEndian.Uint16(v[10:]))
	t := &BoardThermocouple{State: []string{"not_responding", "configured"}[v[1]], Type: "K", NotchHz: 60,
		NewConversion: fresh, Faults: []string{}}
	if v[5]&0x10 != 0 {
		t.NotchHz = 50
	}
	for bit, name := range thermocoupleFaults {
		if fault&(1<<bit) != 0 {
			t.Faults = append(t.Faults, name)
		}
	}
	if v[1] == 0 && (valid != 0 || fresh || fault != 0) {
		return nil, ErrBadTelemetry
	}
	// Valid needs a new conversion: any fault rejects the thermocouple, and a
	// cold-junction fault rejects the cold junction.
	if valid != 0 && !fresh || valid&1 != 0 && fault != 0 || valid&2 != 0 && fault&coldJunctionFaults != 0 {
		return nil, ErrBadTelemetry
	}
	if valid&1 == 0 && tc != 0 || valid&1 != 0 && (tc < -27000 || tc > 180000) {
		return nil, ErrBadTelemetry
	}
	if valid&2 == 0 && cj != 0 || valid&2 != 0 && (cj < -5500 || cj > 12500) {
		return nil, ErrBadTelemetry
	}
	if valid&1 != 0 {
		t.ThermocoupleC = centi(tc)
	}
	if valid&2 != 0 {
		t.ColdJunctionC = centi(int32(cj))
	}
	return t, nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func decodeMotion(v []byte, uptime uint64) (*BoardMotion, error) {
	if len(v) != 95 || v[0] != 1 || v[1] > 1 || v[2] > 1 || v[3] > 7 || v[4] > 1 || v[5] > 1 {
		return nil, ErrBadTelemetry
	}
	u16, u32, u64 := binary.BigEndian.Uint16, binary.BigEndian.Uint32, binary.BigEndian.Uint64
	states := []string{"not_responding", "ready"}
	rate := u16(v[6:])
	m := &BoardMotion{IMUState: states[v[1]], MagnetometerState: states[v[2]],
		Profile: []string{"surface", "aerial"}[v[4]], Moving: v[5] != 0, RateHz: float64(rate) / 10,
		AccelRangeG: v[8], GyroRangeDPS: u16(v[9:]), Packets: u32(v[25:]), Overflows: u32(v[29:]),
		Resyncs: u32(v[33:]), RateChanges: u32(v[37:])}
	accelFS := map[uint8]bool{2: true, 4: true, 8: true, 16: true, 32: true}
	gyroFS := map[uint16]bool{125: true, 250: true, 500: true, 1000: true, 2000: true, 4000: true}
	if rate == 0 || rate > 64000 || !accelFS[m.AccelRangeG] || !gyroFS[m.GyroRangeDPS] {
		return nil, ErrBadTelemetry
	}
	accelScale, gyroScale := float64(m.AccelRangeG)/32768, float64(m.GyroRangeDPS)/32768
	valid := v[3]
	if valid&1 == 0 {
		if !allZero(v[11:25]) || !allZero(v[67:75]) {
			return nil, ErrBadTelemetry
		}
	} else {
		s := &BoardIMUSample{UptimeMS: u64(v[67:])}
		temp := int16(u16(v[23:]))
		// The FIFO's one-byte temperature is C = raw / 2 + 25: steps of 0.5 C.
		if v[1] != 1 || s.UptimeMS > uptime || temp < -3900 || temp > 8850 || temp%50 != 0 {
			return nil, ErrBadTelemetry
		}
		s.TemperatureC = float64(temp) / 100
		for i := 0; i < 3; i++ {
			s.AccelCounts[i], s.GyroCounts[i] = int16(u16(v[11+2*i:])), int16(u16(v[17+2*i:]))
			if s.AccelCounts[i] == -32768 || s.GyroCounts[i] == -32768 {
				return nil, ErrBadTelemetry
			}
			s.AccelG[i] = float64(s.AccelCounts[i]) * accelScale
			s.GyroDPS[i] = float64(s.GyroCounts[i]) * gyroScale
		}
		m.Latest = s
	}
	if valid&2 == 0 {
		if !allZero(v[41:67]) {
			return nil, ErrBadTelemetry
		}
	} else {
		w := &BoardMotionWindow{Samples: u32(v[41:]), SpanMS: u32(v[45:]),
			AccelMinG: float64(u16(v[61:])) / 1000, AccelMaxG: float64(u16(v[63:])) / 1000,
			GyroMaxDPS: float64(u16(v[65:])) / 10}
		if w.Samples == 0 || w.SpanMS == 0 || w.AccelMinG > w.AccelMaxG {
			return nil, ErrBadTelemetry
		}
		for i := 0; i < 3; i++ {
			w.AccelMeanG[i] = float64(int16(u16(v[49+2*i:]))) * accelScale
			w.GyroMeanDPS[i] = float64(int16(u16(v[55+2*i:]))) * gyroScale
		}
		m.Window = w
	}
	if valid&4 == 0 {
		if !allZero(v[75:95]) {
			return nil, ErrBadTelemetry
		}
	} else {
		g := &BoardMagnetometer{UptimeMS: u64(v[87:])}
		if v[2] != 1 || g.UptimeMS > uptime {
			return nil, ErrBadTelemetry
		}
		for i := 0; i < 3; i++ {
			g.FieldCounts[i], g.OffsetCounts[i] = int16(u16(v[75+2*i:])), u16(v[81+2*i:])
			// 2048 counts per gauss, 100 microtesla per gauss.
			g.FieldMicrotesla[i] = float64(g.FieldCounts[i]) * 100 / 2048
		}
		m.Magnetometer = g
	}
	return m, nil
}
