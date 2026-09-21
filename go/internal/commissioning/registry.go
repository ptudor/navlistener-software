package commissioning

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ptudor/navlistener/internal/boardid"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/strictjson"
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
	ManufacturerAuthorityID string          `json:"manufacturer_authority_id"`
	Sequence                uint64          `json:"sequence"`
	IssuedAt                time.Time       `json:"issued_at"`
	LedgerHead              string          `json:"ledger_head"`
	Boards                  []RegistryBoard `json:"boards"`
}

// RegistryBoard is one commissioned board. Identifiers are lowercase hex.
type RegistryBoard struct {
	BoardUIDKind string  `json:"board_uid_kind"`
	BoardUID     string  `json:"board_uid"`
	RTCModelID   uint16  `json:"rtc_model_id"`
	RTCEUI64     *string `json:"rtc_eui64"`
	ATECCSerial  string  `json:"atecc_serial"`
	Status       string  `json:"status"`
	Reason       string  `json:"reason,omitempty"`
	Profile      string  `json:"profile"`
	Generation   uint32  `json:"generation"`
	Record       string  `json:"record"` // base64 of the current commissioning record
}

// UnmarshalJSON makes rtc_eui64 a required nullable member. A missing value is
// not equivalent to an explicit null in this signed schema.
func (b *RegistryBoard) UnmarshalJSON(data []byte) error {
	type plain RegistryBoard
	var decoded plain
	if err := strictjson.Decode(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if value, ok := fields["rtc_model_id"]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return errors.New("rtc_model_id is required and cannot be null")
	}
	if _, ok := fields["rtc_eui64"]; !ok {
		return errors.New("rtc_eui64 is required (use null when the record binds no RTC EUI-64)")
	}
	*b = RegistryBoard(decoded)
	return nil
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
	if signer == nil {
		return nil, errors.New("registry signer is required")
	}
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
	ManufacturerAuthorityID string
	SignerSPKI              string
	Sequence                uint64
	IssuedAt                time.Time
	LedgerHead              string
	boards                  map[boardid.ID]indexedBoard
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
func VerifyRegistry(data []byte, keys *KeySet, expectedAuthorityID string) (*RegistryIndex, error) {
	if !identity.ValidScopeID(expectedAuthorityID) {
		return nil, errors.New("expected manufacturer authority id is required and must be a valid scope id")
	}
	if keys == nil {
		return nil, errors.New("registry verification keys are required")
	}
	if len(data) > registryMaxBytes {
		return nil, fmt.Errorf("registry is %d bytes, limit %d", len(data), registryMaxBytes)
	}
	var env registryEnvelope
	if err := strictjson.Decode(data, &env); err != nil {
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
	var signerSPKI string
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
		if pin := keys.SignerFingerprint(id); signerSPKI == "" || pin < signerSPKI {
			signerSPKI = pin
		}
	}
	if !verified {
		return nil, errors.New("registry carries no signature from a pinned registry key")
	}
	var reg Registry
	if err := strictjson.Decode(payload, &reg); err != nil {
		return nil, fmt.Errorf("decode registry payload: %w", err)
	}
	if reg.ManufacturerAuthorityID != expectedAuthorityID {
		return nil, fmt.Errorf("registry manufacturer authority %q does not match configured authority %q", reg.ManufacturerAuthorityID, expectedAuthorityID)
	}
	index, err := indexRegistry(reg)
	if err != nil {
		return nil, err
	}
	index.SignerSPKI = signerSPKI
	return index, nil
}

func indexRegistry(reg Registry) (*RegistryIndex, error) {
	if !identity.ValidScopeID(reg.ManufacturerAuthorityID) {
		return nil, errors.New("registry manufacturer authority id is required and must be a valid scope id")
	}
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
		ManufacturerAuthorityID: reg.ManufacturerAuthorityID,
		Sequence:                reg.Sequence, IssuedAt: reg.IssuedAt, LedgerHead: reg.LedgerHead,
		boards: make(map[boardid.ID]indexedBoard, len(reg.Boards)),
	}
	ateccs := make(map[[9]byte]struct{}, len(reg.Boards))
	rtcs := make(map[[8]byte]struct{}, len(reg.Boards))
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
		if b.BoardUIDKind != s.BoardUID.KindName() || b.BoardUID != s.BoardUID.Hex() || b.RTCModelID != uint16(s.RTCModel) ||
			b.ATECCSerial != hex.EncodeToString(s.ATECCSerial[:]) || b.Profile != s.Profile.String() || b.Generation != s.Generation {
			return nil, fmt.Errorf("registry board %d: columns disagree with the commissioning record", i)
		}
		rtcBound := s.IdentityFlags&IdentityRTCEUIBound != 0
		if rtcBound {
			if b.RTCEUI64 == nil || *b.RTCEUI64 != hex.EncodeToString(s.RTCEUI64[:]) {
				return nil, fmt.Errorf("registry board %d: RTC EUI-64 disagrees with the commissioning record", i)
			}
		} else if b.RTCEUI64 != nil {
			return nil, fmt.Errorf("registry board %d: RTC EUI-64 must be null when the commissioning record does not bind one", i)
		}
		if _, dup := ix.boards[s.BoardUID]; dup {
			return nil, fmt.Errorf("registry board %d: board %s is listed twice", i, b.BoardUID)
		}
		if _, dup := ateccs[s.ATECCSerial]; dup {
			return nil, fmt.Errorf("registry board %d: ATECC serial %s is listed twice", i, b.ATECCSerial)
		}
		ateccs[s.ATECCSerial] = struct{}{}
		if rtcBound {
			if _, dup := rtcs[s.RTCEUI64]; dup {
				return nil, fmt.Errorf("registry board %d: RTC EUI-64 %s is listed twice", i, *b.RTCEUI64)
			}
			rtcs[s.RTCEUI64] = struct{}{}
		}
		ix.boards[s.BoardUID] = indexedBoard{status: b.Status, reason: b.Reason, fingerprint: record.Fingerprint()}
	}
	return ix, nil
}
