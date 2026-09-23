package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"github.com/ptudor/navlistener/internal/boardid"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ptudor/navlistener/internal/attestation"
	"github.com/ptudor/navlistener/internal/commissioning"
)

// The commands are exercised against the shared cross-implementation vectors,
// so the public verifier is pinned to the same bytes as the collector, the
// firmware and the factory tooling.
type fixtures struct {
	ManufacturerPublicKey   string `json:"manufacturer_public_key_pem"`
	ManufacturerAuthorityID string `json:"manufacturer_authority_id"`
	RegistryPublicKey       string `json:"registry_public_key_pem"`
	Cases                   map[string]struct {
		ObserverID  string `json:"observer_id"`
		Record      string `json:"record"`
		Fingerprint string `json:"fingerprint"`
	} `json:"cases"`
	Registry         string `json:"registry"`
	RegistrySequence uint64 `json:"registry_sequence"`
}

func loadFixtures(t *testing.T) (fixtures, string) {
	t.Helper()
	data, err := os.ReadFile("../../../common/fixtures/commissioning-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixtures
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, content := range map[string]string{
		"manufacturer.pem": f.ManufacturerPublicKey, "registry.pem": f.RegistryPublicKey, "registry.json": f.Registry,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return f, dir
}

// capture runs one command and decodes the JSON object it printed.
func capture(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = write
	runErr := run(args)
	os.Stdout = saved
	write.Close()
	printed, err := io.ReadAll(read)
	read.Close()
	if err != nil {
		t.Fatal(err)
	}
	if runErr != nil {
		if len(printed) != 0 {
			t.Fatalf("failed command still printed %q", printed)
		}
		return nil, runErr
	}
	var out map[string]any
	if err := json.Unmarshal(printed, &out); err != nil {
		t.Fatalf("output %q: %v", printed, err)
	}
	return out, nil
}

func TestCommissionVerify(t *testing.T) {
	f, dir := loadFixtures(t)
	manufacturer := filepath.Join(dir, "manufacturer.pem")
	for name, c := range f.Cases {
		out, err := capture(t, "commission-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", manufacturer, "-record", c.Record)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, _ := hex.DecodeString(c.Record)
		record, err := commissioning.ParseRecord(raw)
		if err != nil {
			t.Fatal(err)
		}
		statement, err := record.Statement()
		if err != nil {
			t.Fatal(err)
		}
		if out["ok"] != true || out["profile"] != statement.Profile.String() || out["observer_id"] != c.ObserverID || out["record_fingerprint"] != c.Fingerprint {
			t.Errorf("%s: output = %v", name, out)
		}
	}
	trusted := f.Cases["trusted"].Record
	// The registry key is a valid P-256 key that signed no record: pinning only
	// it must fail, and pinning it beside the manufacturer key must not.
	if _, err := capture(t, "commission-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", filepath.Join(dir, "registry.pem"), "-record", trusted); err == nil {
		t.Error("record verified under a key that did not sign it")
	}
	if _, err := capture(t, "commission-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", filepath.Join(dir, "registry.pem"), "-key", manufacturer, "-record", trusted); err != nil {
		t.Errorf("record rejected with its signer among several pinned keys: %v", err)
	}
	tampered, err := hex.DecodeString(trusted)
	if err != nil {
		t.Fatal(err)
	}
	tampered[65] ^= 1 // one bit of an identifier: still a well-formed statement
	if _, err := capture(t, "commission-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", manufacturer, "-record", hex.EncodeToString(tampered)); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Errorf("tampered record: err = %v, want a signature failure", err)
	}
	for _, args := range [][]string{
		{"commission-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-record", trusted},
		{"commission-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", manufacturer},
		{"commission-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", manufacturer, "-record", trusted[:40]},
	} {
		if _, err := capture(t, args...); err == nil {
			t.Errorf("%v accepted", args)
		}
	}
}

func TestRegistryVerify(t *testing.T) {
	f, dir := loadFixtures(t)
	registry := filepath.Join(dir, "registry.json")
	out, err := capture(t, "registry-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", filepath.Join(dir, "registry.pem"), "-file", registry)
	if err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["sequence"] != float64(f.RegistrySequence) || out["boards"] != float64(len(f.Cases)) || out["issued_at"] == "" {
		t.Errorf("output = %v", out)
	}
	// The manufacturer key is not a registry key: the two sets are separate.
	if _, err := capture(t, "registry-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", filepath.Join(dir, "manufacturer.pem"), "-file", registry); err == nil {
		t.Error("registry verified under the manufacturer key")
	}
	altered := filepath.Join(dir, "altered.json")
	if err := os.WriteFile(altered, []byte(strings.Replace(f.Registry, `"payload":"ey`, `"payload":"eY`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := capture(t, "registry-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", filepath.Join(dir, "registry.pem"), "-file", altered); err == nil {
		t.Error("altered registry verified")
	}
	if _, err := capture(t, "registry-verify", "-manufacturer-authority", f.ManufacturerAuthorityID, "-key", filepath.Join(dir, "registry.pem"), "-file", filepath.Join(dir, "absent.json")); err == nil {
		t.Error("missing registry file accepted")
	}
}

func TestUnknownCommandNamesTheVerifiers(t *testing.T) {
	err := run([]string{"export"})
	if err == nil || !strings.Contains(err.Error(), "commission-verify") || !strings.Contains(err.Error(), "registry-verify") {
		t.Fatalf("err = %v", err)
	}
}

func TestProductionLookingSignCommandIsUnavailable(t *testing.T) {
	if err := run([]string{"sign"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("sign: err = %v, want unknown command", err)
	}
	if err := run([]string{"fixture-sign", "-key", "fixture.pem"}); err == nil || !strings.Contains(err.Error(), "-development-fixture") {
		t.Fatalf("unguarded fixture-sign: err = %v", err)
	}
}

func writePublicKey(t *testing.T, dir, name string, key *ecdsa.PublicKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestVerifyCoreAttestationSelectsExactlyOnePinnedSigner(t *testing.T) {
	dir := t.TempDir()
	signer, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hardware := attestation.HardwareIdentity{Product: 1, BoardRevision: 0x1234}
	hardware.BoardUID = boardid.EEPROM([8]byte{0x00, 0x04, 0xa3, 0xaa, 0xbb, 0xcc, 0xdd, 0xee})
	copy(hardware.ATECCSerial[:], []byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x11})
	record, err := attestation.Sign(attestation.VersionV1, hardware, signer, nil)
	if err != nil {
		t.Fatal(err)
	}
	right := writePublicKey(t, dir, "right.pem", &signer.PublicKey)
	wrong := writePublicKey(t, dir, "wrong.pem", &other.PublicKey)
	base := []string{"verify", "-manufacturer-authority", "test-manufacturer", "-record", hex.EncodeToString(record[:]),
		"-product", "1", "-board-rev", "0x1234", "-board-uid-kind", "eui64", "-board-uid", hardware.BoardUID.Hex(),
		"-atecc-serial", hex.EncodeToString(hardware.ATECCSerial[:])}
	out, err := capture(t, append(base, "-key", wrong, "-key", right)...)
	if err != nil {
		t.Fatal(err)
	}
	id, err := commissioning.KeyID(&signer.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	if out["ok"] != true || out["tier"] != "verified_v1_core" ||
		out["signer_key_id"] != hex.EncodeToString(id[:]) {
		t.Fatalf("output = %v", out)
	}
	if _, err := capture(t, append(base, "-key", wrong)...); err == nil || !strings.Contains(err.Error(), "any pinned key") {
		t.Fatalf("wrong key: err = %v", err)
	}
	if _, err := capture(t, append(base, "-key", right, "-key", right)...); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("duplicate key: err = %v", err)
	}
	badAuthority := append([]string(nil), base...)
	badAuthority[2] = "not valid!"
	if _, err := capture(t, append(badAuthority, "-key", right)...); err == nil || !strings.Contains(err.Error(), "scope id") {
		t.Fatalf("invalid authority: err = %v", err)
	}
}
