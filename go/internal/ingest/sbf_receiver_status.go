package ingest

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// SBFReceiverStatusBlock is the block number of Septentrio's ReceiverStatus. It is the
// one SBF block decoded into live state: receiver telemetry, not navigation data. Field
// layout, units and Do-Not-Use values are from the mosaic-G5 firmware reference guide
// v1.0.1, section 4.2 (ReceiverStatus and its AGCState sub-block); the mosaic-X5 guide
// v4.15.0 gives the same layout.
const SBFReceiverStatusBlock = 4014

// Offsets within the block body, which starts at TOW after the eight-byte header.
const (
	sbfStatusFixedLen   = 20 // TOW(4) WNc(2) CPULoad ExtError UpTime(4) RxState(4) RxError(4)
	sbfStatusCountsLen  = 24 // … N SBLength CmdCount Temperature
	sbfAGCStateMinLen   = 4  // FrontEndID Gain SampleVar BlankingStat
	sbfGainNotLocked    = -128
	sbfTemperatureShift = 100
)

// ReceiverStatus bit fields a consumer may test.
const (
	// SBFRxStateActiveAntenna is set while the receiver senses an antenna drawing
	// current. It is not evidence either way: a mosaic-G5 on firmware 1.0.0 left it
	// clear with an antenna drawing 33 mA, so the front ends' gains are what shows an
	// antenna is connected.
	SBFRxStateActiveAntenna = 1 << 1
	SBFRxErrorSoftware      = 1 << 3
	SBFRxErrorWatchdog      = 1 << 4
	SBFRxErrorAntenna       = 1 << 5 // antenna overcurrent
	SBFRxErrorCongestion    = 1 << 6 // output congestion on a port during the last second
	SBFRxErrorCPUOverload   = 1 << 9
)

// ErrBadSBFReceiverStatus is returned for a ReceiverStatus body shorter than its fixed
// fields, or whose AGCState sub-blocks do not fit the block.
var ErrBadSBFReceiverStatus = errors.New("ingest: malformed SBF ReceiverStatus")

// SBFFrontEnd is one AGCState sub-block: one RF front end's automatic gain control.
// The receiver turns its gain down as the power at that front end's input rises, so a
// gain that falls means noise or interference arriving (or more amplification ahead of
// the receiver) and one that rises means the antenna, its amplifier or its cable giving
// less. The mosaic-G5 hardware manual (4.2.1) gives the amplification ahead of the
// module as 65 dB less this gain.
type SBFFrontEnd struct {
	// ID is the FrontEndID byte as sent: Code in bits 0-4 and Antenna in bits 5-7. It
	// identifies the front end within one receiver.
	ID uint8 `json:"id"`
	// Code is the front end (SBFFrontEndName); Antenna is 0 for the main antenna, 1 for
	// Aux1 and 2 for Aux2.
	Code    uint8 `json:"code"`
	Antenna uint8 `json:"antenna"`
	// GainDB is the AGC gain in dB. It is nil, and Locked false, while the front end's
	// PLL is not locked.
	Locked bool `json:"locked"`
	GainDB *int `json:"gain_db,omitempty"`
	// SampleVariance is the normalized variance of the IF samples, nominally 100, nil
	// when the receiver does not report one.
	SampleVariance *int `json:"sample_variance,omitempty"`
	// BlankedPercent is the share of samples the pulse blanker removes; always zero on
	// a receiver without one.
	BlankedPercent int `json:"blanked_percent"`
}

// SBFReceiverStatus is one decoded ReceiverStatus block. Fields the receiver marked
// Do-Not-Use are nil, never a stand-in value.
type SBFReceiverStatus struct {
	// TOWMS and WNc are the receiver time stamp: milliseconds of the GPS week, and the
	// continuous week number.
	TOWMS *uint32 `json:"tow_ms,omitempty"`
	WNc   *uint16 `json:"wnc,omitempty"`
	// CPULoadPercent is the load on the receiver's processor.
	CPULoadPercent *int `json:"cpu_load_percent,omitempty"`
	// ExtError, RxState and RxError are the receiver's bit fields, as sent.
	ExtError uint8  `json:"ext_error"`
	RxState  uint32 `json:"rx_state"`
	RxError  uint32 `json:"rx_error"`
	// UptimeS is seconds since the receiver started or was last reset.
	UptimeS uint32 `json:"uptime_s"`
	// CmdCount counts the commands that changed the receiver's configuration, 1 to 255
	// and round again.
	CmdCount *int `json:"cmd_count,omitempty"`
	// TemperatureC is the receiver's own temperature.
	TemperatureC *int `json:"temperature_c,omitempty"`
	// FrontEnds are the AGCState sub-blocks in the receiver's order.
	FrontEnds []SBFFrontEnd `json:"front_ends,omitempty"`
}

// sbfFrontEndNames are the reference guide's front-end codes; the three combined front
// ends carry the short names the observer's own log uses.
var sbfFrontEndNames = [...]string{
	"GPSL1/E1", "GLOL1", "E6", "GPSL2", "GLOL2", "L5/E5a/B2a", "E5b/B2b", "E5",
	"L1 combined", "L2 combined", "MSS/L-band", "B1", "B3", "S-band", "B3/E6 combined",
}

// SBFFrontEndName names a front-end code as the reference guide does, or "fe" and the
// number for one it does not define.
func SBFFrontEndName(code uint8) string {
	if int(code) < len(sbfFrontEndNames) {
		return sbfFrontEndNames[code]
	}
	return fmt.Sprintf("fe%d", code)
}

// SBFFrontEndLabel names a front end by its FrontEndID byte, with the antenna when it
// is not the main one: "GPSL1/E1", "aux1 GPSL1/E1".
func SBFFrontEndLabel(id uint8) string {
	name := SBFFrontEndName(id & 0x1f)
	switch antenna := id >> 5; antenna {
	case 0:
		return name
	case 1, 2:
		return fmt.Sprintf("aux%d %s", antenna, name)
	default:
		return fmt.Sprintf("ant%d %s", antenna, name)
	}
}

// DecodeSBFReceiverStatus decodes a ReceiverStatus block body (the block without its
// eight-byte header). A body that ends after RxError, as a receiver without the AGC
// fields sends it, decodes with no front ends. A body whose sub-block count and length
// do not fit it is malformed: nothing is taken from a block that contradicts itself.
func DecodeSBFReceiverStatus(body []byte) (*SBFReceiverStatus, error) {
	if len(body) < sbfStatusFixedLen {
		return nil, ErrBadSBFReceiverStatus
	}
	s := &SBFReceiverStatus{
		ExtError: body[7],
		UptimeS:  binary.LittleEndian.Uint32(body[8:]),
		RxState:  binary.LittleEndian.Uint32(body[12:]),
		RxError:  binary.LittleEndian.Uint32(body[16:]),
	}
	if tow := binary.LittleEndian.Uint32(body[0:]); tow != 0xFFFFFFFF {
		s.TOWMS = &tow
	}
	if wnc := binary.LittleEndian.Uint16(body[4:]); wnc != 0xFFFF {
		s.WNc = &wnc
	}
	if load := int(body[6]); load != 0xFF {
		s.CPULoadPercent = &load
	}
	if len(body) < sbfStatusCountsLen {
		return s, nil
	}
	count, size := int(body[20]), int(body[21])
	if commands := int(body[22]); commands != 0 {
		s.CmdCount = &commands
	}
	if raw := int(body[23]); raw != 0 {
		celsius := raw - sbfTemperatureShift
		s.TemperatureC = &celsius
	}
	if count == 0 {
		return s, nil
	}
	if size < sbfAGCStateMinLen || sbfStatusCountsLen+count*size > len(body) {
		return nil, ErrBadSBFReceiverStatus
	}
	s.FrontEnds = make([]SBFFrontEnd, count)
	for i := range s.FrontEnds {
		sub := body[sbfStatusCountsLen+i*size:]
		fe := SBFFrontEnd{ID: sub[0], Code: sub[0] & 0x1f, Antenna: sub[0] >> 5, BlankedPercent: int(sub[3])}
		if gain := int(int8(sub[1])); gain != sbfGainNotLocked {
			fe.Locked, fe.GainDB = true, &gain
		}
		if variance := int(sub[2]); variance != 0 {
			fe.SampleVariance = &variance
		}
		s.FrontEnds[i] = fe
	}
	return s, nil
}

// attachSBFTelemetry gives a captured SBF block frame the station telemetry its block
// carries: a ReceiverStatus becomes an RF sample, so it reaches the station RF state
// and the private RF history like a u-blox MON-RF report. Every other block, and a
// ReceiverStatus that does not decode, stays a captured block; it reports whether a
// ReceiverStatus failed to decode, for the caller's error count.
func attachSBFTelemetry(f *RawFrame) (malformed bool) {
	if f.MsgType != SBFReceiverStatusBlock {
		return false
	}
	status, err := DecodeSBFReceiverStatus(f.Bytes)
	if err != nil {
		return true
	}
	f.RF = &RawRF{Status: status}
	return false
}
