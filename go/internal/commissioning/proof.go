package commissioning

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	// ExporterLabel is the RFC 5705 / RFC 8446 §7.5 exporter label both ends
	// use to derive the value a session proof signs. It is never sent.
	ExporterLabel = "EXPERIMENTAL-navlistener-mcu-proof-v1"
	// ExporterLength is the exported keying material length.
	ExporterLength = 32

	mcuKeyBits     = 3072
	mcuKeyExponent = 65537

	// EvidenceVersionV1 prefixes the GNF1 evidence frame payload.
	EvidenceVersionV1 = byte(0x01)
	// EvidenceMaxLen bounds the evidence frame a collector will read.
	EvidenceMaxLen = 2048
	maxMCUKeyLen   = 1024
	maxProofLen    = 512
)

var proofDomain = []byte("NAVL-MCU-PROOF-v1")

// ProofDigest is the value the microcontroller key signs for one TLS session.
// The exported keying material exists only inside that session, so a proof
// cannot be replayed on, or relayed to, any other connection; the record
// fingerprint ties the proof to one exact commissioning record.
func ProofDigest(exported []byte, record Record) ([32]byte, error) {
	if len(exported) != ExporterLength {
		return [32]byte{}, fmt.Errorf("exported keying material is %d bytes, want %d", len(exported), ExporterLength)
	}
	fp := record.Fingerprint()
	body := make([]byte, 0, ExporterLength+len(fp))
	body = append(body, exported...)
	body = append(body, fp[:]...)
	return digest(proofDomain, body), nil
}

// VerifyProof checks that the holder of the microcontroller key named by the
// statement signed this session. The caller has already verified the record.
func VerifyProof(s Statement, record Record, mcuKeyDER, exported, proof []byte) error {
	if s.MCUKeyAlg != MCUKeyRSA3072PSS {
		return errors.New("statement names no microcontroller key to prove")
	}
	sum := sha256.Sum256(mcuKeyDER)
	if subtle.ConstantTimeCompare(sum[:], s.MCUKeySHA256[:]) != 1 {
		return errors.New("presented microcontroller key is not the commissioned key")
	}
	parsed, err := x509.ParsePKIXPublicKey(mcuKeyDER)
	if err != nil {
		return fmt.Errorf("parse microcontroller key: %w", err)
	}
	pub, ok := parsed.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("microcontroller key type %T: want RSA", parsed)
	}
	if pub.N.BitLen() != mcuKeyBits || pub.E != mcuKeyExponent {
		return fmt.Errorf("microcontroller key is %d-bit with exponent %d: want %d-bit, exponent %d",
			pub.N.BitLen(), pub.E, mcuKeyBits, mcuKeyExponent)
	}
	d, err := ProofDigest(exported, record)
	if err != nil {
		return err
	}
	opts := &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash, Hash: crypto.SHA256}
	if err := rsa.VerifyPSS(pub, crypto.SHA256, d[:], proof, opts); err != nil {
		return errors.New("session proof signature verification failed")
	}
	return nil
}

// Evidence is what a commissioned device presents after authenticating: its
// record and, when the record names a microcontroller key, that key and a
// proof for this session.
type Evidence struct {
	Record Record
	MCUKey []byte // DER SubjectPublicKeyInfo; empty when the record names no key
	Proof  []byte // RSASSA-PSS signature; empty when MCUKey is
}

// MarshalBinary encodes the GNF1 evidence frame payload:
//
//	[1B version][2B len][record][2B len][mcu key][2B len][proof]
func (e Evidence) MarshalBinary() ([]byte, error) {
	if len(e.MCUKey) > maxMCUKeyLen || len(e.Proof) > maxProofLen {
		return nil, errors.New("evidence field exceeds its length limit")
	}
	if (len(e.MCUKey) == 0) != (len(e.Proof) == 0) {
		return nil, errors.New("evidence carries a microcontroller key and a proof together, or neither")
	}
	out := make([]byte, 0, 1+2+RecordSize+2+len(e.MCUKey)+2+len(e.Proof))
	out = append(out, EvidenceVersionV1)
	for _, field := range [][]byte{e.Record[:], e.MCUKey, e.Proof} {
		out = binary.BigEndian.AppendUint16(out, uint16(len(field)))
		out = append(out, field...)
	}
	return out, nil
}

// ParseEvidence decodes an evidence frame payload. It validates the shape and
// the embedded statement; signatures are checked by Verifier.Evaluate.
func ParseEvidence(b []byte) (Evidence, error) {
	if len(b) > EvidenceMaxLen {
		return Evidence{}, fmt.Errorf("evidence is %d bytes, limit %d", len(b), EvidenceMaxLen)
	}
	if len(b) < 1 || b[0] != EvidenceVersionV1 {
		return Evidence{}, errors.New("evidence version is unsupported")
	}
	rest := b[1:]
	next := func(limit int) ([]byte, error) {
		if len(rest) < 2 {
			return nil, errors.New("evidence is truncated")
		}
		n := int(binary.BigEndian.Uint16(rest))
		rest = rest[2:]
		if n > limit || n > len(rest) {
			return nil, errors.New("evidence field length is invalid")
		}
		field := rest[:n]
		rest = rest[n:]
		return field, nil
	}
	rawRecord, err := next(RecordSize)
	if err != nil {
		return Evidence{}, err
	}
	record, err := ParseRecord(rawRecord)
	if err != nil {
		return Evidence{}, err
	}
	key, err := next(maxMCUKeyLen)
	if err != nil {
		return Evidence{}, err
	}
	proof, err := next(maxProofLen)
	if err != nil {
		return Evidence{}, err
	}
	if len(rest) != 0 {
		return Evidence{}, errors.New("evidence has trailing bytes")
	}
	if (len(key) == 0) != (len(proof) == 0) {
		return Evidence{}, errors.New("evidence carries a microcontroller key and a proof together, or neither")
	}
	// Copy away from the caller's frame buffer.
	return Evidence{Record: record, MCUKey: append([]byte(nil), key...), Proof: append([]byte(nil), proof...)}, nil
}
