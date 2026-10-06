package reception

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

const (
	SiderealSeconds   = int64(86164)
	PowerPhaseSeconds = int64(300)
	PowerHistoryDays  = 8
	powerModelVersion = 1
	powerModelHeader  = 52
	powerModelCell    = 32
)

var ErrPowerModel = errors.New("invalid reception power model")

type PowerObservation struct {
	GNSS      uint8
	SV        uint8
	Signal    uint8
	CN0       uint8
	Elevation int16
}

type powerKey struct {
	GNSS, SV, Signal uint8
	Bin              uint16
}

type powerCell struct {
	Cycle      int64
	Sum        uint32
	Count      uint16
	History    [PowerHistoryDays]uint8
	HistoryLen uint8
	Next       uint8
}

// PowerModel holds a bounded, station-specific repeating-ground-track model.
// One sample day is one sidereal cycle, so repeated receiver reports within a
// pass cannot inflate the support count.
type PowerModel struct {
	fingerprint                             [32]byte
	generation                              uint64
	minDeviation, madMultiplier, minSupport uint8
	cells                                   map[powerKey]*powerCell
}

func powerFingerprint(site Site) [32]byte {
	var position [3]float64
	copy(position[:], site.Position)
	h := sha256.New()
	fmt.Fprintf(h, "reception-power-v1\x00%s\x00%s\x00%.9f\x00%.9f\x00%.3f\x00%d\x00%d\x00%d",
		site.Observer, site.PowerModelEpoch, position[0], position[1], position[2],
		site.PowerMinDeviation, site.PowerMADMultiplier, site.PowerMinSupport)
	signals := append([]string(nil), site.Signals...)
	sort.Strings(signals)
	for _, signal := range signals {
		fmt.Fprintf(h, "\x00%s", signal)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func NewPowerModel(site Site) *PowerModel {
	if site.PowerModelEpoch == "" {
		site.PowerModelEpoch = "1"
	}
	if site.PowerMinDeviation == 0 {
		site.PowerMinDeviation = 6
	}
	if site.PowerMADMultiplier == 0 {
		site.PowerMADMultiplier = 4
	}
	if site.PowerMinSupport == 0 {
		site.PowerMinSupport = 3
	}
	return &PowerModel{
		fingerprint: powerFingerprint(site), generation: 1, cells: make(map[powerKey]*powerCell),
		minDeviation: site.PowerMinDeviation, madMultiplier: site.PowerMADMultiplier, minSupport: site.PowerMinSupport,
	}
}

func powerPhase(unix int64) (cycle int64, bin uint16) {
	cycle = unix / SiderealSeconds
	phase := unix % SiderealSeconds
	if phase < 0 {
		phase += SiderealSeconds
		cycle--
	}
	return cycle, uint16(phase / PowerPhaseSeconds)
}

func sortedValues(cell *powerCell) []uint8 {
	v := append([]uint8(nil), cell.History[:cell.HistoryLen]...)
	sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
	return v
}

func median(v []uint8) uint8 {
	if len(v) == 0 {
		return 0
	}
	if len(v)%2 != 0 {
		return v[len(v)/2]
	}
	return uint8((uint16(v[len(v)/2-1]) + uint16(v[len(v)/2]) + 1) / 2)
}

func cellReference(cell *powerCell, minSupport uint8) (expected, mad, support uint8, ok bool) {
	if cell.HistoryLen < minSupport {
		return 0, 0, cell.HistoryLen, false
	}
	v := sortedValues(cell)
	expected = median(v)
	dev := make([]uint8, len(v))
	for i, x := range v {
		if x > expected {
			dev[i] = x - expected
		} else {
			dev[i] = expected - x
		}
	}
	sort.Slice(dev, func(i, j int) bool { return dev[i] < dev[j] })
	return expected, median(dev), cell.HistoryLen, true
}

func (m *PowerModel) finalize(cell *powerCell) {
	if cell.Count == 0 {
		return
	}
	value := uint8((cell.Sum + uint32(cell.Count)/2) / uint32(cell.Count))
	cell.History[cell.Next] = value
	if cell.HistoryLen < PowerHistoryDays {
		cell.HistoryLen++
	}
	cell.Next = (cell.Next + 1) % PowerHistoryDays
	m.generation++
	if m.generation == 0 {
		m.generation = 1
	}
}

func (m *PowerModel) accept(cell *powerCell, cn0 uint8) bool {
	expected, mad, _, ok := cellReference(cell, m.minSupport)
	if !ok {
		return true
	}
	deviation := int(cn0) - int(expected)
	if deviation < 0 {
		deviation = -deviation
	}
	threshold := int(m.madMultiplier) * int(mad)
	if threshold < int(m.minDeviation) {
		threshold = int(m.minDeviation)
	}
	return deviation < threshold
}

// Observe adds a sample to its current sidereal pass. It returns false for an
// invalid, stale, or mature-model outlier; rejected values never train the model.
func (m *PowerModel) Observe(unix int64, o PowerObservation) bool {
	if m == nil || unix < 946684800 || unix >= 4102444800 || o.GNSS > 7 || o.GNSS == 4 ||
		o.SV == 0 || (o.Signal != Satellite && o.Signal > 31) || o.CN0 == 0 || o.CN0 > 99 {
		return false
	}
	cycle, bin := powerPhase(unix)
	key := powerKey{o.GNSS, o.SV, o.Signal, bin}
	cell := m.cells[key]
	if cell == nil {
		cell = &powerCell{Cycle: cycle}
		m.cells[key] = cell
	} else if cycle < cell.Cycle {
		return false
	} else if cycle > cell.Cycle {
		m.finalize(cell)
		cell.Cycle, cell.Sum, cell.Count = cycle, 0, 0
	}
	if !m.accept(cell, o.CN0) || cell.Count == ^uint16(0) {
		return false
	}
	cell.Sum += uint32(o.CN0)
	cell.Count++
	return true
}

func (m *PowerModel) ModelID() uint64 {
	if m == nil {
		return 0
	}
	b := make([]byte, 40)
	copy(b, m.fingerprint[:])
	binary.BigEndian.PutUint64(b[32:], m.generation)
	sum := sha256.Sum256(b)
	id := binary.BigEndian.Uint64(sum[:])
	if id == 0 {
		return 1
	}
	return id
}

func (m *PowerModel) SiteID() uint64 {
	if m == nil {
		return 0
	}
	id := binary.BigEndian.Uint64(m.fingerprint[:])
	if id == 0 {
		return 1
	}
	return id
}

// Forecast creates a companion whose entry order is exactly base. Signals for
// which this receiver has no power telemetry remain unknown.
func (m *PowerModel) Forecast(base Expectation) PowerExpectation {
	p := PowerExpectation{
		ExpectationID: base.ID, ModelID: m.ModelID(), SiteID: m.SiteID(), Issued: base.Issued,
		MinDeviation: m.minDeviation, MADMultiplier: m.madMultiplier, MinSupport: m.minSupport,
		Entries: make([]PowerEntry, len(base.Entries)),
	}
	for i, entry := range base.Entries {
		for slot := 0; slot < Slots; slot++ {
			if entry.Slots&(1<<uint(slot)) == 0 {
				continue
			}
			_, bin := powerPhase(base.Issued + int64(slot*SlotSeconds+SlotSeconds/2))
			cell := m.cells[powerKey{entry.GNSS, entry.SV, entry.Signal, bin}]
			if cell == nil {
				continue
			}
			expected, mad, support, ok := cellReference(cell, m.minSupport)
			p.Entries[i].Support[slot] = support
			if ok {
				p.Entries[i].Valid |= 1 << uint(slot)
				p.Entries[i].Expected[slot], p.Entries[i].MAD[slot] = expected, mad
			}
		}
	}
	return p
}

func (m *PowerModel) MarshalBinary() ([]byte, error) {
	if m == nil || m.ModelID() == 0 || m.minDeviation < 3 || m.minDeviation > 30 || m.madMultiplier == 0 ||
		m.madMultiplier > 16 || m.minSupport < 2 || m.minSupport > PowerHistoryDays || len(m.cells) > 1_000_000 {
		return nil, ErrPowerModel
	}
	keys := make([]powerKey, 0, len(m.cells))
	for key := range m.cells {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.GNSS != b.GNSS {
			return a.GNSS < b.GNSS
		}
		if a.SV != b.SV {
			return a.SV < b.SV
		}
		if a.Signal != b.Signal {
			return a.Signal < b.Signal
		}
		return a.Bin < b.Bin
	})
	b := make([]byte, powerModelHeader+powerModelCell*len(keys))
	copy(b, "NRPM")
	b[4], b[5], b[6], b[7] = powerModelVersion, m.minDeviation, m.madMultiplier, m.minSupport
	binary.BigEndian.PutUint64(b[8:], m.generation)
	copy(b[16:48], m.fingerprint[:])
	binary.BigEndian.PutUint32(b[48:], uint32(len(keys)))
	for i, key := range keys {
		cell := m.cells[key]
		o := powerModelHeader + powerModelCell*i
		b[o], b[o+1], b[o+2] = key.GNSS, key.SV, key.Signal
		binary.BigEndian.PutUint16(b[o+4:], key.Bin)
		binary.BigEndian.PutUint64(b[o+6:], uint64(cell.Cycle))
		binary.BigEndian.PutUint32(b[o+14:], cell.Sum)
		binary.BigEndian.PutUint16(b[o+18:], cell.Count)
		b[o+20], b[o+21] = cell.HistoryLen, cell.Next
		copy(b[o+22:o+30], cell.History[:])
	}
	return b, nil
}

func (m *PowerModel) UnmarshalBinary(b []byte) error {
	if m == nil || len(b) < powerModelHeader || !bytes.Equal(b[:4], []byte("NRPM")) || b[4] != powerModelVersion ||
		b[5] != m.minDeviation || b[6] != m.madMultiplier || b[7] != m.minSupport ||
		!bytes.Equal(b[16:48], m.fingerprint[:]) {
		return ErrPowerModel
	}
	n := int(binary.BigEndian.Uint32(b[48:]))
	if n > 1_000_000 || len(b) != powerModelHeader+powerModelCell*n {
		return ErrPowerModel
	}
	cells := make(map[powerKey]*powerCell, n)
	for i := 0; i < n; i++ {
		o := powerModelHeader + powerModelCell*i
		if b[o+3] != 0 || b[o+30] != 0 || b[o+31] != 0 {
			return ErrPowerModel
		}
		key := powerKey{b[o], b[o+1], b[o+2], binary.BigEndian.Uint16(b[o+4:])}
		cell := &powerCell{Cycle: int64(binary.BigEndian.Uint64(b[o+6:])), Sum: binary.BigEndian.Uint32(b[o+14:]),
			Count: binary.BigEndian.Uint16(b[o+18:]), HistoryLen: b[o+20], Next: b[o+21]}
		copy(cell.History[:], b[o+22:o+30])
		if key.GNSS > 7 || key.GNSS == 4 || key.SV == 0 || (key.Signal != Satellite && key.Signal > 31) ||
			int64(key.Bin)*PowerPhaseSeconds >= SiderealSeconds || cell.HistoryLen > PowerHistoryDays ||
			cell.Next >= PowerHistoryDays || (cell.HistoryLen < PowerHistoryDays && cell.Next != cell.HistoryLen) ||
			(cell.Count == 0 && cell.Sum != 0) ||
			(cell.Count != 0 && (cell.Sum < uint32(cell.Count) || cell.Sum > 99*uint32(cell.Count))) {
			return ErrPowerModel
		}
		for j, value := range cell.History {
			if (j < int(cell.HistoryLen) && (value == 0 || value > 99)) || (j >= int(cell.HistoryLen) && value != 0 && cell.HistoryLen < PowerHistoryDays) {
				return ErrPowerModel
			}
		}
		if _, exists := cells[key]; exists {
			return ErrPowerModel
		}
		cells[key] = cell
	}
	generation := binary.BigEndian.Uint64(b[8:])
	if generation == 0 {
		return ErrPowerModel
	}
	m.generation, m.cells = generation, cells
	return nil
}
