package frame

import (
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ptudor/gnss"
	"github.com/ptudor/gnss/physconst"
)

// twos returns v's two's complement bit pattern for setBits, which keeps only
// the field's low bits.
func twos(v int64) uint64 { return uint64(v) }

// midiAlmanacWords builds a CRC-valid message type 40 whose midi almanac
// (BDS-SIS-B2a-1.0 Figure 6-20, at bits 69–224) carries the given raw fields.
func midiAlmanacWords(prn, satType, wn, toaRaw uint64, mutate func(buf []byte)) []uint32 {
	buf := make([]byte, 36)
	setBits(buf, 0, 6, 30)
	setBits(buf, 6, 6, 40)
	setBits(buf, 12, 18, 1)
	setBits(buf, 69, 6, prn)
	setBits(buf, 75, 2, satType)
	setBits(buf, 77, 13, wn)
	setBits(buf, 90, 8, toaRaw)
	setBits(buf, 98, 11, 300)        // e
	setBits(buf, 109, 11, twos(-50)) // δi
	setBits(buf, 120, 17, 84522)     // √A ≈ 5282.6
	setBits(buf, 137, 16, 12345)     // Ω0
	setBits(buf, 153, 11, twos(-280))
	setBits(buf, 164, 16, twos(-20000)) // ω
	setBits(buf, 180, 16, 30000)        // M0
	setBits(buf, 196, 11, twos(-7))     // af0
	setBits(buf, 207, 10, 9)            // af1
	setBits(buf, 217, 8, 0x40)          // health
	if mutate != nil {
		mutate(buf)
	}
	c := CRC24Q(buf[:33])
	buf[33], buf[34], buf[35] = byte(c>>16), byte(c>>8), byte(c)
	words := make([]uint32, 9)
	for i := range words {
		words[i] = binary.BigEndian.Uint32(buf[i*4:])
	}
	return words
}

// TestBCNAV2MidiAlmanacDecodes: message type 40's midi almanac describes PRNa
// with its own week and orbit type, at the Table 7-13 scalings, and converts
// with i0 = 0.30π for IGSO/MEO.
func TestBCNAV2MidiAlmanacDecodes(t *testing.T) {
	m, err := DecodeBeiDouBCNAV2(midiAlmanacWords(23, 3, 1070, 103, nil))
	if err != nil {
		t.Fatal(err)
	}
	a := m.Almanac
	semi := physconst.Pi
	if a == nil || a.PRN != 23 || a.SatType != 3 || a.WN != 1070 || a.Toa != 103*4096 || a.Health != 0x40 ||
		a.Ecc != 300.0/65536 || a.DeltaI != -50.0/16384*semi || a.SqrtA != 84522.0/16 ||
		a.Omega0 != 12345.0/32768*semi || a.OmegaDot != -280.0/float64(uint64(1)<<33)*semi ||
		a.Omega != -20000.0/32768*semi || a.M0 != 30000.0/32768*semi ||
		a.Af0 != -7.0/(1<<20) || a.Af1 != 9.0/float64(uint64(1)<<37) {
		t.Fatalf("midi almanac = %+v", a)
	}
	eph, err := a.Ephemeris()
	if err != nil {
		t.Fatal(err)
	}
	if eph.ID != gnss.BeiDou || eph.SVID != 23 || !eph.Almanac || eph.Toe != a.Toa ||
		math.Abs(eph.I0-(0.30*semi+a.DeltaI)) > 1e-15 {
		t.Fatalf("ephemeris = %+v", eph)
	}
}

// TestBCNAV2MidiAlmanacOrbitTypes: GEO almanacs reference i0 = 0, a reserved
// SatType cannot be placed, PRNa 0 carries no almanac, and toa past its
// 602 112 s range rejects the message.
func TestBCNAV2MidiAlmanacOrbitTypes(t *testing.T) {
	m, err := DecodeBeiDouBCNAV2(midiAlmanacWords(3, 1, 1070, 103, nil))
	if err != nil {
		t.Fatal(err)
	}
	if eph, err := m.Almanac.Ephemeris(); err != nil || eph.I0 != m.Almanac.DeltaI {
		t.Fatalf("GEO i0 not 0: %+v, %v", eph, err)
	}
	m, _ = DecodeBeiDouBCNAV2(midiAlmanacWords(3, 0, 1070, 103, nil))
	if _, err := m.Almanac.Ephemeris(); !errors.Is(err, errBadSatType) {
		t.Fatalf("reserved SatType: %v", err)
	}
	if m, err := DecodeBeiDouBCNAV2(midiAlmanacWords(0, 3, 1070, 103, nil)); err != nil || m.Almanac != nil {
		t.Fatalf("PRNa 0: %+v, %v", m, err)
	}
	if _, err := DecodeBeiDouBCNAV2(midiAlmanacWords(23, 3, 1070, 148, nil)); !errors.Is(err, errBadEpoch) {
		t.Fatalf("toa past 602112 s: %v", err)
	}
}
