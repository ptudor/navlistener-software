package attestation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"github.com/ptudor/navlistener/internal/boardid"
	"testing"

	"github.com/ptudor/navlistener/internal/identity"
)

func testIdentity() HardwareIdentity {
	return HardwareIdentity{
		Product:       1,
		BoardRevision: 0x0102,
		BoardUID:      boardid.EEPROM([8]byte{0x00, 0x04, 0xa3, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}),
		ATECCSerial:   [9]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x11},
	}
}

func testKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestV1BindsEveryCoreIdentifier(t *testing.T) {
	h, key := testIdentity(), testKey(t)
	record, err := Sign(VersionV1, h, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Verify(record, h, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != identity.AttestationVerifiedV1Core || result.StatementDigest == ([32]byte{}) || result.RecordFingerprint == ([32]byte{}) {
		t.Fatalf("verification result = %+v", result)
	}

	mutations := map[string]func(*HardwareIdentity){
		"product":      func(x *HardwareIdentity) { x.Product++ },
		"board rev":    func(x *HardwareIdentity) { x.BoardRevision++ },
		"board EUI":    func(x *HardwareIdentity) { x.BoardUID[3] ^= 1 },
		"ATECC serial": func(x *HardwareIdentity) { x.ATECCSerial[3] ^= 1 },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := h
			mutate(&changed)
			if _, err := Verify(record, changed, &key.PublicKey); err == nil {
				t.Fatal("changed identity verified")
			}
		})
	}
}

func TestRecordAndIdentityValidationFailClosed(t *testing.T) {
	h, key := testIdentity(), testKey(t)
	record, err := Sign(VersionV1, h, key, nil)
	if err != nil {
		t.Fatal(err)
	}

	badReserved := record
	badReserved[2] = 1
	if _, err := Verify(badReserved, h, &key.PublicKey); err == nil {
		t.Fatal("nonzero reserved byte verified")
	}
	unknown := record
	unknown[0] = 2
	if _, err := Verify(unknown, h, &key.PublicKey); err == nil {
		t.Fatal("unknown version verified")
	}
	if _, err := ParseRecord(record[:len(record)-1]); err == nil {
		t.Fatal("short slot record parsed")
	}
	for name, mutate := range map[string]func(*HardwareIdentity){
		"zero product":    func(x *HardwareIdentity) { x.Product = 0 },
		"blank board EUI": func(x *HardwareIdentity) { x.BoardUID = boardid.ID{} },
		"erased ATECC serial": func(x *HardwareIdentity) {
			x.ATECCSerial = [9]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := h
			mutate(&bad)
			if _, err := Sign(VersionV1, bad, key, nil); err == nil {
				t.Fatal("invalid core identity signed")
			}
		})
	}
	record[10] ^= 1
	if _, err := Verify(record, h, &key.PublicKey); err == nil {
		t.Fatal("tampered signature verified")
	}
}

func TestDigestMatchesNormativeConcatenation(t *testing.T) {
	h := testIdentity()
	got, err := Digest(VersionV1, h)
	if err != nil {
		t.Fatal(err)
	}
	prehash := append([]byte("ATECC-MFG-CORE-v1"), 0, 0, 0, 0)
	binary.BigEndian.PutUint16(prehash[17:19], h.Product)
	binary.BigEndian.PutUint16(prehash[19:21], h.BoardRevision)
	prehash = append(prehash, h.BoardUID[:]...)
	prehash = append(prehash, h.ATECCSerial[:]...)
	if len(prehash) != 65 {
		t.Fatalf("prehash length = %d, want 65", len(prehash))
	}
	if want := sha256.Sum256(prehash); got != want {
		t.Fatalf("digest = %x, want %x", got, want)
	}
	if _, err := Digest(2, h); err == nil {
		t.Fatal("prototype version 2 accepted")
	}
}
