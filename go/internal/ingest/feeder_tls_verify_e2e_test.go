package ingest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// selfSignedSANs is selfSigned with the subject alternative names under the test's
// control, so a collector certificate can be minted with or without the IP SAN the
// feeder's --server literal must match.
func selfSignedSANs(t *testing.T, ips []net.IP, dns []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-collector"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  ips,
		DNSNames:     dns,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// writeCAPEM writes a certificate's leaf as a PEM bundle for the feeder's --ca: a
// self-signed collector certificate is its own trust anchor.
func writeCAPEM(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ingest-ca.pem")
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// testCA is an in-memory issuing CA, the shape of the fleet's ingest-ca.pem: the
// collector presents a leaf it signed, and the feeder trusts the CA, not the leaf.
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  tls.Certificate // the CA certificate, for writeCAPEM
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ingest-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{cert: cert, key: key, pem: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}}
}

// issue mints a collector leaf signed by the CA with the given SANs.
func (ca *testCA) issue(t *testing.T, ips []net.IP, dns []string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "test-collector"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  ips,
		DNSNames:     dns,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// TestNavfeederVerifiedTLS is the one e2e case that does NOT pass --insecure: the feeder
// verifies the collector against --ca, matches the 127.0.0.1 literal against the
// certificate's IP SAN (SSL_set1_host with an IP literal) and omits SNI for it, over the
// TLS 1.2 pin. Every other feeder test runs --insecure, so without this the verified
// path — the one the fleet actually uses — was never exercised against the Go server.
func TestNavfeederVerifiedTLS(t *testing.T) {
	cert := selfSignedSANs(t, []net.IP{net.ParseIP("127.0.0.1")}, nil)
	tc := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	runFeederE2EWith(t, false, tc, false, "--ca", writeCAPEM(t, cert))
}

// TestNavfeederTLSFailuresNameTheReason pins the diagnostic for a verifying feeder that
// cannot trust the collector. With SSL_VERIFY_PEER the verification failure aborts the
// handshake inside SSL_connect, so the log used to say only "collector TLS connect
// failed" every 30 s — a wrong --ca, an expired collector cert or a host whose cert lacks
// the right SAN were indistinguishable from a dead network. The OpenSSL verify verdict
// must be named. The collector presents a CA-issued leaf, as the fleet's does: that is
// what makes a wrong --ca read "unable to get local issuer certificate" (a self-signed
// leaf would read "self-signed certificate" instead).
func TestNavfeederTLSFailuresNameTheReason(t *testing.T) {
	bin := feederBinary(t)
	ca, otherCA := newTestCA(t), newTestCA(t)
	withIPSAN := ca.issue(t, []net.IP{net.ParseIP("127.0.0.1")}, nil)
	withoutIPSAN := ca.issue(t, nil, []string{"collector.invalid"})
	for _, tc := range []struct {
		name      string
		collector tls.Certificate
		ca        tls.Certificate
		want      string
	}{
		{"wrong --ca", withIPSAN, otherCA.pem, "unable to get local issuer certificate"},
		{"certificate without an IP SAN", withoutIPSAN, ca.pem, "IP address mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := make(chan *RawFrame, 64)
			cfg := &tls.Config{Certificates: []tls.Certificate{tc.collector}, MinVersion: tls.VersionTLS12}
			srv := newPushServer("127.0.0.1:0", cfg, out, tokenAuth("tls-e2e", "s3cret", "ubx"),
				25*time.Millisecond, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			pushLn, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer pushLn.Close()
			go func() { _ = srv.serve(ctx, pushLn) }()

			srcLn, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer srcLn.Close()
			go func() {
				for {
					c, err := srcLn.Accept()
					if err != nil {
						return
					}
					go func(c net.Conn) {
						defer c.Close()
						_, _ = c.Write(syntheticSFRBXCapture(4, 0))
						<-ctx.Done()
					}(c)
				}
			}()

			ferr := &syncBuffer{}
			cmd := exec.CommandContext(ctx, bin,
				"--server", pushLn.Addr().String(), "--source", srcLn.Addr().String(),
				"--station", "tls-e2e", "--token", "s3cret", "--feed", "ubx",
				"--ca", writeCAPEM(t, tc.ca), "--spool", "64")
			cmd.Stderr = ferr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = cmd.Process.Kill() })

			deadline := time.Now().Add(15 * time.Second)
			for !strings.Contains(ferr.String(), tc.want) {
				if time.Now().After(deadline) {
					t.Fatalf("the feeder never named %q; stderr:\n%s", tc.want, ferr.String())
				}
				time.Sleep(25 * time.Millisecond)
			}
			if !strings.Contains(ferr.String(), "collector TLS connect failed") {
				t.Errorf("the verify verdict is not attached to the connect failure line:\n%s", ferr.String())
			}
			if strings.Contains(ferr.String(), "s3cret") {
				t.Errorf("the TLS diagnostic leaked the credential")
			}
			select {
			case f := <-out:
				t.Fatalf("a frame (%v) reached the collector through an unverified session", f)
			default:
			}
		})
	}
}
