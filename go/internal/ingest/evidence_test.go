package ingest

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ptudor/gnss"
	"github.com/ptudor/navlistener/internal/authority"
	"github.com/ptudor/navlistener/internal/commissioning"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/metrics"
	"github.com/ptudor/navlistener/internal/wire"
)

// These tests drive the evidence exchange (docs/COMMISSIONING.md §6) over real
// TLS sessions, because the session proof is bound to keying material that only
// a real handshake produces.

const (
	evidenceObserver = "00-04-a3-aa-bb-cc-dd-ee"
	evidenceToken    = "s3cret"
)

// evidenceMCUKey is generated once per package: a 3072-bit RSA key is slow
// enough, especially under the race detector, that every test must share it.
var evidenceMCUKey = sync.OnceValue(func() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		panic(err)
	}
	return key
})

const testManufacturerAuthority = "test-manufacturer"

// evidenceBench stands in for the manufacturer: it signs commissioning records
// and registries, and hands out the verifier a collector would pin.
type evidenceBench struct {
	manufacturer, operations commissioning.Signer
	manufacturerKeys         *commissioning.KeySet
	operationsKeys           *commissioning.KeySet
	mcuKeyDER                []byte
}

func newEvidenceBench(t *testing.T) *evidenceBench {
	t.Helper()
	signer := func() (commissioning.Signer, *commissioning.KeySet) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		s, err := commissioning.NewKeySigner(key, nil)
		if err != nil {
			t.Fatal(err)
		}
		keys, err := commissioning.NewKeySet(&key.PublicKey)
		if err != nil {
			t.Fatal(err)
		}
		return s, keys
	}
	b := &evidenceBench{}
	b.manufacturer, b.manufacturerKeys = signer()
	b.operations, b.operationsKeys = signer()
	der, err := x509.MarshalPKIXPublicKey(&evidenceMCUKey().PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	b.mcuKeyDER = der
	return b
}

func (b *evidenceBench) statement(profile commissioning.Profile) commissioning.Statement {
	s := commissioning.Statement{
		Profile: profile, MCUFamily: commissioning.MCUESP32S3, Product: commissioning.ProductObserver,
		BoardRevision: 0x0102, Generation: 1, CommissionedAt: 1789646400,
		IdentityFlags: commissioning.IdentityRTCPresent | commissioning.IdentityRTCEUIBound,
		RTCModel:      commissioning.RTCModelMCP79412,
		ATECCSerial:   [9]byte{0x01, 0x23, 0x45, 0x67, 0x89, 0xab, 0xcd, 0xef, 0x11},
		RTCEUI64:      [8]byte{0x00, 0x04, 0xa3, 0x12, 0x34, 0x56, 0x78, 0x90},
		BoardEUI64:    [8]byte{0x00, 0x04, 0xa3, 0xaa, 0xbb, 0xcc, 0xdd, 0xee},
		MCUMAC:        [6]byte{0x34, 0x85, 0x18, 0x01, 0x02, 0x03},
		Attestation:   sha256.Sum256([]byte("slot 14 record")),
	}
	if profile == commissioning.ProfileTrusted {
		s.MCUKeyAlg, s.Security = commissioning.MCUKeyRSA3072PSS, commissioning.SecTrusted
		s.MCUKeySHA256 = sha256.Sum256(b.mcuKeyDER)
		s.SecureBootKeys = sha256.Sum256([]byte("secure boot key digests"))
	}
	return s
}

func (b *evidenceBench) record(t *testing.T, s commissioning.Statement) commissioning.Record {
	t.Helper()
	record, err := commissioning.Sign(s, b.manufacturer)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func (b *evidenceBench) verifier(t *testing.T) *commissioning.Verifier {
	t.Helper()
	v, err := commissioning.NewVerifier(testManufacturerAuthority, b.manufacturerKeys)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.SetProducts([]commissioning.ProductPolicy{{Product: 1, Revision: 258, RTCModels: []uint16{0, 1, 2}}}); err != nil {
		t.Fatal(err)
	}
	return v
}

// registry signs a registry listing record under status and loads it.
func (b *evidenceBench) loadRegistry(t *testing.T, v *commissioning.Verifier, sequence uint64, status string, record commissioning.Record) {
	t.Helper()
	s, err := record.Statement()
	if err != nil {
		t.Fatal(err)
	}
	rtcEUI := hex.EncodeToString(s.RTCEUI64[:])
	data, err := commissioning.SignRegistry(commissioning.Registry{
		ManufacturerAuthorityID: testManufacturerAuthority,
		Sequence:                sequence, IssuedAt: time.Unix(1789650000, 0).UTC(), LedgerHead: strings.Repeat("ab", 32),
		Boards: []commissioning.RegistryBoard{{
			BoardEUI64: hex.EncodeToString(s.BoardEUI64[:]), RTCModelID: uint16(s.RTCModel), RTCEUI64: &rtcEUI,
			ATECCSerial: hex.EncodeToString(s.ATECCSerial[:]), Status: status, Reason: "returned",
			Profile: s.Profile.String(), Generation: s.Generation, Record: base64.StdEncoding.EncodeToString(record[:]),
		}},
	}, b.operations)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.LoadRegistry(data); err != nil {
		t.Fatal(err)
	}
}

// exported is the keying material a device derives from its own TLS session.
func exported(t *testing.T, conn *tls.Conn) []byte {
	t.Helper()
	state := conn.ConnectionState()
	material, err := state.ExportKeyingMaterial(commissioning.ExporterLabel, nil, commissioning.ExporterLength)
	if err != nil {
		t.Fatal(err)
	}
	return material
}

// prove signs the session proof the way the microcontroller key would.
func prove(t *testing.T, material []byte, record commissioning.Record) []byte {
	t.Helper()
	digest, err := commissioning.ProofDigest(material, record)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := rsa.SignPSS(rand.Reader, evidenceMCUKey(), crypto.SHA256, digest[:], &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func startEvidenceServer(t *testing.T, ctx context.Context, verifier *commissioning.Verifier, recheck time.Duration) (string, chan *RawFrame) {
	t.Helper()
	out := make(chan *RawFrame, 32)
	tc := &tls.Config{Certificates: []tls.Certificate{selfSigned(t)}, MinVersion: tls.VersionTLS12}
	srv := newPushServer("127.0.0.1:0", tc, out, tokenAuth(evidenceObserver, evidenceToken, "ubx"), 25*time.Millisecond, 0,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv.SetEvidenceVerifier(commissioning.Authorities{testManufacturerAuthority: verifier})
	registered, err := authority.New([]authority.Operational{{ID: "local", Enabled: true, Manufacturers: []string{testManufacturerAuthority}}}, map[string]bool{testManufacturerAuthority: true}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	srv.SetAuthorities(registered)
	srv.auth = enrolledEvidenceAuth{srv.auth}
	srv.SetReauthorizationInterval(recheck)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.serve(ctx, ln) }()
	return ln.Addr().String(), out
}

type enrolledEvidenceAuth struct{ Authenticator }

func (a enrolledEvidenceAuth) Authenticate(ctx context.Context, token, station, feed string) (identity.ObserverContext, bool) {
	c, ok := a.Authenticator.Authenticate(ctx, token, station, feed)
	c.ManufacturerAuthorityID = testManufacturerAuthority
	c.HardwareProduct, c.HardwareRevision = 1, 258
	fp := sha256.Sum256([]byte("slot 14 record"))
	c.CoreAttestationFingerprint = hex.EncodeToString(fp[:])
	return c, ok
}

// hello sends HELLO and, when payload is non-nil, the EVIDENCE frame straight
// after it without waiting — one flight, as a device does — then reads WELCOME.
func hello(t *testing.T, conn *tls.Conn, token, session string, payload []byte) (wire.WelcomeMsg, []byte) {
	t.Helper()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: token, Station: evidenceObserver, Feed: "ubx", Session: session, Evidence: payload != nil}); err != nil {
		t.Fatal(err)
	}
	if payload != nil {
		if err := wire.WriteFrame(conn, wire.Evidence, payload); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	ft, raw, err := wire.ReadFrame(conn)
	if err != nil || ft != wire.Welcome {
		t.Fatalf("welcome frame: ft=%d err=%v", ft, err)
	}
	welcome, err := parseWelcome(raw)
	if err != nil {
		t.Fatal(err)
	}
	return welcome, raw
}

func marshalEvidence(t *testing.T, e commissioning.Evidence) []byte {
	t.Helper()
	payload, err := e.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// sendData pushes one nav record and returns the frame the decode stage got.
func sendData(t *testing.T, conn *tls.Conn, out chan *RawFrame, seq uint64) *RawFrame {
	t.Helper()
	rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(seq, rec)); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case f := <-out:
			if f.ScopeRevocation != nil {
				t.Fatalf("hardware evidence caused an audience reset: %+v", f.ScopeRevocation)
			}
			return f
		case <-time.After(5 * time.Second):
			t.Fatal("frame did not reach the decode channel")
		}
	}
}

func rejected(reason string) float64 {
	return testutil.ToFloat64(metrics.PushEvidenceRejectedTotal.WithLabelValues(reason))
}

func TestPushEvidenceTrusted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	record := bench.record(t, bench.statement(commissioning.ProfileTrusted))
	addr, out := startEvidenceServer(t, ctx, bench.verifier(t), time.Hour)
	fingerprint := record.Fingerprint()

	for name, maxVersion := range map[string]uint16{"TLS 1.3": tls.VersionTLS13, "TLS 1.2 with the extended master secret": tls.VersionTLS12} {
		t.Run(name, func(t *testing.T) {
			sessions := testutil.ToFloat64(metrics.PushHardwareTrustSessionsTotal.WithLabelValues("trusted"))
			conn, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true, MaxVersion: maxVersion})
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := wire.WriteMagic(conn); err != nil {
				t.Fatal(err)
			}
			payload := marshalEvidence(t, commissioning.Evidence{Record: record, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), record)})
			welcome, raw := hello(t, conn, evidenceToken, "boot-trusted", payload)
			if !welcome.OK || welcome.HardwareTrust != "trusted" || welcome.EvidenceError != "" {
				t.Fatalf("welcome = %s", raw)
			}
			if !strings.Contains(string(raw), `"ok":true`) {
				t.Fatalf("welcome lost the compact acceptance sequence: %s", raw)
			}
			f := sendData(t, conn, out, 1)
			if f.Observer.HardwareTrust != identity.HardwareTrustTrusted ||
				f.Observer.ManufacturerAuthorityID != testManufacturerAuthority ||
				f.Observer.CommissioningFingerprint != hex.EncodeToString(fingerprint[:]) {
				t.Fatalf("receipt context = %q / %q / %q", f.Observer.HardwareTrust,
					f.Observer.ManufacturerAuthorityID, f.Observer.CommissioningFingerprint)
			}
			if got := testutil.ToFloat64(metrics.PushHardwareTrustSessionsTotal.WithLabelValues("trusted")); got != sessions+1 {
				t.Errorf("trusted sessions counted = %v, want %v", got, sessions+1)
			}
		})
	}
}

func TestPushEvidenceOpenBoardNeedsNoProof(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	record := bench.record(t, bench.statement(commissioning.ProfileOpen))
	addr, out := startEvidenceServer(t, ctx, bench.verifier(t), time.Hour)

	conn := dialPush(t, addr)
	defer conn.Close()
	welcome, raw := hello(t, conn, evidenceToken, "boot-open", marshalEvidence(t, commissioning.Evidence{Record: record}))
	if !welcome.OK || welcome.HardwareTrust != "open" || welcome.EvidenceError != "" {
		t.Fatalf("welcome = %s", raw)
	}
	if f := sendData(t, conn, out, 1); f.Observer.HardwareTrust != identity.HardwareTrustOpen {
		t.Fatalf("receipt hardware trust = %q", f.Observer.HardwareTrust)
	}
}

func TestPushEvidenceWithoutRTC(t *testing.T) {
	for _, profile := range []commissioning.Profile{commissioning.ProfileTrusted, commissioning.ProfileOpen, commissioning.ProfileTest} {
		t.Run(profile.String(), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			bench := newEvidenceBench(t)
			statement := bench.statement(profile)
			statement.IdentityFlags = 0
			statement.RTCModel = commissioning.RTCModelNone
			statement.RTCEUI64 = [8]byte{}
			record := bench.record(t, statement)
			addr, out := startEvidenceServer(t, ctx, bench.verifier(t), time.Hour)
			conn := dialPush(t, addr)
			defer conn.Close()
			evidence := commissioning.Evidence{Record: record}
			if profile == commissioning.ProfileTrusted {
				evidence.MCUKey = bench.mcuKeyDER
				evidence.Proof = prove(t, exported(t, conn), record)
			}
			welcome, raw := hello(t, conn, evidenceToken, "no-rtc", marshalEvidence(t, evidence))
			if !welcome.OK || welcome.HardwareTrust != profile.String() {
				t.Fatalf("welcome %s", raw)
			}
			frame := sendData(t, conn, out, 1)
			if string(frame.Observer.HardwareTrust) != profile.String() || frame.Details != nil {
				t.Fatal("no-RTC frame lost trust or fabricated auxiliary data")
			}
		})
	}
}

func TestPushWithoutEvidenceIsUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	addr, out := startEvidenceServer(t, ctx, bench.verifier(t), time.Hour)

	conn := dialPush(t, addr)
	defer conn.Close()
	welcome, raw := hello(t, conn, evidenceToken, "boot-plain", nil)
	if !welcome.OK {
		t.Fatalf("welcome = %s", raw)
	}
	if strings.Contains(string(raw), "hardware_trust") || strings.Contains(string(raw), "evidence_error") {
		t.Fatalf("welcome to a feeder that announced no evidence mentions it: %s", raw)
	}
	f := sendData(t, conn, out, 1)
	if f.Observer.HardwareTrust != identity.HardwareTrustNone || f.Observer.ManufacturerAuthorityID != testManufacturerAuthority || f.Observer.CommissioningFingerprint != "" {
		t.Fatalf("receipt context = %q / %q", f.Observer.HardwareTrust, f.Observer.CommissioningFingerprint)
	}
}

func TestPushEvidenceFlagWithoutTheFrameIsAProtocolError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	addr, out := startEvidenceServer(t, ctx, bench.verifier(t), time.Hour)

	conn := dialPush(t, addr)
	defer conn.Close()
	if err := wire.WriteHello(conn, wire.HelloMsg{Token: evidenceToken, Station: evidenceObserver, Feed: "ubx", Session: "boot-skip", Evidence: true}); err != nil {
		t.Fatal(err)
	}
	rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), GnssID: gnss.GPS, SvID: 5, Raw: make([]byte, 40)}
	if err := wire.WriteFrame(conn, wire.Data, wire.EncodeData(1, rec)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, raw, err := wire.ReadFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	if welcome, _ := parseWelcome(raw); welcome.OK || welcome.Error != "expected evidence" {
		t.Fatalf("welcome = %s", raw)
	}
	if _, _, err := wire.ReadFrame(conn); err == nil {
		t.Fatal("connection stayed open after the protocol error")
	}
	select {
	case f := <-out:
		t.Fatalf("refused session enqueued a frame: %+v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPushEvidenceRejectionsLabelTheSessionAndNeverRefuseIt covers every way
// authenticated evidence can prove nothing: the session is admitted, its data
// flows as hardware_trust none, and WELCOME says why.
func TestPushEvidenceRejectionsLabelTheSessionAndNeverRefuseIt(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	trusted := bench.record(t, bench.statement(commissioning.ProfileTrusted))

	elsewhere := bench.statement(commissioning.ProfileTrusted)
	elsewhere.RTCEUI64[7], elsewhere.BoardEUI64[7] = 0x91, 0xef
	otherObserver := bench.record(t, elsewhere)

	stranger := newEvidenceBench(t)
	forged := stranger.record(t, stranger.statement(commissioning.ProfileTrusted))

	sibling := bench.statement(commissioning.ProfileTrusted)
	sibling.Product = commissioning.ProductObserver + 1
	otherProduct := bench.record(t, sibling)

	revoking := bench.verifier(t)
	if err := revoking.UseRegistry(bench.operationsKeys, false); err != nil {
		t.Fatal(err)
	}
	bench.loadRegistry(t, revoking, 1, commissioning.StatusRevoked, trusted)

	superseding := bench.verifier(t)
	if err := superseding.UseRegistry(bench.operationsKeys, false); err != nil {
		t.Fatal(err)
	}
	replacement := bench.statement(commissioning.ProfileTrusted)
	replacement.Generation = 2
	bench.loadRegistry(t, superseding, 1, commissioning.StatusActive, bench.record(t, replacement))

	for name, tc := range map[string]struct {
		verifier *commissioning.Verifier
		evidence func(t *testing.T, conn *tls.Conn) []byte
		reason   string
	}{
		"record for a different observer": {bench.verifier(t), func(t *testing.T, conn *tls.Conn) []byte {
			return marshalEvidence(t, commissioning.Evidence{Record: otherObserver, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), otherObserver)})
		}, commissioning.ReasonIdentity},
		"record signed by an unpinned manufacturer": {bench.verifier(t), func(t *testing.T, conn *tls.Conn) []byte {
			return marshalEvidence(t, commissioning.Evidence{Record: forged, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), forged)})
		}, commissioning.ReasonSignature},
		"genuine record for another product line": {bench.verifier(t), func(t *testing.T, conn *tls.Conn) []byte {
			return marshalEvidence(t, commissioning.Evidence{Record: otherProduct, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), otherProduct)})
		}, commissioning.ReasonProduct},
		"board revoked by the registry": {revoking, func(t *testing.T, conn *tls.Conn) []byte {
			return marshalEvidence(t, commissioning.Evidence{Record: trusted, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), trusted)})
		}, commissioning.ReasonRevoked},
		"record superseded by a replacement microcontroller": {superseding, func(t *testing.T, conn *tls.Conn) []byte {
			return marshalEvidence(t, commissioning.Evidence{Record: trusted, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), trusted)})
		}, commissioning.ReasonSuperseded},
		"trusted record without a proof": {bench.verifier(t), func(t *testing.T, _ *tls.Conn) []byte {
			return marshalEvidence(t, commissioning.Evidence{Record: trusted})
		}, commissioning.ReasonProofMissing},
		"collector pins no manufacturer keys": {nil, func(t *testing.T, conn *tls.Conn) []byte {
			return marshalEvidence(t, commissioning.Evidence{Record: trusted, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), trusted)})
		}, commissioning.ReasonUnconfigured},
		"malformed payload": {bench.verifier(t), func(*testing.T, *tls.Conn) []byte {
			return []byte{commissioning.EvidenceVersionV1, 0x00}
		}, commissioning.ReasonMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			addr, out := startEvidenceServer(t, ctx, tc.verifier, time.Hour)
			before := rejected(tc.reason)
			conn := dialPush(t, addr)
			defer conn.Close()
			welcome, raw := hello(t, conn, evidenceToken, "boot-rejected", tc.evidence(t, conn))
			if !welcome.OK || welcome.HardwareTrust != "none" || welcome.EvidenceError != tc.reason {
				t.Fatalf("welcome = %s, want ok with none/%s", raw, tc.reason)
			}
			f := sendData(t, conn, out, 1)
			if f.Observer.HardwareTrust != identity.HardwareTrustNone || f.Observer.ManufacturerAuthorityID != testManufacturerAuthority || f.Observer.CommissioningFingerprint != "" {
				t.Fatalf("receipt context = %q / %q", f.Observer.HardwareTrust, f.Observer.CommissioningFingerprint)
			}
			if got := rejected(tc.reason); got != before+1 {
				t.Errorf("rejections{%s} = %v, want %v", tc.reason, got, before+1)
			}
		})
	}
}

// TestPushEvidenceProofCannotBeRelayed: a proof a genuine unit made on one TLS
// session is worthless on another, which is what stops an unlocked board from
// borrowing a locked one's proof through a connection it controls.
func TestPushEvidenceProofCannotBeRelayed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	record := bench.record(t, bench.statement(commissioning.ProfileTrusted))
	addr, _ := startEvidenceServer(t, ctx, bench.verifier(t), time.Hour)

	genuine := dialPush(t, addr)
	defer genuine.Close()
	relay := dialPush(t, addr)
	defer relay.Close()
	borrowed := prove(t, exported(t, genuine), record)
	welcome, raw := hello(t, relay, evidenceToken, "boot-relay", marshalEvidence(t, commissioning.Evidence{Record: record, MCUKey: bench.mcuKeyDER, Proof: borrowed}))
	if !welcome.OK || welcome.HardwareTrust != "none" || welcome.EvidenceError != commissioning.ReasonProof {
		t.Fatalf("relayed proof: welcome = %s", raw)
	}
	// The same proof is good on the session it was made for.
	welcome, raw = hello(t, genuine, evidenceToken, "boot-genuine", marshalEvidence(t, commissioning.Evidence{Record: record, MCUKey: bench.mcuKeyDER, Proof: borrowed}))
	if !welcome.OK || welcome.HardwareTrust != "trusted" {
		t.Fatalf("genuine session: welcome = %s", raw)
	}
}

// TestPushEvidenceIsNeverParsedBeforeAuthentication: an unauthenticated peer
// cannot make the collector parse a record or verify a signature. The evidence
// here is malformed, so parsing it would have counted a rejection.
func TestPushEvidenceIsNeverParsedBeforeAuthentication(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	addr, out := startEvidenceServer(t, ctx, bench.verifier(t), time.Hour)
	before := testutil.CollectAndCount(metrics.PushEvidenceRejectedTotal)
	malformed := rejected(commissioning.ReasonMalformed)

	conn := dialPush(t, addr)
	defer conn.Close()
	welcome, raw := hello(t, conn, "wrong-token", "boot-unauthenticated", []byte{commissioning.EvidenceVersionV1, 0x00})
	if welcome.OK || welcome.Error != "unauthorized" || welcome.HardwareTrust != "" || welcome.EvidenceError != "" {
		t.Fatalf("welcome = %s", raw)
	}
	if got := rejected(commissioning.ReasonMalformed); got != malformed || testutil.CollectAndCount(metrics.PushEvidenceRejectedTotal) != before {
		t.Fatal("evidence from an unauthenticated peer was evaluated")
	}
	select {
	case f := <-out:
		t.Fatalf("unauthenticated session enqueued a frame: %+v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestPushEvidenceSurvivesRecheckAndSharesOnePolicy: the periodic authorization
// recheck resolves no evidence, and a second session of the same observer may
// prove something different. Neither is a policy change: no session is closed
// and no audience reset is enqueued.
func TestPushEvidenceSurvivesRecheckAndSharesOnePolicy(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	record := bench.record(t, bench.statement(commissioning.ProfileTrusted))
	addr, out := startEvidenceServer(t, ctx, bench.verifier(t), 10*time.Millisecond)

	commissioned := dialPush(t, addr)
	defer commissioned.Close()
	payload := marshalEvidence(t, commissioning.Evidence{Record: record, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, commissioned), record)})
	if welcome, raw := hello(t, commissioned, evidenceToken, "boot-a", payload); welcome.HardwareTrust != "trusted" {
		t.Fatalf("welcome = %s", raw)
	}
	plain := dialPush(t, addr)
	defer plain.Close()
	if welcome, raw := hello(t, plain, evidenceToken, "boot-b", nil); !welcome.OK {
		t.Fatalf("second session: welcome = %s", raw)
	}
	time.Sleep(150 * time.Millisecond) // many recheck intervals
	if f := sendData(t, commissioned, out, 1); f.Observer.HardwareTrust != identity.HardwareTrustTrusted {
		t.Fatalf("commissioned session receipt = %q", f.Observer.HardwareTrust)
	}
	if f := sendData(t, plain, out, 1); f.Observer.HardwareTrust != identity.HardwareTrustNone {
		t.Fatalf("plain session receipt = %q", f.Observer.HardwareTrust)
	}
}

// TestPushRegistryWithdrawalEndsALiveSession: a board revoked while connected
// does not keep its trust until it happens to reconnect. Its session is closed
// at the next recheck and the reconnect is evaluated against the new registry.
func TestPushRegistryWithdrawalEndsALiveSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bench := newEvidenceBench(t)
	record := bench.record(t, bench.statement(commissioning.ProfileTrusted))
	verifier := bench.verifier(t)
	if err := verifier.UseRegistry(bench.operationsKeys, false); err != nil {
		t.Fatal(err)
	}
	bench.loadRegistry(t, verifier, 1, commissioning.StatusActive, record)
	addr, out := startEvidenceServer(t, ctx, verifier, 10*time.Millisecond)

	conn := dialPush(t, addr)
	defer conn.Close()
	payload := marshalEvidence(t, commissioning.Evidence{Record: record, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, conn), record)})
	if welcome, raw := hello(t, conn, evidenceToken, "boot-a", payload); welcome.HardwareTrust != "trusted" {
		t.Fatalf("welcome = %s", raw)
	}
	sendData(t, conn, out, 1)

	before := rejected(commissioning.ReasonRevoked)
	bench.loadRegistry(t, verifier, 2, commissioning.StatusRevoked, record)
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		if _, _, err := wire.ReadFrame(conn); err != nil {
			break // closed by the collector; a deadline error fails the check below
		}
	}
	if got := rejected(commissioning.ReasonRevoked); got != before+1 {
		t.Fatalf("live withdrawal counted %v revocations, want %v", got, before+1)
	}
	select {
	case f := <-out:
		if f.ScopeRevocation != nil {
			t.Fatal("a registry withdrawal reset the observer's audiences")
		}
	case <-time.After(50 * time.Millisecond):
	}

	again := dialPush(t, addr)
	defer again.Close()
	payload = marshalEvidence(t, commissioning.Evidence{Record: record, MCUKey: bench.mcuKeyDER, Proof: prove(t, exported(t, again), record)})
	welcome, raw := hello(t, again, evidenceToken, "boot-a", payload)
	if !welcome.OK || welcome.HardwareTrust != "none" || welcome.EvidenceError != commissioning.ReasonRevoked {
		t.Fatalf("reconnect after revocation: welcome = %s", raw)
	}
}
