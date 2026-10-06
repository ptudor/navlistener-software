package ingest

import (
	"encoding/binary"
	"time"
)

// TelemReceiverSolution is the GNF1 telemetry type of one navigation epoch of the
// receiver's own solution: position and velocity, clock, and status
// (docs/proposals/STATION-ASSURANCE.md §2). It is receiver-neutral; a u-blox source
// fills it from NAV-PVT, NAV-CLOCK and NAV-STATUS.
const TelemReceiverSolution = 0x03

// Present-block bits of a ReceiverSolution body.
const (
	solutionHasPVT    = 0x01
	solutionHasClock  = 0x02
	solutionHasStatus = 0x04
)

const (
	solutionPVTLen    = 64
	solutionClockLen  = 20
	solutionStatusLen = 16
	solutionWeekMS    = 7 * 24 * 3600 * 1000
)

// UTC validity bits of SolutionPVT.UTCValid.
const (
	UTCValidDate          = 0x01
	UTCValidTime          = 0x02
	UTCValidFullyResolved = 0x04
)

// Fix flag bits of SolutionPVT.FixFlags.
const (
	FixFlagOK           = 0x01
	FixFlagDifferential = 0x02
	FixFlagCarrierFloat = 0x40
	FixFlagCarrierFixed = 0x80
)

// PosFlagInvalidLLH marks coordinates the receiver itself says are invalid.
const PosFlagInvalidLLH = 0x0001

// ReceiverSolution is one epoch of the receiver's solution. Any block may be absent;
// at least one is present.
type ReceiverSolution struct {
	PVT    *SolutionPVT    `json:"pvt,omitempty"`
	Clock  *SolutionClock  `json:"clock,omitempty"`
	Status *SolutionStatus `json:"status,omitempty"`
}

// SolutionPVT is the position, velocity and time block, in the receiver's integer
// units: degrees × 1e7, millimetres, mm/s and nanoseconds.
type SolutionPVT struct {
	TOWMS    uint32 `json:"tow_ms"`
	Year     uint16 `json:"year"`
	Month    uint8  `json:"month"`
	Day      uint8  `json:"day"`
	Hour     uint8  `json:"hour"`
	Minute   uint8  `json:"minute"`
	Second   uint8  `json:"second"`
	UTCValid uint8  `json:"utc_valid"`
	TAccNS   uint32 `json:"t_acc_ns"`
	NanoNS   int32  `json:"nano_ns"`
	FixType  uint8  `json:"fix_type"`
	FixFlags uint8  `json:"fix_flags"`
	NumSV    uint8  `json:"num_sv"`
	LonE7    int32  `json:"lon_e7"`
	LatE7    int32  `json:"lat_e7"`
	HeightMM int32  `json:"height_mm"`
	HAccMM   uint32 `json:"h_acc_mm"`
	VAccMM   uint32 `json:"v_acc_mm"`
	VelNMMS  int32  `json:"vel_n_mm_s"`
	VelEMMS  int32  `json:"vel_e_mm_s"`
	VelDMMS  int32  `json:"vel_d_mm_s"`
	SAccMMS  uint32 `json:"s_acc_mm_s"`
	PDOPx100 uint16 `json:"pdop_x100"`
	PosFlags uint16 `json:"pos_flags"`
}

// UTC returns the epoch's UTC instant and whether the receiver reports it valid:
// date valid, time valid and fully resolved.
func (p *SolutionPVT) UTC() (time.Time, bool) {
	const all = UTCValidDate | UTCValidTime | UTCValidFullyResolved
	if p.UTCValid&all != all {
		return time.Time{}, false
	}
	t := time.Date(int(p.Year), time.Month(p.Month), int(p.Day), int(p.Hour), int(p.Minute), int(p.Second), 0, time.UTC)
	return t.Add(time.Duration(p.NanoNS)), true
}

// SolutionClock is the receiver clock block.
type SolutionClock struct {
	TOWMS    uint32 `json:"tow_ms"`
	BiasNS   int32  `json:"bias_ns"`
	DriftNSS int32  `json:"drift_ns_s"`
	TAccNS   uint32 `json:"t_acc_ns"`
	FAccPSS  uint32 `json:"f_acc_ps_s"`
}

// SolutionStatus is the receiver status block.
type SolutionStatus struct {
	TOWMS      uint32 `json:"tow_ms"`
	FixType    uint8  `json:"fix_type"`
	Flags      uint8  `json:"flags"`
	FixStatus  uint8  `json:"fix_status"`
	SpoofState uint8  `json:"spoof_state"`
	TTFFMS     uint32 `json:"ttff_ms"`
	SinceStart uint32 `json:"since_start_ms"`
}

// EncodeReceiverSolution serializes a solution into its 0x03 body.
func EncodeReceiverSolution(s *ReceiverSolution) []byte {
	var present byte
	n := 2
	if s.PVT != nil {
		present |= solutionHasPVT
		n += solutionPVTLen
	}
	if s.Clock != nil {
		present |= solutionHasClock
		n += solutionClockLen
	}
	if s.Status != nil {
		present |= solutionHasStatus
		n += solutionStatusLen
	}
	b := make([]byte, n)
	b[0], b[1] = telemBodyVersion, present
	o := 2
	be := binary.BigEndian
	if p := s.PVT; p != nil {
		be.PutUint32(b[o:], p.TOWMS)
		be.PutUint16(b[o+4:], p.Year)
		b[o+6], b[o+7], b[o+8], b[o+9], b[o+10] = p.Month, p.Day, p.Hour, p.Minute, p.Second
		b[o+11] = p.UTCValid
		be.PutUint32(b[o+12:], p.TAccNS)
		be.PutUint32(b[o+16:], uint32(p.NanoNS))
		b[o+20], b[o+21], b[o+22] = p.FixType, p.FixFlags, p.NumSV
		be.PutUint32(b[o+24:], uint32(p.LonE7))
		be.PutUint32(b[o+28:], uint32(p.LatE7))
		be.PutUint32(b[o+32:], uint32(p.HeightMM))
		be.PutUint32(b[o+36:], p.HAccMM)
		be.PutUint32(b[o+40:], p.VAccMM)
		be.PutUint32(b[o+44:], uint32(p.VelNMMS))
		be.PutUint32(b[o+48:], uint32(p.VelEMMS))
		be.PutUint32(b[o+52:], uint32(p.VelDMMS))
		be.PutUint32(b[o+56:], p.SAccMMS)
		be.PutUint16(b[o+60:], p.PDOPx100)
		be.PutUint16(b[o+62:], p.PosFlags)
		o += solutionPVTLen
	}
	if c := s.Clock; c != nil {
		be.PutUint32(b[o:], c.TOWMS)
		be.PutUint32(b[o+4:], uint32(c.BiasNS))
		be.PutUint32(b[o+8:], uint32(c.DriftNSS))
		be.PutUint32(b[o+12:], c.TAccNS)
		be.PutUint32(b[o+16:], c.FAccPSS)
		o += solutionClockLen
	}
	if st := s.Status; st != nil {
		be.PutUint32(b[o:], st.TOWMS)
		b[o+4], b[o+5], b[o+6], b[o+7] = st.FixType, st.Flags, st.FixStatus, st.SpoofState
		be.PutUint32(b[o+8:], st.TTFFMS)
		be.PutUint32(b[o+12:], st.SinceStart)
	}
	return b
}

// decodeReceiverSolution parses a 0x03 body. The length must match the present
// blocks exactly, every reserved bit and byte must be zero, and every field the
// checks interpret must be in range; anything else is ErrBadTelemetry.
func decodeReceiverSolution(b []byte) (*ReceiverSolution, error) {
	if len(b) < 2 || b[0] != telemBodyVersion || b[1] == 0 || b[1]&^(solutionHasPVT|solutionHasClock|solutionHasStatus) != 0 {
		return nil, ErrBadTelemetry
	}
	present := b[1]
	want := 2
	if present&solutionHasPVT != 0 {
		want += solutionPVTLen
	}
	if present&solutionHasClock != 0 {
		want += solutionClockLen
	}
	if present&solutionHasStatus != 0 {
		want += solutionStatusLen
	}
	if len(b) != want {
		return nil, ErrBadTelemetry
	}
	be := binary.BigEndian
	s := &ReceiverSolution{}
	o := 2
	if present&solutionHasPVT != 0 {
		v := b[o : o+solutionPVTLen]
		p := &SolutionPVT{
			TOWMS: be.Uint32(v), Year: be.Uint16(v[4:]), Month: v[6], Day: v[7], Hour: v[8], Minute: v[9], Second: v[10],
			UTCValid: v[11], TAccNS: be.Uint32(v[12:]), NanoNS: int32(be.Uint32(v[16:])),
			FixType: v[20], FixFlags: v[21], NumSV: v[22],
			LonE7: int32(be.Uint32(v[24:])), LatE7: int32(be.Uint32(v[28:])), HeightMM: int32(be.Uint32(v[32:])),
			HAccMM: be.Uint32(v[36:]), VAccMM: be.Uint32(v[40:]),
			VelNMMS: int32(be.Uint32(v[44:])), VelEMMS: int32(be.Uint32(v[48:])), VelDMMS: int32(be.Uint32(v[52:])),
			SAccMMS: be.Uint32(v[56:]), PDOPx100: be.Uint16(v[60:]), PosFlags: be.Uint16(v[62:]),
		}
		if !p.valid() || v[23] != 0 {
			return nil, ErrBadTelemetry
		}
		s.PVT = p
		o += solutionPVTLen
	}
	if present&solutionHasClock != 0 {
		v := b[o : o+solutionClockLen]
		c := &SolutionClock{TOWMS: be.Uint32(v), BiasNS: int32(be.Uint32(v[4:])), DriftNSS: int32(be.Uint32(v[8:])),
			TAccNS: be.Uint32(v[12:]), FAccPSS: be.Uint32(v[16:])}
		if c.TOWMS >= solutionWeekMS {
			return nil, ErrBadTelemetry
		}
		s.Clock = c
		o += solutionClockLen
	}
	if present&solutionHasStatus != 0 {
		v := b[o : o+solutionStatusLen]
		st := &SolutionStatus{TOWMS: be.Uint32(v), FixType: v[4], Flags: v[5], FixStatus: v[6], SpoofState: v[7],
			TTFFMS: be.Uint32(v[8:]), SinceStart: be.Uint32(v[12:])}
		if st.TOWMS >= solutionWeekMS || st.FixType > 5 || st.SpoofState > 3 {
			return nil, ErrBadTelemetry
		}
		s.Status = st
	}
	return s, nil
}

// valid checks the PVT block's ranges. UTC calendar fields are checked only when the
// receiver claims them valid; it may report anything while they are not.
func (p *SolutionPVT) valid() bool {
	if p.TOWMS >= solutionWeekMS || p.UTCValid&^(UTCValidDate|UTCValidTime|UTCValidFullyResolved) != 0 ||
		p.FixType > 5 || p.FixFlags&^(FixFlagOK|FixFlagDifferential|FixFlagCarrierFloat|FixFlagCarrierFixed) != 0 ||
		p.FixFlags&(FixFlagCarrierFloat|FixFlagCarrierFixed) == FixFlagCarrierFloat|FixFlagCarrierFixed ||
		p.LatE7 < -900_000_000 || p.LatE7 > 900_000_000 || p.LonE7 < -1_800_000_000 || p.LonE7 > 1_800_000_000 ||
		p.PosFlags&^PosFlagInvalidLLH != 0 || p.NanoNS <= -1_000_000_000 || p.NanoNS >= 1_000_000_000 {
		return false
	}
	if p.UTCValid&UTCValidDate != 0 && (p.Year < 1999 || p.Year > 2200 || p.Month < 1 || p.Month > 12 || p.Day < 1 || p.Day > 31) {
		return false
	}
	if p.UTCValid&UTCValidTime != 0 && (p.Hour > 23 || p.Minute > 59 || p.Second > 60) {
		return false
	}
	return true
}

// tow returns the epoch time of week of the first present block.
func (s *ReceiverSolution) tow() uint32 {
	switch {
	case s.PVT != nil:
		return s.PVT.TOWMS
	case s.Clock != nil:
		return s.Clock.TOWMS
	case s.Status != nil:
		return s.Status.TOWMS
	}
	return 0
}

// u-blox NAV message identifiers and payload lengths for the solution blocks.
const (
	ubxIDNAVSTATUS  = 0x03
	ubxIDNAVPVT     = 0x07
	ubxIDNAVCLOCK   = 0x22
	ubxIDNAVEOE     = 0x61
	ubxNAVPVTLen    = 92
	ubxNAVCLOCKLen  = 20
	ubxNAVSTATUSLen = 16
	ubxNAVEOELen    = 4
)

// parseNAVPVT converts a UBX-NAV-PVT payload into the PVT block.
func parseNAVPVT(p []byte) *SolutionPVT {
	if len(p) != ubxNAVPVTLen {
		return nil
	}
	le := binary.LittleEndian
	flags := p[21]
	pvt := &SolutionPVT{
		TOWMS: le.Uint32(p), Year: le.Uint16(p[4:]), Month: p[6], Day: p[7], Hour: p[8], Minute: p[9], Second: p[10],
		UTCValid: p[11] & (UTCValidDate | UTCValidTime | UTCValidFullyResolved),
		TAccNS:   le.Uint32(p[12:]), NanoNS: int32(le.Uint32(p[16:])),
		FixType: p[20], FixFlags: flags & (FixFlagOK | FixFlagDifferential | FixFlagCarrierFloat | FixFlagCarrierFixed),
		NumSV: p[23],
		LonE7: int32(le.Uint32(p[24:])), LatE7: int32(le.Uint32(p[28:])), HeightMM: int32(le.Uint32(p[32:])),
		HAccMM: le.Uint32(p[40:]), VAccMM: le.Uint32(p[44:]),
		VelNMMS: int32(le.Uint32(p[48:])), VelEMMS: int32(le.Uint32(p[52:])), VelDMMS: int32(le.Uint32(p[56:])),
		SAccMMS: le.Uint32(p[68:]), PDOPx100: le.Uint16(p[76:]), PosFlags: le.Uint16(p[78:]) & PosFlagInvalidLLH,
	}
	if !pvt.valid() {
		return nil
	}
	return pvt
}

// parseNAVCLOCK converts a UBX-NAV-CLOCK payload into the clock block.
func parseNAVCLOCK(p []byte) *SolutionClock {
	if len(p) != ubxNAVCLOCKLen {
		return nil
	}
	le := binary.LittleEndian
	c := &SolutionClock{TOWMS: le.Uint32(p), BiasNS: int32(le.Uint32(p[4:])), DriftNSS: int32(le.Uint32(p[8:])),
		TAccNS: le.Uint32(p[12:]), FAccPSS: le.Uint32(p[16:])}
	if c.TOWMS >= solutionWeekMS {
		return nil
	}
	return c
}

// parseNAVSTATUS converts a UBX-NAV-STATUS payload into the status block. The
// spoofing state is flags2 bits 3..4.
func parseNAVSTATUS(p []byte) *SolutionStatus {
	if len(p) != ubxNAVSTATUSLen {
		return nil
	}
	le := binary.LittleEndian
	st := &SolutionStatus{TOWMS: le.Uint32(p), FixType: p[4], Flags: p[5], FixStatus: p[6], SpoofState: (p[7] >> 3) & 0x03,
		TTFFMS: le.Uint32(p[8:]), SinceStart: le.Uint32(p[12:])}
	if st.TOWMS >= solutionWeekMS || st.FixType > 5 {
		return nil
	}
	return st
}

// solutionAssembler joins the NAV-PVT, NAV-CLOCK and NAV-STATUS blocks of one
// navigation epoch into a single record. An epoch is complete when a block from a
// different epoch arrives or the receiver sends NAV-EOE for it. It matches
// common/receiver_solution.h, the edge implementation.
type solutionAssembler struct {
	pending *ReceiverSolution
	// arrived is when the pending epoch's first block arrived. The record is
	// stamped with it, not the completion time, which without NAV-EOE is the
	// next epoch.
	arrived time.Time
	// lastDone is the last completed epoch; a later block with its time of week
	// (a polled repeat) is dropped rather than sent twice.
	lastDone uint32
	haveDone bool
}

// completedEpoch is an assembled epoch with the arrival time of its first block.
type completedEpoch struct {
	solution *ReceiverSolution
	arrived  time.Time
}

// add merges one block that arrived at now, returning the previous epoch if this
// block starts a new one.
func (a *solutionAssembler) add(tow uint32, now time.Time, merge func(*ReceiverSolution)) *completedEpoch {
	if a.haveDone && tow == a.lastDone {
		return nil
	}
	var done *completedEpoch
	if a.pending != nil && a.pending.tow() != tow {
		done = &completedEpoch{a.pending, a.arrived}
		a.pending = nil
		a.lastDone, a.haveDone = done.solution.tow(), true
	}
	if a.pending == nil {
		a.pending, a.arrived = &ReceiverSolution{}, now
	}
	merge(a.pending)
	return done
}

// endOfEpoch completes the pending epoch if it is the one that ended.
func (a *solutionAssembler) endOfEpoch(tow uint32) *completedEpoch {
	if a.pending == nil || a.pending.tow() != tow {
		return nil
	}
	done := &completedEpoch{a.pending, a.arrived}
	a.pending = nil
	a.lastDone, a.haveDone = tow, true
	return done
}

func solutionFrame(e *completedEpoch, source string) *RawFrame {
	return &RawFrame{Source: source, Recv: e.arrived, MsgType: TelemReceiverSolution, Solution: e.solution}
}
