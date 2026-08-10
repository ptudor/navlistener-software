// mfgattest is the offline manufacturing/enrollment utility for ATECC slot-14
// originality records. It never issues operational device certificates.
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

	"github.com/ptudor/navlistener/internal/attestation"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mfgattest:", err)
		os.Exit(2)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: mfgattest sign|verify [flags]")
	}
	switch args[0] {
	case "sign":
		return runSign(args[1:])
	case "verify":
		return runVerify(args[1:])
	default:
		return fmt.Errorf("unknown command %q (want sign or verify)", args[0])
	}
}

type identityFlags struct {
	atecc string
	rtc   string
	board string
	rev   uint
}

func (i *identityFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&i.atecc, "atecc-serial", "", "9-byte ATECC factory serial as hex")
	fs.StringVar(&i.rtc, "rtc-eui", "", "8-byte MCP79412 EUI-64 as hex")
	fs.StringVar(&i.board, "board-eui", "", "8-byte 24AA025E64 EUI-64 as hex (required by v2)")
	fs.UintVar(&i.rev, "board-rev", 0, "board revision uint16 (decimal or use 0x prefix)")
}

func (i identityFlags) value() (attestation.HardwareIdentity, error) {
	var h attestation.HardwareIdentity
	if i.rev > 0xffff {
		return h, fmt.Errorf("board-rev %d exceeds uint16", i.rev)
	}
	atecc, err := fixedHex(i.atecc, len(h.ATECCSerial))
	if err != nil {
		return h, fmt.Errorf("atecc-serial: %w", err)
	}
	rtc, err := fixedHex(i.rtc, len(h.RTCEUI64))
	if err != nil {
		return h, fmt.Errorf("rtc-eui: %w", err)
	}
	board, err := fixedHex(i.board, len(h.BoardEUI64))
	if err != nil && i.board != "" {
		return h, fmt.Errorf("board-eui: %w", err)
	}
	copy(h.ATECCSerial[:], atecc)
	copy(h.RTCEUI64[:], rtc)
	copy(h.BoardEUI64[:], board)
	h.BoardRevision = uint16(i.rev)
	return h, nil
}

func runSign(args []string) error {
	fs := flag.NewFlagSet("sign", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyPath := fs.String("key", "", "manufacturer P-256 private key PEM")
	version := fs.Uint("version", 2, "attestation version (1 or 2)")
	var ids identityFlags
	ids.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *keyPath == "" {
		return errors.New("sign requires -key and identity flags")
	}
	if *version != uint(attestation.VersionV1) && *version != uint(attestation.VersionV2) {
		return fmt.Errorf("version %d is unsupported (want 1 or 2)", *version)
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
		"version": *version,
		"record":  hex.EncodeToString(record[:]),
	})
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyPath := fs.String("key", "", "manufacturer P-256 public key or certificate PEM")
	recordHex := fs.String("record", "", "72-byte slot record as hex")
	var ids identityFlags
	ids.register(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 || *keyPath == "" || *recordHex == "" {
		return errors.New("verify requires -key, -record, and identity flags")
	}
	key, err := readPublicKey(*keyPath)
	if err != nil {
		return err
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
	verified, err := attestation.Verify(record, h, key)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ok":                 true,
		"tier":               verified.Tier,
		"statement_digest":   hex.EncodeToString(verified.StatementDigest[:]),
		"record_fingerprint": hex.EncodeToString(verified.RecordFingerprint[:]),
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
