package boardid

import "testing"

func TestKindsAndCanonicalWire(t *testing.T) {
	for _, tc := range []struct{ kind, value, id string }{
		{"serial128", "00112233445566778899aabbccddeeff", "board-0003-00112233445566778899aabbccddeeff"},
		{"st_uid128", "20e00eff445566778899aabbccddeeff", "board-0004-20e00eff445566778899aabbccddeeff"},
	} {
		id, err := Parse(tc.kind, tc.value)
		if err != nil {
			t.Fatal(err)
		}
		if id.Hex() != tc.value || id.ObserverID() != tc.id {
			t.Fatalf("UID lost bytes: %v", id)
		}
		changed := id
		changed[34] = 1
		if changed.Validate() == nil {
			t.Fatal("accepted padding")
		}
		changed = id
		changed[2]--
		if changed.Validate() == nil {
			t.Fatal("accepted short identity")
		}
		changed = id
		changed[1] = 99
		if changed.Validate() == nil {
			t.Fatal("accepted unknown kind")
		}
	}
	for _, value := range []string{"0011223344556677", "00000000000000000000000000000000", "ffffffffffffffffffffffffffffffff", "00112233445566778899AABBCCDDEEFF"} {
		if _, err := Parse("serial128", value); err == nil {
			t.Fatalf("accepted %s", value)
		}
	}
	for _, kind := range []uint16{0, 2, 99} {
		if _, err := FromBytes(kind, []byte{1, 2, 3, 4, 5, 6, 7, 8}); err == nil {
			t.Fatalf("accepted unregistered kind %d", kind)
		}
	}
}

// Every board identity is a 128-bit factory serial; wire code 1 is retired.
func TestRetiredKindIsRejected(t *testing.T) {
	if _, err := Parse("eui64", "0004a3aabbccddee"); err == nil {
		t.Fatal("accepted a 64-bit board identity")
	}
	for _, value := range [][]byte{{0x00, 0x04, 0xa3, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}, make([]byte, 16)} {
		value[len(value)-1] |= 1
		if _, err := FromBytes(1, value); err == nil {
			t.Fatalf("accepted kind 1 with %d bytes", len(value))
		}
	}
}

func TestFormerKindNamesAreRejected(t *testing.T) {
	for _, kind := range []string{"microchip_eui64", "microchip_cs128"} {
		if _, err := Parse(kind, "00112233445566778899aabbccddeeff"); err == nil {
			t.Fatalf("accepted former kind name %s", kind)
		}
	}
}

func TestVendorNamespace(t *testing.T) {
	a, _ := Parse("serial128", "20e00eff445566778899aabbccddeeff")
	b, _ := Parse("st_uid128", a.Hex())
	if a == b || a.ObserverID() == b.ObserverID() {
		t.Fatal("vendor namespaces merged")
	}
}
