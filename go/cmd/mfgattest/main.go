// mfgattest verifies ATECC slot-14 originality records, commissioning records,
// and the signed registry (docs/COMMISSIONING.md). It never issues production
// records or operational device certificates. Its deliberately gated signer is
// only for generating development fixtures.
package main

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ptudor/navlistener/internal/attestation"
	"github.com/ptudor/navlistener/internal/boardid"
	"github.com/ptudor/navlistener/internal/commissioning"
	identitypkg "github.com/ptudor/navlistener/internal/identity"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mfgattest:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mfgattest verify|commission-verify|registry-verify|fixture-sign [flags]")
	}
	switch args[0] {
	case "fixture-sign":
		return runFixtureSign(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "commission-verify":
		return runCommissionVerify(args[1:])
	case "registry-verify":
		return runRegistryVerify(args[1:])
	default:
		return fmt.Errorf("unknown command %q (want verify, commission-verify, registry-verify or fixture-sign)", args[0])
	}
}

// keyFiles collects a repeatable -key flag. Verifiers pin sets of keys: a
// replaced signer adds one, and what the earlier key signed stays valid.
type keyFiles []string

func (k *keyFiles) String() string { return strings.Join(*k, ",") }
func (k *keyFiles) Set(path string) error {
	if path == "" {
		return errors.New("key path is empty")
	}
	*k = append(*k, path)
	return nil
}

// runCommissionVerify checks one commissioning record against the pinned
// manufacturer keys and prints what it states. It verifies the manufacturer's
// signature only: that the commissioned microcontroller is the one on a
// connection is a per-session proof, which only a collector can check.
func runCommissionVerify(args []string) error {
	fs := flag.NewFlagSet("commission-verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var keys keyFiles
	fs.Var(&keys, "key", "manufacturer P-256 public key or certificate PEM (repeatable)")
	authorityID := fs.String("manufacturer-authority", "", "configured manufacturer authority id")
	recordHex := fs.String("record", "", fmt.Sprintf("%d-byte commissioning record as hex", commissioning.RecordSize))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || len(keys) == 0 || *authorityID == "" || *recordHex == "" {
		return errors.New("commission-verify requires -manufacturer-authority, -key (one or more), and -record")
	}
	if !identitypkg.ValidScopeID(*authorityID) {
		return errors.New("manufacturer-authority must be a valid scope id")
	}
	pinned, err := commissioning.LoadKeySet(keys)
	if err != nil {
		return err
	}
	raw, err := fixedHex(*recordHex, commissioning.RecordSize)
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	record, err := commissioning.ParseRecord(raw)
	if err != nil {
		return err
	}
	s, err := pinned.Verify(record)
	if err != nil {
		return err
	}
	signer, fingerprint := record.KeyID(), record.Fingerprint()
	var rtcEUI any
	if s.IdentityFlags&commissioning.IdentityRTCEUIBound != 0 {
		rtcEUI = hex.EncodeToString(s.RTCEUI64[:])
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ok":                        true,
		"manufacturer_authority_id": *authorityID,
		"observer_id":               s.ObserverID(),
		"profile":                   s.Profile.String(),
		"product":                   uint16(s.Product),
		"generation":                s.Generation,
		"commissioned_at":           time.Unix(int64(s.CommissionedAt), 0).UTC().Format(time.RFC3339),
		"board_revision":            s.BoardRevision,
		"security":                  s.Security,
		"identity_flags":            s.IdentityFlags,
		"rtc_model_id":              uint16(s.RTCModel),
		"atecc_serial":              hex.EncodeToString(s.ATECCSerial[:]),
		"rtc_eui64":                 rtcEUI,
		"board_uid_kind":            s.BoardUID.KindName(),
		"board_uid":                 s.BoardUID.Hex(),
		"mcu_family":                uint8(s.MCUFamily),
		"mcu_mac":                   hex.EncodeToString(s.MCUMAC[:]),
		"mcu_key_alg":               uint8(s.MCUKeyAlg),
		"mcu_key_sha256":            hex.EncodeToString(s.MCUKeySHA256[:]),
		"secure_boot_keys_sha256":   hex.EncodeToString(s.SecureBootKeys[:]),
		"attestation_sha256":        hex.EncodeToString(s.Attestation[:]),
		"signer_key_id":             hex.EncodeToString(signer[:]),
		"record_fingerprint":        hex.EncodeToString(fingerprint[:]),
	})
}

// runRegistryVerify checks a signed registry file against the pinned registry
// (operations) keys. Those are a different set from the manufacturer keys: the
// registry can only withdraw trust.
func runRegistryVerify(args []string) error {
	fs := flag.NewFlagSet("registry-verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var keys keyFiles
	fs.Var(&keys, "key", "registry P-256 public key or certificate PEM (repeatable)")
	authorityID := fs.String("manufacturer-authority", "", "configured manufacturer authority id")
	path := fs.String("file", "", "signed registry file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || len(keys) == 0 || *authorityID == "" || *path == "" {
		return errors.New("registry-verify requires -manufacturer-authority, -key (one or more), and -file")
	}
	if !identitypkg.ValidScopeID(*authorityID) {
		return errors.New("manufacturer-authority must be a valid scope id")
	}
	pinned, err := commissioning.LoadKeySet(keys)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(*path)
	if err != nil {
		return fmt.Errorf("read registry: %w", err)
	}
	registry, err := commissioning.VerifyRegistry(data, pinned, *authorityID)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ok":                        true,
		"manufacturer_authority_id": registry.ManufacturerAuthorityID,
		"sequence":                  registry.Sequence,
		"issued_at":                 registry.IssuedAt.UTC().Format(time.RFC3339),
		"ledger_head":               registry.LedgerHead,
		"boards":                    registry.Len(),
	})
}

type identityFlags struct {
	atecc   string
	board   string
	kind    string
	product uint
	rev     uint
}

func (i *identityFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&i.atecc, "atecc-serial", "", "9-byte ATECC factory serial as hex")
	fs.StringVar(&i.board, "board-uid", "", "board UID as lowercase hex")
	fs.StringVar(&i.kind, "board-uid-kind", "", "eui64, serial128 or st_uid128")
	fs.UintVar(&i.product, "product", 0, "manufacturer product uint16 (decimal or use 0x prefix)")
	fs.UintVar(&i.rev, "board-rev", 0, "board revision uint16 (decimal or use 0x prefix)")
}

func (i identityFlags) value() (attestation.HardwareIdentity, error) {
	var h attestation.HardwareIdentity
	if i.product > 0xffff {
		return h, fmt.Errorf("product %d exceeds uint16", i.product)
	}
	if i.rev > 0xffff {
		return h, fmt.Errorf("board-rev %d exceeds uint16", i.rev)
	}
	atecc, err := fixedHex(i.atecc, len(h.ATECCSerial))
	if err != nil {
		return h, fmt.Errorf("atecc-serial: %w", err)
	}
	board, err := boardid.Parse(i.kind, i.board)
	if err != nil {
		return h, fmt.Errorf("board-uid: %w", err)
	}
	copy(h.ATECCSerial[:], atecc)
	h.BoardUID = board
	h.Product = uint16(i.product)
	h.BoardRevision = uint16(i.rev)
	return h, nil
}

func runFixtureSign(args []string) error {
	fs := flag.NewFlagSet("fixture-sign", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	allow := fs.Bool("development-fixture", false, "acknowledge that this command must not issue production records")
	keyPath := fs.String("key", "", "development-fixture P-256 private key PEM")
	version := fs.Uint("version", 1, "attestation version (only 1 is defined)")
	var ids identityFlags
	ids.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || !*allow || *keyPath == "" {
		return errors.New("fixture-sign requires -development-fixture, -key, and identity flags")
	}
	if *version != uint(attestation.VersionV1) {
		return fmt.Errorf("version %d is unsupported (want 1)", *version)
	}
	if st, err := os.Stat(*keyPath); err != nil {
		return fmt.Errorf("private key: %w", err)
	} else if st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("private key %s is group/world accessible (mode %04o); require 0600", *keyPath, st.Mode().Perm())
	}
	key, err := readPrivateKey(*keyPath)
	if err != nil {
		return err
	}
	h, err := ids.value()
	if err != nil {
		return err
	}
	record, err := attestation.Sign(byte(*version), h, key, nil)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"development_fixture": true,
		"version":             *version,
		"record":              hex.EncodeToString(record[:]),
	})
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var keys keyFiles
	fs.Var(&keys, "key", "manufacturer P-256 public key or certificate PEM (repeatable)")
	authorityID := fs.String("manufacturer-authority", "", "configured manufacturer authority id")
	recordHex := fs.String("record", "", "72-byte slot record as hex")
	var ids identityFlags
	ids.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *authorityID == "" || len(keys) == 0 || *recordHex == "" {
		return errors.New("verify requires -manufacturer-authority, -key (one or more), -record, and identity flags")
	}
	if !identitypkg.ValidScopeID(*authorityID) {
		return errors.New("manufacturer-authority must be a valid scope id")
	}
	raw, err := fixedHex(*recordHex, attestation.SlotRecordSize)
	if err != nil {
		return fmt.Errorf("record: %w", err)
	}
	record, err := attestation.ParseRecord(raw)
	if err != nil {
		return err
	}
	h, err := ids.value()
	if err != nil {
		return err
	}
	type match struct {
		verification attestation.Verification
		keyID        [commissioning.KeyIDSize]byte
	}
	var matches []match
	seen := make(map[[commissioning.KeyIDSize]byte]struct{}, len(keys))
	for _, path := range keys {
		key, err := readPublicKey(path)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		keyID, err := commissioning.KeyID(key)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		if _, duplicate := seen[keyID]; duplicate {
			return fmt.Errorf("manufacturer key id %x is listed more than once", keyID)
		}
		seen[keyID] = struct{}{}
		verified, err := attestation.Verify(record, h, key)
		if err == nil {
			matches = append(matches, match{verification: verified, keyID: keyID})
		}
	}
	if len(matches) == 0 {
		return errors.New("manufacturer attestation did not verify under any pinned key")
	}
	if len(matches) != 1 {
		return errors.New("manufacturer attestation verifies under more than one pinned key")
	}
	verified := matches[0].verification
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ok":                        true,
		"manufacturer_authority_id": *authorityID,
		"signer_key_id":             hex.EncodeToString(matches[0].keyID[:]),
		"tier":                      verified.Tier,
		"statement_digest":          hex.EncodeToString(verified.StatementDigest[:]),
		"record_fingerprint":        hex.EncodeToString(verified.RecordFingerprint[:]),
	})
}

func fixedHex(s string, n int) ([]byte, error) {
	s = strings.NewReplacer(":", "", "-", "", " ", "").Replace(strings.TrimSpace(s))
	if s == "" {
		return nil, errors.New("value is required")
	}
	if len(s) != n*2 {
		return nil, fmt.Errorf("got %d hex digits, want %d", len(s), n*2)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return b, nil
}

func readPrivateKey(path string) (*ecdsa.PrivateKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read private key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("private key: no PEM block")
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	key, ok := anyKey.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("private key type %T: want ECDSA P-256", anyKey)
	}
	return key, nil
}

func readPublicKey(path string) (*ecdsa.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read public key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("public key: no PEM block")
	}
	if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
		key, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok {
			return nil, fmt.Errorf("certificate key type %T: want ECDSA P-256", cert.PublicKey)
		}
		return key, nil
	}
	anyKey, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse public key: %w", err)
	}
	key, ok := anyKey.(*ecdsa.PublicKey)
	if !ok {
		return nil, fmt.Errorf("public key type %T: want ECDSA P-256", anyKey)
	}
	return key, nil
}
