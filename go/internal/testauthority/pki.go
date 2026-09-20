// Package testauthority creates synthetic independent CA and manufacturer roles
// for tests. It is never imported by a production executable.
package testauthority

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/authority"
)

type Pair struct {
	Config            authority.Operational
	Roots             [2]*x509.Certificate
	RootKeys          [2]*ecdsa.PrivateKey
	Issuers           [2]*x509.Certificate
	IssuingKey        *ecdsa.PrivateKey
	ManufacturerKeys  [2]*ecdsa.PrivateKey
	ManufacturerPaths []string
	RegistryKey       *ecdsa.PrivateKey
	RegistryPath      string
}

func Key(t testing.TB) *ecdsa.PrivateKey {
	t.Helper()
	k, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	return k
}
func Cert(t testing.TB, template, parent *x509.Certificate, pub *ecdsa.PublicKey, key *ecdsa.PrivateKey) *x509.Certificate {
	t.Helper()
	der, e := x509.CreateCertificate(rand.Reader, template, parent, pub, key)
	if e != nil {
		t.Fatal(e)
	}
	c, e := x509.ParseCertificate(der)
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func Write(t testing.TB, dir, name, kind string, der []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}), 0600); e != nil {
		t.Fatal(e)
	}
	return path
}

func New(t testing.TB, id string, manufacturers ...string) *Pair {
	t.Helper()
	dir := t.TempDir()
	p := &Pair{Config: authority.Operational{ID: id, Enabled: true, Manufacturers: manufacturers}, IssuingKey: Key(t), RegistryKey: Key(t)}
	now := time.Now()
	for i := 0; i < 2; i++ {
		name := string(rune('a' + i))
		p.RootKeys[i], p.ManufacturerKeys[i] = Key(t), Key(t)
		root := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 1)), Subject: pkix.Name{CommonName: id + " root " + name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, MaxPathLen: 1}
		p.Roots[i] = Cert(t, root, root, &p.RootKeys[i].PublicKey, p.RootKeys[i])
		issuer := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 10)), Subject: pkix.Name{CommonName: id + " issuing"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(12 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, MaxPathLen: 0, MaxPathLenZero: true}
		p.Issuers[i] = Cert(t, issuer, p.Roots[i], &p.IssuingKey.PublicKey, p.RootKeys[i])
		p.Config.Roots = append(p.Config.Roots, Write(t, dir, "root-"+name+".pem", "CERTIFICATE", p.Roots[i].Raw))
		p.Config.Issuers = append(p.Config.Issuers, Write(t, dir, "issuer-"+name+".pem", "CERTIFICATE", p.Issuers[i].Raw))
		der, e := x509.MarshalPKIXPublicKey(&p.ManufacturerKeys[i].PublicKey)
		if e != nil {
			t.Fatal(e)
		}
		p.ManufacturerPaths = append(p.ManufacturerPaths, Write(t, dir, "manufacturer-"+name+".pem", "PUBLIC KEY", der))
	}
	der, e := x509.MarshalPKIXPublicKey(&p.RegistryKey.PublicKey)
	if e != nil {
		t.Fatal(e)
	}
	p.RegistryPath = Write(t, dir, "registry.pem", "PUBLIC KEY", der)
	return p
}

func (p *Pair) Leaf(t testing.TB, id string) (*x509.Certificate, *ecdsa.PrivateKey, string) {
	t.Helper()
	key := Key(t)
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{DNSNames: []string{id}}, key)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(100), DNSNames: []string{id}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	return Cert(t, template, p.Issuers[0], &key.PublicKey, p.IssuingKey), key, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}))
}
