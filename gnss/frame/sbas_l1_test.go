package frame

import "testing"

// sbasWords builds a synthetic 250-bit SBAS L1 message (32-byte/8-word buffer,
// 256 bits capacity) with the given preamble and message type, and a valid
// trailing CRC-24Q over the leading 226 bits.
func sbasWords(preamble uint64, mt int) []uint32 {
	buf := make([]byte, 32)
	setBits(buf, 0, 8, preamble)
	setBits(buf, 8, 6, uint64(mt))
	crc := CRC24QBits(buf, 0, 226)
	setBits(buf, 226, 24, uint64(crc))

	words := make([]uint32, 8)
	for i := 0; i < 8; i++ {
		words[i] = uint32(buf[i*4])<<24 | uint32(buf[i*4+1])<<16 | uint32(buf[i*4+2])<<8 | uint32(buf[i*4+3])
	}
	return words
}

// TestDecodeSBASL1RejectsBadCRC guards a well-formed message decodes
// cleanly, and any single flipped bit must now be rejected outright via the
// CRC-24Q check, not silently decoded with corrupted fields.
func TestDecodeSBASL1RejectsBadCRC(t *testing.T) {
	words := sbasWords(0x53, 7) // preamble ok, a non-zero (not "do not use") type
	m, err := DecodeSBASL1(131, words)
	if err != nil {
		t.Fatalf("well-formed message must decode cleanly: %v", err)
	}
	if m.Type != 7 || m.DoNotUse || !m.PreambleOK {
		t.Fatalf("decoded fields wrong: %+v", m)
	}

	corrupted := append([]uint32(nil), words...)
	corrupted[0] ^= 1 // flip the last (pad-adjacent) bit of word 0
	if _, err := DecodeSBASL1(131, corrupted); err != ErrBadCRC {
		t.Errorf("corrupted message: err = %v, want ErrBadCRC", err)
	}
}

// TestDecodeSBASL1FabricatedDoNotUseNowRejected guards the finding's exact
// scenario: a message broadcasting a non-zero type gets its type field
// corrupted to 0 by a single bit flip. Before the regression fix this silently decoded with
// DoNotUse=true (a fabricated safety alarm); it must now be rejected outright.
// The type field occupies absolute bits 8-13 (MSB-first); bit 13 (its LSB) is
// bit index 13 within word 0, i.e. mask 1<<(31-13) = 1<<18.
func TestDecodeSBASL1FabricatedDoNotUseNowRejected(t *testing.T) {
	words := sbasWords(0x53, 1) // type = 1 (binary 000001)
	corrupted := append([]uint32(nil), words...)
	corrupted[0] ^= 1 << 18 // flip the type field's LSB: 1 -> 0
	m, err := DecodeSBASL1(131, corrupted)
	if err == nil {
		t.Fatalf("corrupted-to-type-0 message must be rejected, not decoded as DoNotUse=%v (type=%d)", m.DoNotUse, m.Type)
	}
	if err != ErrBadCRC {
		t.Errorf("err = %v, want ErrBadCRC", err)
	}
}
