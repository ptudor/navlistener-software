package ingest

import "encoding/binary"

// BoardRTCTemperature is tag 26: the temperature the ZED/X20's DS3231M (or a MAX31328) measures
// for its own oscillator compensation, in 0.25 C steps and typically within 3 C. It is the RTC's
// reading, kept apart from the environment component so that tag 1 means the same on every board.
type BoardRTCTemperature struct {
	// State is "answering" or "not_answering".
	State        string   `json:"state"`
	TemperatureC *float64 `json:"temperature_c"`
}

// The register's 10-bit range in 0.01 C, -128 to +127.75 C, in steps of 0.25 C.
const (
	rtcTemperatureMinCentiC, rtcTemperatureMaxCentiC = -12800, 12775
	rtcTemperatureStepCentiC                         = 25
)

// decodeRTCTemperature reads tag 26 (5 bytes). A withheld measurement must be zero on the wire.
func decodeRTCTemperature(v []byte) (*BoardRTCTemperature, error) {
	if len(v) != 5 || v[0] != 1 || v[1] > 1 || v[2] > 1 || (v[2] == 1 && v[1] == 0) {
		return nil, ErrBadTelemetry
	}
	t := &BoardRTCTemperature{State: []string{"not_answering", "answering"}[v[1]]}
	centiC := int16(binary.BigEndian.Uint16(v[3:]))
	if v[2] == 0 {
		if centiC != 0 {
			return nil, ErrBadTelemetry
		}
		return t, nil
	}
	if centiC < rtcTemperatureMinCentiC || centiC > rtcTemperatureMaxCentiC || centiC%rtcTemperatureStepCentiC != 0 {
		return nil, ErrBadTelemetry
	}
	c := float64(centiC) / 100
	t.TemperatureC = &c
	return t, nil
}
