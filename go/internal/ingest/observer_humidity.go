package ingest

import "encoding/binary"

// BoardExtraHumidity is one of the humidity sensors beside the HDC: the SHT3x (tag 24, the
// P3's SHT35 at 0x44) or the HIH8121 (tag 25, at 0x27). Each is an ambient reading of its
// own, kept apart from the environment component so that tag 1 means the same on every board.
type BoardExtraHumidity struct {
	// State is "ready" or "not_identified" for the SHT3x, and "answering" or "not_answering"
	// for the HIH8121.
	State string `json:"state"`
	// Status is the HIH8121's fetched status while it answers: "fresh", "stale",
	// "command_mode" or "unused". Only fresh data is a measurement. Empty for the SHT3x.
	Status          string   `json:"status,omitempty"`
	TemperatureC    *float64 `json:"temperature_c"`
	HumidityPercent *float64 `json:"humidity_percent"`
}

// Both parts' operating range in 0.01 C, and full-scale humidity in 0.01 %RH.
const (
	extraHumidityMinCentiC, extraHumidityMaxCentiC = -4000, 12500
	extraHumidityMaxCentiRH                        = 10000
)

// decodeExtraHumidity reads tag 24 (hih false, 7 bytes) or tag 25 (hih true, 8 bytes, with
// the status byte at offset 3). A withheld measurement must be zero on the wire.
func decodeExtraHumidity(v []byte, hih bool) (*BoardExtraHumidity, error) {
	size, values := 7, 3
	if hih {
		size, values = 8, 4
	}
	if len(v) != size || v[0] != 1 || v[1] > 1 || v[2] > 1 || (v[2] == 1 && v[1] == 0) {
		return nil, ErrBadTelemetry
	}
	h := &BoardExtraHumidity{State: []string{"not_identified", "ready"}[v[1]]}
	if hih {
		status := v[3]
		if status > 3 || (v[1] == 0 && status != 0) || (v[2] == 1 && status != 0) {
			return nil, ErrBadTelemetry
		}
		h.State = []string{"not_answering", "answering"}[v[1]]
		if v[1] == 1 {
			h.Status = []string{"fresh", "stale", "command_mode", "unused"}[status]
		}
	}
	centiC, centiRH := int16(binary.BigEndian.Uint16(v[values:])), binary.BigEndian.Uint16(v[values+2:])
	if v[2] == 0 {
		if centiC != 0 || centiRH != 0 {
			return nil, ErrBadTelemetry
		}
		return h, nil
	}
	if centiC < extraHumidityMinCentiC || centiC > extraHumidityMaxCentiC || centiRH > extraHumidityMaxCentiRH {
		return nil, ErrBadTelemetry
	}
	c, rh := float64(centiC)/100, float64(centiRH)/100
	h.TemperatureC, h.HumidityPercent = &c, &rh
	return h, nil
}
