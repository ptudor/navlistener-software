package frame

import (
	"testing"

	"github.com/ptudor/gnss"
)

// setBits writes n bits of val (right-aligned) into buf at absolute bit offset
// start, MSB-first — the inverse of BitReader.Bits, for building synthetic CNAV
// messages with known field values.
func setBits(buf []byte, start, n int, val uint64) {
	for i := 0; i < n; i++ {
		bit := start + i
		b := (val >> uint(n-1-i)) & 1
		if b != 0 {
			buf[bit>>3] |= 1 << (7 - uint(bit&7))
		} else {
			buf[bit>>3] &^= 1 << (7 - uint(bit&7))
		}
	}
}

func cnavMsg10Words(wn, health, uraED int, toeRaw uint64) []uint32 {
	buf := make([]byte, 40) // 320 bits
	setBits(buf, 14, 6, 10) // MsgType = 10
	setBits(buf, 38, 13, uint64(wn))
	setBits(buf, 51, 3, uint64(health))
	setBits(buf, 65, 5, uint64(uraED))
	setBits(buf, 70, 11, toeRaw)

	words := make([]uint32, 10)
	for i := 0; i < 10; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestDecodeGPSCNAVMsg10Integrity guards message type 10 previously decoded
// only the ephemeris fields, silently dropping WN (bits 39-51), the L1/L2/L5 health
// flags (52-54), and URA_ED (66-70) — integrity-relevant fields already captured
// for LNAV but missing from CNAV. A table of known bit patterns, at the exact
// offsets/widths the finding specified, confirms the new fields decode correctly
// without disturbing any existing ephemeris offset.
func TestDecodeGPSCNAVMsg10Integrity(t *testing.T) {
	cases := []struct {
		name              string
		wn, health, uraED int
	}{
		{"zero", 0, 0, 0},
		{"max", 8191, 7, 31}, // 13-bit, 3-bit, 5-bit field maxima
		{"typical", 2296, 0, 2},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			words := cnavMsg10Words(c.wn, c.health, c.uraED, 400)
			m, err := DecodeGPSCNAV(gnss.GPS, words)
			if err != nil {
				t.Fatalf("decode error: %v", err)
			}
			if m.MsgType != 10 {
				t.Fatalf("MsgType = %d, want 10", m.MsgType)
			}
			if m.WN != c.wn {
				t.Errorf("WN = %d, want %d", m.WN, c.wn)
			}
			if m.Health != c.health {
				t.Errorf("Health = %d, want %d", m.Health, c.health)
			}
			if m.URAED != c.uraED {
				t.Errorf("URAED = %d, want %d", m.URAED, c.uraED)
			}
			// The pre-existing ephemeris field (toe, bits 70-80) must be unaffected by
			// the new fields at lower bit offsets.
			wantToe := 400.0 * cnavT0
			if m.eph.Toe != wantToe {
				t.Errorf("Toe = %v, want %v (new fields must not shift existing offsets)", m.eph.Toe, wantToe)
			}
		})
	}
}
