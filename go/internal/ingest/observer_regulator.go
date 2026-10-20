package ingest

import "encoding/binary"

// BoardRegulator is the system regulator's heat-spreader sensor and the Wi-Fi throttle it
// drives (tag 23): on the P3, the MCP9808 U47 beside the LDL1117's tab. The LDO's loss
// follows the 3V3_SYS current, which Wi-Fi transmission dominates, so the board lowers the
// radio's maximum transmit power in steps while the spreader is hot. This is the regulator's
// temperature, not the air's: it is no environmental reading and implies no alarm.
type BoardRegulator struct {
	State string `json:"state"`
	// SpreaderC is the latest reading; nil while the sensor is not responding.
	SpreaderC *float64 `json:"spreader_c"`
	// PeakC is the highest reading since the previous report, or the last one seen.
	PeakC *float64 `json:"peak_c"`
	// ThrottleLevel is 0 (unthrottled) to 4; WiFiTXCapDBm is the transmit cap it applies,
	// nil at level 0, where the radio's own maximum stands.
	ThrottleLevel    uint8    `json:"throttle_level"`
	WiFiTXCapDBm     *float64 `json:"wifi_tx_cap_dbm"`
	ThrottleChanges  uint32   `json:"throttle_changes"`
	ThrottledSeconds uint32   `json:"throttled_s"`
	// The board's limits at the sensor: throttle a step at a time from ThrottleC, entirely
	// from FullC, and release a step at a time at or below ReleaseC.
	ThrottleC float64 `json:"throttle_c"`
	FullC     float64 `json:"full_c"`
	ReleaseC  float64 `json:"release_c"`
}

// The MCP9808's specified range in 0.01 C, and the 0.25 dBm transmit caps ESP-IDF accepts.
const (
	regulatorMinCentiC, regulatorMaxCentiC = -4000, 12500
	regulatorMinCap, regulatorMaxCap       = 8, 84
	regulatorLevels                        = 5
)

func decodeRegulator(v []byte) (*BoardRegulator, error) {
	if len(v) != 23 || v[0] != 1 || v[1] > 1 || v[2]&^3 != 0 || v[3] >= regulatorLevels || (v[1] == 0 && v[2]&1 != 0) {
		return nil, ErrBadTelemetry
	}
	u16, u32 := binary.BigEndian.Uint16, binary.BigEndian.Uint32
	temperature := func(raw uint16) (float64, bool) {
		c := int16(raw)
		return float64(c) / 100, c >= regulatorMinCentiC && c <= regulatorMaxCentiC
	}
	g := &BoardRegulator{State: []string{"not_responding", "ready"}[v[1]], ThrottleLevel: v[3],
		ThrottleChanges: u32(v[9:]), ThrottledSeconds: u32(v[13:])}
	for i, dst := range []**float64{&g.SpreaderC, &g.PeakC} {
		raw := u16(v[4+2*i:])
		if v[2]&(1<<i) == 0 {
			if raw != 0 {
				return nil, ErrBadTelemetry
			}
			continue
		}
		c, ok := temperature(raw)
		if !ok {
			return nil, ErrBadTelemetry
		}
		*dst = &c
	}
	switch tx := v[8]; {
	case v[3] == 0 && tx != 0, v[3] != 0 && (tx < regulatorMinCap || tx > regulatorMaxCap):
		return nil, ErrBadTelemetry
	case tx != 0:
		dbm := float64(tx) / 4
		g.WiFiTXCapDBm = &dbm
	}
	var ok [3]bool
	g.ThrottleC, ok[0] = temperature(u16(v[17:]))
	g.FullC, ok[1] = temperature(u16(v[19:]))
	g.ReleaseC, ok[2] = temperature(u16(v[21:]))
	if !ok[0] || !ok[1] || !ok[2] || g.ReleaseC >= g.ThrottleC || g.ThrottleC > g.FullC {
		return nil, ErrBadTelemetry
	}
	return g, nil
}
