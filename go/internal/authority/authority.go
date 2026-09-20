// Package authority binds configured public keys to administrative authorities.
// A verified root chain is never an operational enrollment grant.
package authority

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

// Operational is server-owned registration data. Issuers are certificates for
// exact Issuing keys; each certificate must validate under this authority's roots.
type Operational struct {
	ID            string   `toml:"id"`
	Enabled       bool     `toml:"enabled"`
	Roots         []string `toml:"roots"`
	Issuers       []string `toml:"issuers"`
	Manufacturers []string `toml:"manufacturer_authorities"`
}

type registration struct {
	enabled       bool
	manufacturers map[string]bool
}

// Set is immutable after construction. Disabled keys remain reserved to their
// original authority and cannot be silently registered to another one.
type Set struct {
	manufacturers map[string]bool
	operational   map[string]registration
	issuers       map[string]string
	caKeys        map[string]bool
	pool          *x509.CertPool
}

func Fingerprint(der []byte) string {
	d := sha256.Sum256(der)
	return hex.EncodeToString(d[:])
}

func Certificates(path string) ([]*x509.Certificate, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var certs []*x509.Certificate
	for len(b) > 0 {
		block, rest := pem.Decode(b)
		if block == nil {
			return nil, fmt.Errorf("%s: invalid certificate PEM", path)
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("%s: expected certificate", path)
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
		b = rest
	}
	if len(certs) == 0 {
		return nil, errors.New("certificate file is empty")
	}
	return certs, nil
}

func New(config []Operational, manufacturers map[string]bool, now time.Time) (*Set, error) {
	s := &Set{manufacturers: map[string]bool{}, operational: map[string]registration{}, issuers: map[string]string{}, caKeys: map[string]bool{}, pool: x509.NewCertPool()}
	for id, enabled := range manufacturers {
		s.manufacturers[id] = enabled
	}
	for _, op := range config {
		if !identity.ValidScopeID(op.ID) {
			return nil, errors.New("operational authority requires a valid id")
		}
		if _, exists := s.operational[op.ID]; exists {
			return nil, fmt.Errorf("duplicate operational authority %q", op.ID)
		}
		r := registration{enabled: op.Enabled, manufacturers: map[string]bool{}}
		for _, id := range op.Manufacturers {
			if _, exists := manufacturers[id]; !exists {
				return nil, fmt.Errorf("%s: unknown manufacturer authority %q", op.ID, id)
			}
			if r.manufacturers[id] {
				return nil, fmt.Errorf("%s: duplicate manufacturer pairing %q", op.ID, id)
			}
			r.manufacturers[id] = true
		}
		s.operational[op.ID] = r
		if len(op.Issuers) == 0 && len(op.Roots) == 0 {
			continue
		} // token-only authority
		if len(op.Issuers) == 0 || len(op.Roots) == 0 {
			return nil, fmt.Errorf("%s: roots and issuing certificates are required together", op.ID)
		}
		roots := x509.NewCertPool()
		for _, path := range op.Roots {
			certs, err := Certificates(path)
			if err != nil {
				return nil, err
			}
			for _, cert := range certs {
				if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.CheckSignatureFrom(cert) != nil {
					return nil, fmt.Errorf("%s: root is not a self-signed CA", path)
				}
				roots.AddCert(cert)
				s.caKeys[Fingerprint(cert.RawSubjectPublicKeyInfo)] = true
			}
		}
		for _, path := range op.Issuers {
			certs, err := Certificates(path)
			if err != nil {
				return nil, err
			}
			for _, cert := range certs {
				if !cert.IsCA || !cert.BasicConstraintsValid || !cert.MaxPathLenZero || cert.KeyUsage&x509.KeyUsageCertSign == 0 || cert.CheckSignatureFrom(cert) == nil {
					return nil, fmt.Errorf("%s: expected a non-root Issuing intermediate", path)
				}
				if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
					return nil, fmt.Errorf("%s: issuing chain: %w", path, err)
				}
				pin := Fingerprint(cert.RawSubjectPublicKeyInfo)
				if prior, exists := s.issuers[pin]; exists && prior != op.ID {
					return nil, fmt.Errorf("issuing key is registered to both %s and %s", prior, op.ID)
				}
				s.issuers[pin] = op.ID
				s.caKeys[pin] = true
				if op.Enabled {
					s.pool.AddCert(cert)
				}
			}
		}
	}
	return s, nil
}

func (s *Set) ClientPool() *x509.CertPool { return s.pool.Clone() }
func (s *Set) CAKey(pin string) bool      { return s != nil && s.caKeys[pin] }

func (s *Set) Allows(operational, manufacturer string) error {
	if s == nil {
		return errors.New("no operational authorities registered")
	}
	r, ok := s.operational[operational]
	if !ok || !r.enabled {
		return errors.New("unknown or disabled operational authority")
	}
	if manufacturer != "" && (!r.manufacturers[manufacturer] || !s.manufacturers[manufacturer]) {
		return errors.New("operational/manufacturer pairing is not permitted")
	}
	return nil
}

// MatchIssuer examines only TLS-verified chains, never peer-supplied chain order
// or issuer display names. Cross-signed certificates for the same key agree.
func (s *Set) MatchIssuer(chains [][]*x509.Certificate, expected string) (string, error) {
	if err := s.Allows(expected, ""); err != nil {
		return "", err
	}
	var matched string
	for _, chain := range chains {
		if len(chain) < 2 {
			return "", errors.New("verified issuing intermediate is missing")
		}
		pin := Fingerprint(chain[1].RawSubjectPublicKeyInfo)
		if s.issuers[pin] != expected {
			return "", errors.New("verified issuing key does not belong to enrolled operational authority")
		}
		if matched != "" && matched != pin {
			return "", errors.New("verified chains disagree about issuing key")
		}
		matched = pin
	}
	if matched == "" {
		return "", errors.New("no verified client chain")
	}
	return matched, nil
}
