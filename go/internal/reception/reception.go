// Package reception defines the bounded station expectation and local alarm contract.
package reception

import (
	"encoding/binary"
	"errors"
	"time"
)

const (
	Version     = 1
	MaxEntries  = 128
	HeaderSize  = 40
	MaxSize     = HeaderSize + 4*MaxEntries
	SampleSize  = 76
	Satellite   = 255 // NAV-SAT observation, independent of navigation solution use
	SlotSeconds = 60
	Slots       = 5
)

var ErrWire = errors.New("invalid reception contract")

type Entry struct {
	GNSS   uint8 `json:"gnss"`
	SV     uint8 `json:"sv"`
	Signal uint8 `json:"signal"`
	Slots  uint8 `json:"slots"`
}

// Expectation binds a five-minute forecast to an administratively configured site.
// Signals use canonical UBX signal groups; 255 means a satellite-level check.
type Expectation struct {
	ID             uint64  `json:"id,string"`
	Issued         int64   `json:"issued_unix"`
	Latitude       int32   `json:"latitude_e7"`
	Longitude      int32   `json:"longitude_e7"`
	RadiusM        uint16  `json:"radius_m"`
	AlarmSeconds   uint16  `json:"alarm_seconds"`
	ClearSeconds   uint16  `json:"clear_seconds"`
	MinExpected    uint8   `json:"min_expected"`
	MinMissing     uint8   `json:"min_missing"`
	MissingPercent uint8   `json:"missing_percent"`
	Entries        []Entry `json:"entries"`
}

func (e Expectation) Valid() bool {
	if e.ID == 0 || e.Issued < 946684800 || e.Issued >= 4102444800 || len(e.Entries) > MaxEntries ||
		e.Latitude < -900000000 || e.Latitude > 900000000 || e.Longitude < -1800000000 || e.Longitude > 1800000000 ||
		e.RadiusM == 0 || e.AlarmSeconds < 5 || e.AlarmSeconds > 120 || e.ClearSeconds < 5 || e.ClearSeconds > 120 ||
		e.MinExpected == 0 || e.MinMissing == 0 || e.MissingPercent == 0 || e.MissingPercent > 100 {
		return false
	}
	seen := map[[3]uint8]bool{}
	for _, v := range e.Entries {
		key := [3]uint8{v.GNSS, v.SV, v.Signal}
		if v.GNSS > 7 || v.GNSS == 4 || v.SV == 0 || v.Slots == 0 || v.Slots>>Slots != 0 ||
			(v.Signal != Satellite && v.Signal > 31) || seen[key] {
			return false
		}
		seen[key] = true
	}
	return true
}

func (e Expectation) Encode() ([]byte, error) {
	if !e.Valid() {
		return nil, ErrWire
	}
	b := make([]byte, HeaderSize+4*len(e.Entries))
	b[0] = Version
	b[1] = Slots
	b[2] = SlotSeconds
	b[3] = byte(len(e.Entries))
	binary.BigEndian.PutUint64(b[4:], e.ID)
	binary.BigEndian.PutUint64(b[12:], uint64(e.Issued))
	binary.BigEndian.PutUint32(b[20:], uint32(e.Latitude))
	binary.BigEndian.PutUint32(b[24:], uint32(e.Longitude))
	binary.BigEndian.PutUint16(b[28:], e.RadiusM)
	binary.BigEndian.PutUint16(b[30:], e.AlarmSeconds)
	binary.BigEndian.PutUint16(b[32:], e.ClearSeconds)
	b[34] = e.MinExpected
	b[35] = e.MinMissing
	b[36] = e.MissingPercent
	for i, v := range e.Entries {
		copy(b[40+4*i:], []byte{v.GNSS, v.SV, v.Signal, v.Slots})
	}
	return b, nil
}

func Decode(b []byte) (Expectation, error) {
	if len(b) < HeaderSize || b[0] != Version || b[1] != Slots || b[2] != SlotSeconds || len(b) != HeaderSize+4*int(b[3]) || b[37] != 0 || b[38] != 0 || b[39] != 0 {
		return Expectation{}, ErrWire
	}
	e := Expectation{ID: binary.BigEndian.Uint64(b[4:]), Issued: int64(binary.BigEndian.Uint64(b[12:])), Latitude: int32(binary.BigEndian.Uint32(b[20:])), Longitude: int32(binary.BigEndian.Uint32(b[24:])), RadiusM: binary.BigEndian.Uint16(b[28:]), AlarmSeconds: binary.BigEndian.Uint16(b[30:]), ClearSeconds: binary.BigEndian.Uint16(b[32:]), MinExpected: b[34], MinMissing: b[35], MissingPercent: b[36]}
	for i := 0; i < int(b[3]); i++ {
		o := 40 + 4*i
		e.Entries = append(e.Entries, Entry{b[o], b[o+1], b[o+2], b[o+3]})
	}
	if !e.Valid() {
		return Expectation{}, ErrWire
	}
	return e, nil
}

// Sample carries observations against an exact expectation and the edge's verdict.
// Valid is a constellation mask; absent/stale measurements never become a measured zero.
// Matched uses expectation entry indices. Boot/Event identify persistent local transitions.
type Sample struct {
	ExpectationID uint64   `json:"expectation_id,string"`
	Unix          int64    `json:"sample_unix"`
	UptimeMS      uint64   `json:"uptime_ms"`
	Boot          uint64   `json:"boot,string"`
	Event         uint64   `json:"event,string"`
	Valid         uint8    `json:"valid_mask"`
	Alarm         uint8    `json:"alarm_mask"`
	Expected      [8]uint8 `json:"expected"`
	Observed      [8]uint8 `json:"observed"`
	Matched       [16]byte `json:"matched"`
}

// The counts are derived for display, never trusted by the collector's evaluator.
func (s Sample) Encode() []byte {
	b := make([]byte, SampleSize)
	b[0] = Version
	b[1] = s.Valid
	b[2] = s.Alarm
	binary.BigEndian.PutUint64(b[4:], s.ExpectationID)
	binary.BigEndian.PutUint64(b[12:], uint64(s.Unix))
	binary.BigEndian.PutUint64(b[20:], s.UptimeMS)
	binary.BigEndian.PutUint64(b[28:], s.Boot)
	binary.BigEndian.PutUint64(b[36:], s.Event)
	copy(b[44:60], s.Matched[:])
	copy(b[60:68], s.Expected[:])
	copy(b[68:76], s.Observed[:])
	return b
}

func DecodeSample(b []byte) (Sample, error) {
	if len(b) != SampleSize || b[0] != Version || b[3] != 0 || b[1]&16 != 0 || b[2]&16 != 0 {
		return Sample{}, ErrWire
	}
	s := Sample{ExpectationID: binary.BigEndian.Uint64(b[4:]), Unix: int64(binary.BigEndian.Uint64(b[12:])), UptimeMS: binary.BigEndian.Uint64(b[20:]), Boot: binary.BigEndian.Uint64(b[28:]), Event: binary.BigEndian.Uint64(b[36:]), Valid: b[1], Alarm: b[2]}
	copy(s.Matched[:], b[44:60])
	copy(s.Expected[:], b[60:68])
	copy(s.Observed[:], b[68:76])
	return s, nil
}

// Counts independently recomputes the reception deficit from the reported matches.
func Counts(e Expectation, s Sample) (expected, observed [8]uint8, valid, bad uint8) {
	age := s.Unix - e.Issued
	if s.ExpectationID != e.ID || age < 0 || age >= SlotSeconds*Slots {
		return
	}
	slot := uint8(1 << uint(age/SlotSeconds))
	valid = s.Valid
	for i, v := range e.Entries {
		if v.Slots&slot == 0 {
			continue
		}
		expected[v.GNSS]++
		if s.Matched[i/8]&(1<<uint(i%8)) != 0 {
			observed[v.GNSS]++
		}
	}
	for g := 0; g < 8; g++ {
		if expected[g] < e.MinExpected {
			valid &^= 1 << uint(g)
			continue
		}
		missing := int(expected[g] - observed[g])
		if missing >= int(e.MinMissing) && 100*missing >= int(e.MissingPercent)*int(expected[g]) {
			bad |= 1 << uint(g)
		}
	}
	bad &= valid
	return
}

// Machine holds an alarm through unknown coverage and requires continuous fresh
// samples for both onset and recovery. A new forecast must not restart the dwell.
type Machine struct {
	Alarm           uint8
	target, pending uint8
	since           [8]int64
	last            int64
}

func (m *Machine) Step(valid, bad uint8, at time.Time, alarmSeconds, clearSeconds uint16) uint8 {
	now := at.UnixMilli()
	if m.last != 0 && (now <= m.last || now-m.last > 15000) {
		m.pending = 0
	}
	if now <= m.last {
		return m.Alarm
	}
	m.last = now
	for g := 0; g < 8; g++ {
		bit := uint8(1 << uint(g))
		if valid&bit == 0 {
			m.pending &^= bit
			continue
		}
		want := bad & bit
		if want == m.Alarm&bit {
			m.pending &^= bit
			continue
		}
		if m.pending&bit == 0 || m.target&bit != want {
			m.pending |= bit
			m.target = (m.target &^ bit) | want
			m.since[g] = now
		}
		dwell := clearSeconds
		if want != 0 {
			dwell = alarmSeconds
		}
		if now-m.since[g] >= int64(dwell)*1000 {
			m.Alarm = (m.Alarm &^ bit) | want
			m.pending &^= bit
		}
	}
	return m.Alarm
}
