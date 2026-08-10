// Package attestation implements the manufacturer originality statement stored
// in ATECC slot 14. It is deliberately separate from operational enrollment
// certificates: this proves the factory hardware binding, not current ownership,
// organization, publication policy, or authority to ingest.
package attestation

import (
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"math/big"

	"github.com/ptudor/navlistener/internal/identity"
)

const (
	// SlotRecordSize is the full 72-byte ATECC slot-14 payload:
	// version, seven reserved zero bytes, and a fixed-width P-256 R||S.
	SlotRecordSize = 72
	VersionV1      = byte(0x01)
	VersionV2      = byte(0x02)
)

var (
	domainV1 = []byte("ATECC-MFG-ATTEST-v1")
	domainV2 = []byte("ATECC-MFG-ATTEST-v2")
)

// HardwareIdentity is the live/factory identity presented to enrollment. V1
// signs ATECCSerial + RTCEUI64 + BoardRevision. V2 additionally signs
// BoardEUI64, closing the complete three-part board binding.
type HardwareIdentity struct {
	ATECCSerial   [9]byte
	RTCEUI64      [8]byte
	BoardEUI64    [8]byte
	BoardRevision uint16
}

// Record is the exact slot representation. Reserved bytes must remain zero so
// future format changes require a new version rather than ambiguous extensions.
type Record [SlotRecordSize]byte

// Verification is the immutable result stored by an enrollment/control plane.
type Verification struct {
	Tier              identity.AttestationTier
	StatementDigest   [32]byte
	RecordFingerprint [32]byte
}

// ParseRecord validates the fixed slot shape and copies it away from caller
// memory. Signature validity is checked separately by Verify.
func ParseRecord(raw []byte) (Record, error) {
	var r Record
	if len(raw) != SlotRecordSize {
		return r, fmt.Errorf("manufacturer attestation record is %d bytes, want %d", len(raw), SlotRecordSize)
	}
	copy(r[:], raw)
	if r[0] != VersionV1 && r[0] != VersionV2 {
		return Record{}, fmt.Errorf("manufacturer attestation version 0x%02x is unsupported", r[0])
	}
	for i, b := range r[1:8] {
		if b != 0 {
			return Record{}, fmt.Errorf("manufacturer attestation reserved byte %d is nonzero", i+1)
		}
	}
	return r, nil
}

// Digest returns the normative domain-separated statement digest.
func Digest(version byte, h HardwareIdentity) ([32]byte, error) {
	if err := validateIdentity(version, h); err != nil {
		return [32]byte{}, err
	}
	prefix := domainV1
	capacity := len(prefix) + len(h.ATECCSerial) + len(h.RTCEUI64) + 2
	if version == VersionV2 {
		prefix = domainV2
		capacity += len(h.BoardEUI64)
	}
	statement := make([]byte, 0, capacity)
	statement = append(statement, prefix...)
	statement = append(statement, h.ATECCSerial[:]...)
	statement = append(statement, h.RTCEUI64[:]...)
	if version == VersionV2 {
		statement = append(statement, h.BoardEUI64[:]...)
	}
	var rev [2]byte
	binary.BigEndian.PutUint16(rev[:], h.BoardRevision)
	statement = append(statement, rev[:]...)
	return sha256.Sum256(statement), nil
}

// Sign creates the exact slot record using an offline manufacturer P-256 key.
// rng is injectable for deterministic tests; nil uses crypto/rand.Reader.
func Sign(version byte, h HardwareIdentity, key *ecdsa.PrivateKey, rng io.Reader) (Record, error) {
	if err := validatePrivateKey(key); err != nil {
		return Record{}, err
	}
	digest, err := Digest(version, h)
	if err != nil {
		return Record{}, err
	}
	if rng == nil {
		rng = rand.Reader
	}
	r, s, err := ecdsa.Sign(rng, key, digest[:])
	if err != nil {
		return Record{}, fmt.Errorf("sign manufacturer attestation: %w", err)
	}
	var out Record
	out[0] = version
	r.FillBytes(out[8:40])
	s.FillBytes(out[40:72])
	return out, nil
}

// Verify checks the slot record against live-read identifiers and the distinct
// manufacturer trust root. It never treats an operational CA as this key.
func Verify(record Record, h HardwareIdentity, manufacturer *ecdsa.PublicKey) (Verification, error) {
	if _, err := ParseRecord(record[:]); err != nil {
		return Verification{}, err
	}
	if err := validatePublicKey(manufacturer); err != nil {
		return Verification{}, err
	}
	digest, err := Digest(record[0], h)
	if err != nil {
		return Verification{}, err
	}
	r := new(big.Int).SetBytes(record[8:40])
	s := new(big.Int).SetBytes(record[40:72])
	if r.Sign() <= 0 || s.Sign() <= 0 || r.Cmp(manufacturer.Params().N) >= 0 || s.Cmp(manufacturer.Params().N) >= 0 {
		return Verification{}, fmt.Errorf("manufacturer attestation signature scalar is out of range")
	}
	if !ecdsa.Verify(manufacturer, digest[:], r, s) {
		return Verification{}, fmt.Errorf("manufacturer attestation signature verification failed")
	}
	tier := identity.AttestationVerifiedV1Partial
	if record[0] == VersionV2 {
		tier = identity.AttestationVerifiedV2
	}
	return Verification{
		Tier:              tier,
		StatementDigest:   digest,
		RecordFingerprint: sha256.Sum256(record[:]),
	}, nil
}

func validateIdentity(version byte, h HardwareIdentity) error {
	if version != VersionV1 && version != VersionV2 {
		return fmt.Errorf("manufacturer attestation version 0x%02x is unsupported", version)
	}
	if invalidFactoryID(h.ATECCSerial[:]) {
		return fmt.Errorf("ATECC serial is blank or erased")
	}
	if invalidFactoryID(h.RTCEUI64[:]) {
		return fmt.Errorf("RTC EUI-64 is blank or erased")
	}
	if version == VersionV2 && invalidFactoryID(h.BoardEUI64[:]) {
		return fmt.Errorf("board EEPROM EUI-64 is blank or erased")
	}
	return nil
}

func invalidFactoryID(b []byte) bool {
	allZero, allFF := true, true
	for _, v := range b {
		allZero = allZero && v == 0
		allFF = allFF && v == 0xff
	}
	return allZero || allFF
}

func validatePrivateKey(key *ecdsa.PrivateKey) error {
	if key == nil || key.D == nil {
		return fmt.Errorf("manufacturer private key is required")
	}
	return validatePublicKey(&key.PublicKey)
}

func validatePublicKey(key *ecdsa.PublicKey) error {
	if key == nil || key.Curve == nil || key.X == nil || key.Y == nil {
		return fmt.Errorf("manufacturer public key is required")
	}
	if key.Curve.Params().Name != "P-256" {
		return fmt.Errorf("manufacturer key curve %q: want P-256", key.Curve.Params().Name)
	}
	if !key.Curve.IsOnCurve(key.X, key.Y) {
		return fmt.Errorf("manufacturer public key is not on P-256")
	}
	return nil
}
