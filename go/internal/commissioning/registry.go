package commissioning

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// RegistryFormat names the signed registry envelope.
const RegistryFormat = "navlistener-registry-v1"

const (
	// StatusActive boards keep whatever their commissioning record proves.
	StatusActive = "active"
	// StatusRevoked boards prove nothing, whatever they present.
	StatusRevoked = "revoked"

	// registryMaxBytes bounds a registry file before it is parsed.
	registryMaxBytes = 64 << 20
)

var registryDomain = []byte("NAVL-REGISTRY-v1")

// Registry is the manufacturer's published view of commissioned boards. It
// can only take trust away: a board is trusted because of its manufacturer-
// signed record and its session proof, and the registry says which record is
// current and which boards have been withdrawn. Its signing key is therefore
// an operations key, separate from the offline manufacturer key, and its
// compromise cannot create a trusted board.
type Registry struct {
	Sequence   uint64          `json:"sequence"`
	IssuedAt   time.Time       `json:"issued_at"`
	LedgerHead string          `json:"ledger_head"`
	Boards     []RegistryBoard `json:"boards"`
}

// RegistryBoard is one commissioned board. Identifiers are lowercase hex.
type RegistryBoard struct {
	BoardEUI64  string `json:"board_eui64"`
	RTCEUI64    string `json:"rtc_eui64"`
	ATECCSerial string `json:"atecc_serial"`
	Status      string `json:"status"`
	Reason      string `json:"reason,omitempty"`
	Profile     string `json:"profile"`
	Generation  uint32 `json:"generation"`
	Record      string `json:"record"` // base64 of the current commissioning record
}

type registryEnvelope struct {
	Format     string              `json:"format"`
	Payload    string              `json:"payload"`
	Signatures []registrySignature `json:"signatures"`
}

type registrySignature struct {
	KeyID     string `json:"key_id"`
	Signature string `json:"signature"`
}

// SignRegistry validates and signs a registry, returning the envelope bytes.
func SignRegistry(reg Registry, signer Signer) ([]byte, error) {
	if _, err := indexRegistry(reg); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(reg)
	if err != nil {
		return nil, fmt.Errorf("encode registry: %w", err)
	}
	sig, err := signer.SignDigest(digest(registryDomain, payload))
	if err != nil {
		return nil, err
	}
	id := signer.KeyID()
	out, err := json.Marshal(registryEnvelope{
		Format:  RegistryFormat,
		Payload: base64.StdEncoding.EncodeToString(payload),
		Signatures: []registrySignature{{
			KeyID:     hex.EncodeToString(id[:]),
			Signature: base64.StdEncoding.EncodeToString(sig[:]),
		}},
	})
	if err != nil {
		return nil, fmt.Errorf("encode registry envelope: %w", err)
	}
	return append(out, '\n'), nil
}

// RegistryIndex is a verified registry keyed for lookup.
type RegistryIndex struct {
	Sequence   uint64
	IssuedAt   time.Time
	LedgerHead string
	boards     map[[8]byte]indexedBoard
}

type indexedBoard struct {
	status      string
	reason      string
	fingerprint [32]byte
}

// Len reports how many boards the registry lists.
func (ix *RegistryIndex) Len() int { return len(ix.boards) }

// VerifyRegistry checks an envelope against the pinned registry keys and
// returns its index. One valid signature from a pinned key is sufficient;
// signatures from unknown keys are ignored so a key can be introduced before
// every verifier pins it.
func VerifyRegistry(data []byte, keys *KeySet) (*RegistryIndex, error) {
	if len(data) > registryMaxBytes {
		return nil, fmt.Errorf("registry is %d bytes, limit %d", len(data), registryMaxBytes)
	}
	var env registryEnvelope
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return nil, fmt.Errorf("decode registry envelope: %w", err)
	}
	if env.Format != RegistryFormat {
		return nil, fmt.Errorf("registry format %q is unsupported", env.Format)
	}
	payload, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode registry payload: %w", err)
	}
	d := digest(registryDomain, payload)
	verified := false
	for _, s := range env.Signatures {
		rawID, err := hex.DecodeString(s.KeyID)
		if err != nil || len(rawID) != KeyIDSize {
			continue
		}
		var id [KeyIDSize]byte
		copy(id[:], rawID)
		if _, pinned := keys.keys[id]; !pinned {
			continue
		}
		sig, err := base64.StdEncoding.DecodeString(s.Signature)
		if err != nil {
			return nil, fmt.Errorf("decode registry signature from %s: %w", s.KeyID, err)
		}
		if err := keys.verifyDigest(id, d, sig); err != nil {
			return nil, fmt.Errorf("registry signature from %s: %w", s.KeyID, err)
		}
		verified = true
	}
	if !verified {
		return nil, errors.New("registry carries no signature from a pinned registry key")
	}
	var reg Registry
	dec = json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&reg); err != nil {
		return nil, fmt.Errorf("decode registry payload: %w", err)
	}
	return indexRegistry(reg)
}

func indexRegistry(reg Registry) (*RegistryIndex, error) {
	if reg.Sequence == 0 {
		return nil, errors.New("registry sequence starts at 1")
	}
	if reg.IssuedAt.IsZero() {
		return nil, errors.New("registry issue time is required")
	}
	if head, err := hex.DecodeString(reg.LedgerHead); err != nil || len(head) != 32 {
		return nil, errors.New("registry ledger head must be a 32-byte hex digest")
	}
	ix := &RegistryIndex{
		Sequence: reg.Sequence, IssuedAt: reg.IssuedAt, LedgerHead: reg.LedgerHead,
		boards: make(map[[8]byte]indexedBoard, len(reg.Boards)),
	}
	observers := make(map[[8]byte]struct{}, len(reg.Boards))
	for i, b := range reg.Boards {
		if b.Status != StatusActive && b.Status != StatusRevoked {
			return nil, fmt.Errorf("registry board %d: status %q is unknown", i, b.Status)
		}
		raw, err := base64.StdEncoding.DecodeString(b.Record)
		if err != nil {
			return nil, fmt.Errorf("registry board %d: decode record: %w", i, err)
		}
		record, err := ParseRecord(raw)
		if err != nil {
			return nil, fmt.Errorf("registry board %d: %w", i, err)
		}
		s, err := record.Statement()
		if err != nil {
			return nil, fmt.Errorf("registry board %d: %w", i, err)
		}
		// The descriptive columns exist for people and for other importers; they
		// must agree with the signed record they summarise.
		if b.BoardEUI64 != hex.EncodeToString(s.BoardEUI64[:]) || b.RTCEUI64 != hex.EncodeToString(s.RTCEUI64[:]) ||
			b.ATECCSerial != hex.EncodeToString(s.ATECCSerial[:]) || b.Profile != s.Profile.String() || b.Generation != s.Generation {
			return nil, fmt.Errorf("registry board %d: columns disagree with the commissioning record", i)
		}
		if _, dup := ix.boards[s.BoardEUI64]; dup {
			return nil, fmt.Errorf("registry board %d: board %s is listed twice", i, b.BoardEUI64)
		}
		if _, dup := observers[s.RTCEUI64]; dup {
			return nil, fmt.Errorf("registry board %d: observer %s is listed twice", i, b.RTCEUI64)
		}
		observers[s.RTCEUI64] = struct{}{}
		ix.boards[s.BoardEUI64] = indexedBoard{status: b.Status, reason: b.Reason, fingerprint: record.Fingerprint()}
	}
	return ix, nil
}
