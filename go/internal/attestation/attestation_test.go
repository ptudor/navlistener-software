package attestation

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"testing"

	"github.com/ptudor/navlistener/internal/identity"
)

func testIdentity() HardwareIdentity {
	return HardwareIdentity{
		ATECCSerial:   [9]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x11},
		RTCEUI64:      [8]byte{0x00, 0x04, 0xa3, 0x12, 0x34, 0x56, 0x78, 0x90},
		BoardEUI64:    [8]byte{0x00, 0x04, 0xa3, 0xaa, 0xbb, 0xcc, 0xdd, 0xee},
		BoardRevision: 0x0102,
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

func TestV2BindsEveryFactoryIdentifier(t *testing.T) {
	h, key := testIdentity(), testKey(t)
	record, err := Sign(VersionV2, h, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Verify(record, h, &key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if result.Tier != identity.AttestationVerifiedV2 || result.StatementDigest == ([32]byte{}) || result.RecordFingerprint == ([32]byte{}) {
		t.Fatalf("verification result = %+v", result)
	}

	mutations := map[string]func(*HardwareIdentity){
		"ATECC serial": func(x *HardwareIdentity) { x.ATECCSerial[3] ^= 1 },
		"RTC EUI":      func(x *HardwareIdentity) { x.RTCEUI64[3] ^= 1 },
		"board EUI":    func(x *HardwareIdentity) { x.BoardEUI64[3] ^= 1 },
		"board rev":    func(x *HardwareIdentity) { x.BoardRevision++ },
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

func TestV1IsExplicitlyPartial(t *testing.T) {
	h, key := testIdentity(), testKey(t)
	record, err := Sign(VersionV1, h, key, nil)
	if err != nil {
		t.Fatal(err)
	}
	changedBoard := h
	changedBoard.BoardEUI64[4] ^= 0xff
	result, err := Verify(record, changedBoard, &key.PublicKey)
	if err != nil {
		t.Fatalf("v1 should not claim or bind board EUI: %v", err)
	}
	if result.Tier != identity.AttestationVerifiedV1Partial {
		t.Fatalf("v1 tier = %q", result.Tier)
	}
}

func TestRecordAndIdentityValidationFailClosed(t *testing.T) {
	h, key := testIdentity(), testKey(t)
	record, err := Sign(VersionV2, h, key, nil)
	if err != nil {
		t.Fatal(err)
	}

	badReserved := record
	badReserved[2] = 1
	if _, err := Verify(badReserved, h, &key.PublicKey); err == nil {
		t.Fatal("nonzero reserved byte verified")
	}
	unknown := record
	unknown[0] = 3
	if _, err := Verify(unknown, h, &key.PublicKey); err == nil {
		t.Fatal("unknown version verified")
	}
	if _, err := ParseRecord(record[:len(record)-1]); err == nil {
		t.Fatal("short slot record parsed")
	}
	blank := h
	blank.BoardEUI64 = [8]byte{}
	if _, err := Sign(VersionV2, blank, key, nil); err == nil {
		t.Fatal("blank v2 board EUI signed")
	}
	record[10] ^= 1
	if _, err := Verify(record, h, &key.PublicKey); err == nil {
		t.Fatal("tampered signature verified")
	}
}

func TestDigestMatchesNormativeConcatenation(t *testing.T) {
	h := testIdentity()
	v1, err := Digest(VersionV1, h)
	if err != nil {
		t.Fatal(err)
	}
	v2, err := Digest(VersionV2, h)
	if err != nil {
		t.Fatal(err)
	}
	if v1 == v2 {
		t.Fatal("v1 and v2 statements share a digest")
	}
	changed := h
	changed.BoardEUI64[0] ^= 1
	v1Changed, _ := Digest(VersionV1, changed)
	v2Changed, _ := Digest(VersionV2, changed)
	if v1Changed != v1 {
		t.Fatal("v1 unexpectedly includes board EUI")
	}
	if v2Changed == v2 {
		t.Fatal("v2 omitted board EUI")
	}
}
