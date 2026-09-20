package commissioning

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// mcuKey is generated once: a 3072-bit RSA key takes long enough that every
// test sharing it matters.
var mcuKey = sync.OnceValue(func() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, mcuKeyBits)
	if err != nil {
		panic(err)
	}
	return k
})

const testManufacturerAuthority = "test-manufacturer"

func mcuKeyDER(t *testing.T, key *rsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func testSigner(t *testing.T) (Signer, *KeySet) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewKeySigner(k, nil)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewKeySet(&k.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return signer, keys
}

func trustedStatement(t *testing.T) Statement {
	t.Helper()
	return Statement{
		Profile: ProfileTrusted, MCUFamily: MCUESP32S3, MCUKeyAlg: MCUKeyRSA3072PSS,
		Product: ProductObserver, BoardRevision: 0x0102, Security: SecTrusted,
		IdentityFlags: IdentityRTCPresent | IdentityRTCEUIBound, RTCModel: RTCModelMCP79412,
		Generation: 1, CommissionedAt: 1789646400,
		ATECCSerial:    [9]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x11},
		RTCEUI64:       [8]byte{0x00, 0x04, 0xa3, 0x12, 0x34, 0x56, 0x78, 0x90},
		BoardEUI64:     [8]byte{0x00, 0x04, 0xa3, 0xaa, 0xbb, 0xcc, 0xdd, 0xee},
		MCUMAC:         [6]byte{0x34, 0x85, 0x18, 0x01, 0x02, 0x03},
		MCUKeySHA256:   sha256.Sum256(mcuKeyDER(t, mcuKey())),
		SecureBootKeys: sha256.Sum256([]byte("secure boot key digests")),
		Attestation:    sha256.Sum256([]byte("slot 14 record")),
	}
}

func openStatement(t *testing.T) Statement {
	s := trustedStatement(t)
	s.Profile, s.MCUKeyAlg, s.Security = ProfileOpen, MCUKeyNone, 0
	s.MCUKeySHA256, s.SecureBootKeys = [32]byte{}, [32]byte{}
	return s
}

func prove(t *testing.T, key *rsa.PrivateKey, exported []byte, record Record) []byte {
	t.Helper()
	d, err := ProofDigest(exported, record)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := rsa.SignPSS(rand.Reader, key, crypto.SHA256, d[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	if err != nil {
		t.Fatal(err)
	}
	return sig
}

func exportedFor(label string) []byte {
	sum := sha256.Sum256([]byte(label))
	return sum[:]
}

func TestStatementRoundTripAndLayout(t *testing.T) {
	s := trustedStatement(t)
	b, err := s.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != StatementSize || b[0] != VersionV1 || b[1] != 1 || b[3] != 1 {
		t.Fatalf("layout header = % x (len %d)", b[:4], len(b))
	}
	if got := hex.EncodeToString(b[4:6]); got != "0001" {
		t.Fatalf("product at offset 4 = %s", got)
	}
	if got := hex.EncodeToString(b[26:34]); got != "0004a3aabbccddee" {
		t.Fatalf("board EUI-64 at offset 26 = %s", got)
	}
	if got := hex.EncodeToString(b[43:51]); got != "0004a31234567890" {
		t.Fatalf("RTC EUI-64 at offset 43 = %s", got)
	}
	back, err := ParseStatement(b)
	if err != nil {
		t.Fatal(err)
	}
	if back != s {
		t.Fatalf("round trip changed the statement:\n got %+v\nwant %+v", back, s)
	}
	if s.ObserverID() != "00-04-a3-aa-bb-cc-dd-ee" {
		t.Fatalf("observer id = %q", s.ObserverID())
	}
}

func TestStatementValidation(t *testing.T) {
	cases := map[string]func(*Statement){
		"unknown profile":            func(s *Statement) { s.Profile = 9 },
		"unknown family":             func(s *Statement) { s.MCUFamily = 2 },
		"no product":                 func(s *Statement) { s.Product = 0 },
		"unknown key algorithm":      func(s *Statement) { s.MCUKeyAlg = 7 },
		"reserved security bit":      func(s *Statement) { s.Security |= 1 << 9 },
		"reserved identity bit":      func(s *Statement) { s.IdentityFlags |= 1 << 9 },
		"bound RTC not present":      func(s *Statement) { s.IdentityFlags = IdentityRTCEUIBound },
		"unknown RTC model":          func(s *Statement) { s.RTCModel = 99 },
		"bound blank RTC":            func(s *Statement) { s.RTCEUI64 = [8]byte{} },
		"zero generation":            func(s *Statement) { s.Generation = 0 },
		"zero time":                  func(s *Statement) { s.CommissionedAt = 0 },
		"erased ATECC serial":        func(s *Statement) { s.ATECCSerial = [9]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff} },
		"blank board EUI":            func(s *Statement) { s.BoardEUI64 = [8]byte{} },
		"blank MAC":                  func(s *Statement) { s.MCUMAC = [6]byte{} },
		"trusted without a key":      func(s *Statement) { s.MCUKeyAlg, s.MCUKeySHA256 = MCUKeyNone, [32]byte{} },
		"trusted but not locked":     func(s *Statement) { s.Security &^= SecFlashEncryptionRelease },
		"key named without a digest": func(s *Statement) { s.MCUKeySHA256 = [32]byte{} },
		"secure boot without keys":   func(s *Statement) { s.SecureBootKeys = [32]byte{} },
		"production unattested":      func(s *Statement) { s.Attestation = [32]byte{} },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := trustedStatement(t)
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatal("invalid statement validated")
			}
		})
	}
	t.Run("open board may not be locked", func(t *testing.T) {
		s := openStatement(t)
		s.Security, s.SecureBootKeys = SecSecureBoot, sha256.Sum256([]byte("k"))
		if err := s.Validate(); err == nil {
			t.Fatal("locked open statement validated")
		}
	})
	t.Run("test board may be unattested and keyless", func(t *testing.T) {
		s := openStatement(t)
		s.Profile, s.Attestation = ProfileTest, [32]byte{}
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("RTC declarations", func(t *testing.T) {
		for name, mutate := range map[string]func(*Statement){
			"none": func(s *Statement) {
				s.IdentityFlags, s.RTCModel, s.RTCEUI64 = 0, RTCModelNone, [8]byte{}
			},
			"model only": func(s *Statement) {
				s.IdentityFlags, s.RTCModel, s.RTCEUI64 = IdentityRTCPresent, RTCModelMCP79412, [8]byte{}
			},
			"model and EUI": func(s *Statement) {},
		} {
			t.Run(name, func(t *testing.T) {
				s := trustedStatement(t)
				mutate(&s)
				if err := s.Validate(); err != nil {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("unbound RTC bytes are not ignored", func(t *testing.T) {
		s := trustedStatement(t)
		s.IdentityFlags = IdentityRTCPresent
		if err := s.Validate(); err == nil {
			t.Fatal("unbound RTC identifier validated")
		}
	})
}

func TestRecordBindsEveryField(t *testing.T) {
	signer, keys := testSigner(t)
	record, err := Sign(trustedStatement(t), signer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Verify(record); err != nil {
		t.Fatal(err)
	}
	// Flip one bit in every statement byte that leaves the statement valid:
	// the signature must not survive any of them.
	for i := 4; i < StatementSize; i++ {
		changed := record
		changed[i] ^= 0x01
		if _, err := ParseStatement(changed[:StatementSize]); err != nil {
			continue
		}
		if _, err := keys.Verify(changed); err == nil {
			t.Fatalf("record verified after changing statement byte %d", i)
		}
	}
	_, other := testSigner(t)
	if _, err := other.Verify(record); err == nil {
		t.Fatal("record verified under an unrelated key set")
	}
}

func TestSignatureDomainsAreSeparate(t *testing.T) {
	signer, keys := testSigner(t)
	s := trustedStatement(t)
	body, _ := s.MarshalBinary()
	// A signature made over the same bytes under the registry domain must not
	// verify as a commissioning record.
	sig, err := signer.SignDigest(digest(registryDomain, body))
	if err != nil {
		t.Fatal(err)
	}
	record, err := Assemble(s, signer.KeyID(), sig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Verify(record); err == nil {
		t.Fatal("cross-domain signature verified")
	}
}

func TestKeySetRejectsEmptyAndDuplicateKeys(t *testing.T) {
	if _, err := NewKeySet(); err == nil {
		t.Fatal("empty key set accepted")
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if _, err := NewKeySet(&k.PublicKey, &k.PublicKey); err == nil {
		t.Fatal("duplicate key accepted")
	}
	p384, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if _, err := NewKeySet(&p384.PublicKey); err == nil {
		t.Fatal("P-384 key accepted")
	}
}

func TestProofIsBoundToSessionRecordAndKey(t *testing.T) {
	signer, _ := testSigner(t)
	s := trustedStatement(t)
	record, err := Sign(s, signer)
	if err != nil {
		t.Fatal(err)
	}
	key, exported := mcuKey(), exportedFor("session a")
	der, proof := mcuKeyDER(t, key), prove(t, key, exported, record)
	if err := VerifyProof(s, record, der, exported, proof); err != nil {
		t.Fatal(err)
	}
	if err := VerifyProof(s, record, der, exportedFor("session b"), proof); err == nil {
		t.Fatal("proof verified on a different session")
	}
	later := s
	later.Generation = 2
	otherRecord, _ := Sign(later, signer)
	if err := VerifyProof(s, otherRecord, der, exported, proof); err == nil {
		t.Fatal("proof verified against a different record")
	}
	stranger, err := rsa.GenerateKey(rand.Reader, mcuKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyProof(s, record, mcuKeyDER(t, stranger), exported, prove(t, stranger, exported, record)); err == nil {
		t.Fatal("proof verified under a key the record does not name")
	}
	if err := VerifyProof(s, record, der, exported[:16], proof); err == nil {
		t.Fatal("short exported keying material accepted")
	}
}

func TestProofRejectsUndersizedKey(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := testSigner(t)
	s := trustedStatement(t)
	der := mcuKeyDER(t, small)
	s.MCUKeySHA256 = sha256.Sum256(der)
	record, _ := Sign(s, signer)
	exported := exportedFor("session")
	if err := VerifyProof(s, record, der, exported, prove(t, small, exported, record)); err == nil {
		t.Fatal("2048-bit microcontroller key accepted")
	}
}

func TestEvidenceRoundTripAndMalformed(t *testing.T) {
	signer, _ := testSigner(t)
	record, _ := Sign(trustedStatement(t), signer)
	e := Evidence{Record: record, MCUKey: mcuKeyDER(t, mcuKey()), Proof: prove(t, mcuKey(), exportedFor("s"), record)}
	b, err := e.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseEvidence(b)
	if err != nil {
		t.Fatal(err)
	}
	if back.Record != e.Record || string(back.MCUKey) != string(e.MCUKey) || string(back.Proof) != string(e.Proof) {
		t.Fatal("round trip changed the evidence")
	}
	openRecord, _ := Sign(openStatement(t), signer)
	keyless, err := Evidence{Record: openRecord}.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseEvidence(keyless); err != nil {
		t.Fatal(err)
	}

	for n := 0; n < len(b); n++ {
		if _, err := ParseEvidence(b[:n]); err == nil {
			t.Fatalf("truncated evidence of %d bytes parsed", n)
		}
	}
	if _, err := ParseEvidence(append(append([]byte(nil), b...), 0)); err == nil {
		t.Fatal("trailing byte accepted")
	}
	wrongVersion := append([]byte(nil), b...)
	wrongVersion[0] = 2
	if _, err := ParseEvidence(wrongVersion); err == nil {
		t.Fatal("unknown evidence version accepted")
	}
	if _, err := (Evidence{Record: record, MCUKey: e.MCUKey}).MarshalBinary(); err == nil {
		t.Fatal("key without proof encoded")
	}
	if _, err := ParseEvidence(make([]byte, EvidenceMaxLen+1)); err == nil {
		t.Fatal("oversized evidence accepted")
	}
}

func registryFor(t *testing.T, sequence uint64, status string, records ...Record) Registry {
	t.Helper()
	reg := Registry{ManufacturerAuthorityID: testManufacturerAuthority, Sequence: sequence, IssuedAt: time.Unix(1789650000, 0).UTC(), LedgerHead: strings.Repeat("ab", 32)}
	for _, r := range records {
		s, err := r.Statement()
		if err != nil {
			t.Fatal(err)
		}
		var rtcEUI *string
		if s.IdentityFlags&IdentityRTCEUIBound != 0 {
			value := hex.EncodeToString(s.RTCEUI64[:])
			rtcEUI = &value
		}
		reg.Boards = append(reg.Boards, RegistryBoard{
			BoardEUI64: hex.EncodeToString(s.BoardEUI64[:]), RTCModelID: uint16(s.RTCModel), RTCEUI64: rtcEUI,
			ATECCSerial: hex.EncodeToString(s.ATECCSerial[:]), Status: status, Profile: s.Profile.String(),
			Generation: s.Generation, Record: base64.StdEncoding.EncodeToString(r[:]),
		})
	}
	return reg
}

func TestRegistrySignAndVerify(t *testing.T) {
	mfg, _ := testSigner(t)
	ops, opsKeys := testSigner(t)
	record, _ := Sign(trustedStatement(t), mfg)
	data, err := SignRegistry(registryFor(t, 7, StatusActive, record), ops)
	if err != nil {
		t.Fatal(err)
	}
	ix, err := VerifyRegistry(data, opsKeys, testManufacturerAuthority)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Sequence != 7 || ix.Len() != 1 {
		t.Fatalf("index = sequence %d, %d boards", ix.Sequence, ix.Len())
	}
	if _, err := VerifyRegistry(data, opsKeys, "other-manufacturer"); err == nil {
		t.Fatal("registry verified for a different manufacturer authority")
	}
	_, strangers := testSigner(t)
	if _, err := VerifyRegistry(data, strangers, testManufacturerAuthority); err == nil {
		t.Fatal("registry verified with no pinned signer")
	}
	tampered := []byte(strings.Replace(string(data), `"payload":"ey`, `"payload":"eY`, 1))
	if _, err := VerifyRegistry(tampered, opsKeys, testManufacturerAuthority); err == nil {
		t.Fatal("tampered registry verified")
	}
	if _, err := VerifyRegistry(append(append([]byte(nil), data...), []byte(`{}`)...), opsKeys, testManufacturerAuthority); err == nil {
		t.Fatal("registry envelope with trailing content verified")
	}

	t.Run("duplicate board", func(t *testing.T) {
		if _, err := SignRegistry(registryFor(t, 1, StatusActive, record, record), ops); err == nil {
			t.Fatal("duplicate board signed")
		}
	})
	t.Run("duplicate ATECC", func(t *testing.T) {
		other := trustedStatement(t)
		other.BoardEUI64[7] ^= 1
		other.RTCEUI64[7] ^= 1
		otherRecord, err := Sign(other, mfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SignRegistry(registryFor(t, 1, StatusActive, record, otherRecord), ops); err == nil {
			t.Fatal("duplicate ATECC serial signed")
		}
	})
	t.Run("duplicate bound RTC", func(t *testing.T) {
		other := trustedStatement(t)
		other.BoardEUI64[7] ^= 1
		other.ATECCSerial[8] ^= 1
		otherRecord, err := Sign(other, mfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := SignRegistry(registryFor(t, 1, StatusActive, record, otherRecord), ops); err == nil {
			t.Fatal("duplicate RTC EUI-64 signed")
		}
	})
	t.Run("model-only RTC is explicitly null", func(t *testing.T) {
		modelOnly := trustedStatement(t)
		modelOnly.IdentityFlags = IdentityRTCPresent
		modelOnly.RTCEUI64 = [8]byte{}
		modelOnlyRecord, err := Sign(modelOnly, mfg)
		if err != nil {
			t.Fatal(err)
		}
		reg := registryFor(t, 1, StatusActive, modelOnlyRecord)
		if reg.Boards[0].RTCEUI64 != nil {
			t.Fatal("model-only RTC encoded a registry instance identity")
		}
		if _, err := SignRegistry(reg, ops); err != nil {
			t.Fatal(err)
		}
		value := "0004a31234567890"
		reg.Boards[0].RTCEUI64 = &value
		if _, err := SignRegistry(reg, ops); err == nil {
			t.Fatal("registry added an RTC binding absent from the record")
		}
	})
	t.Run("nullable RTC member is required", func(t *testing.T) {
		var row RegistryBoard
		if err := json.Unmarshal([]byte(`{"board_eui64":"00"}`), &row); err == nil {
			t.Fatal("registry row omitted rtc_eui64")
		}
		if err := json.Unmarshal([]byte(`{"rtc_model_id":0,"rtc_eui64":null}`), &row); err != nil {
			t.Fatalf("explicit null RTC rejected: %v", err)
		}
	})
	t.Run("columns must match the record", func(t *testing.T) {
		reg := registryFor(t, 1, StatusActive, record)
		reg.Boards[0].Profile = "open"
		if _, err := SignRegistry(reg, ops); err == nil {
			t.Fatal("mismatched column signed")
		}
	})
	t.Run("unknown status", func(t *testing.T) {
		if _, err := SignRegistry(registryFor(t, 1, "lost", record), ops); err == nil {
			t.Fatal("unknown status signed")
		}
	})
}

func TestEvaluate(t *testing.T) {
	mfg, mfgKeys := testSigner(t)
	ops, opsKeys := testSigner(t)
	s := trustedStatement(t)
	record, _ := Sign(s, mfg)
	exported := exportedFor("session")
	evidence := Evidence{Record: record, MCUKey: mcuKeyDER(t, mcuKey()), Proof: prove(t, mcuKey(), exported, record)}
	observer := s.ObserverID()

	newVerifier := func(t *testing.T, requireEntry bool, reg *Registry) *Verifier {
		t.Helper()
		v, err := newTestVerifier(testManufacturerAuthority, mfgKeys)
		if err != nil {
			t.Fatal(err)
		}
		if reg != nil {
			if err := v.UseRegistry(opsKeys, requireEntry); err != nil {
				t.Fatal(err)
			}
			data, err := SignRegistry(*reg, ops)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := v.LoadRegistry(data); err != nil {
				t.Fatal(err)
			}
		}
		return v
	}
	expectReason := func(t *testing.T, result Result, err error, reason string) {
		t.Helper()
		var rejection *Rejection
		if !errors.As(err, &rejection) || rejection.Reason != reason {
			t.Fatalf("error = %v, want rejection %q", err, reason)
		}
		if result.Trust != identity.HardwareTrustNone {
			t.Fatalf("rejected evidence left trust %q", result.Trust)
		}
	}

	t.Run("trusted", func(t *testing.T) {
		result, err := newVerifier(t, false, nil).Evaluate(observer, evidence, exported)
		if err != nil || result.Trust != identity.HardwareTrustTrusted || result.Fingerprint != record.Fingerprint() {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})
	t.Run("trusted and current in the registry", func(t *testing.T) {
		reg := registryFor(t, 1, StatusActive, record)
		result, err := newVerifier(t, true, &reg).Evaluate(observer, evidence, exported)
		if err != nil || result.Trust != identity.HardwareTrustTrusted {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})
	t.Run("record for another product line", func(t *testing.T) {
		other := s
		other.Product = 2
		otherRecord, _ := Sign(other, mfg)
		e := Evidence{Record: otherRecord, MCUKey: evidence.MCUKey, Proof: prove(t, mcuKey(), exported, otherRecord)}
		result, err := newVerifier(t, false, nil).Evaluate(observer, e, exported)
		expectReason(t, result, err, ReasonProduct)
	})
	t.Run("record for another observer", func(t *testing.T) {
		result, err := newVerifier(t, false, nil).Evaluate("00-04-a3-00-00-00-00-01", evidence, exported)
		expectReason(t, result, err, ReasonIdentity)
	})
	t.Run("unknown manufacturer", func(t *testing.T) {
		stranger, _ := testSigner(t)
		forged, _ := Sign(s, stranger)
		e := Evidence{Record: forged, MCUKey: evidence.MCUKey, Proof: prove(t, mcuKey(), exported, forged)}
		result, err := newVerifier(t, false, nil).Evaluate(observer, e, exported)
		expectReason(t, result, err, ReasonSignature)
	})
	t.Run("revoked", func(t *testing.T) {
		reg := registryFor(t, 1, StatusRevoked, record)
		result, err := newVerifier(t, false, &reg).Evaluate(observer, evidence, exported)
		expectReason(t, result, err, ReasonRevoked)
	})
	t.Run("superseded by a replacement microcontroller", func(t *testing.T) {
		next := s
		next.Generation = 2
		current, _ := Sign(next, mfg)
		reg := registryFor(t, 1, StatusActive, current)
		result, err := newVerifier(t, false, &reg).Evaluate(observer, evidence, exported)
		expectReason(t, result, err, ReasonSuperseded)
	})
	t.Run("unlisted", func(t *testing.T) {
		other := s
		other.BoardEUI64[7], other.RTCEUI64[7] = 0x01, 0x01
		otherRecord, _ := Sign(other, mfg)
		reg := registryFor(t, 1, StatusActive, otherRecord)
		result, err := newVerifier(t, true, &reg).Evaluate(observer, evidence, exported)
		expectReason(t, result, err, ReasonUnlisted)
		// A lagging registry copy that is not required to list every board
		// leaves a valid record standing.
		if result, err := newVerifier(t, false, &reg).Evaluate(observer, evidence, exported); err != nil || result.Trust != identity.HardwareTrustTrusted {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})
	t.Run("trusted record without a proof", func(t *testing.T) {
		result, err := newVerifier(t, false, nil).Evaluate(observer, Evidence{Record: record}, exported)
		expectReason(t, result, err, ReasonProofMissing)
	})
	t.Run("proof from another session", func(t *testing.T) {
		result, err := newVerifier(t, false, nil).Evaluate(observer, evidence, exportedFor("relayed"))
		expectReason(t, result, err, ReasonProof)
	})
	t.Run("open", func(t *testing.T) {
		openRecord, _ := Sign(openStatement(t), mfg)
		result, err := newVerifier(t, false, nil).Evaluate(observer, Evidence{Record: openRecord}, exported)
		if err != nil || result.Trust != identity.HardwareTrustOpen {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})
	t.Run("test", func(t *testing.T) {
		st := openStatement(t)
		st.Profile = ProfileTest
		testRecord, _ := Sign(st, mfg)
		result, err := newVerifier(t, false, nil).Evaluate(observer, Evidence{Record: testRecord}, exported)
		if err != nil || result.Trust != identity.HardwareTrustTest {
			t.Fatalf("result = %+v, %v", result, err)
		}
	})
}

func TestRegistryNeverMovesBackwards(t *testing.T) {
	mfg, mfgKeys := testSigner(t)
	ops, opsKeys := testSigner(t)
	record, _ := Sign(trustedStatement(t), mfg)
	v, _ := newTestVerifier(testManufacturerAuthority, mfgKeys)
	if _, err := v.LoadRegistry(nil); err == nil {
		t.Fatal("registry loaded with no pinned registry keys")
	}
	if err := v.UseRegistry(opsKeys, false); err != nil {
		t.Fatal(err)
	}
	newer, _ := SignRegistry(registryFor(t, 5, StatusRevoked, record), ops)
	older, _ := SignRegistry(registryFor(t, 4, StatusActive, record), ops)
	if _, err := v.LoadRegistry(newer); err != nil {
		t.Fatal(err)
	}
	if _, err := v.LoadRegistry(older); err == nil {
		t.Fatal("older registry replaced a newer one")
	}
	if v.Registry().Sequence != 5 {
		t.Fatalf("loaded sequence = %d", v.Registry().Sequence)
	}
}

func TestWatchRegistryReloadsAndKeepsLastGood(t *testing.T) {
	mfg, mfgKeys := testSigner(t)
	ops, opsKeys := testSigner(t)
	record, _ := Sign(trustedStatement(t), mfg)
	path := filepath.Join(t.TempDir(), "registry.json")
	v, _ := newTestVerifier(testManufacturerAuthority, mfgKeys)
	if err := v.UseRegistry(opsKeys, false); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := v.WatchRegistry(ctx, path, 5*time.Millisecond, nil, nil); err == nil {
		t.Fatal("watch started without a registry file")
	}
	first, _ := SignRegistry(registryFor(t, 1, StatusActive, record), ops)
	if err := os.WriteFile(path, first, 0o600); err != nil {
		t.Fatal(err)
	}
	loads := make(chan error, 16)
	if err := v.WatchRegistry(ctx, path, 5*time.Millisecond, nil, func(_ *RegistryIndex, err error) { loads <- err }); err != nil {
		t.Fatal(err)
	}
	if err := <-loads; err != nil {
		t.Fatal(err)
	}
	waitLoad := func() error {
		t.Helper()
		select {
		case err := <-loads:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("registry was not reloaded")
			return nil
		}
	}
	if err := os.WriteFile(path, []byte("not a registry, and a different length"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitLoad(); err == nil {
		t.Fatal("corrupt registry loaded")
	}
	if v.Registry().Sequence != 1 {
		t.Fatal("corrupt registry displaced the one in force")
	}
	second, _ := SignRegistry(registryFor(t, 2, StatusRevoked, record), ops)
	if err := os.WriteFile(path, second, 0o600); err != nil {
		t.Fatal(err)
	}
	for v.Registry().Sequence != 2 {
		if err := waitLoad(); err != nil {
			continue
		}
	}
}

func TestLoadKeySet(t *testing.T) {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(&k.PublicKey)
	path := filepath.Join(t.TempDir(), "manufacturer.pem")
	pemBytes := "-----BEGIN PUBLIC KEY-----\n" + base64.StdEncoding.EncodeToString(der) + "\n-----END PUBLIC KEY-----\n"
	if err := os.WriteFile(path, []byte(pemBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	keys, err := LoadKeySet([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	want, _ := KeyID(&k.PublicKey)
	if got := keys.IDs(); len(got) != 1 || got[0] != hex.EncodeToString(want[:]) {
		t.Fatalf("key ids = %v", got)
	}
	if _, err := LoadKeySet([]string{filepath.Join(t.TempDir(), "absent.pem")}); err == nil {
		t.Fatal("missing key file accepted")
	}
}

func TestRecheckFollowsTheRegistry(t *testing.T) {
	mfg, mfgKeys := testSigner(t)
	ops, opsKeys := testSigner(t)
	s := trustedStatement(t)
	record, _ := Sign(s, mfg)
	exported := exportedFor("session")
	evidence := Evidence{Record: record, MCUKey: mcuKeyDER(t, mcuKey()), Proof: prove(t, mcuKey(), exported, record)}
	v, _ := newTestVerifier(testManufacturerAuthority, mfgKeys)
	if err := v.UseRegistry(opsKeys, false); err != nil {
		t.Fatal(err)
	}
	active, _ := SignRegistry(registryFor(t, 1, StatusActive, record), ops)
	if _, err := v.LoadRegistry(active); err != nil {
		t.Fatal(err)
	}
	result, err := v.Evaluate(s.ObserverID(), evidence, exported)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Recheck(result); err != nil {
		t.Fatalf("current board failed its recheck: %v", err)
	}
	revoked, _ := SignRegistry(registryFor(t, 2, StatusRevoked, record), ops)
	if _, err := v.LoadRegistry(revoked); err != nil {
		t.Fatal(err)
	}
	var rejection *Rejection
	if err := v.Recheck(result); !errors.As(err, &rejection) || rejection.Reason != ReasonRevoked {
		t.Fatalf("recheck after revocation = %v, want %q", err, ReasonRevoked)
	}
	// A session that proved nothing has nothing to withdraw.
	if err := v.Recheck(Result{Trust: identity.HardwareTrustNone}); err != nil {
		t.Fatal(err)
	}
}
