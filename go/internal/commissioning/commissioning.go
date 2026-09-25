// Package commissioning implements the manufacturer commissioning statement:
// the signed record, made once a board has been assembled, attested and (for the
// trusted profile) locked, that binds the board's permanent identifiers to the
// microcontroller fitted to it and to the profile it shipped under.
//
// It complements internal/attestation. The slot-14 attestation proves the board
// and its secure element are original hardware; it says nothing about the
// microcontroller, and whether a unit is locked is a microcontroller property.
// The commissioning record closes that gap, and the session proof in proof.go
// shows the commissioned microcontroller is the one on the connection.
//
// docs/COMMISSIONING.md is the normative description of every layout here.
package commissioning

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/ptudor/navlistener/internal/boardid"
	"io"
	"math/big"
)

const (
	// VersionV1 is the current pre-release typed-identity contract.
	VersionV1 = byte(0x01)

	// StatementSize is the fixed v1 statement length.
	StatementSize = 180
	// KeyIDSize is the signer hint carried beside the signature.
	KeyIDSize = 8
	// SignatureSize is a fixed-width P-256 R||S.
	SignatureSize = 64
	// RecordSize is statement || signer key id || signature.
	RecordSize = StatementSize + KeyIDSize + SignatureSize
)

var statementDomain = []byte("MFG-COMMISSION-v1")

// Profile is the track a board was commissioned to follow.
type Profile uint8

const (
	// ProfileTrusted boards were locked at the bench and hold a microcontroller key.
	ProfileTrusted Profile = 1
	// ProfileOpen boards are original hardware that is never locked.
	ProfileOpen Profile = 2
	// ProfileTest boards are bench and development units outside the production fleet.
	ProfileTest Profile = 3
)

func (p Profile) String() string {
	switch p {
	case ProfileTrusted:
		return "trusted"
	case ProfileOpen:
		return "open"
	case ProfileTest:
		return "test"
	default:
		return fmt.Sprintf("profile(%d)", uint8(p))
	}
}

// ParseProfile accepts the names String produces.
func ParseProfile(s string) (Profile, error) {
	switch s {
	case "trusted":
		return ProfileTrusted, nil
	case "open":
		return ProfileOpen, nil
	case "test":
		return ProfileTest, nil
	default:
		return 0, fmt.Errorf("commissioning profile %q is unknown (want trusted, open or test)", s)
	}
}

// Product names the product line a board was built as. One manufacturer key
// signs for every product line the manufacturer makes, so the statement says
// which one it describes and a verifier honours only the products it serves.
type Product uint16

// ProductObserver is the NavListen GNSS observer. Zero is reserved; other
// values belong to the manufacturer's other product lines.
const ProductObserver Product = 1

// Identity flags describe the RTC fitted at commissioning, recorded for the
// unit's history. Trust rests on the board UID (the public identity) and the
// ATECC (the private one): no verifier compares the recorded RTC with the live
// board or with a product policy, so replacing an RTC leaves the record valid.
// All unassigned bits are reserved.
const (
	IdentityRTCEUIRecorded uint16 = 1 << 0
	IdentityRTCPresent     uint16 = 1 << 1
	identityKnown                 = IdentityRTCEUIRecorded | IdentityRTCPresent
)

// RTCModel names the recorded RTC part. Zero records no RTC. Only the
// MCP79412 carries a factory EUI-64.
type RTCModel uint16

const (
	RTCModelNone     RTCModel = 0
	RTCModelMCP79412 RTCModel = 1
	RTCModelDS3231   RTCModel = 2
	RTCModelMAX31328 RTCModel = 3
)

// MCUFamily names the microcontroller family the statement describes.
type MCUFamily uint8

// MCUESP32S3 is the ESP32-S3 on every current observer board.
const MCUESP32S3 MCUFamily = 1

// MCUKeyAlg names how the microcontroller key signs a session proof.
type MCUKeyAlg uint8

const (
	// MCUKeyNone marks a board with no microcontroller key (open boards).
	MCUKeyNone MCUKeyAlg = 0
	// MCUKeyRSA3072PSS is a 3072-bit RSA key held by the ESP32-S3 Digital
	// Signature peripheral, signing RSASSA-PSS with SHA-256.
	MCUKeyRSA3072PSS MCUKeyAlg = 1
)

// Security bits record the lock state observed at commissioning.
const (
	SecSecureBoot             uint16 = 1 << 0 // Secure Boot v2 enabled
	SecFlashEncryptionRelease uint16 = 1 << 1 // flash encryption in release mode
	SecJTAGDisabled           uint16 = 1 << 2 // pad and USB JTAG permanently disabled
	SecSecureDownload         uint16 = 1 << 3 // ROM download mode secure or disabled
	SecMCUKeyProtected        uint16 = 1 << 4 // microcontroller key block burned and read-protected

	// SecTrusted is the complete set a trusted statement must carry.
	SecTrusted = SecSecureBoot | SecFlashEncryptionRelease | SecJTAGDisabled | SecSecureDownload | SecMCUKeyProtected
	secKnown   = SecTrusted
)

// Statement is the decoded v1 commissioning statement.
type Statement struct {
	Profile        Profile
	MCUFamily      MCUFamily
	MCUKeyAlg      MCUKeyAlg
	Product        Product
	BoardRevision  uint16
	Security       uint16
	IdentityFlags  uint16
	RTCModel       RTCModel
	Generation     uint32 // 1 for the first commissioning; rises by one each time the board is commissioned again
	CommissionedAt uint64 // Unix seconds, UTC
	BoardUID       boardid.ID
	ATECCSerial    [9]byte
	RTCEUI64       [8]byte
	MCUMAC         [6]byte  // factory base MAC: a name for the part, never a proof
	MCUKeySHA256   [32]byte // SHA-256 of the key's DER SubjectPublicKeyInfo
	SecureBootKeys [32]byte // SHA-256 over the three Secure Boot key digests in slot order
	Attestation    [32]byte // SHA-256 of the board's 72-byte slot-14 record
}

// Validate enforces the rules every signer and verifier applies, so a
// statement that is signed is also one that is internally consistent.
func (s Statement) Validate() error {
	switch s.Profile {
	case ProfileTrusted, ProfileOpen, ProfileTest:
	default:
		return fmt.Errorf("commissioning profile %d is unknown", uint8(s.Profile))
	}
	if s.MCUFamily != MCUESP32S3 {
		return fmt.Errorf("microcontroller family %d is unknown", uint8(s.MCUFamily))
	}
	if s.MCUKeyAlg != MCUKeyNone && s.MCUKeyAlg != MCUKeyRSA3072PSS {
		return fmt.Errorf("microcontroller key algorithm %d is unknown", uint8(s.MCUKeyAlg))
	}
	if s.Product == 0 {
		return errors.New("product is required")
	}
	if s.Security&^secKnown != 0 {
		return fmt.Errorf("security bits 0x%04x include reserved bits", s.Security)
	}
	if s.IdentityFlags&^identityKnown != 0 {
		return fmt.Errorf("identity flags 0x%04x include reserved bits", s.IdentityFlags)
	}
	rtcPresent := s.IdentityFlags&IdentityRTCPresent != 0
	rtcRecorded := s.IdentityFlags&IdentityRTCEUIRecorded != 0
	if rtcRecorded && !rtcPresent {
		return errors.New("a recorded RTC EUI-64 requires an RTC declaration")
	}
	if !rtcPresent {
		if s.RTCModel != RTCModelNone {
			return errors.New("RTC model must be zero when no RTC is declared")
		}
	} else if s.RTCModel != RTCModelMCP79412 && s.RTCModel != RTCModelDS3231 && s.RTCModel != RTCModelMAX31328 {
		return fmt.Errorf("RTC model %d is unknown", uint16(s.RTCModel))
	}
	if rtcRecorded {
		if s.RTCModel != RTCModelMCP79412 {
			return errors.New("RTC model has no factory EUI-64")
		}
		if blank(s.RTCEUI64[:]) {
			return errors.New("RTC EUI-64 is blank or erased")
		}
	} else if s.RTCEUI64 != ([8]byte{}) {
		return errors.New("RTC EUI-64 must be zero when it is not recorded")
	}
	if s.Generation == 0 {
		return errors.New("commissioning generation starts at 1")
	}
	if s.CommissionedAt == 0 {
		return errors.New("commissioning time is required")
	}
	for name, id := range map[string][]byte{
		"ATECC serial":        s.ATECCSerial[:],
		"microcontroller MAC": s.MCUMAC[:],
	} {
		if blank(id) {
			return fmt.Errorf("%s is blank or erased", name)
		}
	}
	if err := s.BoardUID.Validate(); err != nil {
		return err
	}
	hasKey := s.MCUKeyAlg != MCUKeyNone
	if hasKey == (s.MCUKeySHA256 == [32]byte{}) {
		return errors.New("microcontroller key digest must be present exactly when a key algorithm is named")
	}
	if !hasKey && s.Security&SecMCUKeyProtected != 0 {
		return errors.New("security bits claim a protected microcontroller key that the statement does not name")
	}
	if (s.Security&SecSecureBoot != 0) == (s.SecureBootKeys == [32]byte{}) {
		return errors.New("Secure Boot key digest must be present exactly when Secure Boot is enabled")
	}
	switch s.Profile {
	case ProfileTrusted:
		if !hasKey || s.Security != SecTrusted {
			return fmt.Errorf("trusted profile requires a microcontroller key and security bits 0x%04x, got 0x%04x", SecTrusted, s.Security)
		}
	case ProfileOpen:
		if hasKey || s.Security != 0 {
			return errors.New("open profile describes a board that is never locked: no microcontroller key and no security bits")
		}
	}
	if s.Profile != ProfileTest && s.Attestation == ([32]byte{}) {
		return errors.New("a production board is attested before it is commissioned")
	}
	return nil
}

// MarshalBinary returns the exact signed layout.
func (s Statement) MarshalBinary() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	b := make([]byte, StatementSize)
	b[0] = VersionV1
	b[1] = byte(s.Profile)
	b[2] = byte(s.MCUFamily)
	b[3] = byte(s.MCUKeyAlg)
	binary.BigEndian.PutUint16(b[4:], uint16(s.Product))
	binary.BigEndian.PutUint16(b[6:], s.BoardRevision)
	binary.BigEndian.PutUint16(b[8:], s.Security)
	binary.BigEndian.PutUint16(b[10:], s.IdentityFlags)
	binary.BigEndian.PutUint16(b[12:], uint16(s.RTCModel))
	binary.BigEndian.PutUint32(b[14:], s.Generation)
	binary.BigEndian.PutUint64(b[18:], s.CommissionedAt)
	copy(b[26:61], s.BoardUID[:])
	copy(b[61:70], s.ATECCSerial[:])
	copy(b[70:78], s.RTCEUI64[:])
	copy(b[78:84], s.MCUMAC[:])
	copy(b[84:116], s.MCUKeySHA256[:])
	copy(b[116:148], s.SecureBootKeys[:])
	copy(b[148:180], s.Attestation[:])
	return b, nil
}

// ParseStatement decodes and validates the fixed layout.
func ParseStatement(b []byte) (Statement, error) {
	if len(b) != StatementSize {
		return Statement{}, fmt.Errorf("commissioning statement is %d bytes, want %d", len(b), StatementSize)
	}
	if b[0] != VersionV1 {
		return Statement{}, fmt.Errorf("commissioning statement version 0x%02x is unsupported", b[0])
	}
	s := Statement{
		Profile:        Profile(b[1]),
		MCUFamily:      MCUFamily(b[2]),
		MCUKeyAlg:      MCUKeyAlg(b[3]),
		Product:        Product(binary.BigEndian.Uint16(b[4:])),
		BoardRevision:  binary.BigEndian.Uint16(b[6:]),
		Security:       binary.BigEndian.Uint16(b[8:]),
		IdentityFlags:  binary.BigEndian.Uint16(b[10:]),
		RTCModel:       RTCModel(binary.BigEndian.Uint16(b[12:])),
		Generation:     binary.BigEndian.Uint32(b[14:]),
		CommissionedAt: binary.BigEndian.Uint64(b[18:]),
	}
	copy(s.BoardUID[:], b[26:61])
	copy(s.ATECCSerial[:], b[61:70])
	copy(s.RTCEUI64[:], b[70:78])
	copy(s.MCUMAC[:], b[78:84])
	copy(s.MCUKeySHA256[:], b[84:116])
	copy(s.SecureBootKeys[:], b[116:148])
	copy(s.Attestation[:], b[148:180])
	if err := s.Validate(); err != nil {
		return Statement{}, err
	}
	return s, nil
}

// Digest is the domain-separated value the manufacturer key signs.
func (s Statement) Digest() ([32]byte, error) {
	b, err := s.MarshalBinary()
	if err != nil {
		return [32]byte{}, err
	}
	return digest(statementDomain, b), nil
}

// ObserverID includes the immutable source kind and the complete UID value.
func (s Statement) ObserverID() string { return s.BoardUID.ObserverID() }
func ObserverID(id boardid.ID) string  { return id.ObserverID() }

// Record is the signed form that is stored, carried by the device and
// published in the registry.
type Record [RecordSize]byte

// ParseRecord checks the shape and the embedded statement. The signature is
// checked separately by KeySet.Verify.
func ParseRecord(b []byte) (Record, error) {
	var r Record
	if len(b) != RecordSize {
		return r, fmt.Errorf("commissioning record is %d bytes, want %d", len(b), RecordSize)
	}
	copy(r[:], b)
	if _, err := ParseStatement(r[:StatementSize]); err != nil {
		return Record{}, err
	}
	return r, nil
}

// Statement decodes the embedded statement.
func (r Record) Statement() (Statement, error) { return ParseStatement(r[:StatementSize]) }

// KeyID is the signer hint: the first eight bytes of the SHA-256 of the
// signer's DER SubjectPublicKeyInfo.
func (r Record) KeyID() (id [KeyIDSize]byte) {
	copy(id[:], r[StatementSize:StatementSize+KeyIDSize])
	return id
}

// Fingerprint identifies this exact record, signature included.
func (r Record) Fingerprint() [32]byte { return sha256.Sum256(r[:]) }

// Assemble joins a statement with a signature produced elsewhere, such as in
// a signing ceremony. It does not verify the signature.
func Assemble(s Statement, keyID [KeyIDSize]byte, signature [SignatureSize]byte) (Record, error) {
	b, err := s.MarshalBinary()
	if err != nil {
		return Record{}, err
	}
	var r Record
	copy(r[:], b)
	copy(r[StatementSize:], keyID[:])
	copy(r[StatementSize+KeyIDSize:], signature[:])
	return r, nil
}

// Signer produces a fixed-width P-256 R||S over a 32-byte digest. The
// production manufacturer key lives in a hardware signer and is reached
// through a ceremony; NewKeySigner exists for test hierarchies.
type Signer interface {
	KeyID() [KeyIDSize]byte
	SignDigest(digest [32]byte) ([SignatureSize]byte, error)
}

type keySigner struct {
	key *ecdsa.PrivateKey
	id  [KeyIDSize]byte
	rng io.Reader
}

// NewKeySigner signs with an in-memory P-256 key. rng is injectable for
// deterministic tests; nil uses crypto/rand.Reader.
func NewKeySigner(key *ecdsa.PrivateKey, rng io.Reader) (Signer, error) {
	if key == nil || key.D == nil {
		return nil, errors.New("signing key is required")
	}
	id, err := KeyID(&key.PublicKey)
	if err != nil {
		return nil, err
	}
	if rng == nil {
		rng = rand.Reader
	}
	return keySigner{key: key, id: id, rng: rng}, nil
}

func (k keySigner) KeyID() [KeyIDSize]byte { return k.id }

func (k keySigner) SignDigest(d [32]byte) ([SignatureSize]byte, error) {
	var out [SignatureSize]byte
	r, s, err := ecdsa.Sign(k.rng, k.key, d[:])
	if err != nil {
		return out, fmt.Errorf("sign: %w", err)
	}
	r.FillBytes(out[:32])
	s.FillBytes(out[32:])
	return out, nil
}

// Sign produces a record with signer.
func Sign(s Statement, signer Signer) (Record, error) {
	d, err := s.Digest()
	if err != nil {
		return Record{}, err
	}
	sig, err := signer.SignDigest(d)
	if err != nil {
		return Record{}, err
	}
	return Assemble(s, signer.KeyID(), sig)
}

// KeyID derives the signer hint for a P-256 public key.
func KeyID(pub *ecdsa.PublicKey) ([KeyIDSize]byte, error) {
	var id [KeyIDSize]byte
	if err := validatePublicKey(pub); err != nil {
		return id, err
	}
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return id, fmt.Errorf("encode public key: %w", err)
	}
	sum := sha256.Sum256(der)
	copy(id[:], sum[:KeyIDSize])
	return id, nil
}

// KeySet is a pinned set of P-256 verification keys. More than one key is
// normal over a fleet's life: a replaced signer adds a key, and records signed
// by the earlier one stay valid for as long as its key stays pinned.
type KeySet struct {
	keys map[[KeyIDSize]byte]*ecdsa.PublicKey
}

// NewKeySet pins keys. An empty set is rejected: a verifier with no trust
// root must fail at configuration time, not quietly verify nothing.
func NewKeySet(keys ...*ecdsa.PublicKey) (*KeySet, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one verification key is required")
	}
	ks := &KeySet{keys: make(map[[KeyIDSize]byte]*ecdsa.PublicKey, len(keys))}
	for _, k := range keys {
		id, err := KeyID(k)
		if err != nil {
			return nil, err
		}
		if _, dup := ks.keys[id]; dup {
			return nil, fmt.Errorf("verification key %x is listed twice", id)
		}
		ks.keys[id] = k
	}
	return ks, nil
}

// IDs lists the pinned key ids in hex, for logs and status output.
func (ks *KeySet) IDs() []string {
	out := make([]string, 0, len(ks.keys))
	for id := range ks.keys {
		out = append(out, hex.EncodeToString(id[:]))
	}
	return out
}

// Verify checks a commissioning record against the pinned manufacturer keys.
func (ks *KeySet) Verify(r Record) (Statement, error) {
	s, err := r.Statement()
	if err != nil {
		return Statement{}, err
	}
	d, err := s.Digest()
	if err != nil {
		return Statement{}, err
	}
	if err := ks.verifyDigest(r.KeyID(), d, r[StatementSize+KeyIDSize:]); err != nil {
		return Statement{}, fmt.Errorf("commissioning record: %w", err)
	}
	return s, nil
}

func (ks *KeySet) verifyDigest(id [KeyIDSize]byte, d [32]byte, sig []byte) error {
	key, ok := ks.keys[id]
	if !ok {
		return fmt.Errorf("signer %x is not a pinned key", id)
	}
	if len(sig) != SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), SignatureSize)
	}
	r := new(big.Int).SetBytes(sig[:32])
	s := new(big.Int).SetBytes(sig[32:])
	n := key.Params().N
	if r.Sign() <= 0 || s.Sign() <= 0 || r.Cmp(n) >= 0 || s.Cmp(n) >= 0 {
		return errors.New("signature scalar is out of range")
	}
	if !ecdsa.Verify(key, d[:], r, s) {
		return errors.New("signature verification failed")
	}
	return nil
}

func digest(domain, body []byte) [32]byte {
	h := sha256.New()
	h.Write(domain)
	h.Write(body)
	var out [32]byte
	h.Sum(out[:0])
	return out
}

func blank(b []byte) bool {
	allZero, allFF := true, true
	for _, v := range b {
		allZero = allZero && v == 0
		allFF = allFF && v == 0xff
	}
	return allZero || allFF
}

func validatePublicKey(key *ecdsa.PublicKey) error {
	if key == nil || key.Curve == nil || key.X == nil || key.Y == nil {
		return errors.New("public key is required")
	}
	if key.Curve != elliptic.P256() {
		return fmt.Errorf("key curve %q: want P-256", key.Curve.Params().Name)
	}
	// ECDH rejects the point at infinity and any point off the curve.
	if _, err := key.ECDH(); err != nil {
		return fmt.Errorf("public key is not a valid P-256 point: %w", err)
	}
	return nil
}
