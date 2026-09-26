package ingest

import "encoding/binary"

// BoardRails is the ZED/X20's INA3221 rail monitor (tag 16): per channel, the voltage at the
// load side of the shunt and the voltage across it, each a mean over one 0.42 s averaging
// cycle, with the board's shunt resistance. Current is shunt voltage over resistance. The
// X20's channels are +5V after the input eFuse (20 mOhm), 3V3_GNSS after its regulator
// (50 mOhm; the receiver and the antenna feed) and 3V3_SYS (20 mOhm).
type BoardRails struct {
	State    string      `json:"state"`
	Channels []BoardRail `json:"channels"`
}

// BoardRail is one INA3221 channel. A channel without a shunt (0 mOhm) is unused; readings
// are null when not measured.
type BoardRail struct {
	Channel         int      `json:"channel"`
	ShuntMilliohms  uint16   `json:"shunt_milliohms"`
	BusVolts        *float64 `json:"bus_v"`
	ShuntMicrovolts *int32   `json:"shunt_uv"`
	CurrentAmps     *float64 `json:"current_a"`
}

// The INA3221's steps and shunt full scale: bus 8 mV steps (an I16 holds its 32.76 V full
// scale), shunt 40 uV steps to 163.8 mV.
const (
	railBusStep                 = 8
	railShuntStep, railShuntMax = 40, 163800
)

func decodeRails(v []byte) (*BoardRails, error) {
	if len(v) != 27 || v[0] != 1 || v[1] > 1 || v[2]&^7 != 0 || (v[1] == 0 && v[2] != 0) {
		return nil, ErrBadTelemetry
	}
	r := &BoardRails{State: []string{"not_responding", "ready"}[v[1]], Channels: make([]BoardRail, 3)}
	for c := range r.Channels {
		p := v[3+8*c:]
		bus, shunt, mohm := int16(binary.BigEndian.Uint16(p)), int32(binary.BigEndian.Uint32(p[2:])), binary.BigEndian.Uint16(p[6:])
		r.Channels[c] = BoardRail{Channel: c + 1, ShuntMilliohms: mohm}
		if v[2]&(1<<c) == 0 {
			if bus != 0 || shunt != 0 {
				return nil, ErrBadTelemetry
			}
			continue
		}
		if mohm == 0 || bus%railBusStep != 0 || shunt%railShuntStep != 0 ||
			shunt > railShuntMax || shunt < -railShuntMax-railShuntStep {
			return nil, ErrBadTelemetry
		}
		volts, amps := float64(bus)/1000, float64(shunt)/float64(mohm)/1000
		r.Channels[c].BusVolts, r.Channels[c].ShuntMicrovolts, r.Channels[c].CurrentAmps = &volts, &shunt, &amps
	}
	return r, nil
}
