package commissioning

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/ptudor/navlistener/internal/identity"
)

// The fixtures are the cross-implementation contract: the firmware's C
// formatter and the factory's Python implementation are tested against the
// same files. Regenerate them only for a deliberate format change:
//
//	go test ./internal/commissioning -run TestFixtures -update-fixtures
var updateFixtures = flag.Bool("update-fixtures", false, "regenerate common/fixtures/commissioning-*")

const fixtureDir = "../../../common/fixtures"

type fixtureCase struct {
	ObserverID   string `json:"observer_id"`
	Trust        string `json:"trust"`
	Statement    string `json:"statement"`
	Digest       string `json:"digest"`
	Record       string `json:"record"`
	Fingerprint  string `json:"fingerprint"`
	MCUPublicKey string `json:"mcu_public_key_der,omitempty"`
	Exported     string `json:"exported,omitempty"`
	ProofDigest  string `json:"proof_digest,omitempty"`
	Proof        string `json:"proof,omitempty"`
	Evidence     string `json:"evidence"`
}

type fixtureFile struct {
	ExporterLabel           string                 `json:"exporter_label"`
	ManufacturerAuthorityID string                 `json:"manufacturer_authority_id"`
	ManufacturerPublicKey   string                 `json:"manufacturer_public_key_pem"`
	RegistryPublicKey       string                 `json:"registry_public_key_pem"`
	Cases                   map[string]fixtureCase `json:"cases"`
	Registry                string                 `json:"registry"`
	RegistrySequence        uint64                 `json:"registry_sequence"`
	RegistryRevokedBoard    string                 `json:"registry_revoked_board_eui64"`
}

func publicPEM(t *testing.T, key *ecdsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
}

func keySetFromPEM(t *testing.T, text string) *KeySet {
	t.Helper()
	block, _ := pem.Decode([]byte(text))
	if block == nil {
		t.Fatal("fixture key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := NewKeySet(parsed.(*ecdsa.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func generateFixtures(t *testing.T) fixtureFile {
	t.Helper()
	mfgKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	opsKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	mfg, _ := NewKeySigner(mfgKey, nil)
	ops, _ := NewKeySigner(opsKey, nil)
	mcu, err := rsa.GenerateKey(rand.Reader, mcuKeyBits)
	if err != nil {
		t.Fatal(err)
	}
	der := mcuKeyDER(t, mcu)

	trusted := trustedStatement(t)
	trusted.MCUKeySHA256 = sha256.Sum256(der)
	open := openStatement(t)
	open.RTCEUI64[7], open.BoardEUI64[7], open.ATECCSerial[8], open.MCUMAC[5] = 0x91, 0xef, 0x12, 0x04
	bench := openStatement(t)
	bench.Profile, bench.Attestation = ProfileTest, [32]byte{}
	bench.RTCEUI64[7], bench.BoardEUI64[7], bench.ATECCSerial[8], bench.MCUMAC[5] = 0x92, 0xf0, 0x13, 0x05

	out := fixtureFile{
		ExporterLabel:           ExporterLabel,
		ManufacturerAuthorityID: testManufacturerAuthority,
		ManufacturerPublicKey:   publicPEM(t, &mfgKey.PublicKey),
		RegistryPublicKey:       publicPEM(t, &opsKey.PublicKey),
		Cases:                   map[string]fixtureCase{},
	}
	var records []Record
	cases := map[string]Statement{"trusted": trusted, "open": open, "test": bench}
	for i, name := range []string{"trusted", "open", "test"} {
		s := cases[name]
		s.IdentityFlags = 0
		s.RTCModel = RTCModelNone
		s.RTCEUI64 = [8]byte{}
		s.BoardEUI64[7] = byte(0xa0 + i)
		s.ATECCSerial[8] = byte(0xa0 + i)
		cases[name+"-no-rtc"] = s
	}
	for i, model := range []RTCModel{RTCModelMCP79412, RTCModelDS3231} {
		s := open
		s.IdentityFlags = IdentityRTCPresent
		s.RTCModel = model
		s.RTCEUI64 = [8]byte{}
		s.BoardEUI64[7] = byte(0xb0 + i)
		s.ATECCSerial[8] = byte(0xb0 + i)
		cases[[]string{"mcp79412-model", "ds3231-model"}[i]] = s
	}
	for name, s := range cases {
		body, err := s.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		d, _ := s.Digest()
		record, err := Sign(s, mfg)
		if err != nil {
			t.Fatal(err)
		}
		fp := record.Fingerprint()
		c := fixtureCase{
			ObserverID: s.ObserverID(), Trust: s.Profile.String(), Statement: hex.EncodeToString(body),
			Digest: hex.EncodeToString(d[:]), Record: hex.EncodeToString(record[:]), Fingerprint: hex.EncodeToString(fp[:]),
		}
		e := Evidence{Record: record}
		if s.Profile == ProfileTrusted {
			exported := exportedFor("fixture session")
			pd, _ := ProofDigest(exported, record)
			e.MCUKey, e.Proof = der, prove(t, mcu, exported, record)
			c.MCUPublicKey, c.Exported = hex.EncodeToString(der), hex.EncodeToString(exported)
			c.ProofDigest, c.Proof = hex.EncodeToString(pd[:]), hex.EncodeToString(e.Proof)
		}
		frame, err := e.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		c.Evidence = hex.EncodeToString(frame)
		out.Cases[name] = c
		records = append(records, record)
	}
	// The registry lists the trusted and open boards as active and the bench
	// board as revoked, so importers exercise both statuses.
	reg := Registry{ManufacturerAuthorityID: testManufacturerAuthority, Sequence: 3, IssuedAt: registryFor(t, 1, StatusActive).IssuedAt, LedgerHead: registryFor(t, 1, StatusActive).LedgerHead}
	for _, r := range records {
		s, _ := r.Statement()
		row := registryFor(t, 1, StatusActive, r).Boards[0]
		if s.Profile == ProfileTest {
			row.Status, row.Reason = StatusRevoked, "bench unit retired"
			if s.BoardEUI64 == bench.BoardEUI64 {
				out.RegistryRevokedBoard = row.BoardEUI64
			}
		}
		reg.Boards = append(reg.Boards, row)
	}
	data, err := SignRegistry(reg, ops)
	if err != nil {
		t.Fatal(err)
	}
	out.Registry, out.RegistrySequence = string(data), reg.Sequence
	return out
}

func writeFixtures(t *testing.T, f fixtureFile) {
	t.Helper()
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"commissioning-v1.json": append(data, '\n')}
	for name, c := range f.Cases {
		files["commissioning-"+name+"-statement-v1.hex"] = []byte(c.Statement + "\n")
		files["commissioning-"+name+"-digest-v1.hex"] = []byte(c.Digest + "\n")
	}
	// Single-value hex files for the firmware's host tests, which carry no JSON
	// parser: the trusted case, field by field.
	c := f.Cases["trusted"]
	for name, value := range map[string]string{
		"commissioning-statement-v1.hex": c.Statement, "commissioning-digest-v1.hex": c.Digest,
		"commissioning-record-v1.hex": c.Record, "commissioning-exported-v1.hex": c.Exported,
		"commissioning-proof-digest-v1.hex": c.ProofDigest, "commissioning-mcu-key-v1.hex": c.MCUPublicKey,
		"commissioning-proof-v1.hex": c.Proof, "commissioning-evidence-v1.hex": c.Evidence,
	} {
		files[name] = []byte(value + "\n")
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(fixtureDir, name), content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFixtures(t *testing.T) {
	if *updateFixtures {
		writeFixtures(t, generateFixtures(t))
	}
	data, err := os.ReadFile(filepath.Join(fixtureDir, "commissioning-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixtureFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	if f.ExporterLabel != ExporterLabel {
		t.Fatalf("fixture exporter label %q, package %q", f.ExporterLabel, ExporterLabel)
	}
	v, err := newTestVerifier(f.ManufacturerAuthorityID, keySetFromPEM(t, f.ManufacturerPublicKey))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Cases) != 8 {
		t.Fatalf("fixture has %d cases, want eight profile/RTC combinations", len(f.Cases))
	}
	for name, c := range f.Cases {
		t.Run(name, func(t *testing.T) {
			s, err := ParseStatement(mustHex(t, c.Statement))
			if err != nil {
				t.Fatal(err)
			}
			again, _ := s.MarshalBinary()
			if hex.EncodeToString(again) != c.Statement {
				t.Fatal("statement does not re-encode to the fixture bytes")
			}
			if d, _ := s.Digest(); hex.EncodeToString(d[:]) != c.Digest {
				t.Fatal("statement digest differs from the fixture")
			}
			evidence, err := ParseEvidence(mustHex(t, c.Evidence))
			if err != nil {
				t.Fatal(err)
			}
			if hex.EncodeToString(evidence.Record[:]) != c.Record {
				t.Fatal("evidence does not carry the fixture record")
			}
			if fp := evidence.Record.Fingerprint(); hex.EncodeToString(fp[:]) != c.Fingerprint {
				t.Fatal("record fingerprint differs from the fixture")
			}
			exported := exportedFor("unused")
			if c.Exported != "" {
				exported = mustHex(t, c.Exported)
				if d, _ := ProofDigest(exported, evidence.Record); hex.EncodeToString(d[:]) != c.ProofDigest {
					t.Fatal("proof digest differs from the fixture")
				}
			}
			result, err := v.Evaluate(c.ObserverID, evidence, exported)
			if err != nil || result.Trust != identity.HardwareTrust(c.Trust) {
				t.Fatalf("evaluate = %q, %v; want %q", result.Trust, err, c.Trust)
			}
		})
	}
	ix, err := VerifyRegistry([]byte(f.Registry), keySetFromPEM(t, f.RegistryPublicKey), f.ManufacturerAuthorityID)
	if err != nil {
		t.Fatal(err)
	}
	if ix.Sequence != f.RegistrySequence || ix.Len() != len(f.Cases) {
		t.Fatalf("registry = sequence %d with %d boards", ix.Sequence, ix.Len())
	}
	var revoked [8]byte
	copy(revoked[:], mustHex(t, f.RegistryRevokedBoard))
	if ix.boards[revoked].status != StatusRevoked {
		t.Fatal("fixture registry does not revoke the bench board")
	}
	// The single-value hex files must stay in step with the JSON master.
	for name, want := range map[string]string{
		"commissioning-statement-v1.hex": f.Cases["trusted"].Statement,
		"commissioning-record-v1.hex":    f.Cases["trusted"].Record,
		"commissioning-evidence-v1.hex":  f.Cases["trusted"].Evidence,
	} {
		got, err := os.ReadFile(filepath.Join(fixtureDir, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want+"\n" {
			t.Fatalf("%s is out of step with commissioning-v1.json", name)
		}
	}
}
