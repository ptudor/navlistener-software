package config

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
	"strings"
	"testing"
	"time"

	"github.com/ptudor/navlistener/internal/identity"
)

const goodHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// testKeypair generates a self-signed cert+key, writes them to temp files, and returns the
// cert path and key path. regression fix makes finalizePush actually load the TLS keypair, so push
// tests need real files. The self-signed cert also serves as a valid client_ca PEM.
func testKeypair(t *testing.T) (certPath, keyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath = filepath.Join(dir, "cert.pem")
	keyPath = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func pushConfig(p Push) *Config {
	c := defaults()
	if p.MaxConns == 0 {
		p.MaxConns = c.Push.MaxConns
	}
	c.Push = p
	return c
}

// TestPushDisabled: with no addr the push endpoint is a no-op and validation passes,
// with the ack interval defaulted.
func TestPushDisabled(t *testing.T) {
	c := pushConfig(Push{})
	if err := c.finalizePush(); err != nil {
		t.Fatalf("disabled push should validate: %v", err)
	}
	if c.Push.AckInterval <= 0 {
		t.Errorf("ack interval not defaulted: %v", c.Push.AckInterval)
	}
}

// TestSnapshotInterval: an unset snapshot_interval defaults to 5m; an explicit "0s"
// disables it; a valid duration is honoured; garbage is a hard error.
func TestSnapshotInterval(t *testing.T) {
	def := defaults()
	if err := def.finalize(); err != nil {
		t.Fatalf("defaults should finalize: %v", err)
	}
	if def.Serve.SnapshotEvery != 5*time.Minute {
		t.Errorf("unset snapshot_interval = %v, want 5m", def.Serve.SnapshotEvery)
	}

	off := defaults()
	off.Serve.SnapshotEverys = "0s"
	if err := off.finalize(); err != nil || off.Serve.SnapshotEvery != 0 {
		t.Errorf("explicit 0s should disable, got %v (err %v)", off.Serve.SnapshotEvery, err)
	}

	custom := defaults()
	custom.Serve.SnapshotEverys = "2m"
	if err := custom.finalize(); err != nil || custom.Serve.SnapshotEvery != 2*time.Minute {
		t.Errorf("2m not honoured: %v (err %v)", custom.Serve.SnapshotEvery, err)
	}

	bad := defaults()
	bad.Serve.SnapshotEverys = "nonsense"
	if err := bad.finalize(); err == nil {
		t.Error("garbage snapshot_interval accepted, want error")
	}
}

func TestServeAudienceFailsClosed(t *testing.T) {
	def := defaults()
	if err := def.finalize(); err != nil {
		t.Fatal(err)
	}
	if def.Serve.Audience != "public" || def.Serve.AudienceContext.Kind != identity.AudiencePublic {
		t.Fatalf("default serve audience = %+v / %q, want public", def.Serve.AudienceContext, def.Serve.Audience)
	}

	op := defaults()
	op.Serve.Audience = "operator"
	if err := op.finalize(); err != nil || op.Serve.AudienceContext.Kind != identity.AudienceOperator {
		t.Fatalf("operator audience rejected: %+v (err %v)", op.Serve.AudienceContext, err)
	}

	for _, unsafe := range []string{"organization:customer-a", "collection:public", "anything"} {
		c := defaults()
		c.Serve.Audience = unsafe
		if err := c.finalize(); err == nil {
			t.Errorf("unauthenticated audience %q accepted", unsafe)
		}
	}
}

func TestDatabaseAuthorizationIsBoundedAndHasNoStaticFallback(t *testing.T) {
	c := defaults()
	c.Authorization.DSN = "postgres://navlistener:secret@localhost/controlplane"
	if err := c.finalize(); err != nil {
		t.Fatalf("database authorization config rejected: %v", err)
	}
	if c.Authorization.CacheTTL != 30*time.Second || c.Authorization.RecheckEvery != 10*time.Second {
		t.Fatalf("authorization bounds = %v/%v", c.Authorization.CacheTTL, c.Authorization.RecheckEvery)
	}
	if !c.holdsSecrets() {
		t.Fatal("authorization DSN was not classified as config secret material")
	}

	ambiguous := defaults()
	ambiguous.Authorization.DSN = c.Authorization.DSN
	ambiguous.Push.Observers = []PushObserver{{Station: "observer-a"}}
	if err := ambiguous.finalize(); err == nil {
		t.Fatal("database authorization accepted a static credential fallback")
	}

	unbounded := defaults()
	unbounded.Authorization.CacheTTLs = "6m"
	if err := unbounded.finalize(); err == nil {
		t.Fatal("authorization cache TTL beyond revocation bound accepted")
	}
}

// TestLeapSecondsValidated guards state.leap_seconds is an interim override
// for the compiled-in ΔtLS default; unset (0) must pass validation as a no-op, an
// ICD-plausible value must pass, and an out-of-band value (a fat-fingered config,
// not a real leap-second schedule) must be rejected.
func TestLeapSecondsValidated(t *testing.T) {
	unset := defaults()
	if err := unset.finalize(); err != nil {
		t.Fatalf("state.leap_seconds unset (0) should validate: %v", err)
	}

	good := defaults()
	good.State.LeapSeconds = 19
	if err := good.finalize(); err != nil {
		t.Errorf("state.leap_seconds = 19 should validate: %v", err)
	}

	bad := defaults()
	bad.State.LeapSeconds = 5
	if err := bad.finalize(); err == nil {
		t.Error("state.leap_seconds = 5 (outside 10-30) accepted, want error")
	}
}

// TestLoggingLevelValidated guards unlike logging.format, logging.level was
// never validated, so a typo ("trace"/"warning") silently mapped to info with no
// diagnostic. The empty-string default and the four real levels must still pass.
func TestLoggingLevelValidated(t *testing.T) {
	for _, level := range []string{"", "debug", "info", "warn", "error"} {
		c := defaults()
		c.Logging.Level = level
		if err := c.finalize(); err != nil {
			t.Errorf("logging.level %q should validate, got %v", level, err)
		}
	}
	for _, bad := range []string{"trace", "warning", "DEBUG", " info"} {
		c := defaults()
		c.Logging.Level = bad
		if err := c.finalize(); err == nil {
			t.Errorf("logging.level %q accepted, want a hard error", bad)
		}
	}
}

// TestStoreIntervalsValidatedAtFinalize guards raw_retention/compress_after
// were only checked when the store actually connected, so `-check-config` reported
// a malformed interval (or an injection attempt into the later policy DDL) as
// valid. finalize (which -check-config runs) must catch it up front, using the
// same config.IntervalRe the store re-checks in applyPolicies.
func TestStoreIntervalsValidatedAtFinalize(t *testing.T) {
	for _, good := range []string{"", "7 days", "1 hour", "30 minutes", "2 weeks"} {
		c := defaults()
		c.Store.RawRetention, c.Store.CompressAfter = good, ""
		if err := c.finalize(); err != nil {
			t.Errorf("raw retention interval %q should validate, got %v", good, err)
		}
		c = defaults()
		c.Store.RawRetention, c.Store.CompressAfter = "", good
		if err := c.finalize(); err != nil {
			t.Errorf("compression interval %q should validate, got %v", good, err)
		}
	}
	for _, bad := range []string{"soon", "; DROP TABLE nav_frames;--", "7", "days", "0 days"} {
		c := defaults()
		c.Store.RawRetention = bad
		if err := c.finalize(); err == nil {
			t.Errorf("store.raw_retention %q accepted, want a hard error", bad)
		}
		c = defaults()
		c.Store.CompressAfter = bad
		if err := c.finalize(); err == nil {
			t.Errorf("store.compress_after %q accepted, want a hard error", bad)
		}
	}
}

func TestStoreCompressionMustPrecedeRetention(t *testing.T) {
	bad := defaults()
	bad.Store.RawRetention, bad.Store.CompressAfter = "7 days", "8 days"
	if err := bad.finalize(); err == nil || !strings.Contains(err.Error(), "dropped before compression") {
		t.Fatalf("misordered policies error = %v, want actionable rejection", err)
	}
	equal := defaults()
	equal.Store.RawRetention, equal.Store.CompressAfter = "1 day", "1 day"
	if err := equal.finalize(); err == nil {
		t.Fatal("equal compression/retention ages accepted")
	}
	if err := defaults().finalize(); err != nil {
		t.Fatalf("default 1 day < 7 days rejected: %v", err)
	}
}

func TestNTRIPFieldsRejectedOnOtherSourceTypes(t *testing.T) {
	cases := []struct {
		name  string
		src   Source
		field string
	}{
		{"rtcm mountpoint", Source{Name: "s", Type: "rtcm", Addr: "host:1", CaptureOnly: true, Mountpoint: "M"}, "mountpoint"},
		{"ubx username", Source{Name: "s", Type: "ubx", Addr: "host:1", Username: "u"}, "username"},
		{"sbf plaintext opt-in", Source{Name: "s", Type: "sbf", Addr: "host:1", CaptureOnly: true, AllowInsecurePlaintext: true}, "allow_insecure_plaintext"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := defaults()
			c.Ingest = []Source{tc.src}
			if err := c.finalize(); err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("error = %v, want field %q", err, tc.field)
			}
		})
	}
}

func TestListenerAddressPortsValidated(t *testing.T) {
	for _, field := range []string{"metrics.addr", "serve.addr", "push.addr"} {
		for _, addr := range []string{"127.0.0.1:9I00", "127.0.0.1:99999", "127.0.0.1:", "127.0.0.1:0"} {
			if err := validateAddr(field, addr); err == nil || !strings.Contains(err.Error(), field) {
				t.Errorf("%s %q error = %v, want field-named rejection", field, addr, err)
			}
		}
		for _, addr := range []string{"127.0.0.1:9100", ":5580", "localhost:http"} {
			if err := validateAddr(field, addr); err != nil {
				t.Errorf("%s %q rejected: %v", field, addr, err)
			}
		}
	}
}

// TestCapabilitiesParsing: valid "gnss:sig" tuples parse; malformed, out-of-range, and
// duplicate declarations are hard errors so a mistyped fingerprint can't silently disarm the
// plausibility detector.
func TestCapabilitiesParsing(t *testing.T) {
	got, err := parseCapabilities([]string{"0:0", "2:3", " 6 : 0 "})
	if err != nil {
		t.Fatalf("valid capabilities rejected: %v", err)
	}
	want := []Capability{{0, 0}, {2, 3}, {6, 0}}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("cap[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	for _, bad := range []string{"2", "2:", ":3", "8:0", "x:0", "0:999", "2:3:4", ""} {
		if _, err := parseCapabilities([]string{bad}); err == nil {
			t.Errorf("capability %q accepted, want rejected", bad)
		}
	}
	if _, err := parseCapabilities([]string{"2:0", "2:0"}); err == nil {
		t.Error("duplicate capability accepted, want rejected")
	}
}

// TestNtripSource: an ntrip dial source requires a mountpoint; a valid one passes; ntrip is a
// dial type but not a push feed grant.
func TestNtripSource(t *testing.T) {
	c := defaults()
	c.Ingest = []Source{{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", CaptureOnly: true}} // no mountpoint
	if err := c.finalize(); err == nil || !strings.Contains(err.Error(), "mountpoint") {
		t.Fatalf("ntrip without mountpoint should fail on mountpoint, got %v", err)
	}

	ok := defaults()
	ok.Ingest = []Source{{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "P472_RTCM3", CaptureOnly: true}}
	if err := ok.finalize(); err != nil {
		t.Fatalf("valid ntrip source rejected: %v", err)
	}

	// a mountpoint containing CRLF/space injects headers or mangles the
	// caster's HTTP request line -- the fix spec's exact PoC.
	bad := defaults()
	bad.Ingest = []Source{{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "P472 RTCM3\r\nX-Evil: 1", CaptureOnly: true}}
	if err := bad.finalize(); err == nil || !strings.Contains(err.Error(), "mountpoint") {
		t.Fatalf("ntrip mountpoint with whitespace/CRLF should fail validation, got %v", err)
	}

	// ntrip is not a valid push feed grant (a feeder can't push "ntrip").
	if knownIngestTypes["ntrip"] {
		t.Error("ntrip must not be a push feed type")
	}
}

func TestNtripTransportSecurityValidation(t *testing.T) {
	secure := defaults()
	secure.Ingest = []Source{{Name: "secure", Type: "ntrip", Addr: "caster.invalid:443", Mountpoint: "M", Username: "u", Password: "p", CaptureOnly: true}}
	if err := secure.finalize(); err != nil {
		t.Fatalf("TLS-default NTRIP credentials rejected: %v", err)
	}
	plain := defaults()
	plain.Ingest = []Source{{Name: "plain", Type: "ntrip", Addr: "caster.invalid:2101", Mountpoint: "M", AllowInsecurePlaintext: true, CaptureOnly: true}}
	if err := plain.finalize(); err != nil {
		t.Fatalf("explicit credential-free plaintext rejected: %v", err)
	}
	plain.Ingest[0].Username = "u"
	if err := plain.finalize(); err == nil || !strings.Contains(err.Error(), "allow_plaintext_credentials") {
		t.Fatalf("plaintext credentials accepted without separate approval: %v", err)
	}
	plain.Ingest[0].AllowPlaintextCredentials = true
	if err := plain.finalize(); err != nil {
		t.Fatalf("explicitly approved plaintext credentials rejected: %v", err)
	}
}

func TestByteSourcesRequireExplicitCaptureOnly(t *testing.T) {
	for _, typ := range []string{"sbf", "rtcm", "ntrip"} {
		c := defaults()
		s := Source{Name: typ, Type: typ, Addr: "127.0.0.1:1"}
		if typ == "ntrip" {
			s.Mountpoint = "M"
		}
		c.Ingest = []Source{s}
		if err := c.finalize(); err == nil || !strings.Contains(err.Error(), "capture_only") {
			t.Fatalf("%s without capture_only accepted: %v", typ, err)
		}
		c.Ingest[0].CaptureOnly = true
		if err := c.finalize(); err != nil {
			t.Fatalf("explicit capture-only %s rejected: %v", typ, err)
		}
	}
}

// TestPushRequiresTLS: enabling the endpoint without cert/key is a hard error —
// unauthenticated feeder ingest must never be possible.
func TestPushRequiresTLS(t *testing.T) {
	c := pushConfig(Push{Addr: "0.0.0.0:5580"})
	err := c.finalizePush()
	if err == nil || !strings.Contains(err.Error(), "tls_cert") {
		t.Fatalf("err = %v, want a tls requirement error", err)
	}
}

// TestPushTokenHashValidated: a token hash that isn't 64 hex chars is rejected, so a
// truncated or placeholder credential can't slip into the observer table.
func TestPushTokenHashValidated(t *testing.T) {
	base := Push{Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem"}
	for _, bad := range []string{"deadbeef", strings.Repeat("z", 64)} {
		c := pushConfig(base)
		c.Push.Observers = []PushObserver{{Station: "s", TokenSHA256: bad, Feeds: []string{"ubx"}}}
		if err := c.finalizePush(); err == nil {
			t.Errorf("token %q accepted, want rejected", bad)
		}
	}
}

// TestPushFeedGrantValidated: an observer must name at least one known feed type.
func TestPushFeedGrantValidated(t *testing.T) {
	base := Push{Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem"}
	c := pushConfig(base)
	c.Push.Observers = []PushObserver{{Station: "s", TokenSHA256: goodHash, Feeds: []string{"nmea"}}}
	if err := c.finalizePush(); err == nil {
		t.Error("unknown feed type accepted")
	}
	c.Push.Observers[0].Feeds = nil
	if err := c.finalizePush(); err == nil {
		t.Error("empty feed grant accepted")
	}
}

// TestPushSBFFeedGrantRejected guards GNF1's 1-byte frame_type field
// cannot carry an SBF block number, so an sbf push grant must be rejected at
// config validation until a block-number carriage is defined -- dial-mode sbf
// (a separate, decoder-owned connector) is unaffected.
func TestPushSBFFeedGrantRejected(t *testing.T) {
	c := pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem"})
	c.Push.Observers = []PushObserver{{Station: "s", TokenSHA256: goodHash, Feeds: []string{"sbf"}}}
	if err := c.finalizePush(); err == nil {
		t.Error("sbf push feed grant accepted, want rejected (no block-number carriage yet)")
	}
	// Dial-mode sbf remains valid -- this finding is push-only.
	ok := defaults()
	ok.Ingest = []Source{{Name: "sbf-recv", Type: "sbf", Addr: "127.0.0.1:5555", CaptureOnly: true}}
	if err := ok.finalize(); err != nil {
		t.Errorf("dial-mode sbf source rejected: %v (should be unaffected by the push-only regression fix restriction)", err)
	}
}

// TestPushValid: a complete, well-formed observer table validates.
func TestPushValid(t *testing.T) {
	cert, key := testKeypair(t)
	c := pushConfig(Push{
		Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key, AckIntervals: "500ms",
		Observers: []PushObserver{
			{Station: "observer16", TokenSHA256: goodHash, Feeds: []string{"ubx", "rtcm"}},
		},
	})
	if err := c.finalizePush(); err != nil {
		t.Fatalf("valid push config rejected: %v", err)
	}
	if c.Push.AckInterval.String() != "500ms" {
		t.Errorf("ack interval = %v, want 500ms", c.Push.AckInterval)
	}
	ctx := c.Push.Observers[0].ObserverContext
	if ctx.OrganizationID != identity.UnassignedOrganization || ctx.PublicEligible() {
		t.Errorf("omitted observer policy widened beyond private/unassigned: %+v", ctx)
	}
}

func TestObserverPublicationContextValidation(t *testing.T) {
	c, err := finalizeObserverContext(
		"observer16", "institution-c", "enrollment-1", "hosted-west",
		[]string{"institution-c-roof", "public-community"},
		"public_attributed", "coarse", "public", "named_peers", []string{"peer-b"}, []string{"0:0", "2:3"},
		"policy-3", identity.CredentialToken,
	)
	if err != nil {
		t.Fatalf("valid publication context rejected: %v", err)
	}
	if !c.PublicEligible() || !c.PublicAttributed() || c.OrganizationID != "institution-c" {
		t.Fatalf("publication context normalized incorrectly: %+v", c)
	}
	if c.Publication.EventVisibility != identity.EventsPublic {
		t.Fatalf("event visibility = %q", c.Publication.EventVisibility)
	}
	if c.Publication.RawExport != identity.RawExportNamedPeers || !c.Publication.NamesFederationPeer("peer-b") || !c.Publication.AllowsSignal(2, 3) {
		t.Fatalf("export policy normalized incorrectly: %+v", c.Publication)
	}
	if _, err := finalizeObserverContext(
		"observer16", "customer a", "", "", nil,
		"private", "none", "private", "deny", nil, nil, "", identity.CredentialToken,
	); err == nil {
		t.Fatal("invalid organization scope accepted")
	}
	if _, err := finalizeObserverContext(
		"observer16", "institution-c", "", "", nil,
		"public_attributed", "none", "public", "deny", nil, nil, "", identity.CredentialToken,
	); err == nil {
		t.Fatal("attributed public policy without metadata accepted")
	}
}

func TestFederationExportGrantValidation(t *testing.T) {
	c := defaults()
	c.Federation.ExportGrants = []FederationExportGrant{{
		SourceCollector: "collector-a", DestinationPeer: "peer-b",
		Organizations: []string{"customer-a"}, Signals: []string{"0:0", "2:3"},
		DataClasses: []string{"aggregate", "raw"}, Attribution: "origin_id",
		MaxRetentions: "24h", Purposes: []string{"integrity-monitoring"},
		ValidFroms: "2026-08-10T00:00:00Z", ValidUntils: "2027-08-10T00:00:00Z",
		ApprovedBy: "owner-a", Revision: "grant-v1", Enabled: true,
	}}
	if err := c.finalize(); err != nil {
		t.Fatalf("valid federation grant rejected: %v", err)
	}
	grant := c.Federation.ExportGrants[0].Grant
	if grant.DestinationPeerID != "peer-b" || grant.MaxRetention != 24*time.Hour || len(grant.Signals) != 2 {
		t.Fatalf("grant normalized incorrectly: %+v", grant)
	}

	unsafe := defaults()
	unsafe.Federation.ExportGrants = []FederationExportGrant{{
		SourceCollector: "collector-a", DestinationPeer: "peer-b",
		DataClasses: []string{"raw"}, Attribution: "full_provenance", MaxRetentions: "24h",
		Purposes: []string{"anything"}, ValidFroms: "2026-08-10T00:00:00Z", ValidUntils: "2027-08-10T00:00:00Z",
		ApprovedBy: "owner-a", Revision: "grant-v1", Enabled: true,
	}}
	if err := unsafe.finalize(); err == nil {
		t.Fatal("selector-free wildcard export grant accepted")
	}
}

// TestPushDuplicateStation: two observers with the same station id are rejected.
func TestPushDuplicateStation(t *testing.T) {
	c := pushConfig(Push{
		Addr: "0.0.0.0:5580", TLSCert: "c.pem", TLSKey: "k.pem",
		Observers: []PushObserver{
			{Station: "dup", TokenSHA256: goodHash, Feeds: []string{"ubx"}},
			{Station: "dup", TokenSHA256: goodHash, Feeds: []string{"sbf"}},
		},
	})
	if err := c.finalizePush(); err == nil {
		t.Error("duplicate station accepted")
	}
}

func TestStrictUnknownFields(t *testing.T) {
	for name, body := range map[string]string{
		"top-level":      "unexpected = true\n",
		"logging":        "[logging]\nlevle = \"info\"\n",
		"metrics":        "[metrics]\nadrr = \"127.0.0.1:9100\"\n",
		"state":          "[state]\nshardz = 4\n",
		"store":          "[store]\nbatch_sze = 10\n",
		"serve":          "[serve]\nrefresh_intervl = \"1s\"\n",
		"push-client-ca": "[push]\nclient_caa = \"ca.pem\"\n",
		"ingest":         "[[ingest]]\nname = \"x\"\ntype = \"ubx\"\naddr = \"127.0.0.1:1\"\ndisabeld = true\n",
		"push-observer":  "[[push.observer]]\nstaton = \"x\"\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("unknown field accepted:\n%s", body)
			}
		})
	}
}

// TestPushStationMustBindToCertWhenMTLS: with push.client_ca set, a station name
// the mTLS SAN comparison could never match (here an underscore) must fail at
// config load, not lock the observer out at connect time. Without client_ca the
// same name stays valid (bearer-only mode has no certificate binding).
func TestPushStationMustBindToCertWhenMTLS(t *testing.T) {
	cert, key := testKeypair(t)
	obs := []PushObserver{{Station: "observer_16", TokenSHA256: goodHash, Feeds: []string{"ubx"}}}
	c := pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key,
		ClientCA: cert, Observers: obs}) // self-signed cert doubles as the client_ca PEM
	err := c.finalizePush()
	if err == nil || !strings.Contains(err.Error(), "certificate-bindable") {
		t.Fatalf("unbindable station with client_ca error = %v, want certificate-bindable rejection", err)
	}
	c = pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key, Observers: obs})
	if err := c.finalizePush(); err != nil {
		t.Fatalf("bearer-only station name rejected: %v", err)
	}
}

// TestCheckConfigParity guards a malformed push.addr, an unloadable TLS keypair, an
// unparsable store.dsn, and a missing ntrip ca_file must each fail config finalize with a
// named-field error (parity with what startup requires), not pass -check-config and die later.
func TestCheckConfigParity(t *testing.T) {
	cert, key := testKeypair(t)

	badAddr := pushConfig(Push{Addr: "5580", TLSCert: cert, TLSKey: key,
		Observers: []PushObserver{{Station: "s", TokenSHA256: goodHash, Feeds: []string{"ubx"}}}})
	if err := badAddr.finalizePush(); err == nil || !strings.Contains(err.Error(), "push.addr") {
		t.Errorf("push.addr=5580: err = %v, want a push.addr error", err)
	}

	badCert := pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: "/nonexistent", TLSKey: "/nonexistent",
		Observers: []PushObserver{{Station: "s", TokenSHA256: goodHash, Feeds: []string{"ubx"}}}})
	if err := badCert.finalizePush(); err == nil || !strings.Contains(err.Error(), "push.tls_cert") {
		t.Errorf("tls_cert=/nonexistent: err = %v, want a push.tls_cert error", err)
	}

	badDSN := defaults()
	badDSN.Store.DSN = "not a dsn"
	if err := badDSN.finalize(); err == nil || !strings.Contains(err.Error(), "store.dsn") {
		t.Errorf("store.dsn='not a dsn': err = %v, want a store.dsn error", err)
	}

	badCA := defaults()
	badCA.Ingest = []Source{{Name: "crtn", Type: "ntrip", Addr: "caster.invalid:2101",
		Mountpoint: "M", CaptureOnly: true, NTRIPCAFile: "/nonexistent"}}
	if err := badCA.finalize(); err == nil || !strings.Contains(err.Error(), "ca_file") {
		t.Errorf("ntrip ca_file=/nonexistent: err = %v, want a ca_file error", err)
	}
}

// TestConfigRejectsExplicitNonPositiveAndUbxCaptureOnly guards an explicitly-set
// non-positive duration is a hard error (not silently coerced to a default), and capture_only
// on a ubx source is rejected rather than silently ignored.
func TestConfigRejectsExplicitNonPositiveAndUbxCaptureOnly(t *testing.T) {
	// The set-ness fix must preserve the documented values when the integer fields
	// are genuinely absent from TOML.
	unsetPath := filepath.Join(t.TempDir(), "unset.toml")
	if err := os.WriteFile(unsetPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unset, err := Load(unsetPath)
	if err != nil {
		t.Fatalf("empty config should receive defaults: %v", err)
	}
	if unset.Store.BatchSize != 1000 || unset.Push.MaxConns != 512 {
		t.Fatalf("unset sizes = batch %d / max_conns %d, want 1000 / 512",
			unset.Store.BatchSize, unset.Push.MaxConns)
	}

	zeroTTL := defaults()
	zeroTTL.State.SVTTLs = "0s"
	if err := zeroTTL.finalize(); err == nil || !strings.Contains(err.Error(), "state.sv_ttl") {
		t.Errorf("sv_ttl=0s: err = %v, want a state.sv_ttl positive-duration error", err)
	}

	negBatch := defaults()
	negBatch.Store.BatchEverys = "-5s"
	if err := negBatch.finalize(); err == nil || !strings.Contains(err.Error(), "store.batch_interval") {
		t.Errorf("batch_interval=-5s: err = %v, want a store.batch_interval error", err)
	}

	// Integer fields have no separate set marker. Load begins with defaults and
	// TOML overwrites them, so these explicit zeroes must remain distinguishable
	// from absent fields and fail rather than being re-defaulted.
	for name, body := range map[string]string{
		"batch-size-zero": "[store]\nbatch_size = 0\n",
		"max-conns-zero":  "[push]\nmax_conns = 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatalf("explicit zero accepted:\n%s", body)
			}
		})
	}

	negSnapshot := defaults()
	negSnapshot.Serve.SnapshotEverys = "-1s"
	if err := negSnapshot.finalize(); err == nil || !strings.Contains(err.Error(), "snapshot_interval") {
		t.Errorf("snapshot_interval=-1s: err = %v, want a non-negative-duration error", err)
	}

	ubxCapture := defaults()
	ubxCapture.Ingest = []Source{{Name: "u", Type: "ubx", Addr: "127.0.0.1:1", CaptureOnly: true}}
	if err := ubxCapture.finalize(); err == nil || !strings.Contains(err.Error(), "capture_only") {
		t.Errorf("ubx capture_only=true: err = %v, want a capture_only rejection", err)
	}
}

func TestPushDuplicateTokenHash(t *testing.T) {
	cert, key := testKeypair(t)
	c := pushConfig(Push{Addr: "0.0.0.0:5580", TLSCert: cert, TLSKey: key,
		Observers: []PushObserver{
			{Station: "first", TokenSHA256: goodHash, Feeds: []string{"ubx"}},
			{Station: "second", TokenSHA256: strings.ToUpper(goodHash), Feeds: []string{"rtcm"}},
		}})
	err := c.finalizePush()
	if err == nil || !strings.Contains(err.Error(), "first") || !strings.Contains(err.Error(), "second") {
		t.Fatalf("case-varied duplicate token error = %v, want both station names", err)
	}
}
