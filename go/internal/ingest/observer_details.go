package ingest

import (
	"encoding/binary"
	"encoding/hex"

	"github.com/ptudor/navlistener/internal/boardid"
)

const TelemObserverDetails = 0x04

// ObserverDetails is a complete low-rate board snapshot, not a navigation frame.
// Missing components were not reported. Nil measurements mean unavailable, not zero.
// No field can select an observer, organization, or publication scope.
type ObserverDetails struct {
	Version       uint8                `json:"version"`
	Reason        uint8                `json:"reason"`
	UptimeMS      uint64               `json:"uptime_ms"`
	EventCount    uint32               `json:"event_count"`
	EventUptimeMS uint64               `json:"event_uptime_ms"`
	EventFlags    uint8                `json:"event_flags"`
	EventStates   uint8                `json:"event_states"`
	Environment   *BoardEnvironment    `json:"environment,omitempty"`
	Heater        *BoardHumidityHeater `json:"humidity_heater,omitempty"`
	Barometer     *BoardBarometer      `json:"barometer,omitempty"`
	Thermocouple  *BoardThermocouple   `json:"thermocouple,omitempty"`
	Motion        *BoardMotion         `json:"motion,omitempty"`
	RTC           *BoardRTC            `json:"rtc,omitempty"`
	ATECC         *BoardATECC          `json:"atecc,omitempty"`
	EEPROM        *BoardEEPROM         `json:"eeprom,omitempty"`
	Resources     *BoardResources      `json:"resources,omitempty"`
	Receiver      *BoardReceiver       `json:"receiver,omitempty"`
	Firmware      string               `json:"firmware,omitempty"`
	Update        *BoardUpdate         `json:"update,omitempty"`
	Timing        *BoardTiming         `json:"timing,omitempty"`
}
type BoardEnvironment struct {
	ReadyMask       uint8    `json:"ready_mask"`
	MCP9808C        *float64 `json:"mcp9808_c"`
	HDC2080C        *float64 `json:"hdc2080_c"`
	BMP3C           *float64 `json:"bmp388_bmp384_c"`
	HumidityPercent *float64 `json:"humidity_percent"`
	PressurePa      *uint32  `json:"pressure_pa"`
}
type BoardRTC struct {
	Flags     uint8   `json:"flags"`
	Epoch     *uint64 `json:"unix_seconds"`
	SampledMS uint64  `json:"sampled_uptime_ms"`
}
type BoardATECC struct {
	CheckedMS  uint64 `json:"checked_uptime_ms"`
	Revision   string `json:"revision,omitempty"`
	ConfigLock string `json:"config_lock"`
	DataLock   string `json:"data_lock"`
	RNG        string `json:"rng_screening"`
}
type BoardEEPROM struct {
	Action            string `json:"action"`
	BoardUIDKind      string `json:"board_uid_kind,omitempty"`
	BoardUID          string `json:"board_uid,omitempty"`
	CapabilitiesValid bool   `json:"capabilities_valid"`
	Revision          uint8  `json:"revision"`
	ComponentCount    uint8  `json:"component_count"`
}
type BoardResources struct {
	SpoolPSRAM   bool   `json:"spool_psram"`
	Used         uint32 `json:"spool_used_bytes"`
	Capacity     uint32 `json:"spool_capacity_bytes"`
	Queued       uint32 `json:"spool_records"`
	Dropped      uint64 `json:"spool_dropped_records"`
	InternalFree uint32 `json:"internal_free_bytes"`
	PSRAMFree    uint32 `json:"psram_free_bytes"`
}
type BoardReceiver struct {
	Supported uint8    `json:"supported_mask"`
	Expected  uint8    `json:"expected_mask"`
	Tracked   [8]uint8 `json:"tracked"`
	Valid     uint8    `json:"valid_mask"`
	Jamming   uint8    `json:"jamming_state"`
	Spoofing  uint8    `json:"spoofing_state"`
	RFMS      uint64   `json:"rf_uptime_ms"`
	StatusMS  uint64   `json:"status_uptime_ms"`
}

func decodeObserverDetails(b []byte) (*ObserverDetails, error) {
	if len(b) < 24 || len(b) > 1024 || b[0] != 1 || b[1] == 0 || b[1]&^15 != 0 {
		return nil, ErrBadTelemetry
	}
	u16, u32, u64 := binary.BigEndian.Uint16, binary.BigEndian.Uint32, binary.BigEndian.Uint64
	d := &ObserverDetails{Version: 1, Reason: b[1], UptimeMS: u64(b[2:]), EventCount: u32(b[10:]), EventUptimeMS: u64(b[14:]), EventFlags: b[22], EventStates: b[23]}
	if d.EventUptimeMS > d.UptimeMS || d.EventFlags > 3 || d.EventStates > 15 || (d.EventCount == 0 && (d.EventFlags != 0 || d.EventStates != 0 || d.EventUptimeMS != 0)) {
		return nil, ErrBadTelemetry
	}
	var seen [256]bool
	known := 0
	for off := 24; off < len(b); {
		if len(b)-off < 3 {
			return nil, ErrBadTelemetry
		}
		tag, n := b[off], int(u16(b[off+1:]))
		off += 3
		if tag == 0 || seen[tag] || n > len(b)-off {
			return nil, ErrBadTelemetry
		}
		seen[tag] = true
		v := b[off : off+n]
		off += n
		switch tag {
		case 1:
			if n != 14 || v[0]&^v[1] != 0 || v[1] > 7 {
				return nil, ErrBadTelemetry
			}
			e := &BoardEnvironment{ReadyMask: v[1]}
			temps := []**float64{&e.MCP9808C, &e.HDC2080C, &e.BMP3C}
			for i, dest := range temps {
				raw := int16(u16(v[2+2*i:]))
				valid := v[0]&(1<<i) != 0
				max := int16(12500)
				if i == 2 {
					max = 8500
				}
				if (valid && (raw < -4000 || raw > max)) || (!valid && raw != 0) {
					return nil, ErrBadTelemetry
				}
				if valid {
					value := float64(raw) / 100
					*dest = &value
				}
			}
			rh, pressure := u16(v[8:]), u32(v[10:])
			if rh > 10000 || (v[0]&2 == 0 && rh != 0) || (v[0]&4 == 0 && pressure != 0) || (v[0]&4 != 0 && (pressure < 30000 || pressure > 125000)) {
				return nil, ErrBadTelemetry
			}
			if v[0]&2 != 0 {
				value := float64(rh) / 100
				e.HumidityPercent = &value
			}
			if v[0]&4 != 0 {
				e.PressurePa = &pressure
			}
			d.Environment = e
		case 2:
			if n != 17 || v[0] > 31 || u64(v[9:]) > d.UptimeMS {
				return nil, ErrBadTelemetry
			}
			rtc := &BoardRTC{Flags: v[0], SampledMS: u64(v[9:])}
			epoch := u64(v[1:])
			if v[0]&16 != 0 {
				if v[0]&3 != 3 || epoch < 946684800 || epoch >= 4102444800 {
					return nil, ErrBadTelemetry
				}
				rtc.Epoch = &epoch
			} else if epoch != 0 {
				return nil, ErrBadTelemetry
			}
			d.RTC = rtc
		case 3:
			if n != 16 || u64(v) > d.UptimeMS || v[8] > 1 || v[13] > 3 || v[14] > 3 || v[15] > 3 {
				return nil, ErrBadTelemetry
			}
			locks := []string{"unknown", "unlocked", "locked", "invalid"}
			c := &BoardATECC{CheckedMS: u64(v), ConfigLock: locks[v[13]], DataLock: locks[v[14]], RNG: []string{"untested", "screening_pass", "repeating_output", "io_error"}[v[15]]}
			if v[8] != 0 {
				c.Revision = hex.EncodeToString(v[9:13])
			}
			d.ATECC = c
		case 4:
			const caps = 2 + boardid.Size // capabilities-valid, then revision and count
			if n != caps+3 || v[0] > 6 || v[1] > 1 || v[caps] > 1 {
				return nil, ErrBadTelemetry
			}
			m := &BoardEEPROM{Action: []string{"io_error", "initialization_required", "recovery_required", "replacement_confirmation_required", "invalid_manifest", "use_manifest", "absent"}[v[0]], CapabilitiesValid: v[caps] != 0, Revision: v[caps+1], ComponentCount: v[caps+2]}
			var uid boardid.ID
			copy(uid[:], v[2:caps])
			if v[1] != 0 {
				if uid.Validate() != nil || v[0] == 6 {
					return nil, ErrBadTelemetry
				}
				m.BoardUIDKind, m.BoardUID = uid.KindName(), uid.Hex()
			} else if uid != (boardid.ID{}) {
				return nil, ErrBadTelemetry
			}
			if (v[caps] != 0 && v[0] != 0 && v[0] != 3 && v[0] != 5) || (v[caps] != 0 && v[1] == 0) || (v[caps] == 0 && (v[caps+1] != 0 || v[caps+2] != 0)) || (v[0] == 5 && v[caps] == 0) {
				return nil, ErrBadTelemetry
			}
			d.EEPROM = m
		case 5:
			if n != 29 || v[0] > 1 || u32(v[1:]) > u32(v[5:]) {
				return nil, ErrBadTelemetry
			}
			d.Resources = &BoardResources{SpoolPSRAM: v[0] != 0, Used: u32(v[1:]), Capacity: u32(v[5:]), Queued: u32(v[9:]), Dropped: u64(v[13:]), InternalFree: u32(v[21:]), PSRAMFree: u32(v[25:])}
		case 6:
			if n != 29 || v[10] > 15 || v[11] > 3 || v[12] > 3 || v[1]&^v[0] != 0 || u64(v[13:]) > d.UptimeMS || u64(v[21:]) > d.UptimeMS {
				return nil, ErrBadTelemetry
			}
			r := &BoardReceiver{Supported: v[0], Expected: v[1], Valid: v[10], Jamming: v[11], Spoofing: v[12], RFMS: u64(v[13:]), StatusMS: u64(v[21:])}
			copy(r.Tracked[:], v[2:10])
			d.Receiver = r
		case 7:
			if n == 0 || n > 32 {
				return nil, ErrBadTelemetry
			}
			for _, c := range v {
				if c < 32 || c > 126 {
					return nil, ErrBadTelemetry
				}
			}
			d.Firmware = string(v)
		case 9:
			update, err := DecodeBoardUpdate(v)
			if err != nil {
				return nil, err
			}
			d.Update = update
		case 8:
			var err error
			d.Timing, err = decodeBoardTiming(v, d.UptimeMS)
			if err != nil {
				return nil, err
			}
		case 10:
			var err error
			d.Heater, err = decodeHumidityHeater(v, d.UptimeMS)
			if err != nil {
				return nil, err
			}
		case 11:
			var err error
			if d.Barometer, err = decodeBarometer(v); err != nil {
				return nil, err
			}
		case 12:
			var err error
			if d.Thermocouple, err = decodeThermocouple(v); err != nil {
				return nil, err
			}
		case 13:
			var err error
			if d.Motion, err = decodeMotion(v, d.UptimeMS); err != nil {
				return nil, err
			}
		default:
			continue // bounded unknown extensions are skipped, not interpreted
		}
		known++
	}
	if known == 0 {
		return nil, ErrBadTelemetry
	}
	return d, nil
}
