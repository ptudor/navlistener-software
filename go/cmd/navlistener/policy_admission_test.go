package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/ptudor/navlistener/internal/audience"
	"github.com/ptudor/navlistener/internal/config"
	"github.com/ptudor/navlistener/internal/identity"
	"github.com/ptudor/navlistener/internal/ingest"
	"github.com/ptudor/navlistener/internal/state"
	"github.com/ptudor/navlistener/internal/wire"
)

type policyTestAuth struct {
	current atomic.Pointer[identity.ObserverContext]
}

func (a *policyTestAuth) Authenticate(context.Context, string, string, string) (identity.ObserverContext, bool) {
	c := a.current.Load()
	if c == nil {
		return identity.ObserverContext{}, false
	}
	return *c, true
}

func (a *policyTestAuth) ReconcileObserver(ctx context.Context, digest, station, feed string) (identity.ObserverContext, bool) {
	return a.Authenticate(ctx, digest, station, feed)
}

func policyTestPush(t *testing.T, auth ingest.Authenticator) (string, <-chan *ingest.RawFrame) {
	t.Helper()
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	defer fixture.Close()
	cert := fixture.TLS.Certificates[0]
	key, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	for path, block := range map[string]*pem.Block{certPath: {Type: "CERTIFICATE", Bytes: cert.Certificate[0]}, keyPath: {Type: "PRIVATE KEY", Bytes: key}} {
		if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
			t.Fatal(err)
		}
	}
	out := make(chan *ingest.RawFrame, 8)
	p, err := ingest.NewPushServer(config.Push{Addr: "127.0.0.1:0", TLSCert: certPath, TLSKey: keyPath}, out, auth, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	p.SetReauthorizationInterval(20 * time.Millisecond)
	ln, err := p.Listen()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), out
}

func policyTestSession(t *testing.T, addr, session string, compressed bool) (*tls.Conn, io.Writer, func()) {
	t.Helper()
	c, err := tls.Dial("tcp", addr, &tls.Config{InsecureSkipVerify: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	wire.WriteMagic(c)
	wire.WriteHello(c, wire.HelloMsg{Token: "token", Station: "observer", Feed: "ubx", Session: session, Zstd: compressed})
	_, b, err := wire.ReadFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	var w wire.WelcomeMsg
	if err := json.Unmarshal(b, &w); err != nil || !w.OK {
		t.Fatal("handshake rejected")
	}
	if !compressed {
		return c, c, func() {}
	}
	enc, err := zstd.NewWriter(c)
	if err != nil {
		t.Fatal(err)
	}
	return c, enc, func() {
		if err := enc.Flush(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLateSessionFramesCannotRepopulateResetViews(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		for _, revoked := range []bool{false, true} {
			t.Run(fmtPolicyCase(compressed, revoked), func(t *testing.T) {
				old := identity.NewPrivateContext("observer", identity.CredentialToken)
				old.OrganizationID = "old-org"
				old.CollectionIDs = []string{"old-fleet"}
				old.FeedGrants = []string{"ubx"}
				old.DeclaredCapabilities = []identity.Signal{{GnssID: 0, SigID: 0}}
				old.Publication.AggregateUse = identity.AggregatePublicAttributed
				old.Publication.StationMetadata = identity.MetadataFull
				old.Publication.EventVisibility = identity.EventsPublic
				old.Publication.Revision = "v1"
				auth := &policyTestAuth{}
				auth.current.Store(&old)
				addr, out := policyTestPush(t, auth)
				_, writer, flush := policyTestSession(t, addr, "old-boot", compressed)
				rec := wire.RawRecord{RecvUnixNs: time.Now().UnixNano(), FrameType: ingest.TelemJammingStats, Raw: ingest.EncodeJammingStats([]ingest.RFBand{{Block: 0, AGC: 100}})}
				for seq := uint64(1); seq <= 2; seq++ {
					if err := wire.WriteFrame(writer, wire.Data, wire.EncodeData(seq, rec)); err != nil {
						t.Fatal(err)
					}
				}
				flush()
				receive := func() *ingest.RawFrame {
					select {
					case f := <-out:
						return f
					case <-time.After(time.Second):
						t.Fatal("missing ingest handoff")
						return nil
					}
				}
				initial, held := receive(), receive() // hold decoder/handoff work across transition
				live, pub, events := state.New(1), state.New(1), state.New(1)
				registry := audience.NewRegistry(1, nil)
				registry.Register(identity.Audience{Kind: identity.AudienceOperator, ID: identity.LocalCollectorInstance}, live, nil)
				registry.Register(identity.Audience{Kind: identity.AudiencePublic}, pub, nil)
				controller := scopeController{registry: registry, publicEvents: events, epochs: audience.NewPolicyEpochs(time.Now())}
				var last atomic.Int64
				decode := func(frames ...*ingest.RawFrame) {
					ch := make(chan *ingest.RawFrame, len(frames))
					for _, f := range frames {
						ch <- f
					}
					close(ch)
					decodeLoop(ch, live, pub, events, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), &last, registry, controller.Apply)
				}
				decode(initial)
				if len(pub.FeedCapabilityReports(time.Now())) != 1 {
					t.Fatal("public fixture did not contribute")
				}
				if revoked {
					auth.current.Store(nil)
				} else {
					next := old
					next.OrganizationID = "new-org"
					next.CollectionIDs = []string{"new-fleet"}
					next.Publication.Revision = "v2"
					auth.current.Store(&next)
					policyTestSession(t, addr, "new-boot", false)
				}
				marker := receive()
				if marker.ScopeRevocation == nil {
					t.Fatal("missing reset marker")
				}
				decode(marker, held)
				for _, view := range registry.Views() {
					if len(view.Store.FeedCapabilityReports(time.Now())) != 0 || len(view.Store.FeedStationRF(time.Now())) != 0 {
						t.Fatalf("stale DATA repopulated %s", view.Audience.Key())
					}
				}
				if len(events.FeedCapabilityReports(time.Now())) != 0 {
					t.Fatal("stale DATA reached public detector input")
				}
			})
		}
	}
}
func fmtPolicyCase(compressed, revoked bool) string {
	if compressed {
		if revoked {
			return "zstd/revoked"
		}
		return "zstd/transfer"
	}
	if revoked {
		return "plain/revoked"
	}
	return "plain/transfer"
}
