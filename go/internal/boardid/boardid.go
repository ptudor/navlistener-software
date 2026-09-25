// Package boardid defines the signed, typed identity of a physical board: its
// EEPROM's 128-bit factory serial. Kind numbers are permanent assignments; an
// unknown or retired kind is never accepted.
package boardid

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
)

const (
	Size         = 35 // uint16 kind, uint8 length, 32 bytes zero-padded on the right
	MaxValueSize = 32
	ValueSize    = 16 // every board identity is a 128-bit factory serial
	// Kind names follow esp_hardware_discovery's factory-ID kinds. Code 1 is retired.
	Serial128 uint16 = 3 // Microchip 24CS128/24CS256/24CS512 factory serial
	STUID128  uint16 = 4 // ST M24128-U unique ID
)

// ID is comparable, so registry keys include both kind and value.
type ID [Size]byte

func (id ID) Kind() uint16 { return binary.BigEndian.Uint16(id[:2]) }
func (id ID) KindName() string {
	switch id.Kind() {
	case Serial128:
		return "serial128"
	case STUID128:
		return "st_uid128"
	default:
		return "unknown"
	}
}
func (id ID) Value() []byte {
	if id[2] > MaxValueSize {
		return nil
	}
	return id[3 : 3+int(id[2])]
}
func (id ID) Hex() string        { return hex.EncodeToString(id.Value()) }
func (id ID) ObserverID() string { return fmt.Sprintf("board-%04x-%s", id.Kind(), id.Hex()) }

func (id ID) Validate() error {
	n := ValueSize
	switch id.Kind() {
	case Serial128, STUID128:
	default:
		return fmt.Errorf("unsupported board UID kind %d", id.Kind())
	}
	if int(id[2]) != n {
		return fmt.Errorf("board UID kind %s requires %d bytes", id.KindName(), n)
	}
	zero, erased := true, true
	for _, b := range id.Value() {
		zero = zero && b == 0
		erased = erased && b == 0xff
	}
	if zero || erased {
		return fmt.Errorf("board UID is blank or erased")
	}
	for _, b := range id[3+n:] {
		if b != 0 {
			return fmt.Errorf("board UID padding is nonzero")
		}
	}

	return nil
}

func FromBytes(kind uint16, value []byte) (ID, error) {
	var id ID
	if len(value) > MaxValueSize {
		return id, fmt.Errorf("board UID exceeds %d bytes", MaxValueSize)
	}
	binary.BigEndian.PutUint16(id[:], kind)
	id[2] = byte(len(value))
	copy(id[3:], value)
	return id, id.Validate()
}

func Parse(kind, value string) (ID, error) {
	var k uint16
	switch kind {
	case "serial128":
		k = Serial128
	case "st_uid128":
		k = STUID128
	default:
		return ID{}, fmt.Errorf("unsupported board UID kind %q", kind)
	}
	raw, err := hex.DecodeString(value)
	if err != nil || hex.EncodeToString(raw) != value {
		return ID{}, fmt.Errorf("board UID must be canonical lowercase hex")
	}
	return FromBytes(k, raw)
}

// MustParse is Parse for constants and tests: it panics on an invalid identity.
func MustParse(kind, value string) ID {
	id, err := Parse(kind, value)
	if err != nil {
		panic(err)
	}
	return id
}
