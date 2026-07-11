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

// setCNAVPreambleAndCRC stamps the 0x8B preamble and a valid CRC-24Q  onto
// a synthetic 320-bit CNAV buffer (bits 0-299 used, 20 pad bits): preamble
// occupies the whole of byte 0 (bits 0-7, before any field this file sets), and
// the CRC is computed over the 276-bit payload and written into its own trailing
// bits 276-299 — after which CRC24QBits(buf, 0, 300) is 0 by construction, so
// DecodeGPSCNAV's own preamble/CRC checks pass.
func setCNAVPreambleAndCRC(buf []byte) {
	buf[0] = 0x8B
	crc := CRC24QBits(buf, 0, 276)
	setBits(buf, 276, 24, uint64(crc))
}

func cnavMsg10Words(wn, health, uraED int, toeRaw uint64) []uint32 {
	buf := make([]byte, 40) // 320 bits
	setBits(buf, 14, 6, 10) // MsgType = 10
	setBits(buf, 38, 13, uint64(wn))
	setBits(buf, 51, 3, uint64(health))
	setBits(buf, 65, 5, uint64(uraED))
	setBits(buf, 70, 11, toeRaw)
	setCNAVPreambleAndCRC(buf)

	words := make([]uint32, 10)
	for i := 0; i < 10; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// cnavMsg30Words builds a synthetic CNAV message type 30 (clock, iono & group
// delay) with the given raw field counts, signed fields passed as int64 and
// masked to their two's-complement bit width before packing.
func cnavMsg30Words(toc uint64, af0, af1, af2, tgd, iscL1CA, iscL2C, iscL5I5, iscL5Q5 int64) []uint32 {
	buf := make([]byte, 40)
	setBits(buf, 14, 6, 30) // MsgType = 30
	setBits(buf, 60, 11, toc)
	setBits(buf, 71, 26, uint64(af0)&((1<<26)-1))
	setBits(buf, 97, 20, uint64(af1)&((1<<20)-1))
	setBits(buf, 117, 10, uint64(af2)&((1<<10)-1))
	setBits(buf, 127, 13, uint64(tgd)&((1<<13)-1))
	setBits(buf, 140, 13, uint64(iscL1CA)&((1<<13)-1))
	setBits(buf, 153, 13, uint64(iscL2C)&((1<<13)-1))
	setBits(buf, 166, 13, uint64(iscL5I5)&((1<<13)-1))
	setBits(buf, 179, 13, uint64(iscL5Q5)&((1<<13)-1))
	setCNAVPreambleAndCRC(buf)

	words := make([]uint32, 10)
	for i := 0; i < 10; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// cnavMsg31Words builds a synthetic CNAV message type 31 (clock + GGTO): same
// common clock block as MT30, but no TGD/ISC fields at all.
func cnavMsg31Words(toc uint64, af0, af1, af2 int64) []uint32 {
	buf := make([]byte, 40)
	setBits(buf, 14, 6, 31) // MsgType = 31
	setBits(buf, 60, 11, toc)
	setBits(buf, 71, 26, uint64(af0)&((1<<26)-1))
	setBits(buf, 97, 20, uint64(af1)&((1<<20)-1))
	setBits(buf, 117, 10, uint64(af2)&((1<<10)-1))
	setCNAVPreambleAndCRC(buf)

	words := make([]uint32, 10)
	for i := 0; i < 10; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestDecodeGPSCNAVMsg30GroupDelayAndISCs guards message type 30's T_GD
// (IS-GPS-200 Table 30-IV, ICD bit 127, 13 bits, 2⁻³⁵) and the four ISCs
// immediately following it were never decoded — clk.TGD stayed 0 for every
// CNAV-derived clock, biasing the clock offset by up to ~13 ns. Exercises
// positive and negative (two's-complement) raw values at all five fields, and
// confirms the pre-existing Toc/af0/af1/af2 fields are unaffected.
func TestDecodeGPSCNAVMsg30GroupDelayAndISCs(t *testing.T) {
	words := cnavMsg30Words(1000, 12345, -6789, 42, -100, 55, -200, 300, -400)
	m, err := DecodeGPSCNAV(gnss.GPS, words)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if m.MsgType != 30 {
		t.Fatalf("MsgType = %d, want 30", m.MsgType)
	}
	if !m.hasClk {
		t.Fatal("hasClk = false, want true")
	}
	if got, want := m.clk.Toc, 1000.0*cnavT0; got != want {
		t.Errorf("Toc = %v, want %v (must be unaffected by the new TGD/ISC fields)", got, want)
	}
	if got, want := m.clk.Af0, 12345.0*p2m35; got != want {
		t.Errorf("Af0 = %v, want %v", got, want)
	}
	if got, want := m.clk.Af1, -6789.0*p2m48; got != want {
		t.Errorf("Af1 = %v, want %v", got, want)
	}
	if got, want := m.clk.Af2, 42.0*p2m60; got != want {
		t.Errorf("Af2 = %v, want %v", got, want)
	}
	if got, want := m.clk.TGD, -100.0*p2m35; got != want {
		t.Errorf("TGD = %v, want %v", got, want)
	}
	if got, want := m.ISCL1CA, 55.0*p2m35; got != want {
		t.Errorf("ISCL1CA = %v, want %v", got, want)
	}
	if got, want := m.ISCL2C, -200.0*p2m35; got != want {
		t.Errorf("ISCL2C = %v, want %v", got, want)
	}
	if got, want := m.ISCL5I5, 300.0*p2m35; got != want {
		t.Errorf("ISCL5I5 = %v, want %v", got, want)
	}
	if got, want := m.ISCL5Q5, -400.0*p2m35; got != want {
		t.Errorf("ISCL5Q5 = %v, want %v", got, want)
	}
}

// TestDecodeGPSCNAVMsg31NoGroupDelay guards other half: T_GD/ISCs are
// MT30-only content, not common to every clock message (30-37) — a message
// type 31 (clock + GGTO) must decode the shared clock block correctly and
// leave TGD/ISCs at zero, never reading MT30's field layout into MT31's
// differently-structured tail.
func TestDecodeGPSCNAVMsg31NoGroupDelay(t *testing.T) {
	words := cnavMsg31Words(1000, 12345, -6789, 42)
	m, err := DecodeGPSCNAV(gnss.GPS, words)
	if err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if m.MsgType != 31 {
		t.Fatalf("MsgType = %d, want 31", m.MsgType)
	}
	if got, want := m.clk.Toc, 1000.0*cnavT0; got != want {
		t.Errorf("Toc = %v, want %v", got, want)
	}
	if m.clk.TGD != 0 {
		t.Errorf("TGD = %v, want 0 (MT30-only field)", m.clk.TGD)
	}
	if m.ISCL1CA != 0 || m.ISCL2C != 0 || m.ISCL5I5 != 0 || m.ISCL5Q5 != 0 {
		t.Errorf("ISCs = %v/%v/%v/%v, want all 0 (MT30-only fields)",
			m.ISCL1CA, m.ISCL2C, m.ISCL5I5, m.ISCL5Q5)
	}
}

// TestDecodeGPSCNAVRejectsBadPreambleAndCRC guards DecodeGPSCNAV must
// reject a message whose preamble isn't 0x8B, and separately a message with a
// good preamble but a corrupted CRC-24Q — neither push-path frames from remote
// feeders nor any local decode path should be trusted without validating both,
// since no documented u-blox guarantee of pre-validated CNAV was found (unlike
// LNAV's D30*-corrected delivery).
func TestDecodeGPSCNAVRejectsBadPreambleAndCRC(t *testing.T) {
	good := cnavMsg10Words(2296, 0, 2, 400)
	if _, err := DecodeGPSCNAV(gnss.GPS, good); err != nil {
		t.Fatalf("well-formed message must decode cleanly: %v", err)
	}

	badPreamble := append([]uint32(nil), good...)
	badPreamble[0] ^= 0xFF000000 // flip the preamble byte (word 0's top byte)
	if _, err := DecodeGPSCNAV(gnss.GPS, badPreamble); err != ErrBadPreamble {
		t.Errorf("bad preamble: err = %v, want ErrBadPreamble", err)
	}

	badCRC := append([]uint32(nil), good...)
	badCRC[9] ^= 1 << 29 // flip absolute bit 290 -- inside the trailing CRC (276-299), not the pad tail
	if _, err := DecodeGPSCNAV(gnss.GPS, badCRC); err != ErrBadCRC {
		t.Errorf("bad CRC: err = %v, want ErrBadCRC", err)
	}
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
