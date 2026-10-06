package reception

import (
	"encoding/binary"
	"errors"
)

const (
	PowerVersion    = 1
	PowerHeaderSize = 40
	PowerEntrySize  = 16
	PowerMaxSize    = PowerHeaderSize + PowerEntrySize*MaxEntries
	PowerSampleSize = 256
	PowerEventSize  = 68
	PowerMaxSupport = 4

	PowerFlagLocal  = 1 << 0
	PowerFlagRemote = 1 << 1
)

var ErrPowerWire = errors.New("invalid reception power contract")

// PowerEntry is the robust received-power reference for one Entry in an
// Expectation. Values are C/N0 in dB-Hz. A slot is usable only when Valid has
// its bit set; Support records the number of distinct daily passes represented
// by that cell. MAD is the median absolute deviation from Expected.
type PowerEntry struct {
	Valid    uint8        `json:"valid_slots"`
	Expected [Slots]uint8 `json:"expected_cno_dbhz"`
	MAD      [Slots]uint8 `json:"mad_dbhz"`
	Support  [Slots]uint8 `json:"support_days"`
}

// PowerExpectation is a companion to an exact reception Expectation. Entry i
// always describes Expectation.Entries[i]; carrying no satellite identifiers a
// second time prevents the two lists from being joined ambiguously.
type PowerExpectation struct {
	ExpectationID uint64       `json:"expectation_id,string"`
	ModelID       uint64       `json:"model_id,string"`
	SiteID        uint64       `json:"site_id,string"`
	Issued        int64        `json:"issued_unix"`
	MinDeviation  uint8        `json:"minimum_deviation_dbhz"`
	MADMultiplier uint8        `json:"mad_multiplier"`
	MinSupport    uint8        `json:"minimum_support_days"`
	Entries       []PowerEntry `json:"entries"`
}

func (p PowerExpectation) Valid() bool {
	if p.ExpectationID == 0 || p.ModelID == 0 || p.SiteID == 0 || p.Issued < 946684800 || p.Issued >= 4102444800 ||
		len(p.Entries) > MaxEntries || p.MinDeviation < 3 || p.MinDeviation > 30 ||
		p.MADMultiplier == 0 || p.MADMultiplier > 16 || p.MinSupport < 2 || p.MinSupport > PowerMaxSupport {
		return false
	}
	for _, e := range p.Entries {
		if e.Valid>>Slots != 0 {
			return false
		}
		for slot := 0; slot < Slots; slot++ {
			valid := e.Valid&(1<<slot) != 0
			if valid != (e.Support[slot] >= p.MinSupport) ||
				(valid && (e.Expected[slot] == 0 || e.Expected[slot] > 99)) {
				return false
			}
		}
	}
	return true
}

func (p PowerExpectation) Encode() ([]byte, error) {
	if !p.Valid() {
		return nil, ErrPowerWire
	}
	b := make([]byte, PowerHeaderSize+PowerEntrySize*len(p.Entries))
	b[0], b[1], b[2], b[3] = PowerVersion, Slots, SlotSeconds, byte(len(p.Entries))
	binary.BigEndian.PutUint64(b[4:], p.ExpectationID)
	binary.BigEndian.PutUint64(b[12:], p.ModelID)
	binary.BigEndian.PutUint64(b[20:], uint64(p.Issued))
	binary.BigEndian.PutUint64(b[28:], p.SiteID)
	b[36], b[37], b[38] = p.MinDeviation, p.MADMultiplier, p.MinSupport
	for i, e := range p.Entries {
		o := PowerHeaderSize + PowerEntrySize*i
		b[o] = e.Valid
		copy(b[o+1:o+6], e.Expected[:])
		copy(b[o+6:o+11], e.MAD[:])
		copy(b[o+11:o+16], e.Support[:])
	}
	return b, nil
}

func DecodePowerExpectation(b []byte) (PowerExpectation, error) {
	if len(b) < PowerHeaderSize || b[0] != PowerVersion || b[1] != Slots || b[2] != SlotSeconds ||
		len(b) != PowerHeaderSize+PowerEntrySize*int(b[3]) || b[39] != 0 {
		return PowerExpectation{}, ErrPowerWire
	}
	p := PowerExpectation{
		ExpectationID: binary.BigEndian.Uint64(b[4:]),
		ModelID:       binary.BigEndian.Uint64(b[12:]),
		Issued:        int64(binary.BigEndian.Uint64(b[20:])),
		SiteID:        binary.BigEndian.Uint64(b[28:]),
		MinDeviation:  b[36], MADMultiplier: b[37], MinSupport: b[38],
		Entries: make([]PowerEntry, int(b[3])),
	}
	for i := range p.Entries {
		o := PowerHeaderSize + PowerEntrySize*i
		p.Entries[i].Valid = b[o]
		copy(p.Entries[i].Expected[:], b[o+1:o+6])
		copy(p.Entries[i].MAD[:], b[o+6:o+11])
		copy(p.Entries[i].Support[:], b[o+11:o+16])
	}
	if !p.Valid() {
		return PowerExpectation{}, ErrPowerWire
	}
	return p, nil
}

// PowerAssessment is an entry-level comparison against one model. Valid and
// Bad are bitmaps indexed by the associated reception Expectation.
type PowerAssessment struct {
	Valid [16]byte `json:"valid"`
	Bad   [16]byte `json:"bad"`
}

// PowerCheck is the collector's independent interpretation of an edge power
// report. JointAlarm requires both models to flag the same constellation; the
// two models remain one physical RF gate because they use one antenna/receiver.
type PowerCheck struct {
	LocalModelID       uint64   `json:"local_model_id,string"`
	RemoteModelID      uint64   `json:"remote_model_id,string"`
	LocalValid         uint8    `json:"local_valid_mask"`
	LocalAbnormal      uint8    `json:"local_abnormal_mask"`
	RemoteValid        uint8    `json:"remote_valid_mask"`
	RemoteAbnormal     uint8    `json:"remote_abnormal_mask"`
	RemoteAlarm        uint8    `json:"remote_alarm_mask"`
	JointValid         uint8    `json:"joint_valid_mask"`
	JointAbnormal      uint8    `json:"joint_abnormal_mask"`
	JointAlarm         uint8    `json:"joint_alarm_mask"`
	ModelConflict      uint8    `json:"model_conflict_mask"`
	ReportDisagreement uint8    `json:"report_disagreement_mask"`
	Modeled            [8]uint8 `json:"remote_modeled"`
	Anomalous          [8]uint8 `json:"remote_anomalous"`
}

// PowerSample carries the measurements required for collector recomputation,
// plus the edge's results against its local model and the delivered model.
type PowerSample struct {
	Count            uint8             `json:"entry_count"`
	Flags            uint8             `json:"flags"`
	LocalValid       uint8             `json:"local_valid_mask"`
	LocalAlarm       uint8             `json:"local_alarm_mask"`
	RemoteValid      uint8             `json:"remote_valid_mask"`
	RemoteAlarm      uint8             `json:"remote_alarm_mask"`
	ExpectationID    uint64            `json:"expectation_id,string"`
	RemoteModelID    uint64            `json:"remote_model_id,string"`
	LocalModelID     uint64            `json:"local_model_id,string"`
	Unix             int64             `json:"sample_unix"`
	UptimeMS         uint64            `json:"uptime_ms"`
	ObservedValid    [16]byte          `json:"observed_valid"`
	Observed         [MaxEntries]uint8 `json:"observed_cno_dbhz"`
	LocalAssessment  PowerAssessment   `json:"local_assessment"`
	RemoteAssessment PowerAssessment   `json:"remote_assessment"`
}

type PowerEvent struct {
	Flags          uint8  `json:"flags"`
	LocalValid     uint8  `json:"local_valid_mask"`
	LocalAlarm     uint8  `json:"local_alarm_mask"`
	RemoteValid    uint8  `json:"remote_valid_mask"`
	RemoteAlarm    uint8  `json:"remote_alarm_mask"`
	JointValid     uint8  `json:"joint_valid_mask"`
	JointAlarm     uint8  `json:"joint_alarm_mask"`
	ExpectationID  uint64 `json:"expectation_id,string"`
	RemoteModelID  uint64 `json:"remote_model_id,string"`
	LocalModelID   uint64 `json:"local_model_id,string"`
	Unix           int64  `json:"sample_unix"`
	UptimeMS       uint64 `json:"uptime_ms"`
	Boot           uint64 `json:"boot,string"`
	Event          uint64 `json:"event,string"`
	LocalAbnormal  uint8  `json:"local_abnormal_mask"`
	RemoteAbnormal uint8  `json:"remote_abnormal_mask"`
	JointAbnormal  uint8  `json:"joint_abnormal_mask"`
	ModelConflict  uint8  `json:"model_conflict_mask"`
}

func (e PowerEvent) Valid() bool {
	const constellationBits = uint8(1 << 4)
	if e.Flags&^(PowerFlagLocal|PowerFlagRemote) != 0 || e.ExpectationID == 0 || e.Unix < 946684800 || e.Unix >= 4102444800 ||
		e.Boot == 0 || e.Event == 0 || (e.LocalValid|e.LocalAlarm|e.RemoteValid|e.RemoteAlarm|e.JointValid|e.JointAlarm|
		e.LocalAbnormal|e.RemoteAbnormal|e.JointAbnormal|e.ModelConflict)&constellationBits != 0 {
		return false
	}
	if e.Flags&PowerFlagLocal == 0 && (e.LocalModelID != 0 || e.LocalValid != 0 || e.LocalAlarm != 0 || e.LocalAbnormal != 0) {
		return false
	}
	if e.Flags&PowerFlagRemote == 0 && (e.RemoteModelID != 0 || e.RemoteValid != 0 || e.RemoteAlarm != 0 || e.RemoteAbnormal != 0) {
		return false
	}
	if e.Flags&PowerFlagLocal != 0 && e.LocalModelID == 0 || e.Flags&PowerFlagRemote != 0 && e.RemoteModelID == 0 {
		return false
	}
	if e.Flags != (PowerFlagLocal|PowerFlagRemote) && (e.JointValid != 0 || e.JointAlarm != 0 || e.JointAbnormal != 0 || e.ModelConflict != 0) {
		return false
	}
	return true
}

func (e PowerEvent) Encode() ([]byte, error) {
	if !e.Valid() {
		return nil, ErrPowerWire
	}
	b := make([]byte, PowerEventSize)
	b[0], b[1], b[2], b[3] = PowerVersion, e.Flags, e.LocalValid, e.LocalAlarm
	b[4], b[5], b[6], b[7] = e.RemoteValid, e.RemoteAlarm, e.JointValid, e.JointAlarm
	binary.BigEndian.PutUint64(b[8:], e.ExpectationID)
	binary.BigEndian.PutUint64(b[16:], e.RemoteModelID)
	binary.BigEndian.PutUint64(b[24:], e.LocalModelID)
	binary.BigEndian.PutUint64(b[32:], uint64(e.Unix))
	binary.BigEndian.PutUint64(b[40:], e.UptimeMS)
	binary.BigEndian.PutUint64(b[48:], e.Boot)
	binary.BigEndian.PutUint64(b[56:], e.Event)
	b[64], b[65], b[66], b[67] = e.LocalAbnormal, e.RemoteAbnormal, e.JointAbnormal, e.ModelConflict
	return b, nil
}

func DecodePowerEvent(b []byte) (PowerEvent, error) {
	if len(b) != PowerEventSize || b[0] != PowerVersion {
		return PowerEvent{}, ErrPowerWire
	}
	e := PowerEvent{Flags: b[1], LocalValid: b[2], LocalAlarm: b[3], RemoteValid: b[4], RemoteAlarm: b[5],
		JointValid: b[6], JointAlarm: b[7], ExpectationID: binary.BigEndian.Uint64(b[8:]),
		RemoteModelID: binary.BigEndian.Uint64(b[16:]), LocalModelID: binary.BigEndian.Uint64(b[24:]),
		Unix: int64(binary.BigEndian.Uint64(b[32:])), UptimeMS: binary.BigEndian.Uint64(b[40:]),
		Boot: binary.BigEndian.Uint64(b[48:]), Event: binary.BigEndian.Uint64(b[56:]),
		LocalAbnormal: b[64], RemoteAbnormal: b[65], JointAbnormal: b[66], ModelConflict: b[67]}
	if !e.Valid() {
		return PowerEvent{}, ErrPowerWire
	}
	return e, nil
}

func bitmapTailClear(bits [16]byte, count uint8) bool {
	for i := int(count); i < MaxEntries; i++ {
		if bits[i/8]&(1<<uint(i%8)) != 0 {
			return false
		}
	}
	return true
}

func (s PowerSample) Valid() bool {
	if s.Count > MaxEntries || s.Flags&^(PowerFlagLocal|PowerFlagRemote) != 0 || s.ExpectationID == 0 ||
		s.Unix < 946684800 || s.Unix >= 4102444800 ||
		!bitmapTailClear(s.ObservedValid, s.Count) || !bitmapTailClear(s.LocalAssessment.Valid, s.Count) ||
		!bitmapTailClear(s.LocalAssessment.Bad, s.Count) || !bitmapTailClear(s.RemoteAssessment.Valid, s.Count) ||
		!bitmapTailClear(s.RemoteAssessment.Bad, s.Count) {
		return false
	}
	for i := 0; i < int(s.Count); i++ {
		bit := byte(1 << uint(i%8))
		if s.LocalAssessment.Bad[i/8]&^s.LocalAssessment.Valid[i/8]&bit != 0 ||
			s.RemoteAssessment.Bad[i/8]&^s.RemoteAssessment.Valid[i/8]&bit != 0 ||
			(s.ObservedValid[i/8]&bit != 0 && (s.Observed[i] == 0 || s.Observed[i] > 99)) {
			return false
		}
	}
	if s.Flags&PowerFlagLocal == 0 && (s.LocalModelID != 0 || s.LocalValid != 0 || s.LocalAlarm != 0 || s.LocalAssessment != (PowerAssessment{})) {
		return false
	}
	if s.Flags&PowerFlagRemote == 0 && (s.RemoteModelID != 0 || s.RemoteValid != 0 || s.RemoteAlarm != 0 || s.RemoteAssessment != (PowerAssessment{})) {
		return false
	}
	if s.Flags&PowerFlagLocal != 0 && s.LocalModelID == 0 || s.Flags&PowerFlagRemote != 0 && s.RemoteModelID == 0 {
		return false
	}
	return true
}

func (s PowerSample) Encode() ([]byte, error) {
	if !s.Valid() {
		return nil, ErrPowerWire
	}
	b := make([]byte, PowerSampleSize)
	b[0], b[1], b[2], b[3] = PowerVersion, s.Count, s.Flags, s.LocalValid
	b[4], b[5] = s.LocalAlarm, s.RemoteValid
	b[6] = s.RemoteAlarm
	binary.BigEndian.PutUint64(b[8:], s.ExpectationID)
	binary.BigEndian.PutUint64(b[16:], s.RemoteModelID)
	binary.BigEndian.PutUint64(b[24:], s.LocalModelID)
	binary.BigEndian.PutUint64(b[32:], uint64(s.Unix))
	binary.BigEndian.PutUint64(b[40:], s.UptimeMS)
	copy(b[48:64], s.ObservedValid[:])
	copy(b[64:192], s.Observed[:])
	copy(b[192:208], s.LocalAssessment.Valid[:])
	copy(b[208:224], s.LocalAssessment.Bad[:])
	copy(b[224:240], s.RemoteAssessment.Valid[:])
	copy(b[240:256], s.RemoteAssessment.Bad[:])
	return b, nil
}

func DecodePowerSample(b []byte) (PowerSample, error) {
	if len(b) != PowerSampleSize || b[0] != PowerVersion || b[7] != 0 {
		return PowerSample{}, ErrPowerWire
	}
	s := PowerSample{
		Count: b[1], Flags: b[2], LocalValid: b[3], LocalAlarm: b[4], RemoteValid: b[5], RemoteAlarm: b[6],
		ExpectationID: binary.BigEndian.Uint64(b[8:]), RemoteModelID: binary.BigEndian.Uint64(b[16:]),
		LocalModelID: binary.BigEndian.Uint64(b[24:]), Unix: int64(binary.BigEndian.Uint64(b[32:])),
		UptimeMS: binary.BigEndian.Uint64(b[40:]),
	}
	copy(s.ObservedValid[:], b[48:64])
	copy(s.Observed[:], b[64:192])
	copy(s.LocalAssessment.Valid[:], b[192:208])
	copy(s.LocalAssessment.Bad[:], b[208:224])
	copy(s.RemoteAssessment.Valid[:], b[224:240])
	copy(s.RemoteAssessment.Bad[:], b[240:256])
	if !s.Valid() {
		return PowerSample{}, ErrPowerWire
	}
	return s, nil
}

// ComparePower returns the entry-level comparison for the sample's minute.
// An absent observation or immature reference remains unknown.
func ComparePower(p PowerExpectation, s PowerSample) PowerAssessment {
	var a PowerAssessment
	age := s.Unix - p.Issued
	if s.ExpectationID != p.ExpectationID || s.RemoteModelID != p.ModelID || int(s.Count) != len(p.Entries) || age < 0 || age >= Slots*SlotSeconds {
		return a
	}
	slot := int(age / SlotSeconds)
	for i, e := range p.Entries {
		bit := byte(1 << uint(i%8))
		if e.Valid&(1<<uint(slot)) == 0 || s.ObservedValid[i/8]&bit == 0 {
			continue
		}
		a.Valid[i/8] |= bit
		deviation := int(s.Observed[i]) - int(e.Expected[slot])
		if deviation < 0 {
			deviation = -deviation
		}
		threshold := int(p.MADMultiplier) * int(e.MAD[slot])
		if threshold < int(p.MinDeviation) {
			threshold = int(p.MinDeviation)
		}
		if deviation >= threshold {
			a.Bad[i/8] |= bit
		}
	}
	return a
}

// CountPower reduces entry comparisons to constellation coverage and alarms
// using the same configured quorum as the availability detector.
func CountPower(base Expectation, count uint8, a PowerAssessment) (modeled, anomalous [8]uint8, valid, bad uint8) {
	if int(count) != len(base.Entries) {
		return
	}
	for i, e := range base.Entries {
		bit := byte(1 << uint(i%8))
		if a.Valid[i/8]&bit == 0 {
			continue
		}
		modeled[e.GNSS]++
		if a.Bad[i/8]&bit != 0 {
			anomalous[e.GNSS]++
		}
	}
	for g := range modeled {
		if modeled[g] < base.MinExpected {
			continue
		}
		valid |= 1 << uint(g)
		if anomalous[g] >= base.MinMissing && int(anomalous[g])*100 >= int(base.MissingPercent)*int(modeled[g]) {
			bad |= 1 << uint(g)
		}
	}
	return
}
