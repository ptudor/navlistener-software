package frame

import (
	"math"
	"testing"

	"github.com/ptudor/gnss"
)

func TestDecodeGPSCNAVARefByConstellation(t *testing.T) {
	words := cnavMsg10Words(2296, 0, 2, 400)
	gps, err := DecodeGPSCNAV(gnss.GPS, words)
	if err != nil {
		t.Fatal(err)
	}
	qzss, err := DecodeGPSCNAV(gnss.QZSS, words)
	if err != nil {
		t.Fatal(err)
	}
	if gps.eph.SqrtA != math.Sqrt(cnavArefGPS) {
		t.Errorf("GPS sqrtA = %.12f, want %.12f", gps.eph.SqrtA, math.Sqrt(cnavArefGPS))
	}
	if qzss.eph.SqrtA != math.Sqrt(cnavArefQZSS) {
		t.Errorf("QZSS sqrtA = %.12f, want %.12f", qzss.eph.SqrtA, math.Sqrt(cnavArefQZSS))
	}

	// A raw delta-A count changes the reference axis by exactly 2^-9 metres.
	buf := make([]byte, 40)
	setBits(buf, 14, 6, 10)
	setBits(buf, 81, 26, 1)
	setCNAVPreambleAndCRC(buf)
	for i := range words {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	shifted, err := DecodeGPSCNAV(gnss.QZSS, words)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := shifted.eph.SqrtA, math.Sqrt(cnavArefQZSS+p2m9); got != want {
		t.Errorf("QZSS delta-A sqrtA = %.12f, want %.12f", got, want)
	}
}

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
	setBits(buf, 8, 6, 5)   // PRN = 5 (assemblers now require PRN == svid)
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

// cnavMsg11Words builds a synthetic CNAV message type 11 (Ephemeris 2) with the given raw
// toe (bit 38); other fields zeroed. Used to pair with an MT10 for AssembleGPSCNAV.
func cnavMsg11Words(toeRaw uint64) []uint32 {
	buf := make([]byte, 40)
	setBits(buf, 8, 6, 5)   // PRN = 5 
	setBits(buf, 14, 6, 11) // MsgType = 11
	setBits(buf, 38, 11, toeRaw)
	setCNAVPreambleAndCRC(buf)
	words := make([]uint32, 10)
	for i := 0; i < 10; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestAssembleGPSCNAVStaleClockDropped guards IS-GPS-200N §30.3.4.4 requires
// toe == toc for the same CNAV CEI data set. A clock message whose Toc doesn't match the
// ephemeris toe must be dropped (clkOK false, zero clock), not attached as if coherent.
func TestAssembleGPSCNAVStaleClockDropped(t *testing.T) {
	dec := func(w []uint32) *GPSCNAV {
		m, err := DecodeGPSCNAV(gnss.GPS, w)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	m10 := dec(cnavMsg10Words(2296, 0, 2, 400)) // toe raw 400
	m11 := dec(cnavMsg11Words(400))             // same toe

	// A clock whose Toc matches toe (400) is attached.
	clkGood := dec(cnavMsg30Words(400, 12345, -6789, 42, -100, 55, -200, 300, -400))
	_, clk, ok, err := AssembleGPSCNAV(gnss.GPS, 5, m10, m11, clkGood)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || clk.Toc != 400*cnavT0 {
		t.Errorf("matching-Toc clock not attached: ok=%v Toc=%v", ok, clk.Toc)
	}

	// A clock whose Toc (500) differs from toe (400) is dropped.
	clkStale := dec(cnavMsg30Words(500, 12345, -6789, 42, -100, 55, -200, 300, -400))
	_, clk2, ok2, err := AssembleGPSCNAV(gnss.GPS, 5, m10, m11, clkStale)
	if err != nil {
		t.Fatal(err)
	}
	if ok2 {
		t.Error("stale clock (Toc != toe) reported as coherent")
	}
	if clk2.Af0 != 0 || clk2.Toc != 0 {
		t.Errorf("stale clock leaked into the model: Af0=%v Toc=%v, want zero", clk2.Af0, clk2.Toc)
	}
}

func TestAssembleGPSCNAVRejectsWrongSlotsAndClock(t *testing.T) {
	dec := func(w []uint32) *GPSCNAV {
		m, err := DecodeGPSCNAV(gnss.GPS, w)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	m10 := dec(cnavMsg10Words(2296, 0, 2, 0))
	m11 := dec(cnavMsg11Words(0))
	if _, _, _, err := AssembleGPSCNAV(gnss.GPS, 5, m11, m10, nil); err != ErrWrongMsgType {
		t.Errorf("transposed args error = %v, want ErrWrongMsgType", err)
	}
	if _, _, ok, err := AssembleGPSCNAV(gnss.GPS, 5, m10, m11, m10); err != nil || ok {
		t.Errorf("non-clock MT10 attached as clock: ok=%v err=%v", ok, err)
	}
}

// cnavWithPRN re-stamps a built CNAV message's header PRN (ICD bits 9–14) and
// recomputes the CRC, for building cross-SV chimera scenarios.
func cnavWithPRN(words []uint32, prn int) []uint32 {
	buf := make([]byte, 40)
	for i, w := range words {
		buf[i*4], buf[i*4+1], buf[i*4+2], buf[i*4+3] = byte(w>>24), byte(w>>16), byte(w>>8), byte(w)
	}
	setBits(buf, 8, 6, uint64(prn))
	setCNAVPreambleAndCRC(buf)
	out := make([]uint32, 10)
	for i := range out {
		out[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return out
}

// TestAssembleGPSCNAVRejectsCrossSVPair guards the control segment
// routinely uploads batches of SVs sharing one toe, so toe equality cannot
// distinguish SV A's MT10 from SV B's MT11 — a caller pairing messages only on
// the documented "must share a toe" contract could assemble a cross-SV chimera
// with err == nil. The header PRN must match across all supplied messages AND
// the svid the caller is assembling for; a wrong-SV clock is dropped (not
// attached) like a stale one.
func TestAssembleGPSCNAVRejectsCrossSVPair(t *testing.T) {
	dec := func(w []uint32) *GPSCNAV {
		m, err := DecodeGPSCNAV(gnss.GPS, w)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		return m
	}
	m10 := dec(cnavMsg10Words(2296, 0, 2, 400))
	m11same := dec(cnavMsg11Words(400))
	m11other := dec(cnavWithPRN(cnavMsg11Words(400), 6)) // same toe, different SV

	if _, _, _, err := AssembleGPSCNAV(gnss.GPS, 5, m10, m11other, nil); err != errPRNMismatch {
		t.Errorf("cross-SV MT10/MT11 sharing a toe: err = %v, want errPRNMismatch", err)
	}
	// Assembling under the wrong svid must also fail, even with self-consistent messages.
	if _, _, _, err := AssembleGPSCNAV(gnss.GPS, 7, m10, m11same, nil); err != errPRNMismatch {
		t.Errorf("svid 7 with PRN-5 messages: err = %v, want errPRNMismatch", err)
	}
	// A wrong-SV clock whose Toc matches the toe (batch upload) is dropped, not attached.
	clkOther := dec(cnavWithPRN(cnavMsg30Words(400, 12345, -6789, 42, -100, 55, -200, 300, -400), 6))
	_, clk, ok, err := AssembleGPSCNAV(gnss.GPS, 5, m10, m11same, clkOther)
	if err != nil {
		t.Fatal(err)
	}
	if ok || clk.Af0 != 0 {
		t.Errorf("wrong-SV clock attached: ok=%v Af0=%v", ok, clk.Af0)
	}
	// And the well-matched triple still assembles with the clock.
	clkGood := dec(cnavMsg30Words(400, 12345, -6789, 42, -100, 55, -200, 300, -400))
	if _, _, ok, err := AssembleGPSCNAV(gnss.GPS, 5, m10, m11same, clkGood); err != nil || !ok {
		t.Errorf("matched triple: ok=%v err=%v, want clean assembly with clock", ok, err)
	}
}

// cnavMsg30Words builds a synthetic CNAV message type 30 (clock, iono & group
// delay) with the given raw field counts, signed fields passed as int64 and
// masked to their two's-complement bit width before packing.
func cnavMsg30Words(toc uint64, af0, af1, af2, tgd, iscL1CA, iscL2C, iscL5I5, iscL5Q5 int64) []uint32 {
	buf := make([]byte, 40)
	setBits(buf, 8, 6, 5)   // PRN = 5 
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
// (IS-GPS-200N Table 30-IV, ICD bit 127, 13 bits, 2⁻³⁵) and the four ISCs
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

// TestDecodeGPSCNAVAlertFlag guards the common-header alert flag (ICD
// bit 38, the single bit between TOW ending at 37 and the MT10 WN starting at
// 39; 0-indexed 37) is the L2C/L5 "URA may be worse than indicated — use at own
// risk" declaration (IS-GPS-200N §6.4.6.3) and was never decoded. It must
// decode on every message type, and must not disturb the adjacent TOW/WN.
func TestDecodeGPSCNAVAlertFlag(t *testing.T) {
	for _, build := range []func() []uint32{
		func() []uint32 { return cnavMsg10Words(2296, 0, 2, 400) },
		func() []uint32 { return cnavMsg11Words(400) },
		func() []uint32 { return cnavMsg30Words(400, 1, 1, 1, 1, 1, 1, 1, 1) },
	} {
		words := build()
		m, err := DecodeGPSCNAV(gnss.GPS, words)
		if err != nil {
			t.Fatal(err)
		}
		if m.Alert {
			t.Errorf("MT%d: alert reads raised on a clear header", m.MsgType)
		}

		// Re-pack with bit 37 set.
		buf := make([]byte, 40)
		for i, w := range words {
			buf[i*4], buf[i*4+1], buf[i*4+2], buf[i*4+3] = byte(w>>24), byte(w>>16), byte(w>>8), byte(w)
		}
		setBits(buf, 37, 1, 1)
		setCNAVPreambleAndCRC(buf)
		for i := range words {
			words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
		}
		m2, err := DecodeGPSCNAV(gnss.GPS, words)
		if err != nil {
			t.Fatal(err)
		}
		if !m2.Alert {
			t.Errorf("MT%d: alert bit 38 not decoded", m2.MsgType)
		}
		if m2.TOW != m.TOW || m2.WN != m.WN {
			t.Errorf("MT%d: alert bit disturbed TOW/WN: %v/%d vs %v/%d", m2.MsgType, m2.TOW, m2.WN, m.TOW, m.WN)
		}
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
		// URA_ED is signed (+15..−16). bits 11111 = −1 (was mis-read as 31);
		// bits 10000 = −16 (minimum); bits 01111 = +15 (maximum).
		{"neg_one", 8191, 7, -1},
		{"neg_min", 0, 0, -16},
		{"pos_max", 0, 0, 15},
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
